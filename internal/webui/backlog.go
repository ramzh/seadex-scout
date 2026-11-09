package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// backlogRetry is how long a target whose SeaDex best is still not downloaded
// waits before the backlog searches it again; backlogShortRetry is the wait
// when the last search found SeaDex releases the arr rejected, since what the
// trackers and the feed offer changes from one day to the next (Fruit of
// Grisaia S1's pack was approved an hour after a search found none).
const (
	backlogRetry      = 7 * 24 * time.Hour
	backlogShortRetry = 24 * time.Hour
)

// backlogMark is the backlog's last search of one target: the best release it
// searched for, when, and whether that search found SeaDex releases.
type backlogMark struct {
	Release string    `json:"release"`
	At      time.Time `json:"at"`
	Found   bool      `json:"found,omitempty"`
}

// backlogJob is a target whose SeaDex best is not downloaded, and that best.
type backlogJob struct {
	SearchJob
	release string
}

// missingTargets lists the monitored seasons, episode ranges and films whose
// finding says the SeaDex best is not downloaded. An episode range searches
// its whole season, as the dashboard's button does.
// Nothing is listed until the monitored flags have been read once since the
// start, so an unmonitored season is never taken for a monitored one.
func (st *Store) missingTargets() []backlogJob {
	unmonitored := st.unmonitoredTest()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.monitored.At.IsZero() {
		return nil
	}
	var out []backlogJob
	for i := range st.s.Findings {
		f := &st.s.Findings[i]
		// A finding naming no best group has nothing a search can find
		// (Medaka Box S2: the best is on no tracker the scout can reach).
		if f.Str("status") != "not_downloaded" || len(bestGroups(*f)) == 0 || unmonitored(f) {
			continue
		}
		t := Target{Arr: f.Str("arr"), ID: int(f.Int("arr_id")), Season: -1}
		switch f.Str("scope") {
		case "season", "episodes":
			t.Season = int(f.Int("season"))
		case "movie", "series":
		default:
			continue
		}
		if t.ID <= 0 || slices.ContainsFunc(out, func(j backlogJob) bool { return j.key() == t.key() }) {
			continue
		}
		release := f.Str("release_url")
		if release == "" {
			release = f.Str("recommended_group")
		}
		out = append(out, backlogJob{SearchJob: SearchJob{Target: t, Title: f.Str("title")}, release: release})
	}
	return out
}

// autoBacklog puts in line a search for every monitored target whose SeaDex
// best is not downloaded. RSS only sees releases as they are published, so a
// best published before the series was added (or before SeaDex named it) is
// otherwise never grabbed. Each target is searched once per best release, and
// again after backlogRetry while it stays missing; the line starts one search
// a minute, so a long backlog drains slowly by design.
func (l *Library) autoBacklog() {
	jobs := l.store.missingTargets()
	now := time.Now().UTC()
	var queue []SearchJob
	l.mu.Lock()
	marks := make(map[string]backlogMark, len(jobs))
	for _, j := range jobs {
		k := j.key()
		retry := backlogRetry
		if m, ok := l.backlog[k]; ok && m.Found {
			retry = backlogShortRetry
		}
		if m, ok := l.backlog[k]; ok && m.Release == j.release && now.Sub(m.At) < retry {
			marks[k] = m
			continue
		}
		marks[k] = backlogMark{Release: j.release, At: now}
		queue = append(queue, j.SearchJob)
	}
	l.backlog = marks // targets no longer missing drop out
	l.mu.Unlock()
	if len(queue) == 0 {
		return
	}
	n, err := l.QueueSearches(queue, false)
	if err != nil {
		l.log.Warn("could not queue the backlog searches", "error", err)
		return
	}
	if n > 0 {
		l.log.Info("backlog searches queued for SeaDex bests not downloaded", "queued", n, "missing", len(jobs))
	}
}

// markFound records on the backlog's mark for key whether its last search
// found SeaDex releases, which shortens the wait before the next one.
func (l *Library) markFound(key string, found bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m, ok := l.backlog[key]; ok {
		m.Found = found
		l.backlog[key] = m
	}
}

// releaseDecision is the part of a /release answer afterNothing reads: Sonarr
// 5 nests the release and its verdict, Radarr 3 keeps them at the top.
type releaseDecision struct {
	DownloadAllowed *bool `json:"downloadAllowed"`
	Decision        *struct {
		Rejections []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"rejections"`
		Approved bool `json:"approved"`
	} `json:"decision"`
	Release *struct {
		GUID      string `json:"guid"`
		Title     string `json:"title"`
		IndexerID int    `json:"indexerId"`
	} `json:"release"`
	GUID              string   `json:"guid"`
	Title             string   `json:"title"`
	Rejections        []string `json:"rejections"`
	CustomFormatScore int      `json:"customFormatScore"`
	IndexerID         int      `json:"indexerId"`
	Approved          bool     `json:"approved"`
}

func (r *releaseDecision) grabbable() bool {
	ok := r.Approved
	if r.Decision != nil {
		ok = r.Decision.Approved
	}
	return ok && (r.DownloadAllowed == nil || *r.DownloadAllowed)
}

func (r *releaseDecision) ref() (guid, title string, indexer int) {
	if r.Release != nil {
		return r.Release.GUID, r.Release.Title, r.Release.IndexerID
	}
	return r.GUID, r.Title, r.IndexerID
}

// nothingVerdict is afterNothing's answer: why the search grabbed nothing,
// how many SeaDex releases (at the minimum score) the arr found, and the one
// the scout grabbed itself, if any.
type nothingVerdict struct {
	why     string
	grabbed string
	seadex  int
}

// afterNothing reads the arr's verdict on the releases it finds for t now,
// after a search that grabbed nothing. When the arr approves a SeaDex release
// its automatic search still skipped (There's No Freaking Way S1: a pack it
// maps to 17 episodes), the scout grabs the best-scored one itself. Otherwise
// it says why: the rejections of the SeaDex releases, most common first. It
// runs a second search, so the line calls it only when the scout does not
// already say t holds the best.
func (l *Library) afterNothing(ctx context.Context, a *arrAPI, t Target) nothingVerdict {
	q := url.Values{}
	switch {
	case t.Arr == "radarr":
		q.Set("movieId", strconv.Itoa(t.ID))
	case t.Season < 0:
		// Sonarr answers 400 for a series without a season.
		return nothingVerdict{why: "a whole-series search: Sonarr gives its verdict per season only; search a season for the reasons"}
	default:
		q.Set("seriesId", strconv.Itoa(t.ID))
		q.Set("seasonNumber", strconv.Itoa(t.Season))
	}
	var rels []releaseDecision
	if err := a.call(ctx, http.MethodGet, "/release?"+q.Encode(), nil, &rels); err != nil {
		return nothingVerdict{why: "could not read the release decisions: " + err.Error()}
	}
	counts := map[string]int{}
	var v nothingVerdict
	var pick *releaseDecision
	for i := range rels {
		r := &rels[i]
		if r.CustomFormatScore < l.minScore {
			continue
		}
		v.seadex++
		if r.grabbable() {
			if guid, _, ix := r.ref(); guid != "" && ix > 0 && (pick == nil || r.CustomFormatScore > pick.CustomFormatScore) {
				pick = r
			}
			continue
		}
		seen := map[string]bool{}
		add := func(s string) {
			if s != "" && !seen[s] {
				seen[s] = true
				counts[s]++
			}
		}
		if r.Decision != nil {
			for _, j := range r.Decision.Rejections {
				add(j.Message)
			}
		}
		for _, s := range r.Rejections {
			add(s)
		}
	}
	if v.seadex == 0 {
		v.why = fmt.Sprintf("no release reaching score %d among %d found", l.minScore, len(rels))
		return v
	}
	if pick != nil {
		guid, title, ix := pick.ref()
		body := map[string]any{"guid": guid, "indexerId": ix}
		if err := a.call(ctx, http.MethodPost, "/release", body, nil); err != nil {
			v.why = fmt.Sprintf("%s approves %q but its search skipped it, and the grab failed: %v", arrName(t.Arr), title, err)
			return v
		}
		v.grabbed = title
		v.why = fmt.Sprintf("%s approved it but its automatic search skipped it; the scout grabbed it", arrName(t.Arr))
		l.log.Info("approved SeaDex release grabbed after an empty search", "arr", t.Arr, "arr_id", t.ID, "season", t.Season, "release", title)
		return v
	}
	reasons := make([]string, 0, len(counts))
	for s := range counts {
		reasons = append(reasons, s)
	}
	slices.SortFunc(reasons, func(x, y string) int {
		if c := counts[y] - counts[x]; c != 0 {
			return c
		}
		return strings.Compare(x, y)
	})
	if len(reasons) > 3 {
		reasons = reasons[:3]
	}
	for i, s := range reasons {
		reasons[i] = fmt.Sprintf("%s (%d)", s, counts[s])
	}
	v.why = fmt.Sprintf("%d SeaDex releases rejected: %s", v.seadex, strings.Join(reasons, "; "))
	return v
}

// Monitor sets the target monitored in the arr, season and episodes for a
// Sonarr season, and puts a search for it at the head of the line. The page
// offers it on a season with a SeaDex best that Sonarr left unmonitored
// (BLUE LOCK S2, Oshi no Ko S2: new seasons of followed series).
func (l *Library) Monitor(ctx context.Context, t Target, title string) error {
	a, err := l.arr(t.Arr)
	if err != nil {
		return err
	}
	if t.ID <= 0 {
		return errors.New("no id to monitor")
	}
	if t.Arr == "radarr" {
		path := "/movie/" + strconv.Itoa(t.ID)
		var movie map[string]any
		if err := a.call(ctx, http.MethodGet, path, nil, &movie); err != nil {
			return err
		}
		movie["monitored"] = true
		if err := a.call(ctx, http.MethodPut, path, movie, nil); err != nil {
			return err
		}
	} else {
		if t.Season < 0 {
			return errors.New("pick a season to monitor")
		}
		path := "/series/" + strconv.Itoa(t.ID)
		var series map[string]any
		if err := a.call(ctx, http.MethodGet, path, nil, &series); err != nil {
			return err
		}
		seasons, _ := series["seasons"].([]any)
		found := false
		for _, s := range seasons {
			if m, ok := s.(map[string]any); ok && int(num(m["seasonNumber"])) == t.Season {
				m["monitored"], found = true, true
			}
		}
		if !found {
			return fmt.Errorf("season %d is not in this series", t.Season)
		}
		series["monitored"] = true
		if err := a.call(ctx, http.MethodPut, path, series, nil); err != nil {
			return err
		}
		var eps []struct {
			ID           int `json:"id"`
			SeasonNumber int `json:"seasonNumber"`
		}
		if err := a.call(ctx, http.MethodGet, "/episode?seriesId="+strconv.Itoa(t.ID), nil, &eps); err != nil {
			return err
		}
		var ids []int
		for _, e := range eps {
			if e.SeasonNumber == t.Season {
				ids = append(ids, e.ID)
			}
		}
		if len(ids) > 0 {
			if err := a.call(ctx, http.MethodPut, "/episode/monitor", map[string]any{"episodeIds": ids, "monitored": true}, nil); err != nil {
				return err
			}
		}
	}
	l.store.markMonitored(t)
	_, err = l.QueueSearches([]SearchJob{{Target: t, Title: title}}, true)
	return err
}

// num reads a JSON number decoded into any.
func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// Digest says what the library actions did since a moment, for the daily
// ntfy digest: the searches that finished (what they grabbed or imported, and
// why the others found nothing) and the automatic flag fixes since the last
// digest. ok is false when nothing happened.
func (l *Library) Digest(since time.Time) (string, bool) {
	l.mu.Lock()
	fixes := l.fixStats
	l.fixStats = FlagFix{}
	jobs := slices.Clone(l.searches)
	l.mu.Unlock()
	var got, nothing []string
	for _, j := range jobs {
		if j.Finished.Before(since) {
			continue
		}
		label := j.Title
		if j.Arr == "sonarr" && j.Season >= 0 {
			label += fmt.Sprintf(" S%d", j.Season)
		}
		switch j.State {
		case "grabbed", "imported":
			got = append(got, "• "+label+": "+strings.Join(j.Grabbed, " | "))
		case "nothing":
			nothing = append(nothing, "• "+label+": "+j.Message)
		}
	}
	if len(got) == 0 && len(nothing) == 0 && fixes.Fixed+fixes.Cleared == 0 {
		return "", false
	}
	var b strings.Builder
	list := func(head string, lines []string, most int) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s (%d)\n", head, len(lines))
		for i, s := range lines {
			if i == most {
				fmt.Fprintf(&b, "• … %d more on the dashboard\n", len(lines)-most)
				break
			}
			b.WriteString(clip(s, 220) + "\n")
		}
		b.WriteString("\n")
	}
	list("Grabbed or imported", got, 10)
	list("Searched, nothing grabbed", nothing, 8)
	if fixes.Fixed+fixes.Cleared > 0 {
		fmt.Fprintf(&b, "Flags: %d files given the best flags (%d as the alt for an episode no best has), %d stale ones cleared\n",
			fixes.Fixed, fixes.AltCovers, fixes.Cleared)
	}
	return strings.TrimSpace(b.String()), true
}
