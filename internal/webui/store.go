// Package webui is seadex-scout's built-in dashboard: a small HTTP server that
// shows the current upgrades, the library overview, the scout's health and the
// reports, with no Grafana, Loki or Alertmanager in between.
//
// It reads the same structured log lines the Grafana dashboard and the Loki
// rules read (the log contract in docs/monitoring.md), through a slog.Handler
// that tees each record into a Store before passing it on to the real handler.
// The scout itself is untouched: what the dashboard shows is exactly what the
// log says, so the two views can never disagree.
package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

// stateVersion is bumped when the persisted layout changes incompatibly; a file
// with another version is ignored rather than misread.
const stateVersion = 1

// Ring sizes: each list keeps its newest entries only, so memory and the
// persisted file stay bounded however long the container runs.
const (
	maxErrors     = 200
	maxDegraded   = 100
	maxEvents     = 300
	maxCycles     = 60
	maxReports    = 20
	maxLogLines   = 500
	maxPending    = 4
	maxPassLines  = 50000
	stateFileMode = 0o600
)

// Line is one observed log record, with its attributes flattened to
// JSON-friendly values.
type Line struct {
	Time  time.Time      `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Int returns the integer attribute key, or 0 when it is absent or not a number.
func (l *Line) Int(key string) int64 {
	switch v := l.Attrs[key].(type) {
	case int64:
		return v
	case uint64:
		return int64(v) //nolint:gosec // log counters never reach 2^63
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

// Str returns the string attribute key, or "" when it is absent.
func (l *Line) Str(key string) string {
	if s, ok := l.Attrs[key].(string); ok {
		return s
	}
	return ""
}

// Pass describes the newest completed pass of the findings.
type Pass struct {
	ID           int64     `json:"id"`
	At           time.Time `json:"at"`
	Total        int64     `json:"total"`
	Emitted      int64     `json:"emitted"`
	Suppressed   int64     `json:"suppressed"`
	ManualReview int64     `json:"manual_review"`
}

// Indexer counts the Torznab feed's traffic since the dashboard first ran.
type Indexer struct {
	Requests     int64            `json:"requests"`
	FeedRequests int64            `json:"feed_requests"`
	Returned     int64            `json:"returned"`
	ByScope      map[string]int64 `json:"by_scope,omitempty"`
	LastRequest  time.Time        `json:"last_request,omitzero"`
	LastSnapshot *Line            `json:"last_snapshot,omitempty"`
}

// state is everything the dashboard shows that survives a restart.
type state struct {
	Version int `json:"version"`

	Pass     Pass             `json:"pass"`
	Findings []Line           `json:"findings"`
	Views    map[string]int64 `json:"views,omitempty"`

	Library *Line `json:"library,omitempty"`

	LastCheck         *Line     `json:"last_check,omitempty"`
	LastFullCheck     *Line     `json:"last_full_check,omitempty"`
	ReconcileStarted  time.Time `json:"reconcile_started,omitzero"`
	ReconcileComplete time.Time `json:"reconcile_complete,omitzero"`
	Cycles            []Line    `json:"cycles,omitempty"`

	Errors     []Line          `json:"errors,omitempty"`
	Degraded   []Line          `json:"degraded,omitempty"`
	Conditions map[string]Line `json:"conditions,omitempty"`
	// Notified holds the problems a notification announced and has not yet
	// announced the end of, so a restart neither repeats nor loses them.
	Notified map[string]time.Time `json:"notified,omitempty"`
	Events   []Line               `json:"events,omitempty"`

	Indexer Indexer `json:"indexer"`
	Reports []Line  `json:"reports,omitempty"`
}

// Store holds the dashboard's view of the scout, built from the log lines the
// Handler feeds it. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	path    string
	now     func() time.Time
	started time.Time
	dirty   bool

	s       state
	config  *Line
	pending map[int64][]Line
	logs    []Line
	job     ReportJob
	// monitored is not persisted: it is read again within a minute of a start.
	monitored Monitored

	// Notifications (ntfy.go): events is nil until a Notifier reads it, found
	// holds each pass's new upgrades until the pass closes.
	events chan Event
	found  map[int64][]Line
	notify NotifyStatus

	refresh RefreshJob // the dashboard's local library refresh (refresh.go)
	version string     // the build, for the footer
}

// SetVersion names the running build for the dashboard's footer.
func (st *Store) SetVersion(v string) {
	st.mu.Lock()
	st.version = v
	st.mu.Unlock()
}

// NewStore returns an empty Store that persists to path ("" keeps it in memory).
func NewStore(path string) *Store {
	st := &Store{path: path, now: time.Now, pending: map[int64][]Line{}}
	st.started = st.now()
	st.s = freshState()
	return st
}

func freshState() state {
	return state{Version: stateVersion, Conditions: map[string]Line{}, Notified: map[string]time.Time{}, Views: map[string]int64{}}
}

// Load reads the persisted state, if any. A missing file is not an error; a file
// it cannot read is reported and left in place, and the Store starts empty.
func (st *Store) Load() error {
	if st.path == "" {
		return nil
	}
	raw, err := os.ReadFile(st.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	loaded := freshState()
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("dashboard state is not valid JSON: %w", err)
	}
	if loaded.Version != stateVersion {
		return fmt.Errorf("dashboard state has version %d, want %d; starting empty", loaded.Version, stateVersion)
	}
	if loaded.Conditions == nil {
		loaded.Conditions = map[string]Line{}
	}
	if loaded.Notified == nil {
		loaded.Notified = map[string]time.Time{}
	}
	if loaded.Views == nil {
		loaded.Views = map[string]int64{}
	}
	// Restarts before the dedupe in Observe left repeated "found at start"
	// events; replay them through the same rule.
	events := loaded.Events
	loaded.Events = nil
	st.mu.Lock()
	st.s = loaded
	for _, e := range events {
		if after, _ := e.Attrs["after_start"].(bool); after && st.stillFound(e) {
			continue
		}
		st.s.Events = append(st.s.Events, e)
	}
	st.mu.Unlock()
	return nil
}

// Save writes the state when it changed since the last save, through a temp
// file and a rename so a crash never leaves half a file.
func (st *Store) Save() error {
	if st.path == "" {
		return nil
	}
	st.mu.Lock()
	if !st.dirty {
		st.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(&st.s)
	st.dirty = false
	st.mu.Unlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(st.path)
	tmp, err := os.CreateTemp(dir, ".webui-state-*.json")
	if err != nil {
		st.markDirty()
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after a successful rename
	if err := tmp.Chmod(stateFileMode); err == nil {
		_, err = tmp.Write(raw)
		if err == nil {
			err = tmp.Sync()
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), st.path)
		}
	} else {
		tmp.Close() //nolint:errcheck,gosec // the chmod error is the one reported
	}
	if err != nil {
		st.markDirty()
	}
	return err
}

func (st *Store) markDirty() {
	st.mu.Lock()
	st.dirty = true
	st.mu.Unlock()
}

// findingMsgs are the per-finding lines notify emits on every pass.
var findingMsgs = map[string]bool{
	"better release available":                            true,
	"series spans multiple release groups, manual review": true,
	"release group unverifiable, manual review":           true,
	"SeaDex entry is incomplete":                          true,
	"SeaDex lists a theoretical best only":                true,
	"seadex finding":                                      true,
	// This fork's Extended rows (compare.Config.Extended).
	"SeaDex entry not downloaded":      true,
	"library holds the SeaDex best":    true,
	"SeaDex lists no best you can get": true,
}

// quietMsgs are high-volume lines the dashboard reads but leaves out of the
// recent-log list, so that list stays readable.
var quietMsgs = map[string]bool{
	"indexer request": true,
	"report item":     true,
	"upgrade sizes":   true,
	// Activity lists these.
	"upgrade found":    true,
	"upgrade resolved": true,
}

// quietLine also leaves out the indexer's per-request lines.
func quietLine(l Line) bool {
	return quietMsgs[l.Msg] || (l.Msg == "http" && l.Str("component") == "indexer")
}

// eventKey names the upgrade an event is about.
func eventKey(l Line) string {
	return fmt.Sprint(l.Attrs["arr"], l.Attrs["al_id"], l.Attrs["season"], l.Attrs["episodes"])
}

// stillFound reports that the newest event for l's upgrade is a found one.
func (st *Store) stillFound(l Line) bool {
	key := eventKey(l)
	for i := len(st.s.Events) - 1; i >= 0; i-- {
		if e := st.s.Events[i]; eventKey(e) == key {
			return e.Msg == "upgrade found"
		}
	}
	return false
}

// Observe records one log line. It is the Handler's only entry point.
func (st *Store) Observe(l Line) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if findingMsgs[l.Msg] && l.Attrs["pass_id"] != nil {
		pass := l.Int("pass_id")
		if len(st.pending[pass]) < maxPassLines {
			st.pending[pass] = append(st.pending[pass], l)
		}
		st.trimPending()
		return
	}
	if !quietLine(l) {
		st.logs = pushRing(st.logs, l, maxLogLines)
	}
	if l.Level == slog.LevelError.String() {
		st.observeError(l)
	}

	switch l.Msg {
	case "findings reported":
		pass := l.Int("pass_id")
		st.s.Findings = st.pending[pass]
		if st.s.Findings == nil {
			st.s.Findings = []Line{}
		}
		st.s.Pass = Pass{
			ID: pass, At: l.Time, Total: l.Int("total"), Emitted: l.Int("emitted"),
			Suppressed: l.Int("suppressed"), ManualReview: l.Int("manual_review"),
		}
		for id := range st.pending {
			if id <= pass {
				delete(st.pending, id)
			}
		}
		st.closeFound(pass)
	case "upgrade sizes":
		st.s.Views[l.Str("hidden_tier")] = l.Int("upgrades")
	case "upgrade found", "upgrade resolved":
		// After a restart every open upgrade is found again; one the list
		// already shows as found adds nothing.
		if after, _ := l.Attrs["after_start"].(bool); after && st.stillFound(l) {
			break
		}
		st.s.Events = pushRing(st.s.Events, l, maxEvents)
		// After a start every upgrade reads as found; only real arrivals notify.
		if after, _ := l.Attrs["after_start"].(bool); l.Msg == "upgrade found" && !after && st.events != nil {
			if st.found == nil {
				st.found = map[int64][]Line{}
			}
			pass := l.Int("pass_id")
			st.found[pass] = append(st.found[pass], l)
		}
	case "library summary":
		st.s.Library = &l
	case "tick complete", "tick degraded":
		st.s.LastCheck = &l
		if l.Msg == "tick degraded" {
			st.s.Degraded = pushRing(st.s.Degraded, l, maxDegraded)
		}
	case "cycle complete", "cycle degraded":
		st.s.LastCheck = &l
		st.s.LastFullCheck = &l
		st.s.Cycles = pushRing(st.s.Cycles, l, maxCycles)
		if l.Msg == "cycle degraded" {
			st.s.Degraded = pushRing(st.s.Degraded, l, maxDegraded)
		}
	case "reconcile started":
		st.s.ReconcileStarted = l.Time
	case "reconcile complete":
		st.s.ReconcileComplete = l.Time
	case "indexer request":
		st.s.Indexer.Requests++
		if b, ok := l.Attrs["feed"].(bool); ok && b {
			st.s.Indexer.FeedRequests++
		}
		st.s.Indexer.Returned += l.Int("returned")
		if scope := l.Str("scope"); scope != "" {
			if st.s.Indexer.ByScope == nil {
				st.s.Indexer.ByScope = map[string]int64{}
			}
			st.s.Indexer.ByScope[scope]++
		}
		st.s.Indexer.LastRequest = l.Time
	case "indexer feed snapshot written":
		st.s.Indexer.LastSnapshot = &l
	case "report written":
		st.s.Reports = pushRing(st.s.Reports, l, maxReports)
	case "configuration loaded":
		st.config = &l
		return // not persisted, so nothing became dirty
	default:
		if l.Level != slog.LevelError.String() {
			return
		}
	}
	st.dirty = true
}

func (st *Store) observeError(l Line) {
	if cond := l.Str("condition"); cond != "" {
		st.s.Conditions[cond] = l
		if _, told := st.s.Notified[cond]; !told && st.events != nil {
			st.s.Notified[cond] = l.Time
			st.emit(Event{Kind: EventProblems, Key: cond, Lines: []Line{l}})
		}
		return
	}
	st.s.Errors = pushRing(st.s.Errors, l, maxErrors)
	st.emit(Event{Kind: EventErrors, Lines: []Line{l}})
}

// trimPending drops the oldest unfinished passes, so a pass that never logs
// its closing line cannot hold memory forever.
func (st *Store) trimPending() {
	if len(st.pending) <= maxPending {
		return
	}
	ids := slices.Sorted(maps.Keys(st.pending))
	for _, id := range ids[:len(ids)-maxPending] {
		delete(st.pending, id)
	}
}

func pushRing(ring []Line, l Line, limit int) []Line {
	ring = append(ring, l)
	if over := len(ring) - limit; over > 0 {
		ring = slices.Delete(ring, 0, over)
	}
	return ring
}

// Snapshot is what /api/state serves.
type Snapshot struct {
	Now       time.Time    `json:"now"`
	StartedAt time.Time    `json:"started_at"`
	Config    *Line        `json:"config,omitempty"`
	ReportJob ReportJob    `json:"report_job"`
	Monitored Monitored    `json:"monitored"`
	Notify    NotifyStatus `json:"notify"`
	Qbit      QbitStatus   `json:"qbit"`
	Usenet    UsenetStatus `json:"usenet"`
	Refresh   RefreshJob   `json:"refresh"`
	Version   string       `json:"version,omitempty"`
	// ArrScored lists the seasons, series and films whose every file Sonarr
	// or Radarr scores at the minimum (ScoreScan.Clean).
	ArrScored []string `json:"arr_scored,omitempty"`
	// ProvenBest lists the upgrades whose files came from the SeaDex best
	// torrent itself (ScoreScan.ProvenBest).
	ProvenBest []string `json:"proven_best,omitempty"`
	// OnDisk maps "al_id:season" of a finding not at the best to the
	// finished qBittorrent torrent that is its best (Library.OnDisk).
	OnDisk map[string]string `json:"on_disk,omitempty"`
	state
}

// Snapshot returns a copy of the current view, safe to marshal after the call.
func (st *Store) Snapshot() Snapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.s
	s.Findings = slices.Clone(s.Findings)
	s.Errors = slices.Clone(s.Errors)
	s.Degraded = slices.Clone(s.Degraded)
	s.Events = slices.Clone(s.Events)
	s.Cycles = slices.Clone(s.Cycles)
	s.Reports = slices.Clone(s.Reports)
	s.Conditions = maps.Clone(s.Conditions)
	s.Notified = maps.Clone(s.Notified)
	s.Views = maps.Clone(s.Views)
	s.Indexer.ByScope = maps.Clone(s.Indexer.ByScope)
	m := st.monitored
	m.Unmonitored = slices.Clone(m.Unmonitored)
	n := st.notify
	return Snapshot{Now: st.now(), StartedAt: st.started, Config: st.config, ReportJob: st.job, Monitored: m, Notify: n, Refresh: st.refresh, Version: st.version, state: s}
}

// Logs returns the recent log lines, newest last.
func (st *Store) Logs() []Line {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.logs)
}

func (st *Store) setJob(update func(*ReportJob)) {
	st.mu.Lock()
	update(&st.job)
	st.mu.Unlock()
}

func (st *Store) reportJob() ReportJob {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.job
}
