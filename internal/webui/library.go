package webui

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/seadex-scout/internal/release"
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
	// Runs returns the episode run of each SeaDex best, keyed like Best, so
	// an import can place its files by the AniList map. Nil places none.
	Runs func(context.Context) (map[string]EpisodeRun, error)
	Qbit *Qbit
	// DirectCategory is the qBittorrent category a SeaDex best is added
	// under when a search finds nothing Sonarr will grab (DirectGrab): one
	// Sonarr does not watch, so Sonarr never imports it by its own reading
	// of the names. Empty turns direct grabs off. ABPasskey builds the
	// AnimeBytes download link; without it only Nyaa bests are added.
	DirectCategory, ABPasskey string
	directMu                  sync.Mutex
	// Auto runs the flag fix and the queue import on their own, with the
	// same rules as the buttons.
	Auto bool
	// Refresh starts the scout's local library refresh, so the findings
	// catch up with files Sonarr or Radarr imported. Nil leaves them to the
	// daily full check.
	Refresh func() error

	path string // the scan and the searches, kept across restarts

	mu       sync.Mutex
	scan     ScoreScan
	lastScan time.Time
	previews map[int]queuePreview // dry imports of stuck downloads, by queue id
	onDisk   onDiskCache

	searches   []SearchJob // newest first
	lastSearch time.Time
	backlog    map[string]backlogMark // by target key: the backlog's last search
	fixStats   FlagFix                // automatic flag fixes since the last digest
	searchWake chan struct{}
}

// NewLibrary returns the library actions for the arrs set up (a zero Arr
// leaves one out). minScore <= 0 uses DefaultMinScore.
func NewLibrary(sonarr, radarr Arr, minScore int, store *Store, log *slog.Logger) *Library {
	if minScore <= 0 {
		minScore = DefaultMinScore
	}
	l := &Library{sonarr: newArrAPI("sonarr", sonarr), radarr: newArrAPI("radarr", radarr),
		store: store, log: log, minScore: minScore, searchWake: make(chan struct{}, 1), previews: map[int]queuePreview{}}
	if store != nil && store.path != "" {
		l.path = filepath.Join(filepath.Dir(store.path), libraryStateFile)
		l.load()
	}
	return l
}

// libraryStateFile holds the newest scan and the searches beside the
// dashboard's state, so a restart neither forgets the searches in line nor
// shows an empty Library tab until the next scan.
const libraryStateFile = "library-state.json"

type libraryState struct {
	Scan     ScoreScan              `json:"scan"`
	Searches []SearchJob            `json:"searches"`
	Backlog  map[string]backlogMark `json:"backlog,omitempty"`
}

func (l *Library) load() {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return // absent on the first start
	}
	var st libraryState
	if err := json.Unmarshal(raw, &st); err != nil {
		l.log.Warn("library state is not valid JSON; starting empty", "path", l.path, "error", err)
		return
	}
	st.Scan.Running = false
	l.scan, l.lastScan, l.searches, l.backlog = st.Scan, st.Scan.FinishedAt, st.Searches, st.Backlog
}

// persistLocked writes the scan and the searches, through a temp file and a
// rename; l.mu must be held.
// ponytail: writes the whole file on every change (a few KB, every 5 s at
// most while a search runs); batch the writes if that ever shows.
func (l *Library) persistLocked() {
	if l.path == "" {
		return
	}
	raw, err := json.Marshal(libraryState{Scan: l.scan, Searches: l.searches, Backlog: l.backlog})
	if err == nil {
		tmp := l.path + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o600); err == nil {
			err = os.Rename(tmp, l.path)
		}
	}
	if err != nil {
		l.log.Warn("could not save the library state", "path", l.path, "error", err)
	}
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
	Proven    int      `json:"proven"`     // fixed by import proof rather than group
	Cleared   int      `json:"cleared"`    // stale best flags taken off
	AltCovers int      `json:"alt_covers"` // fixed as the alt for an episode no best has
	AlreadyOK int      `json:"already_ok"`
	Skipped   int      `json:"skipped"`
	Groups    []string `json:"groups"`
	Skips     []string `json:"skips,omitempty"`
}

// FixFlags makes the target's best flags agree with SeaDex as the scout reads
// it now, both ways.
//
// It adds them to a file that scores below the minimum and is proven to be the
// SeaDex best: either the scout's own finding says the library holds the best
// and the file belongs to that group, or the file was imported from a torrent
// the curation marks as best for this series (provenFiles). The page cannot
// flag any file it likes.
//
// It takes them off a file grabbed while its group was the best, once SeaDex
// names another release: the flags would keep it at the minimum score and the
// arr would reject the new best as "not an upgrade" (Irregular Witch S1:
// Erai-raws grabbed 2026-11-01, Kitsune made best 2026-11-04). staleRules says
// which files: under a better_release finding, any group but the best; under
// an at_best one, only the extra groups the scout itself lists beside the best
// (Misfit S2's untagged E01). A finding for some episodes covers only their
// files. Files proven imported from a best torrent keep the flags, and nothing
// is cleared when that proof cannot be read.
//
// A file the scout knows nothing about either way keeps its flags.
func (l *Library) FixFlags(ctx context.Context, t Target) (FlagFix, error) {
	var res FlagFix
	a, err := l.arr(t.Arr)
	if err != nil {
		return res, err
	}
	groups := map[string]bool{} // a low file of these gets the flags
	var noGroups error
	f, ok := l.store.findingFor(t)
	status := f.Str("status")
	switch {
	case !ok:
		noGroups = errors.New("no finding for this title and no file imported from a SeaDex best torrent")
	case status != "at_best" && status != "newer_revision":
		noGroups = fmt.Errorf("the scout does not say you hold the SeaDex best here (status %s), and no file was imported from a SeaDex best torrent", status)
	default:
		groups = bestGroups(f)
	}
	for g := range groups {
		res.Groups = append(res.Groups, g)
	}
	slices.Sort(res.Groups)
	files, err := l.files(ctx, a, t.ID)
	if err != nil {
		return res, err
	}
	rules, err := l.staleRules(ctx, a, t)
	if err != nil {
		return res, err
	}
	best := bestFlags(t.Arr)
	var low, flagged []arrFile
	for _, file := range files {
		if t.Arr == "sonarr" && t.Season >= 0 && file.SeasonNumber != t.Season {
			continue
		}
		bits := flagBits(t.Arr, file.IndexerFlags)
		if bits&best != 0 && staleFile(rules, file) {
			flagged = append(flagged, file)
			continue
		}
		if bits&best == best || file.CustomFormatScore >= l.minScore {
			res.AlreadyOK++
			continue
		}
		low = append(low, file)
	}
	if len(low) == 0 && len(flagged) == 0 {
		return res, nil
	}
	tvdb, err := l.tvdbOf(ctx, a, t.ID)
	if err != nil {
		return res, err
	}
	proven, proofErr := l.provenFiles(ctx, a, t.ID, tvdb, append(slices.Clip(low), flagged...), nil)
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
		case altCovers(rules, file):
			res.AltCovers++
		default:
			res.Skipped++
			if len(res.Skips) < 5 {
				res.Skips = append(res.Skips, fmt.Sprintf("%s (group %q)", file.RelativePath, file.ReleaseGroup))
			}
			continue
		}
		updates = append(updates, update{ID: file.ID, IndexerFlags: flagBits(t.Arr, file.IndexerFlags) | best})
	}
	res.Fixed = len(updates)
	for _, file := range flagged {
		switch {
		case proofErr != nil:
			res.Skipped++
		case proven[file.ID]:
			res.AlreadyOK++
		default:
			res.Cleared++
			updates = append(updates, update{ID: file.ID, IndexerFlags: flagBits(t.Arr, file.IndexerFlags) &^ best})
		}
	}
	if len(updates) == 0 {
		res.Fixed = 0
		switch {
		case proofErr != nil && len(flagged) > 0:
			return res, fmt.Errorf("stale flags kept: import proof: %w", proofErr)
		case noGroups != nil && len(low) > 0:
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
		res.Fixed, res.Cleared = 0, 0
		return res, err
	}
	l.forget(t)
	return res, nil
}

// staleRule is one finding's say on the best flags of the files it covers
// (every file of the target when files is nil): which groups are its best and
// alts, and, when known, which files hold an episode no best release has
// (compare.Finding.Uncovered).
type staleRule struct {
	files     map[int]bool
	best      map[string]bool
	alt       map[string]bool
	only      map[string]bool // unknown coverage on an at_best finding: the extra groups the scout lists
	uncovered map[int]bool
	known     bool // uncovered was reported
	better    bool // a better_release finding
	noBest    bool // a theoretical_best or no_best finding: no best to cover anything
}

// staleRules reads the target's better_release, at_best and newer_revision
// findings into staleRules. A finding for some episodes covers those
// episodes' files.
func (l *Library) staleRules(ctx context.Context, a *arrAPI, t Target) ([]staleRule, error) {
	var rules []staleRule
	var byEpisode map[[2]int]int // (season, episode) to file id, read once
	for _, f := range l.store.findingsFor(t) {
		r := staleRule{best: bestGroups(f), alt: groupSet(f.Str("alt_groups"))}
		switch f.Str("status") {
		case "better_release":
			r.better = true
		case "at_best", "newer_revision":
			r.only = map[string]bool{}
			for g := range groupSet(f.Str("current_group")) {
				if !r.best[g] && !r.alt[g] {
					r.only[g] = true
				}
			}
		case "theoretical_best", "no_best":
			r.noBest = true
		default:
			continue
		}
		if len(r.best) == 0 && !r.noBest {
			continue
		}
		if raw, ok := f.Attrs["uncovered_files"].(string); ok {
			r.known, r.uncovered = true, map[int]bool{}
			for id := range strings.SplitSeq(raw, ",") {
				if n, err := strconv.Atoi(id); err == nil {
					r.uncovered[n] = true
				}
			}
		}
		switch f.Str("scope") {
		case "season", "movie":
		case "episodes":
			if byEpisode == nil {
				var err error
				if byEpisode, err = l.episodeFiles(ctx, a, t.ID); err != nil {
					return nil, err
				}
			}
			r.files = map[int]bool{}
			for _, se := range episodeSpan(f.Str("episodes")) {
				if id := byEpisode[se]; id > 0 {
					r.files[id] = true
				}
			}
		default:
			continue
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// ruleFor is the rule that covers file: an episode finding's before a whole
// one's; ok is false when none does.
func ruleFor(rules []staleRule, file arrFile) (staleRule, bool) {
	pick := -1
	for i, r := range rules {
		if r.files != nil && r.files[file.ID] {
			return r, true
		}
		if r.files == nil && pick < 0 {
			pick = i
		}
	}
	if pick < 0 {
		return staleRule{}, false
	}
	return rules[pick], true
}

// staleFile reports whether file's best flags are stale. A file holding an
// episode no best release has keeps them (the best cannot replace it). One the
// best covers loses them unless its group is a best one. Where coverage is
// unknown, a better_release finding clears any group but the best, and an
// at_best one only the extra groups the scout lists beside the best and alts,
// so a group Sonarr names differently from SeaDex never loses its flags.
func staleFile(rules []staleRule, file arrFile) bool {
	r, ok := ruleFor(rules, file)
	if !ok || r.noBest {
		return false
	}
	g := release.NormalizeGroup(file.ReleaseGroup)
	if r.best[g] || r.best[normGroup(file.ReleaseGroup)] {
		return false
	}
	switch {
	case r.known:
		return !r.uncovered[file.ID]
	case r.better:
		return true
	}
	return r.only[g]
}

// altCovers reports whether file is the SeaDex alt for an episode no best
// release has, so it is the best the operator can get and earns the flags
// (Black Clover 52-170: iAHD; every episode of an entry with a theoretical
// best only, or none obtainable). Once the best covers that episode, staleFile
// takes them off again and the arr upgrades.
func altCovers(rules []staleRule, file arrFile) bool {
	r, ok := ruleFor(rules, file)
	if !ok || (!r.noBest && (!r.known || !r.uncovered[file.ID])) {
		return false
	}
	g := release.NormalizeGroup(file.ReleaseGroup)
	return r.alt[g] || r.alt[normGroup(file.ReleaseGroup)]
}

// groupSet reads a comma-separated group list as the scout writes it.
func groupSet(list string) map[string]bool {
	out := map[string]bool{}
	for _, g := range strings.Split(list, ",") {
		if g = normGroup(g); g != "" {
			out[g] = true
		}
	}
	return out
}

// episodeSpanRe reads one "S01E13-E24" or "S00E02" part of a finding's
// episodes.
var episodeSpanRe = regexp.MustCompile(`S(\d+)E(\d+)(?:-E(\d+))?`)

// episodeSpan lists the (season, episode) pairs a finding's episodes name.
func episodeSpan(spec string) [][2]int {
	var out [][2]int
	for _, m := range episodeSpanRe.FindAllStringSubmatch(spec, -1) {
		s, _ := strconv.Atoi(m[1])
		first, _ := strconv.Atoi(m[2])
		last := first
		if m[3] != "" {
			last, _ = strconv.Atoi(m[3])
		}
		for e := first; e <= last && e-first < 2000; e++ {
			out = append(out, [2]int{s, e})
		}
	}
	return out
}

// episodeFiles maps a series' (season, episode) pairs to their file ids.
func (l *Library) episodeFiles(ctx context.Context, a *arrAPI, series int) (map[[2]int]int, error) {
	var eps []struct {
		SeasonNumber  int `json:"seasonNumber"`
		EpisodeNumber int `json:"episodeNumber"`
		EpisodeFileID int `json:"episodeFileId"`
	}
	if err := a.call(ctx, http.MethodGet, "/episode?seriesId="+strconv.Itoa(series), nil, &eps); err != nil {
		return nil, err
	}
	out := make(map[[2]int]int, len(eps))
	for _, e := range eps {
		if e.EpisodeFileID > 0 {
			out[[2]int{e.SeasonNumber, e.EpisodeNumber}] = e.EpisodeFileID
		}
	}
	return out, nil
}

// bestGroups is the finding's SeaDex best groups, normalized.
func bestGroups(f Line) map[string]bool {
	groups := map[string]bool{}
	for _, g := range strings.Split(f.Str("recommended_groups")+","+f.Str("recommended_group"), ",") {
		if g = normGroup(g); g != "" {
			groups[g] = true
		}
	}
	return groups
}

// fixableGroups is the best groups Fix flags trusts for t: those of a
// finding that says the library holds the best, else none.
func (l *Library) fixableGroups(t Target) map[string]bool {
	f, ok := l.store.findingFor(t)
	if st := f.Str("status"); !ok || (st != "at_best" && st != "newer_revision") {
		return nil
	}
	return bestGroups(f)
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
	// Fixable counts the low files Fix flags would flag: proven ones, and
	// those of the best group where the scout says the library holds it.
	Fixable int `json:"fixable"`
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
	// Clean names the seasons, series and films whose every file reaches the
	// minimum score, keyed like Target.key: Sonarr or Radarr already scores
	// them as SeaDex releases.
	Clean []string `json:"clean,omitempty"`
	// ProvenBest names the seasons and films the scout lists as upgrades
	// whose every file was imported from a torrent the curation marks as
	// the SeaDex best: the release on disk is the best, whatever version
	// or group Sonarr or Radarr reads from its file names.
	ProvenBest []string `json:"proven_best,omitempty"`
}

// Scan returns the newest scan.
func (l *Library) Scan() ScoreScan {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.scan
	s.MinScore = l.minScore
	s.Seasons = slices.Clone(s.Seasons)
	s.Clean = slices.Clone(s.Clean)
	s.ProvenBest = slices.Clone(s.ProvenBest)
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
	importsSince := time.Now().UTC()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = autoEvery
		// Files imported since the last look change what the findings and
		// the scan say: refresh both now rather than at the daily check.
		now := time.Now().UTC()
		imported := l.importsSince(ctx, importsSince)
		if imported > 0 {
			importsSince = now
			l.log.Info("new imports in Sonarr or Radarr; refreshing the findings and the score scan", "files", imported)
			if l.Refresh != nil {
				if err := l.Refresh(); err != nil {
					l.log.Debug("library refresh after imports not started", "error", err)
				}
			}
		}
		scanned := false
		if (imported > 0 || time.Since(l.lastScan) >= scoreScanEvery) && l.beginScan() {
			l.runScan(ctx)
			scanned = true
		}
		if l.Auto {
			// A SeaDex change shows in the findings within a cycle (15m), so
			// stale flags come off on the next tick; proven low files wait for
			// a scan.
			l.autoFix(ctx, scanned)
			l.autoSeeding(ctx)
			l.autoBacklog()
			l.autoImport(ctx)
		}
	}
}

// importsSince counts the files Sonarr and Radarr imported since a moment;
// a read that fails counts none.
func (l *Library) importsSince(ctx context.Context, since time.Time) int {
	n := 0
	for _, a := range []*arrAPI{l.sonarr, l.radarr} {
		if a == nil {
			continue
		}
		var recs []json.RawMessage
		q := url.Values{"date": {since.Format(time.RFC3339)}, "eventType": {"downloadFolderImported"}}
		if err := a.call(ctx, http.MethodGet, "/history/since?"+q.Encode(), nil, &recs); err != nil {
			l.log.Debug("could not read new imports", "arr", a.name, "error", err)
			continue
		}
		n += len(recs)
	}
	return n
}

// autoFix runs Fix flags on every season or movie SeaDex now has a better
// release for and, after a scan, on every scanned season that can be proven to
// hold the best (by import, or by the scout's finding). A target whose stale
// flags came off gets a search in line, so the arr grabs the new best: an RSS sync never sees
// a release published before the flags came off.
func (l *Library) autoFix(ctx context.Context, scanned bool) {
	targets := l.store.betterReleaseTargets()
	var seasons []SeasonScore
	if scanned {
		seasons = l.Scan().Seasons
	}
	for _, s := range seasons {
		t := Target{Arr: s.Arr, ID: s.ID, Season: s.Season}
		if s.Arr == "radarr" {
			t.Season = -1
		}
		if s.Proven == 0 {
			// The statuses FixFlags can act on: a best held, a best to make
			// room for, or an alt that is the best there is.
			f, ok := l.store.findingFor(t)
			switch f.Str("status") {
			case "at_best", "newer_revision", "better_release", "theoretical_best", "no_best":
			default:
				ok = false
			}
			if !ok {
				continue
			}
		}
		if !slices.ContainsFunc(targets, func(j SearchJob) bool { return j.key() == t.key() }) {
			targets = append(targets, SearchJob{Target: t, Title: s.Title})
		}
	}
	for _, j := range targets {
		if ctx.Err() != nil {
			return
		}
		res, err := l.FixFlags(ctx, j.Target)
		if err != nil {
			l.log.Debug("automatic flag fix skipped", "arr", j.Arr, "arr_id", j.ID, "season", j.Season, "error", err)
			continue
		}
		if res.Fixed > 0 || res.Cleared > 0 {
			l.mu.Lock()
			l.fixStats.Fixed += res.Fixed
			l.fixStats.Cleared += res.Cleared
			l.fixStats.AltCovers += res.AltCovers
			l.mu.Unlock()
			l.log.Info("indexer flags fixed automatically", "arr", j.Arr, "arr_id", j.ID, "season", j.Season,
				"title", j.Title, "fixed", res.Fixed, "proven", res.Proven, "cleared", res.Cleared, "skipped", res.Skipped)
		}
		if res.Cleared > 0 {
			if _, err := l.QueueSearches([]SearchJob{j}, false); err != nil {
				l.log.Warn("could not queue a search after clearing stale flags", "arr", j.Arr, "arr_id", j.ID, "error", err)
			}
		}
	}
}

// autoImport imports the stuck Sonarr downloads that were grabbed as a
// SeaDex best (the grab carries the best flags), with Import's rules, and
// removes from the queue the stuck ones that hold nothing left to import.
func (l *Library) autoImport(ctx context.Context) {
	items, err := l.Queue(ctx)
	if err != nil {
		l.log.Debug("automatic import could not read the queue", "error", err)
		return
	}
	for _, it := range items {
		if !it.Stuck || !it.CanImport || ctx.Err() != nil {
			continue
		}
		best := bestFlags(it.Arr)
		preview, err := l.ImportQueued(ctx, it.Arr, it.ID, true)
		if err == nil && preview.Done {
			if err := l.RemoveFromQueue(ctx, it.Arr, it.ID); err != nil {
				l.log.Warn("could not remove an imported download from the queue", "queue_id", it.ID, "title", it.Title, "error", err)
			} else {
				l.log.Info("imported download removed from the queue", "queue_id", it.ID, "title", it.Title, "reasons", preview.Reasons)
			}
			continue
		}
		if err != nil || preview.Files == 0 || preview.IndexerFlags&best != best {
			continue
		}
		res, err := l.ImportQueued(ctx, it.Arr, it.ID, false)
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
	var cleanKeys, provenKeys []string
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
			clean := map[int]bool{} // seasons seen, false once one file is low
			var lowFiles []arrFile
			for _, f := range fs {
				files++
				season := f.SeasonNumber
				if a.name == "radarr" {
					season = -1
				}
				if _, seen := clean[season]; !seen {
					clean[season] = true
				}
				if f.CustomFormatScore < l.minScore {
					clean[season] = false
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
			var proven map[int]bool
			if len(idx) > 0 && len(lowFiles) > 0 {
				proven, err = l.provenFiles(ctx, a, it.ID, it.TvdbID, lowFiles, idx)
				if err != nil {
					errs = append(errs, fmt.Sprintf("%s %s history: %v", a.name, it.Title, err))
				}
			}
			groups := map[int]map[string]bool{}
			for _, f := range lowFiles {
				season := f.SeasonNumber
				if a.name == "radarr" {
					season = -1
				}
				g, ok := groups[season]
				if !ok {
					g = l.fixableGroups(Target{Arr: a.name, ID: it.ID, Season: season})
					groups[season] = g
				}
				if proven[f.ID] {
					by[season].Proven++
				}
				if proven[f.ID] || g[normGroup(f.ReleaseGroup)] {
					by[season].Fixable++
				}
			}
			for season := range l.store.upgradeSeasons(a.name, it.ID) {
				if len(idx) == 0 {
					break
				}
				var sf []arrFile
				for _, f := range fs {
					if a.name == "radarr" || f.SeasonNumber == season {
						sf = append(sf, f)
					}
				}
				all, err := l.provenFiles(ctx, a, it.ID, it.TvdbID, sf, idx)
				if err == nil && len(sf) > 0 && len(all) == len(sf) {
					provenKeys = append(provenKeys, Target{Arr: a.name, ID: it.ID, Season: season}.key())
				}
			}
			whole := len(clean) > 0
			for season, ok := range clean {
				whole = whole && ok
				if ok && a.name == "sonarr" {
					cleanKeys = append(cleanKeys, Target{Arr: a.name, ID: it.ID, Season: season}.key())
				}
			}
			if whole {
				cleanKeys = append(cleanKeys, Target{Arr: a.name, ID: it.ID, Season: -1}.key())
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
		Scanned: scanned, Files: files, LowFiles: low, Seasons: seasons, Clean: cleanKeys, ProvenBest: provenKeys}
	l.lastScan = l.scan.FinishedAt
	l.persistLocked()
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
	// Note says why a stuck download cannot be imported from here, or that
	// it holds nothing left to import.
	Note string `json:"note,omitempty"`
}

// queuePreview is a dry import of a stuck download, kept a while so the
// queue page does not ask Sonarr or Radarr again on every load.
type queuePreview struct {
	at  time.Time
	res ImportResult
	err error
}

const previewKeep = 10 * time.Minute

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
			it.CanImport = r.SizeLeft == 0 && r.OutputPath != "" && r.DownloadID != ""
			if it.CanImport && a.name == "radarr" && r.MovieID == 0 && l.store.movieForHash(r.DownloadID) == 0 {
				it.CanImport = false
				it.Note = "Radarr cannot tell which film this is and it is no SeaDex best the scout knows: add the film to Radarr, or import it by hand there"
			}
			if it.Stuck && it.CanImport {
				l.notePreview(ctx, a.name, &it)
			}
			out = append(out, it)
		}
	}
	if len(errs) > 0 && len(out) == 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

// notePreview fills a stuck download's Note from a dry import: nothing the
// scout can import (only Manual Import in the arr helps), or nothing left.
func (l *Library) notePreview(ctx context.Context, arr string, it *QueueItem) {
	l.mu.Lock()
	p, ok := l.previews[it.ID]
	l.mu.Unlock()
	if !ok || time.Since(p.at) > previewKeep {
		p.res, p.err = l.ImportQueued(ctx, arr, it.ID, true)
		p.at = time.Now()
		l.mu.Lock()
		l.previews[it.ID] = p
		l.mu.Unlock()
	}
	switch {
	case p.err != nil:
		it.CanImport, it.Note = false, p.err.Error()
	case p.res.Done:
		it.Note = "Already imported: every file is at the minimum score; remove it from the queue"
	case p.res.Files == 0:
		it.CanImport = false
		it.Note = "Nothing the scout can import (" + strings.Join(p.res.Reasons, ", ") + "): use Manual Import in " + arrName(arr)
	}
}

func arrName(arr string) string {
	if arr == "radarr" {
		return "Radarr"
	}
	return "Sonarr"
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
	// A download Sonarr or Radarr could not match to a series or film cannot
	// be dropped: the removal fails (NullReference) or the entry comes back
	// on the next queue refresh, the torrent still being in their category.
	// Moving it out of that category drops it for good, files untouched.
	if l.Qbit != nil {
		rec, err := l.queueRecord(ctx, a, id)
		if err != nil {
			return err
		}
		if rec.SeriesID == 0 && rec.MovieID == 0 && rec.DownloadID != "" {
			twin, err := l.Qbit.Park(ctx, rec.DownloadID)
			if err != nil {
				return fmt.Errorf("moving the unmatched torrent out of the queue's category: %w", err)
			}
			l.log.Info("unmatched download moved out of the queue by category; the torrent keeps seeding", "arr", arr, "queue_id", id, "category", twin)
			return nil
		}
	}
	q := "?removeFromClient=false&blocklist=false"
	if a.version(ctx) == "v5" {
		q = "?removalMethod=ignore&blocklistMethod=doNotBlocklist"
	}
	return a.call(ctx, http.MethodDelete, "/queue/"+strconv.Itoa(id)+q, nil, nil)
}

// ImportResult is what ImportQueued sent to Sonarr or Radarr.
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
	Size         int64  `json:"size"`
	Series       *struct {
		ID int `json:"id"`
	} `json:"series"`
	Movie *struct {
		ID int `json:"id"`
	} `json:"movie"`
	Episodes []struct {
		ID            int `json:"id"`
		EpisodeFileID int `json:"episodeFileId"`
		SeasonNumber  int `json:"seasonNumber"`
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
// holds more episodes than Sonarr expected from the grab's name, or, once the
// scout named the film, Radarr not knowing which film a download is.
func importableRejection(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "unexpected") || strings.Contains(r, "was not found in the grabbed release") ||
		strings.Contains(r, "unknown movie") || strings.Contains(r, "title mismatch")
}

// ImportQueued imports a finished download by hand, as hardlinks, the way
// Manual Import does. From a Sonarr pack it takes only the files Sonarr
// matched to an episode, overriding only the "episode was unexpected" check
// that stops packs larger than their grab's name, and only for episodes with
// no file or a file below the minimum score. From Radarr it takes the largest
// file, for a film with no file at the minimum score; a download Radarr cannot
// place is placed by the SeaDex best it is (its info hash). The files get the
// grab's indexer flags, which Manual Import otherwise drops, or the best flags
// for a download proven a SeaDex best by its hash.
// With dry set it only reports what it would import.
func (l *Library) ImportQueued(ctx context.Context, arr string, id int, dry bool) (ImportResult, error) {
	var res ImportResult
	a, err := l.arr(arr)
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
	owner := rec.SeriesID
	byHash := false
	if a.name == "radarr" {
		owner = rec.MovieID
		if owner == 0 {
			owner, byHash = l.store.movieForHash(rec.DownloadID), true
		}
		if owner == 0 {
			return res, errors.New("Radarr cannot tell which film this is and it is no SeaDex best the scout knows")
		}
	}
	flags, grabbed, err := l.grabFlags(ctx, a, rec.DownloadID)
	if err != nil {
		return res, err
	}
	if byHash {
		flags |= bestFlags(a.name)
	}
	res.IndexerFlags = flags
	bestGrab := flags&bestFlags(a.name) == bestFlags(a.name)
	q := url.Values{"folder": {rec.OutputPath}, "downloadId": {rec.DownloadID}, "filterExistingFiles": {"false"}}
	var rows []manualImportRow
	if err := a.call(ctx, http.MethodGet, "/manualimport?"+q.Encode(), nil, &rows); err != nil {
		return res, err
	}
	if a.name == "sonarr" && owner > 0 {
		if err := l.matchByNumber(ctx, a, owner, rows, grabbed); err != nil {
			return res, err
		}
	}
	good := map[int]bool{} // episode ids whose file already scores high enough
	filmGood := false
	if owner > 0 {
		files, err := l.files(ctx, a, owner)
		if err != nil {
			return res, err
		}
		goodFile := map[int]bool{}
		for _, f := range files {
			if f.CustomFormatScore >= l.minScore {
				goodFile[f.ID] = true
				filmGood = a.name == "radarr" && !(bestGrab && l.seasonBelowBest(a.name, owner, -1))
			}
		}
		// A file imported from this very download is already what it
		// brings: importing it again only deletes and re-creates it, every
		// 30 minutes while the download stays queued (Durarara!!x2 Part 3's
		// two specials, 15 times).
		fromHere := map[int]bool{}
		if hashes, err := l.importHashes(ctx, a, owner); err == nil {
			for fid, h := range hashes {
				fromHere[fid] = strings.EqualFold(h, rec.DownloadID)
			}
		}
		// A score at the minimum is no proof the file is the SeaDex best:
		// older grabs carry the best flags on alts too. When this download
		// is a best grab and the scout says the season is not at the best,
		// its files replace what is there.
		for _, r := range rows {
			for _, e := range r.Episodes {
				if fromHere[e.EpisodeFileID] ||
					goodFile[e.EpisodeFileID] && !(bestGrab && l.seasonBelowBest(a.name, owner, e.SeasonNumber)) {
					good[e.ID] = true
				}
			}
		}
	}
	if a.name == "radarr" && len(rows) > 1 {
		// A film is one file: the largest, never its extras or samples.
		big := slices.MaxFunc(rows, func(x, y manualImportRow) int { return cmp.Compare(x.Size, y.Size) })
		res.Skipped += len(rows) - 1
		rows = []manualImportRow{big}
	}
	var files []map[string]any
	reasons := map[string]int{}
	atMin, blockedFiles := 0, 0
	// Sonarr can match a pack to the wrong season (Higurashi's S2 to S1):
	// "nothing left" needs an episode the grab was for among those at the
	// minimum, else the match itself is wrong.
	atMinGrabbed := len(grabbed) == 0
	for _, r := range rows {
		blocked := ""
		for _, rj := range r.Rejections {
			if !importableRejection(rj.Reason) {
				blocked = rj.Reason
				break
			}
		}
		if a.name == "radarr" {
			switch {
			case filmGood:
				reasons["the film already has a file at the minimum score"]++
				atMin++
			case blocked != "":
				reasons[blocked]++
				blockedFiles++
			default:
				files = append(files, map[string]any{
					"path": r.Path, "folderName": r.FolderName, "movieId": owner,
					"quality": r.Quality, "languages": r.Languages, "releaseGroup": r.ReleaseGroup,
					"indexerFlags": flags, "downloadId": rec.DownloadID,
				})
				continue
			}
			res.Skipped++
			continue
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
			for _, e := range r.Episodes {
				atMinGrabbed = atMinGrabbed || grabbed[e.ID]
			}
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
	res.Done = len(files) == 0 && atMin > 0 && blockedFiles == 0 && atMinGrabbed
	if len(files) == 0 && atMin > 0 && !atMinGrabbed {
		res.Reasons = append(res.Reasons, "Sonarr matched the files to episodes the grab was not for: check the match in Manual Import")
	}
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
	l.mu.Lock()
	delete(l.previews, id)
	l.mu.Unlock()
	return res, nil
}

var (
	seasonEpisode = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,4})\b`)
	bareNumber    = regexp.MustCompile(`[ _.-](\d{1,4})(?:v\d+)?(?:[ _.]|$)`)
	bracketed     = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)
	leadingGroup  = regexp.MustCompile(`^\[([^\]]+)\]`)
)

// matchByNumber matches the episode files of a pack Sonarr left unmatched by
// the numbers in their names: an absolute number ("Slam Dunk - 046"), an
// SxxEyy Sonarr has, or, when the pack numbers its season differently
// (Gintama's S11E01-E14 for Sonarr's S10E13-E26), the grab's episodes in
// order, only when the counts agree. Two files on one episode drop every guess.
func (l *Library) matchByNumber(ctx context.Context, a *arrAPI, series int, rows []manualImportRow, grabbed map[int]bool) error {
	// Rows Sonarr matched to episodes of the grab's own seasons that the grab
	// was not for (Durarara!!x2 Part 3: its "- 26" to "- 37" files read as
	// S02E01-E12, which the library holds, not E25-E36): they join the
	// order-based match below. A row in another season (the pack's OVA, in
	// Specials) keeps Sonarr's match.
	grabSeasons := map[int]bool{}
	var todo, wrong []int
	for i, r := range rows {
		if !episodeVideo(QbitFile{Name: cmp.Or(r.RelativePath, filepath.Base(r.Path)), Size: r.Size}) {
			continue
		}
		if r.Series == nil || len(r.Episodes) == 0 {
			todo = append(todo, i)
		}
	}
	// With a grab to compare against, the episodes are read even when every
	// row is matched, to find the wrong ones.
	if len(todo) == 0 && (len(grabbed) == 0 || series <= 0) {
		return nil
	}
	var eps []struct {
		ID                    int `json:"id"`
		SeasonNumber          int `json:"seasonNumber"`
		EpisodeNumber         int `json:"episodeNumber"`
		AbsoluteEpisodeNumber int `json:"absoluteEpisodeNumber"`
		EpisodeFileID         int `json:"episodeFileId"`
	}
	if err := a.call(ctx, http.MethodGet, "/episode?seriesId="+strconv.Itoa(series), nil, &eps); err != nil {
		return err
	}
	type key struct{ s, e int }
	bySE, byAbs := map[key]int{}, map[int]int{}
	var grabOrder []int // indexes into eps, by season and number
	for i, e := range eps {
		bySE[key{e.SeasonNumber, e.EpisodeNumber}] = i
		if e.AbsoluteEpisodeNumber > 0 {
			byAbs[e.AbsoluteEpisodeNumber] = i
		}
		if grabbed[e.ID] {
			grabOrder = append(grabOrder, i)
		}
	}
	slices.SortFunc(grabOrder, func(x, y int) int {
		return cmp.Or(cmp.Compare(eps[x].SeasonNumber, eps[y].SeasonNumber), cmp.Compare(eps[x].EpisodeNumber, eps[y].EpisodeNumber))
	})
	for _, j := range grabOrder {
		grabSeasons[eps[j].SeasonNumber] = true
	}
	for i, r := range rows {
		if r.Series == nil || r.Series.ID != series || len(r.Episodes) == 0 ||
			!episodeVideo(QbitFile{Name: cmp.Or(r.RelativePath, filepath.Base(r.Path)), Size: r.Size}) {
			continue
		}
		mis := true
		for _, e := range r.Episodes {
			mis = mis && !grabbed[e.ID] && grabSeasons[e.SeasonNumber]
		}
		if mis {
			wrong = append(wrong, i)
		}
	}
	if len(todo) == 0 && len(wrong) == 0 {
		return nil
	}
	guess := map[int]int{} // row -> eps index
	type offRow struct{ row, s, e int }
	var off []offRow
	bare := map[int]int{} // row -> its bare number
	for _, i := range todo {
		name := filepath.Base(rows[i].Path)
		if m := seasonEpisode.FindStringSubmatch(name); m != nil {
			s, _ := strconv.Atoi(m[1])
			e, _ := strconv.Atoi(m[2])
			if j, ok := bySE[key{s, e}]; ok {
				guess[i] = j
			} else {
				off = append(off, offRow{i, s, e})
			}
			continue
		}
		stem := bracketed.ReplaceAllString(strings.TrimSuffix(name, filepath.Ext(name)), " ")
		if ms := bareNumber.FindAllStringSubmatch(stem+" ", -1); ms != nil {
			n, _ := strconv.Atoi(ms[len(ms)-1][1])
			bare[i] = n
		}
	}
	// Bare numbers are absolute, or count within the season (Durarara!!x2
	// Ketsu's "- 25" to "- 36" are S02E25-E36, while absolute 25 is S02E01).
	// The pack is read the way that lands more files on episodes the grab
	// was for; a tie keeps absolute.
	relative := func(n int) (int, bool) {
		for s := range grabSeasons {
			if j, ok := bySE[key{s, n}]; ok {
				return j, true
			}
		}
		return 0, false
	}
	absHits, relHits := 0, 0
	for _, n := range bare {
		if j, ok := byAbs[n]; ok && grabbed[eps[j].ID] {
			absHits++
		}
		if j, ok := relative(n); ok && grabbed[eps[j].ID] {
			relHits++
		}
	}
	for i, n := range bare {
		if relHits > absHits {
			if j, ok := relative(n); ok {
				guess[i] = j
			}
		} else if j, ok := byAbs[n]; ok {
			guess[i] = j
		}
	}
	// ponytail: one season renumbered as a whole; mixed renumbering stays manual.
	if len(off) > 0 && len(wrong) == 0 && len(off) == len(grabOrder) && !slices.ContainsFunc(off, func(o offRow) bool { return o.s != off[0].s }) {
		slices.SortFunc(off, func(x, y offRow) int { return cmp.Compare(x.e, y.e) })
		for k, o := range off {
			guess[o.row] = grabOrder[k]
		}
	}
	// Rows matched to the wrong episodes, with any still unmatched: when
	// they are exactly as many as the episodes the grab was for, they take
	// those episodes in the order of the numbers in their names. Anything
	// else (a count off by one, a repeated number) stays as Sonarr had it.
	if len(wrong) > 0 {
		var rest []int
		for _, i := range todo {
			if _, ok := guess[i]; !ok {
				rest = append(rest, i)
			}
		}
		rest = append(rest, wrong...)
		nums := map[int]int{}
		distinct := map[int]bool{}
		for _, i := range rest {
			n, ok := fileNumber(filepath.Base(rows[i].Path))
			if !ok || distinct[n] {
				return nil
			}
			distinct[n], nums[i] = true, n
		}
		if len(rest) != len(grabOrder) {
			return nil
		}
		slices.SortFunc(rest, func(x, y int) int { return cmp.Compare(nums[x], nums[y]) })
		for k, i := range rest {
			guess[i] = grabOrder[k]
		}
	}
	// Names Sonarr could not match often parse to quality Unknown; the
	// pack's other files carry the real one.
	var known json.RawMessage
	for _, r := range rows {
		var q struct {
			Quality struct {
				ID int `json:"id"`
			} `json:"quality"`
		}
		if json.Unmarshal(r.Quality, &q) == nil && q.Quality.ID > 0 {
			known = r.Quality
			break
		}
	}
	seen := map[int]bool{}
	for _, j := range guess {
		if seen[j] {
			return nil
		}
		seen[j] = true
	}
	for i, j := range guess {
		e := eps[j]
		rows[i].Series = &struct {
			ID int `json:"id"`
		}{series}
		rows[i].Episodes = []struct {
			ID            int `json:"id"`
			EpisodeFileID int `json:"episodeFileId"`
			SeasonNumber  int `json:"seasonNumber"`
		}{{e.ID, e.EpisodeFileID, e.SeasonNumber}}
		// The rejections were about the match the scout just made.
		rows[i].Rejections = nil
		rows[i].ReleaseType = "singleEpisode"
		if m := leadingGroup.FindStringSubmatch(filepath.Base(rows[i].Path)); rows[i].ReleaseGroup == "" && m != nil {
			rows[i].ReleaseGroup = m[1]
		}
		var q struct {
			Quality struct {
				ID int `json:"id"`
			} `json:"quality"`
		}
		if known != nil && (json.Unmarshal(rows[i].Quality, &q) != nil || q.Quality.ID == 0) {
			rows[i].Quality = known
		}
	}
	return nil
}

// grabFlags reads the indexer flags the download was grabbed with, and the
// episodes it was grabbed for, from the arr's history.
func (l *Library) grabFlags(ctx context.Context, a *arrAPI, downloadID string) (int, map[int]bool, error) {
	var page struct {
		Records []struct {
			EventType string            `json:"eventType"`
			EpisodeID int               `json:"episodeId"`
			Data      map[string]string `json:"data"`
		} `json:"records"`
	}
	q := url.Values{"downloadId": {downloadID}, "page": {"1"}, "pageSize": {"1000"}}
	if err := a.call(ctx, http.MethodGet, "/history?"+q.Encode(), nil, &page); err != nil {
		return 0, nil, err
	}
	// The grab carries the flags; later events of the same download repeat
	// them, so any record with flags will do when the grab is past the page.
	flags, grabFlags := 0, -1
	episodes := map[int]bool{}
	for _, r := range page.Records {
		f := parseFlagNames(a.name, r.Data["indexerFlags"])
		if strings.EqualFold(r.EventType, "grabbed") {
			if grabFlags < 0 {
				grabFlags = f
			}
			if r.EpisodeID > 0 {
				episodes[r.EpisodeID] = true
			}
		}
		if flags == 0 {
			flags = f
		}
	}
	if grabFlags >= 0 {
		flags = grabFlags
	}
	return flags, episodes, nil
}

// seasonBelowBest reports that the scout finds a better SeaDex release for
// the season (or film) than the one on disk.
func (l *Library) seasonBelowBest(arr string, id, season int) bool {
	if arr == "radarr" {
		season = -1
	}
	f, ok := l.store.findingFor(Target{Arr: arr, ID: id, Season: season})
	st := f.Str("status")
	return ok && (st == "better_release" || st == "newer_revision")
}

// upgradeSeasons lists the seasons (-1 for a film or a whole series) the
// newest pass finds a better release for.
func (st *Store) upgradeSeasons(arr string, id int) map[int]bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[int]bool{}
	for _, f := range st.s.Findings {
		if f.Str("arr") != arr || int(f.Int("arr_id")) != id {
			continue
		}
		if s := f.Str("status"); s != "better_release" && s != "newer_revision" {
			continue
		}
		season := -1
		if arr == "sonarr" && (f.Str("scope") == "season" || f.Str("scope") == "episodes") {
			season = int(f.Int("season"))
		}
		out[season] = true
	}
	return out
}

// abTorrentID finds the AnimeBytes torrent id in a release URL
// ("torrents.php?id=51559&torrentid=1096667").
var abTorrentID = regexp.MustCompile(`animebytes\.tv/torrents\.php\?[^ |]*torrentid=(\d+)`)

type onDiskCache struct {
	at  time.Time
	out map[string]string
}

const onDiskKeep = 2 * time.Minute

// OnDisk maps the findings that are not at the best ("al_id:season") to the
// name of a finished qBittorrent torrent that is their SeaDex best release,
// by info hash or AnimeBytes torrent id: a best downloaded but never
// imported. It reads qBittorrent at most every two minutes.
func (l *Library) OnDisk(ctx context.Context) map[string]string {
	if l.Qbit == nil {
		return nil
	}
	l.mu.Lock()
	c := l.onDisk
	l.mu.Unlock()
	if time.Since(c.at) < onDiskKeep {
		return c.out
	}
	torrents, err := l.Qbit.Torrents(ctx)
	if err != nil {
		return c.out
	}
	byHash, byAB := map[string]string{}, map[string]string{}
	for _, t := range torrents {
		if t.Progress < 1 {
			continue
		}
		byHash[strings.ToLower(t.Hash)] = t.Name
		if m := abTorrent.FindStringSubmatch(strings.TrimSpace(t.Comment)); m != nil {
			byAB[m[1]] = t.Name
		}
	}
	out := map[string]string{}
	for _, f := range l.store.Snapshot().Findings {
		if f.Str("status") == "at_best" {
			continue
		}
		name := byHash[strings.ToLower(f.Str("info_hash"))]
		for _, m := range abTorrentID.FindAllStringSubmatch(f.Str("release_urls")+"|"+f.Str("ab_url"), -1) {
			name = cmp.Or(name, byAB[m[1]])
		}
		if name != "" {
			out[fmt.Sprintf("%d:%d", f.Int("al_id"), f.Int("season"))] = name
		}
	}
	l.mu.Lock()
	l.onDisk = onDiskCache{at: time.Now(), out: out}
	l.mu.Unlock()
	return out
}

// movieForHash returns the Radarr film whose SeaDex best release has this
// info hash, by the newest pass's findings, or 0.
func (st *Store) movieForHash(hash string) int {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if hash == "" {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, f := range st.s.Findings {
		if f.Str("arr") == "radarr" && strings.EqualFold(f.Str("info_hash"), hash) {
			return int(f.Int("arr_id"))
		}
	}
	return 0
}

// betterReleaseTargets is the seasons and movies whose best flags may be
// stale: a better_release finding (whole, or for some episodes), or an at_best
// or newer_revision one that lists a held group beside the best and alt ones.
func (st *Store) betterReleaseTargets() []SearchJob {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []SearchJob
	for _, f := range st.s.Findings {
		switch f.Str("status") {
		case "better_release":
		case "at_best", "newer_revision":
			keep := bestGroups(f)
			for g := range groupSet(f.Str("alt_groups")) {
				keep[g] = true
			}
			extra := false
			for g := range groupSet(f.Str("current_group")) {
				extra = extra || !keep[g]
			}
			if !extra {
				continue
			}
		default:
			continue
		}
		t := Target{Arr: f.Str("arr"), ID: int(f.Int("arr_id")), Season: int(f.Int("season"))}
		switch f.Str("scope") {
		case "movie":
			t.Season = -1
		case "season", "episodes":
		default:
			continue
		}
		if !slices.ContainsFunc(out, func(j SearchJob) bool { return j.key() == t.key() }) {
			out = append(out, SearchJob{Target: t, Title: f.Str("title")})
		}
	}
	return out
}

// findingsFor is every finding for the target: for a Sonarr season, its season
// and episode findings; else every finding of the series or movie.
func (st *Store) findingsFor(t Target) []Line {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []Line
	for _, f := range st.s.Findings {
		if f.Str("arr") != t.Arr || int(f.Int("arr_id")) != t.ID {
			continue
		}
		if t.Arr == "sonarr" && t.Season >= 0 {
			if sc := f.Str("scope"); (sc != "season" && sc != "episodes") || int(f.Int("season")) != t.Season {
				continue
			}
		}
		out = append(out, f)
	}
	return out
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

// fileNumber is the episode number a file name carries: an SxxEyy episode,
// else the last bare number before the bracketed tags.
func fileNumber(name string) (int, bool) {
	if m := seasonEpisode.FindStringSubmatch(name); m != nil {
		n, err := strconv.Atoi(m[2])
		return n, err == nil
	}
	stem := bracketed.ReplaceAllString(strings.TrimSuffix(name, filepath.Ext(name)), " ")
	if ms := bareNumber.FindAllStringSubmatch(stem+" ", -1); ms != nil {
		n, err := strconv.Atoi(ms[len(ms)-1][1])
		return n, err == nil
	}
	return 0, false
}
