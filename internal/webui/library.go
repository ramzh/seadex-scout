package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Library actions: the dashboard's buttons that act on Sonarr and Radarr
// directly. They search a season, give the files that already hold the SeaDex
// best the indexer flags the feed's grabs carry (so the SeaDex custom format
// scores them), list the seasons whose files score below the profile's
// minimum, and import or clear queue items Sonarr left waiting.

// Indexer flag bits, as Sonarr and Radarr store them on a file.
var sonarrFlagBits = map[string]int{
	"Freeleech": 1, "Halfleech": 2, "DoubleUpload": 4, "Internal": 8,
	"Scene": 16, "Freeleech75": 32, "Freeleech25": 64, "Nuked": 128,
}

var radarrFlagBits = map[string]int{
	"G_Freeleech": 1, "G_Halfleech": 2, "G_DoubleUpload": 4, "PTP_Golden": 8,
	"PTP_Approved": 16, "G_Internal": 32, "AHD_Internal": 64, "G_Scene": 128,
	"G_Freeleech75": 256, "G_Freeleech25": 512, "AHD_UserRelease": 1024, "Nuked": 2048,
}

// The flags the feed gives a SeaDex best release: Freeleech25 + Scene.
const (
	sonarrBestFlags = 64 | 16
	radarrBestFlags = 512 | 128
)

// DefaultMinScore is the custom format score a SeaDex best file reaches with
// the setup docs/torznab-indexer.md describes; SEADEX_SCOUT_MIN_SCORE changes it.
const DefaultMinScore = 5000

// scoreScanEvery is how often the low-score scan runs on its own; the
// automatic queue imports run every autoEvery.
const (
	scoreScanEvery = 6 * time.Hour
	autoEvery      = 30 * time.Minute
)

// arrAPI is one Sonarr or Radarr, with the API version it serves.
type arrAPI struct {
	name string // "sonarr" or "radarr"
	Arr
	client *http.Client

	mu  sync.Mutex
	ver string // "v3" or "v5", found on first use
}

var errArrOff = errors.New("not set up")

func newArrAPI(name string, a Arr) *arrAPI {
	if a.URL == "" {
		return nil
	}
	return &arrAPI{name: name, Arr: a, client: &http.Client{Timeout: 2 * time.Minute}}
}

// version finds the API version once: Sonarr 5 serves /api/v5, everything
// else /api/v3.
func (a *arrAPI) version(ctx context.Context) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ver != "" {
		return a.ver
	}
	ver := "v3"
	if a.name == "sonarr" {
		var status map[string]any
		if err := a.raw(ctx, http.MethodGet, "/api/v5/system/status", nil, &status); err == nil {
			ver = "v5"
		}
	}
	a.ver = ver
	return ver
}

// call sends one API request under the arr's version; path starts with "/".
func (a *arrAPI) call(ctx context.Context, method, path string, body, out any) error {
	return a.raw(ctx, method, "/api/"+a.version(ctx)+path, body, out)
}

func (a *arrAPI) raw(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader = http.NoBody
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.URL, "/")+path, rd)
	if err != nil {
		return errors.New("the configured URL is not valid")
	}
	req.Header.Set("X-Api-Key", a.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) && uerr.Unwrap() != nil {
			return uerr.Unwrap() // drops the URL, which carries no key but is noise
		}
		return errors.New("request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s %s answered %d: %s", method, strings.SplitN(path, "?", 2)[0], res.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxArrListBytes))
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, maxArrListBytes)).Decode(out)
}

// Library runs the library actions.
type Library struct {
	sonarr, radarr *arrAPI
	store          *Store
	log            *slog.Logger
	minScore       int

	// Best returns the releases the curation marks as SeaDex best (see
	// indexer.BestReleases); Qbit resolves AnimeBytes downloads to their
	// torrent ids. Either may be nil, which only narrows what can be proven.
	Best func(context.Context) (map[string][]int, error)
	Qbit *Qbit
	// Auto runs the flag fix and the queue import on their own, with the
	// same rules as the buttons.
	Auto bool

	mu       sync.Mutex
	scan     ScoreScan
	lastScan time.Time

	searches   []SearchJob // newest first
	lastSearch time.Time
	searchWake chan struct{}
}

// NewLibrary returns the library actions for the arrs set up (a zero Arr
// leaves one out). minScore <= 0 uses DefaultMinScore.
func NewLibrary(sonarr, radarr Arr, minScore int, store *Store, log *slog.Logger) *Library {
	if minScore <= 0 {
		minScore = DefaultMinScore
	}
	return &Library{sonarr: newArrAPI("sonarr", sonarr), radarr: newArrAPI("radarr", radarr),
		store: store, log: log, minScore: minScore, searchWake: make(chan struct{}, 1)}
}

func (l *Library) arr(name string) (*arrAPI, error) {
	var a *arrAPI
	switch name {
	case "sonarr":
		a = l.sonarr
	case "radarr":
		a = l.radarr
	default:
		return nil, fmt.Errorf("unknown app %q", name)
	}
	if a == nil {
		return nil, fmt.Errorf("%s is %w", name, errArrOff)
	}
	return a, nil
}

// Target names what an action works on: a Sonarr series (and season), or a
// Radarr movie. Season -1 means the whole series.
type Target struct {
	Arr    string `json:"arr"`
	ID     int    `json:"id"`
	Season int    `json:"season"`
}

func (t Target) key() string {
	if t.Arr == "sonarr" && t.Season >= 0 {
		return fmt.Sprintf("sonarr:%d:%d", t.ID, t.Season)
	}
	return fmt.Sprintf("%s:%d", t.Arr, t.ID)
}

// Search asks Sonarr or Radarr to search the target now and returns the
// command id.
func (l *Library) Search(ctx context.Context, t Target) (int, error) {
	a, err := l.arr(t.Arr)
	if err != nil {
		return 0, err
	}
	if t.ID <= 0 {
		return 0, errors.New("no id to search")
	}
	var cmd map[string]any
	switch {
	case t.Arr == "radarr":
		cmd = map[string]any{"name": "MoviesSearch", "movieIds": []int{t.ID}}
	case t.Season >= 0:
		cmd = map[string]any{"name": "SeasonSearch", "seriesId": t.ID, "seasonNumber": t.Season}
	default:
		cmd = map[string]any{"name": "SeriesSearch", "seriesId": t.ID}
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := a.call(ctx, http.MethodPost, "/command", cmd, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// arrFile is the part of an episode or movie file the actions read.
type arrFile struct {
	ID                int    `json:"id"`
	SeasonNumber      int    `json:"seasonNumber"`
	RelativePath      string `json:"relativePath"`
	ReleaseGroup      string `json:"releaseGroup"`
	Size              int64  `json:"size"`
	CustomFormatScore int    `json:"customFormatScore"`
	IndexerFlags      any    `json:"indexerFlags"` // a number, or Radarr's list of names
}

func (l *Library) files(ctx context.Context, a *arrAPI, id int) ([]arrFile, error) {
	var out []arrFile
	if a.name == "radarr" {
		err := a.call(ctx, http.MethodGet, "/moviefile?movieId="+strconv.Itoa(id), nil, &out)
		return out, err
	}
	err := a.call(ctx, http.MethodGet, "/episodefile?seriesId="+strconv.Itoa(id), nil, &out)
	return out, err
}

// flagBits reads a file's indexer flags as bits, from a number or a list of
// names.
func flagBits(arr string, v any) int {
	switch f := v.(type) {
	case float64:
		return int(f)
	case []any:
		bits := sonarrFlagBits
		if arr == "radarr" {
			bits = radarrFlagBits
		}
		n := 0
		for _, name := range f {
			if s, ok := name.(string); ok {
				n |= bits[s]
			}
		}
		return n
	}
	return 0
}

// parseFlagNames reads the "Scene, Freeleech25" form grab history stores.
func parseFlagNames(arr, names string) int {
	bits := sonarrFlagBits
	if arr == "radarr" {
		bits = radarrFlagBits
	}
	n := 0
	for _, name := range strings.Split(names, ",") {
		n |= bits[strings.TrimSpace(name)]
	}
	return n
}

func bestFlags(arr string) int {
	if arr == "radarr" {
		return radarrBestFlags
	}
	return sonarrBestFlags
}

// normGroup compares release group names the way the scout does: case and
// surrounding brackets do not count.
func normGroup(g string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(g), "[]()"))
}

// FlagFix is what FixFlags did.
type FlagFix struct {
	Fixed     int      `json:"fixed"`
	Proven    int      `json:"proven"` // fixed by import proof rather than group
	AlreadyOK int      `json:"already_ok"`
	Skipped   int      `json:"skipped"`
	Groups    []string `json:"groups"`
	Skips     []string `json:"skips,omitempty"`
}

// FixFlags gives the target's files the flags a best grab carries, when they
// score below the minimum and are proven to be the SeaDex best: either the
// scout's own finding says the library holds the best and the file belongs to
// that group, or the file was imported from a torrent the curation marks as
// best for this series (provenFiles). The page cannot flag any file it likes.
func (l *Library) FixFlags(ctx context.Context, t Target) (FlagFix, error) {
	var res FlagFix
	a, err := l.arr(t.Arr)
	if err != nil {
		return res, err
	}
	groups := map[string]bool{}
	var noGroups error
	f, ok := l.store.findingFor(t)
	switch status := f.Str("status"); {
	case !ok:
		noGroups = errors.New("no finding for this title and no file imported from a SeaDex best torrent")
	case status != "at_best" && status != "newer_revision":
		noGroups = fmt.Errorf("the scout does not say you hold the SeaDex best here (status %s), and no file was imported from a SeaDex best torrent", status)
	default:
		for _, g := range strings.Split(f.Str("recommended_groups")+","+f.Str("recommended_group"), ",") {
			if g = normGroup(g); g != "" {
				groups[g] = true
				res.Groups = append(res.Groups, g)
			}
		}
		slices.Sort(res.Groups)
		res.Groups = slices.Compact(res.Groups)
	}
	files, err := l.files(ctx, a, t.ID)
	if err != nil {
		return res, err
	}
	tvdb, err := l.tvdbOf(ctx, a, t.ID)
	if err != nil {
		return res, err
	}
	best := bestFlags(t.Arr)
	var low []arrFile
	for _, file := range files {
		if t.Arr == "sonarr" && t.Season >= 0 && file.SeasonNumber != t.Season {
			continue
		}
		if flagBits(t.Arr, file.IndexerFlags)&best == best || file.CustomFormatScore >= l.minScore {
			res.AlreadyOK++
			continue
		}
		low = append(low, file)
	}
	proven, proofErr := l.provenFiles(ctx, a, t.ID, tvdb, low, nil)
	type update struct {
		ID           int `json:"id"`
		IndexerFlags int `json:"indexerFlags"`
	}
	var updates []update
	for _, file := range low {
		switch {
		case groups[normGroup(file.ReleaseGroup)]:
		case proven[file.ID]:
			res.Proven++
		default:
			res.Skipped++
			if len(res.Skips) < 5 {
				res.Skips = append(res.Skips, fmt.Sprintf("%s (group %q)", file.RelativePath, file.ReleaseGroup))
			}
			continue
		}
		updates = append(updates, update{ID: file.ID, IndexerFlags: flagBits(t.Arr, file.IndexerFlags) | best})
	}
	if len(updates) == 0 {
		if noGroups != nil && len(low) > 0 {
			if proofErr != nil {
				return res, fmt.Errorf("%w (import proof: %v)", noGroups, proofErr)
			}
			return res, noGroups
		}
		return res, nil
	}
	path := "/episodefile/bulk"
	if t.Arr == "radarr" {
		path = "/moviefile/bulk"
	}
	if err := a.call(ctx, http.MethodPut, path, updates, nil); err != nil {
		return res, err
	}
	res.Fixed = len(updates)
	l.forget(t)
	return res, nil
}

// abTorrent finds the AnimeBytes torrent id in the comment qBittorrent keeps
// for an AnimeBytes download ("https://animebytes.tv/torrent/368375/group").
// Only a private torrent is trusted: a public one could carry any comment.
var abTorrent = regexp.MustCompile(`^https://animebytes\.tv/torrent/(\d+)(?:/|$)`)

// bestIndex maps the info hash of every SeaDex best release the scout knows
// to the TVDB ids of the entries that call it best. Nyaa releases carry their
// hash in the feed; an AnimeBytes release is found through the torrent id in
// qBittorrent's comment for it.
func (l *Library) bestIndex(ctx context.Context) (map[string][]int, error) {
	if l.Best == nil {
		return nil, errors.New("the feed is not available")
	}
	best, err := l.Best(ctx)
	if err != nil {
		return nil, err
	}
	idx := map[string][]int{}
	for k, v := range best {
		if !strings.Contains(k, ":") {
			idx[k] = v
		}
	}
	if l.Qbit == nil {
		return idx, nil
	}
	torrents, err := l.Qbit.Torrents(ctx)
	if err != nil {
		return idx, err
	}
	for _, t := range torrents {
		if m := abTorrent.FindStringSubmatch(strings.TrimSpace(t.Comment)); m != nil && t.Private {
			if v, ok := best["ab:"+m[1]]; ok {
				idx[strings.ToLower(t.Hash)] = v
			}
		}
	}
	return idx, nil
}

// importHashes maps each file id of a series or movie to the download it was
// imported from, read from the arr's import history.
func (l *Library) importHashes(ctx context.Context, a *arrAPI, id int) (map[int]string, error) {
	type rec struct {
		EventType  string            `json:"eventType"`
		DownloadID string            `json:"downloadId"`
		Data       map[string]string `json:"data"`
	}
	var recs []rec
	var err error
	switch {
	case a.name == "radarr":
		err = a.call(ctx, http.MethodGet, "/history/movie?movieId="+strconv.Itoa(id), nil, &recs)
	case a.version(ctx) == "v5":
		var page struct {
			Records []rec `json:"records"`
		}
		err = a.call(ctx, http.MethodGet, fmt.Sprintf("/history?seriesIds=%d&page=1&pageSize=5000", id), nil, &page)
		recs = page.Records
	default:
		err = a.call(ctx, http.MethodGet, "/history/series?seriesId="+strconv.Itoa(id), nil, &recs)
	}
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for _, r := range recs {
		if r.EventType != "downloadFolderImported" || r.DownloadID == "" {
			continue
		}
		if fid, err := strconv.Atoi(r.Data["fileId"]); err == nil {
			out[fid] = strings.ToLower(r.DownloadID)
		}
	}
	return out, nil
}

// provenFiles returns the files imported from a torrent the curation marks as
// best for this series (its TVDB id must be among the entries calling it
// best; a film's, which Radarr knows by no TVDB id, need only be best). idx is
// the bestIndex, read here when nil.
func (l *Library) provenFiles(ctx context.Context, a *arrAPI, id, tvdb int, files []arrFile, idx map[string][]int) (map[int]bool, error) {
	if len(files) == 0 {
		return nil, nil
	}
	var idxErr error
	if idx == nil {
		if idx, idxErr = l.bestIndex(ctx); len(idx) == 0 {
			return nil, idxErr
		}
	}
	hashes, err := l.importHashes(ctx, a, id)
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, f := range files {
		tvdbs, ok := idx[hashes[f.ID]]
		if ok && (a.name == "radarr" || (tvdb > 0 && slices.Contains(tvdbs, tvdb))) {
			out[f.ID] = true
		}
	}
	return out, idxErr
}

// tvdbOf returns a Sonarr series' TVDB id (0 for Radarr).
func (l *Library) tvdbOf(ctx context.Context, a *arrAPI, id int) (int, error) {
	if a.name == "radarr" {
		return 0, nil
	}
	var it arrItem
	err := a.call(ctx, http.MethodGet, "/series/"+strconv.Itoa(id), nil, &it)
	return it.TvdbID, err
}

// ---------- low-score scan ----------

// SeasonScore is one season or movie with files below the minimum score.
type SeasonScore struct {
	Arr    string   `json:"arr"`
	ID     int      `json:"id"`
	Season int      `json:"season"`
	Title  string   `json:"title"`
	Files  int      `json:"files"`
	Low    int      `json:"low"`
	Min    int      `json:"min_score"`
	Groups []string `json:"groups"`
	Bytes  int64    `json:"bytes"`
	// Proven counts the low files imported from a SeaDex best torrent, which
	// Fix flags can flag whatever their group.
	Proven int `json:"proven"`
}

// ScoreScan is the newest low-score scan.
type ScoreScan struct {
	Running    bool          `json:"running"`
	StartedAt  time.Time     `json:"started_at,omitzero"`
	FinishedAt time.Time     `json:"finished_at,omitzero"`
	Error      string        `json:"error,omitempty"`
	MinScore   int           `json:"min_score"`
	Scanned    int           `json:"scanned"`
	Files      int           `json:"files"`
	LowFiles   int           `json:"low_files"`
	Seasons    []SeasonScore `json:"seasons"`
}

// Scan returns the newest scan.
func (l *Library) Scan() ScoreScan {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.scan
	s.MinScore = l.minScore
	s.Seasons = slices.Clone(s.Seasons)
	return s
}

// forget drops a target from the scan after an action changed it, so the page
// does not offer the same fix twice; the next scan reads it again.
func (l *Library) forget(t Target) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.scan.Seasons = slices.DeleteFunc(l.scan.Seasons, func(s SeasonScore) bool {
		return s.Arr == t.Arr && s.ID == t.ID && (t.Arr == "radarr" || t.Season < 0 || s.Season == t.Season)
	})
}

// ErrScanRunning means a scan is already under way.
var ErrScanRunning = errors.New("a score scan is already running")

// beginScan marks a scan as running, or reports that one already is.
func (l *Library) beginScan() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.scan.Running {
		return false
	}
	l.scan.Running = true
	l.scan.StartedAt = time.Now().UTC()
	return true
}

// StartScan starts a scan in the background.
func (l *Library) StartScan(ctx context.Context) error {
	if !l.beginScan() {
		return ErrScanRunning
	}
	go l.runScan(ctx)
	return nil
}

// Run scans a minute after the start and then every scoreScanEvery. With Auto
// set it fixes flags after each scan and imports stuck SeaDex grabs every
// autoEvery.
func (l *Library) Run(ctx context.Context) {
	if l.sonarr == nil && l.radarr == nil {
		return
	}
	go l.runSearches(ctx)
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = autoEvery
		scanned := false
		if time.Since(l.lastScan) >= scoreScanEvery && l.beginScan() {
			l.runScan(ctx)
			l.lastScan = time.Now()
			scanned = true
		}
		if l.Auto {
			if scanned {
				l.autoFix(ctx)
			}
			l.autoImport(ctx)
		}
	}
}

// autoFix runs Fix flags on every scanned season that can be proven to hold
// the best: by import, or by the scout's finding.
func (l *Library) autoFix(ctx context.Context) {
	for _, s := range l.Scan().Seasons {
		if ctx.Err() != nil {
			return
		}
		t := Target{Arr: s.Arr, ID: s.ID, Season: s.Season}
		if s.Arr == "radarr" {
			t.Season = -1
		}
		if s.Proven == 0 {
			f, ok := l.store.findingFor(t)
			if st := f.Str("status"); !ok || (st != "at_best" && st != "newer_revision") {
				continue
			}
		}
		res, err := l.FixFlags(ctx, t)
		if err != nil {
			l.log.Debug("automatic flag fix skipped", "arr", t.Arr, "arr_id", t.ID, "season", t.Season, "error", err)
			continue
		}
		if res.Fixed > 0 {
			l.log.Info("indexer flags fixed automatically", "arr", t.Arr, "arr_id", t.ID, "season", t.Season,
				"title", s.Title, "fixed", res.Fixed, "proven", res.Proven, "skipped", res.Skipped)
		}
	}
}

// autoImport imports the stuck Sonarr downloads that were grabbed as a
// SeaDex best (the grab carries the best flags), with Import's rules, and
// removes from the queue the stuck ones that hold nothing left to import.
func (l *Library) autoImport(ctx context.Context) {
	if l.sonarr == nil {
		return
	}
	items, err := l.Queue(ctx)
	if err != nil {
		l.log.Debug("automatic import could not read the queue", "error", err)
		return
	}
	best := bestFlags("sonarr")
	for _, it := range items {
		if it.Arr != "sonarr" || !it.Stuck || !it.CanImport || ctx.Err() != nil {
			continue
		}
		preview, err := l.ImportQueued(ctx, it.ID, true)
		if err == nil && preview.Done {
			if err := l.RemoveFromQueue(ctx, "sonarr", it.ID); err != nil {
				l.log.Warn("could not remove an imported download from the queue", "queue_id", it.ID, "title", it.Title, "error", err)
			} else {
				l.log.Info("imported download removed from the queue", "queue_id", it.ID, "title", it.Title, "reasons", preview.Reasons)
			}
			continue
		}
		if err != nil || preview.Files == 0 || preview.IndexerFlags&best != best {
			continue
		}
		res, err := l.ImportQueued(ctx, it.ID, false)
		if err != nil {
			l.log.Warn("automatic import failed", "queue_id", it.ID, "title", it.Title, "error", err)
			continue
		}
		l.log.Info("stuck SeaDex download imported automatically", "queue_id", it.ID, "title", it.Title,
			"files", res.Files, "skipped", res.Skipped, "indexer_flags", res.IndexerFlags, "command", res.Command)
	}
}

func (l *Library) runScan(ctx context.Context) {
	var seasons []SeasonScore
	var errs []string
	scanned, files, low := 0, 0, 0
	idx, idxErr := l.bestIndex(ctx)
	if idxErr != nil {
		errs = append(errs, "import proof: "+idxErr.Error())
	}
	for _, a := range []*arrAPI{l.sonarr, l.radarr} {
		if a == nil {
			continue
		}
		items, err := l.items(ctx, a)
		if err != nil {
			errs = append(errs, a.name+": "+err.Error())
			continue
		}
		for _, it := range items {
			if ctx.Err() != nil {
				return
			}
			fs, err := l.files(ctx, a, it.ID)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s %s: %v", a.name, it.Title, err))
				continue
			}
			scanned++
			by := map[int]*SeasonScore{}
			var lowFiles []arrFile
			for _, f := range fs {
				files++
				season := f.SeasonNumber
				if a.name == "radarr" {
					season = -1
				}
				s := by[season]
				if s == nil {
					s = &SeasonScore{Arr: a.name, ID: it.ID, Season: season, Title: it.Title, Min: f.CustomFormatScore}
					by[season] = s
				}
				s.Files++
				if f.CustomFormatScore < s.Min {
					s.Min = f.CustomFormatScore
				}
				if f.CustomFormatScore < l.minScore {
					lowFiles = append(lowFiles, f)
					s.Low++
					low++
					s.Bytes += f.Size
					if g := f.ReleaseGroup; g != "" && !slices.Contains(s.Groups, g) {
						s.Groups = append(s.Groups, g)
					}
				}
			}
			if len(idx) > 0 && len(lowFiles) > 0 {
				proven, err := l.provenFiles(ctx, a, it.ID, it.TvdbID, lowFiles, idx)
				if err != nil {
					errs = append(errs, fmt.Sprintf("%s %s history: %v", a.name, it.Title, err))
				}
				for _, f := range lowFiles {
					if proven[f.ID] {
						season := f.SeasonNumber
						if a.name == "radarr" {
							season = -1
						}
						by[season].Proven++
					}
				}
			}
			for _, s := range by {
				if s.Low > 0 && it.wanted(s.Season) {
					seasons = append(seasons, *s)
				}
			}
		}
	}
	slices.SortFunc(seasons, func(x, y SeasonScore) int {
		if c := strings.Compare(strings.ToLower(x.Title), strings.ToLower(y.Title)); c != 0 {
			return c
		}
		return x.Season - y.Season
	})
	l.mu.Lock()
	l.scan = ScoreScan{StartedAt: l.scan.StartedAt, FinishedAt: time.Now().UTC(), Error: strings.Join(errs, "; "),
		Scanned: scanned, Files: files, LowFiles: low, Seasons: seasons}
	l.mu.Unlock()
	l.log.Info("library score scan complete", "items", scanned, "files", files, "low_files", low,
		"seasons", len(seasons), "min_score", l.minScore, "errors", len(errs))
}

// arrItem is a series or movie the scan reads, with what is monitored.
type arrItem struct {
	ID        int    `json:"id"`
	TvdbID    int    `json:"tvdbId"`
	Title     string `json:"title"`
	Monitored bool   `json:"monitored"`
	Profile   int    `json:"qualityProfileId"`
	Seasons   []struct {
		SeasonNumber int  `json:"seasonNumber"`
		Monitored    bool `json:"monitored"`
	} `json:"seasons"`
	Statistics struct {
		EpisodeFileCount int `json:"episodeFileCount"`
	} `json:"statistics"`
	HasFile bool `json:"hasFile"`
}

func (it arrItem) wanted(season int) bool {
	if !it.Monitored {
		return false
	}
	if season < 0 {
		return true
	}
	for _, s := range it.Seasons {
		if s.SeasonNumber == season {
			return s.Monitored
		}
	}
	return false
}

func (l *Library) items(ctx context.Context, a *arrAPI) ([]arrItem, error) {
	var all []arrItem
	path := "/series"
	if a.name == "radarr" {
		path = "/movie"
	}
	if err := a.call(ctx, http.MethodGet, path, nil, &all); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(it arrItem) bool {
		if !it.Monitored {
			return true
		}
		if a.name == "radarr" {
			return !it.HasFile
		}
		return it.Statistics.EpisodeFileCount == 0
	}), nil
}

// ---------- queue ----------

// QueueItem is one download Sonarr or Radarr tracks.
type QueueItem struct {
	Arr       string   `json:"arr"`
	ID        int      `json:"id"`
	Title     string   `json:"title"`
	Series    string   `json:"series,omitempty"`
	Status    string   `json:"status"`
	State     string   `json:"state"`
	Health    string   `json:"health"`
	Indexer   string   `json:"indexer"`
	Size      float64  `json:"size"`
	SizeLeft  float64  `json:"size_left"`
	Messages  []string `json:"messages,omitempty"`
	Stuck     bool     `json:"stuck"`
	CanImport bool     `json:"can_import"`
}

type arrQueueRecord struct {
	ID                    int     `json:"id"`
	SeriesID              int     `json:"seriesId"`
	MovieID               int     `json:"movieId"`
	Title                 string  `json:"title"`
	Status                string  `json:"status"`
	TrackedDownloadState  string  `json:"trackedDownloadState"`
	TrackedDownloadStatus string  `json:"trackedDownloadStatus"`
	DownloadID            string  `json:"downloadId"`
	Indexer               string  `json:"indexer"`
	OutputPath            string  `json:"outputPath"`
	Size                  float64 `json:"size"`
	SizeLeft              float64 `json:"sizeleft"`
	SizeLeftV5            float64 `json:"sizeLeft"` // Sonarr 5 spells it this way
	Series                *struct {
		Title string `json:"title"`
	} `json:"series"`
	Movie *struct {
		Title string `json:"title"`
	} `json:"movie"`
	StatusMessages []struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
}

func (l *Library) queue(ctx context.Context, a *arrAPI) ([]arrQueueRecord, error) {
	var page struct {
		Records []arrQueueRecord `json:"records"`
	}
	q := "/queue?page=1&pageSize=500&includeUnknownSeriesItems=true&includeUnknownMovieItems=true"
	if a.name == "sonarr" {
		q += "&includeSeries=true"
	} else {
		q += "&includeMovie=true"
	}
	err := a.call(ctx, http.MethodGet, q, nil, &page)
	for i := range page.Records {
		page.Records[i].SizeLeft = max(page.Records[i].SizeLeft, page.Records[i].SizeLeftV5)
	}
	return page.Records, err
}

// Queue lists what Sonarr and Radarr are downloading or waiting to import.
func (l *Library) Queue(ctx context.Context) ([]QueueItem, error) {
	var out []QueueItem
	var errs []string
	for _, a := range []*arrAPI{l.sonarr, l.radarr} {
		if a == nil {
			continue
		}
		recs, err := l.queue(ctx, a)
		if err != nil {
			errs = append(errs, a.name+": "+err.Error())
			continue
		}
		for _, r := range recs {
			it := QueueItem{Arr: a.name, ID: r.ID, Title: r.Title, Status: r.Status, State: r.TrackedDownloadState,
				Health: r.TrackedDownloadStatus, Indexer: r.Indexer, Size: r.Size, SizeLeft: r.SizeLeft}
			if r.Series != nil {
				it.Series = r.Series.Title
			} else if r.Movie != nil {
				it.Series = r.Movie.Title
			}
			for _, m := range r.StatusMessages {
				for _, msg := range m.Messages {
					it.Messages = append(it.Messages, msg)
				}
				if len(m.Messages) == 0 && m.Title != "" && m.Title != r.Title {
					it.Messages = append(it.Messages, m.Title)
				}
			}
			waiting := r.TrackedDownloadState == "importPending" || r.TrackedDownloadState == "importBlocked" ||
				r.TrackedDownloadState == "failedPending"
			it.Stuck = waiting || (r.TrackedDownloadStatus == "warning" && r.SizeLeft == 0)
			it.CanImport = a.name == "sonarr" && r.SizeLeft == 0 && r.OutputPath != "" && r.DownloadID != ""
			out = append(out, it)
		}
	}
	if len(errs) > 0 && len(out) == 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

func (l *Library) queueRecord(ctx context.Context, a *arrAPI, id int) (arrQueueRecord, error) {
	recs, err := l.queue(ctx, a)
	if err != nil {
		return arrQueueRecord{}, err
	}
	for _, r := range recs {
		if r.ID == id {
			return r, nil
		}
	}
	return arrQueueRecord{}, errors.New("that download is no longer in the queue")
}

// RemoveFromQueue drops a download from Sonarr's or Radarr's queue without
// touching the torrent and without blocklisting it.
func (l *Library) RemoveFromQueue(ctx context.Context, arr string, id int) error {
	a, err := l.arr(arr)
	if err != nil {
		return err
	}
	q := "?removeFromClient=false&blocklist=false"
	if a.version(ctx) == "v5" {
		q = "?removalMethod=ignore&blocklistMethod=doNotBlocklist"
	}
	return a.call(ctx, http.MethodDelete, "/queue/"+strconv.Itoa(id)+q, nil, nil)
}

// ImportResult is what ImportQueued sent to Sonarr.
type ImportResult struct {
	Files        int      `json:"files"`
	Skipped      int      `json:"skipped"`
	IndexerFlags int      `json:"indexer_flags"`
	Command      int      `json:"command"`
	Reasons      []string `json:"reasons,omitempty"`
	// Done means nothing is left to import: every file Sonarr matched is
	// already at the minimum score. Sonarr keeps such a pack queued only for
	// its extra files, so removing it from the queue loses nothing.
	Done bool `json:"done"`
}

type manualImportRow struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	FolderName   string `json:"folderName"`
	Series       *struct {
		ID int `json:"id"`
	} `json:"series"`
	Episodes []struct {
		ID            int `json:"id"`
		EpisodeFileID int `json:"episodeFileId"`
	} `json:"episodes"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup"`
	ReleaseType  string          `json:"releaseType"`
	Rejections   []struct {
		Reason string `json:"reason"`
	} `json:"rejections"`
}

// importableRejection is the one rejection an import overrides: a pack that
// holds more episodes than Sonarr expected from the grab's name.
func importableRejection(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "unexpected") || strings.Contains(r, "was not found in the grabbed release")
}

// ImportQueued imports a finished Sonarr download by hand, as hardlinks, the
// way Sonarr's Manual Import does. It takes only the files Sonarr matched to
// an episode, overriding only the "episode was unexpected" check that stops
// packs larger than their grab's name, and only for episodes with no file or
// a file below the minimum score. The files get the grab's indexer flags,
// which Manual Import otherwise drops.
// With dry set it only reports what it would import.
func (l *Library) ImportQueued(ctx context.Context, id int, dry bool) (ImportResult, error) {
	var res ImportResult
	a, err := l.arr("sonarr")
	if err != nil {
		return res, err
	}
	rec, err := l.queueRecord(ctx, a, id)
	if err != nil {
		return res, err
	}
	if rec.OutputPath == "" || rec.DownloadID == "" || rec.SizeLeft > 0 {
		return res, errors.New("the download has not finished")
	}
	flags, err := l.grabFlags(ctx, a, rec.DownloadID)
	if err != nil {
		return res, err
	}
	res.IndexerFlags = flags
	q := url.Values{"folder": {rec.OutputPath}, "downloadId": {rec.DownloadID}, "filterExistingFiles": {"false"}}
	var rows []manualImportRow
	if err := a.call(ctx, http.MethodGet, "/manualimport?"+q.Encode(), nil, &rows); err != nil {
		return res, err
	}
	good := map[int]bool{} // episode ids whose file already scores high enough
	if rec.SeriesID > 0 {
		files, err := l.files(ctx, a, rec.SeriesID)
		if err != nil {
			return res, err
		}
		goodFile := map[int]bool{}
		for _, f := range files {
			if f.CustomFormatScore >= l.minScore {
				goodFile[f.ID] = true
			}
		}
		for _, r := range rows {
			for _, e := range r.Episodes {
				if goodFile[e.EpisodeFileID] {
					good[e.ID] = true
				}
			}
		}
	}
	var files []map[string]any
	reasons := map[string]int{}
	atMin, blockedFiles := 0, 0
	for _, r := range rows {
		blocked := ""
		for _, rj := range r.Rejections {
			if !importableRejection(rj.Reason) {
				blocked = rj.Reason
				break
			}
		}
		allGood := len(r.Episodes) > 0
		for _, e := range r.Episodes {
			allGood = allGood && good[e.ID]
		}
		switch {
		case r.Series == nil || len(r.Episodes) == 0:
			reasons["not matched to an episode"]++
		case allGood:
			reasons["already at the minimum score"]++
			atMin++
		case blocked != "":
			reasons[blocked]++
			blockedFiles++
		default:
			ids := make([]int, 0, len(r.Episodes))
			for _, e := range r.Episodes {
				ids = append(ids, e.ID)
			}
			files = append(files, map[string]any{
				"path": r.Path, "folderName": r.FolderName, "seriesId": r.Series.ID, "episodeIds": ids,
				"quality": r.Quality, "languages": r.Languages, "releaseGroup": r.ReleaseGroup,
				"releaseType": r.ReleaseType, "indexerFlags": flags, "downloadId": rec.DownloadID,
			})
			continue
		}
		res.Skipped++
	}
	for reason, n := range reasons {
		res.Reasons = append(res.Reasons, fmt.Sprintf("%d × %s", n, reason))
	}
	slices.Sort(res.Reasons)
	res.Files = len(files)
	res.Done = len(files) == 0 && atMin > 0 && blockedFiles == 0
	if len(files) == 0 || dry {
		return res, nil
	}
	var cmd struct {
		ID int `json:"id"`
	}
	if err := a.call(ctx, http.MethodPost, "/command",
		map[string]any{"name": "ManualImport", "files": files, "importMode": "auto"}, &cmd); err != nil {
		return res, err
	}
	res.Command = cmd.ID
	return res, nil
}

// grabFlags reads the indexer flags the download was grabbed with, from the
// arr's history.
func (l *Library) grabFlags(ctx context.Context, a *arrAPI, downloadID string) (int, error) {
	var page struct {
		Records []struct {
			EventType string            `json:"eventType"`
			Data      map[string]string `json:"data"`
		} `json:"records"`
	}
	q := url.Values{"downloadId": {downloadID}, "page": {"1"}, "pageSize": {"50"}}
	if err := a.call(ctx, http.MethodGet, "/history?"+q.Encode(), nil, &page); err != nil {
		return 0, err
	}
	// The grab carries the flags; later events of the same download repeat
	// them, so any record with flags will do when the grab is past the page.
	flags := 0
	for _, r := range page.Records {
		f := parseFlagNames(a.name, r.Data["indexerFlags"])
		if strings.EqualFold(r.EventType, "grabbed") {
			return f, nil
		}
		if flags == 0 {
			flags = f
		}
	}
	return flags, nil
}

// findingFor returns the newest pass's finding for the target: the season's
// own finding when there is one, else the whole series' one.
func (st *Store) findingFor(t Target) (Line, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var whole *Line
	for i, f := range st.s.Findings {
		if f.Str("arr") != t.Arr || int(f.Int("arr_id")) != t.ID {
			continue
		}
		scope := f.Str("scope")
		if t.Arr != "sonarr" || t.Season < 0 {
			return f, true
		}
		if scope == "season" || scope == "episodes" {
			if int(f.Int("season")) == t.Season {
				return f, true
			}
			continue
		}
		if whole == nil {
			whole = &st.s.Findings[i]
		}
	}
	if whole != nil {
		return *whole, true
	}
	return Line{}, false
}
