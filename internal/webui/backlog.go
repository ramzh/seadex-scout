package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// backlogRetry is how long a target whose SeaDex best is still not downloaded
// waits before the backlog searches it again.
const backlogRetry = 7 * 24 * time.Hour

// backlogMark is the backlog's last search of one target: the best release it
// searched for, and when.
type backlogMark struct {
	Release string    `json:"release"`
	At      time.Time `json:"at"`
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
		if f.Str("status") != "not_downloaded" || unmonitored(f) {
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
		if m, ok := l.backlog[k]; ok && m.Release == j.release && now.Sub(m.At) < backlogRetry {
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

// release is the part of a /release answer whySearchFound reads: Sonarr and
// Radarr 5 put the verdict under "decision", 3 and 4 at the top.
type releaseDecision struct {
	CustomFormatScore int `json:"customFormatScore"`
	Decision          *struct {
		Rejections []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"rejections"`
	} `json:"decision"`
	Rejections []string `json:"rejections"`
}

// whyNothing reads the arr's verdict on the releases it finds for t now and
// says why none was grabbed: the rejections of the releases that reach the
// minimum score (the SeaDex ones), most common first. It runs a second search,
// so the line calls it only when the scout does not already say t holds the best.
func (l *Library) whyNothing(ctx context.Context, a *arrAPI, t Target) string {
	q := url.Values{}
	if t.Arr == "radarr" {
		q.Set("movieId", strconv.Itoa(t.ID))
	} else {
		q.Set("seriesId", strconv.Itoa(t.ID))
		if t.Season >= 0 {
			q.Set("seasonNumber", strconv.Itoa(t.Season))
		}
	}
	var rels []releaseDecision
	if err := a.call(ctx, http.MethodGet, "/release?"+q.Encode(), nil, &rels); err != nil {
		return "could not read the release decisions: " + err.Error()
	}
	counts := map[string]int{}
	seadex := 0
	for _, r := range rels {
		if r.CustomFormatScore < l.minScore {
			continue
		}
		seadex++
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
	if seadex == 0 {
		return fmt.Sprintf("no release reaching score %d among %d found", l.minScore, len(rels))
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
	if len(reasons) == 0 {
		return fmt.Sprintf("%d SeaDex releases found and none rejected; the arr may grab on its next pass", seadex)
	}
	return fmt.Sprintf("%d SeaDex releases rejected: %s", seadex, strings.Join(reasons, "; "))
}
