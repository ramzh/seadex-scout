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

// scoreScanEvery is how often the low-score scan runs on its own.
const scoreScanEvery = 6 * time.Hour

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

	mu   sync.Mutex
	scan ScoreScan
}

// NewLibrary returns the library actions for the arrs set up (a zero Arr
// leaves one out). minScore <= 0 uses DefaultMinScore.
func NewLibrary(sonarr, radarr Arr, minScore int, store *Store, log *slog.Logger) *Library {
	if minScore <= 0 {
		minScore = DefaultMinScore
	}
	return &Library{sonarr: newArrAPI("sonarr", sonarr), radarr: newArrAPI("radarr", radarr),
		store: store, log: log, minScore: minScore}
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
	AlreadyOK int      `json:"already_ok"`
	Skipped   int      `json:"skipped"`
	Groups    []string `json:"groups"`
	Skips     []string `json:"skips,omitempty"`
}

// FixFlags gives the target's files that belong to the SeaDex best group the
// flags a best grab carries, when they score below the minimum. The groups
// come from the scout's own finding for the target, which must say the
// library holds the best, so the page cannot flag any file it likes.
func (l *Library) FixFlags(ctx context.Context, t Target) (FlagFix, error) {
	var res FlagFix
	a, err := l.arr(t.Arr)
	if err != nil {
		return res, err
	}
	f, ok := l.store.findingFor(t)
	if !ok {
		return res, errors.New("no finding for this title; run a check first")
	}
	if status := f.Str("status"); status != "at_best" && status != "newer_revision" {
		return res, fmt.Errorf("the scout does not say you hold the SeaDex best here (status %s)", status)
	}
	groups := map[string]bool{}
	for _, g := range strings.Split(f.Str("recommended_groups")+","+f.Str("recommended_group"), ",") {
		if g = normGroup(g); g != "" {
			groups[g] = true
			res.Groups = append(res.Groups, g)
		}
	}
	slices.Sort(res.Groups)
	res.Groups = slices.Compact(res.Groups)
	if len(groups) == 0 {
		return res, errors.New("the finding names no SeaDex best group")
	}
	files, err := l.files(ctx, a, t.ID)
	if err != nil {
		return res, err
	}
	best := bestFlags(t.Arr)
	type update struct {
		ID           int `json:"id"`
		IndexerFlags int `json:"indexerFlags"`
	}
	var updates []update
	for _, file := range files {
		if t.Arr == "sonarr" && t.Season >= 0 && file.SeasonNumber != t.Season {
			continue
		}
		have := flagBits(t.Arr, file.IndexerFlags)
		switch {
		case have&best == best || file.CustomFormatScore >= l.minScore:
			res.AlreadyOK++
		case !groups[normGroup(file.ReleaseGroup)]:
			res.Skipped++
			if len(res.Skips) < 5 {
				res.Skips = append(res.Skips, fmt.Sprintf("%s (group %q)", file.RelativePath, file.ReleaseGroup))
			}
		default:
			updates = append(updates, update{ID: file.ID, IndexerFlags: have | best})
		}
	}
	if len(updates) == 0 {
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

// StartScan starts a scan in the background.
func (l *Library) StartScan(ctx context.Context) error {
	l.mu.Lock()
	if l.scan.Running {
		l.mu.Unlock()
		return ErrScanRunning
	}
	l.scan.Running = true
	l.scan.StartedAt = time.Now().UTC()
	l.mu.Unlock()
	go l.runScan(ctx)
	return nil
}

// Run scans a minute after the start and then every scoreScanEvery.
func (l *Library) Run(ctx context.Context) {
	if l.sonarr == nil && l.radarr == nil {
		return
	}
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		_ = l.StartScan(ctx)
		wait = scoreScanEvery
	}
}

func (l *Library) runScan(ctx context.Context) {
	var seasons []SeasonScore
	var errs []string
	scanned, files, low := 0, 0, 0
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
					s.Low++
					low++
					s.Bytes += f.Size
					if g := f.ReleaseGroup; g != "" && !slices.Contains(s.Groups, g) {
						s.Groups = append(s.Groups, g)
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
func (l *Library) ImportQueued(ctx context.Context, id int) (ImportResult, error) {
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
		case blocked != "":
			reasons[blocked]++
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
	if len(files) == 0 {
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
