package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Searches the dashboard asked Sonarr or Radarr for, followed to the end: the
// command's progress, then any release it grabbed. Bulk searches wait in line
// and start one a minute, so the trackers see a trickle and the page need not
// stay open.

const (
	searchGap      = time.Minute     // between two searches started from the line
	searchPoll     = 5 * time.Second // while any search is unfinished
	searchesKept   = 200             // finished searches kept for the page
	searchGiveUp   = 2 * time.Hour   // a command never seen finishing
	searchGrabSkew = 5 * time.Second // the arr's clock against ours
)

// SearchJob is one search. State is "waiting" (in line), "queued" or
// "started" (the arr's command), then "grabbed", "imported" (from a best
// torrent already seeding), "nothing" (the command finished without a grab),
// "failed" or "cancelled".
type SearchJob struct {
	Target
	Title    string    `json:"title,omitempty"`
	State    string    `json:"state"`
	Command  int       `json:"command,omitempty"`
	Added    time.Time `json:"added"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	Message  string    `json:"message,omitempty"`
	Grabbed  []string  `json:"grabbed,omitempty"`
}

func (j *SearchJob) open() bool {
	return j.State == "waiting" || j.State == "queued" || j.State == "started"
}

// Searches lists the searches, newest first.
func (l *Library) Searches() []SearchJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.searches)
}

// QueueSearches puts targets in line behind any waiting search, skipping a
// target already waiting or running; now starts the first one at once. It
// returns how many it added.
func (l *Library) QueueSearches(targets []SearchJob, now bool) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	added := 0
	for _, t := range targets {
		if _, err := l.arr(t.Arr); err != nil {
			return added, err
		}
		if t.ID <= 0 {
			return added, errors.New("no id to search")
		}
		if slices.ContainsFunc(l.searches, func(j SearchJob) bool { return j.open() && j.key() == t.key() }) {
			continue
		}
		j := SearchJob{Target: t.Target, Title: t.Title, State: "waiting", Added: time.Now().UTC()}
		if now {
			// The oldest waiting search starts next, so a search asked for
			// now goes to the far end of the list, ahead of the line.
			l.searches = append(l.searches, j)
		} else {
			l.searches = append([]SearchJob{j}, l.searches...)
		}
		added++
	}
	if now && added > 0 {
		l.lastSearch = time.Time{}
	}
	l.persistLocked()
	l.wakeSearches()
	return added, nil
}

// CancelSearches drops every search still waiting in line.
func (l *Library) CancelSearches() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for i := range l.searches {
		if l.searches[i].State == "waiting" {
			l.searches[i].State, l.searches[i].Finished = "cancelled", time.Now().UTC()
			n++
		}
	}
	l.persistLocked()
	return n
}

// ClearSearches forgets every finished search.
func (l *Library) ClearSearches() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.searches = slices.DeleteFunc(l.searches, func(j SearchJob) bool { return !j.open() })
	l.persistLocked()
}

func (l *Library) wakeSearches() {
	select {
	case l.searchWake <- struct{}{}:
	default:
	}
}

// runSearches starts waiting searches and follows the running ones until ctx
// ends.
func (l *Library) runSearches(ctx context.Context) {
	for {
		l.stepSearches(ctx)
		select {
		case <-ctx.Done():
			return
		case <-l.searchWake:
		case <-time.After(searchPoll):
		}
	}
}

// stepSearches starts the oldest waiting search when the gap has passed and
// nothing else runs, and updates the running ones.
func (l *Library) stepSearches(ctx context.Context) {
	l.mu.Lock()
	var running []SearchJob
	next := -1
	for i, j := range l.searches {
		switch j.State {
		case "queued", "started":
			running = append(running, j)
		case "waiting":
			next = i // the list is newest first, so the last one seen is the oldest
		}
	}
	start := next >= 0 && len(running) == 0 && time.Since(l.lastSearch) >= searchGap
	var job SearchJob
	if start {
		job = l.searches[next]
		l.lastSearch = time.Now()
	}
	l.mu.Unlock()

	if start {
		id, err := l.Search(ctx, job.Target)
		job.Started = time.Now().UTC()
		if err != nil {
			job.State, job.Message, job.Finished = "failed", err.Error(), job.Started
		} else {
			job.State, job.Command = "queued", id
			l.log.Info("search started", "arr", job.Arr, "arr_id", job.ID, "season", job.Season, "title", job.Title, "command", id)
		}
		l.saveSearch(job)
	}
	for _, j := range running {
		if ctx.Err() != nil {
			return
		}
		l.saveSearch(l.followSearch(ctx, j))
	}
}

// saveSearch writes j back over the entry with the same target and start.
func (l *Library) saveSearch(j SearchJob) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.searches {
		if l.searches[i].key() == j.key() && l.searches[i].Added.Equal(j.Added) {
			if l.searches[i].State == "cancelled" {
				return
			}
			l.searches[i] = j
			break
		}
	}
	var keep []SearchJob
	closed := 0
	for _, s := range l.searches {
		if !s.open() {
			if closed++; closed > searchesKept {
				continue
			}
		}
		keep = append(keep, s)
	}
	l.searches = keep
	l.persistLocked()
}

// followSearch reads the command's status and, once it is done, the grabs it
// made.
func (l *Library) followSearch(ctx context.Context, j SearchJob) SearchJob {
	a, err := l.arr(j.Arr)
	if err != nil {
		j.State, j.Message, j.Finished = "failed", err.Error(), time.Now().UTC()
		return j
	}
	var cmd struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	err = a.call(ctx, http.MethodGet, "/command/"+strconv.Itoa(j.Command), nil, &cmd)
	switch {
	case err != nil && time.Since(j.Started) > searchGiveUp:
		j.State, j.Message, j.Finished = "failed", "the command was never seen finishing: "+err.Error(), time.Now().UTC()
		return j
	case err != nil:
		return j // try again on the next poll
	case (cmd.Status == "queued" || cmd.Status == "started") && time.Since(j.Started) > searchGiveUp:
		// A command stuck behind others is given up on; the line moves on.
		j.State, j.Finished = "failed", time.Now().UTC()
		j.Message = fmt.Sprintf("still %s after %s", cmd.Status, searchGiveUp)
		return j
	}
	j.Message = cmd.Message
	switch cmd.Status {
	case "queued", "started":
		j.State = cmd.Status
		return j
	case "completed":
	default: // failed, aborted, cancelled, orphaned
		j.State, j.Finished = "failed", time.Now().UTC()
		j.Message = strings.TrimSpace(cmd.Status + ": " + cmd.Message)
		return j
	}
	grabs, err := l.grabsSince(ctx, a, j.Target, j.Started.Add(-searchGrabSkew))
	if err != nil {
		return j // the command is done; read the grabs on the next poll
	}
	j.Grabbed, j.Finished = grabs, time.Now().UTC()
	j.State = "nothing"
	var seeded SeedingImport
	if len(grabs) == 0 && j.Arr == "sonarr" && j.Season >= 0 && l.Qbit != nil {
		// A best already seeding cannot be grabbed again (qBittorrent 409):
		// import from it instead.
		seeded, _ = l.ImportSeeding(ctx, j.Target)
	}
	if len(grabs) > 0 {
		j.State = "grabbed"
	} else if seeded.Files > 0 {
		j.State, j.Grabbed = "imported", []string{seeded.Torrent}
		j.Message = fmt.Sprintf("%d files imported from %s, which qBittorrent already seeds", seeded.Files, seeded.Torrent)
	} else if fs := l.store.findingsFor(j.Target); len(fs) > 0 && !slices.ContainsFunc(fs, func(f Line) bool { return f.Str("status") != "at_best" }) {
		j.Message = "the library already holds the SeaDex best"
	} else {
		v := l.afterNothing(ctx, a, j.Target)
		j.Message = v.why
		if v.grabbed != "" {
			j.State, j.Grabbed = "grabbed", []string{v.grabbed}
		} else if l.DirectCategory != "" && j.Arr == "sonarr" && j.Season >= 0 {
			// Nothing Sonarr will take: the scout adds the best itself.
			if name, err := l.DirectGrab(ctx, j.Target); err != nil {
				l.log.Debug("no direct grab", "arr_id", j.ID, "season", j.Season, "error", err)
			} else if name != "" {
				j.State, j.Grabbed = "grabbed", []string{name}
				j.Message = "added to qBittorrent directly: " + name + "; imported by the AniList episode map once complete"
			}
		}
		l.markFound(j.key(), v.seadex > 0 && v.grabbed == "")
	}
	l.log.Info("search finished", "arr", j.Arr, "arr_id", j.ID, "season", j.Season, "title", j.Title, "result", j.State, "grabbed", strings.Join(grabs, " | "), "why", j.Message)
	return j
}

// grabsSince lists the releases the arr grabbed for the target since a
// moment: for a season, only grabs of that season's episodes, so an RSS grab
// of another season during the search is not taken for its result.
func (l *Library) grabsSince(ctx context.Context, a *arrAPI, t Target, since time.Time) ([]string, error) {
	id := t.ID
	type rec struct {
		EventType   string    `json:"eventType"`
		Date        time.Time `json:"date"`
		SourceTitle string    `json:"sourceTitle"`
		EpisodeID   int       `json:"episodeId"`
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
		err = a.call(ctx, http.MethodGet, fmt.Sprintf("/history?seriesIds=%d&page=1&pageSize=1000", id), nil, &page)
		recs = page.Records
	default:
		err = a.call(ctx, http.MethodGet, "/history/series?seriesId="+strconv.Itoa(id), nil, &recs)
	}
	if err != nil {
		return nil, err
	}
	var inSeason map[int]bool
	if a.name == "sonarr" && t.Season >= 0 {
		var eps []struct {
			ID           int `json:"id"`
			SeasonNumber int `json:"seasonNumber"`
		}
		if err := a.call(ctx, http.MethodGet, "/episode?seriesId="+strconv.Itoa(id), nil, &eps); err != nil {
			return nil, err
		}
		inSeason = map[int]bool{}
		for _, e := range eps {
			if e.SeasonNumber == t.Season {
				inSeason[e.ID] = true
			}
		}
	}
	var out []string
	for _, r := range recs {
		if inSeason != nil && !inSeason[r.EpisodeID] {
			continue
		}
		if r.EventType == "grabbed" && !r.Date.Before(since) && !slices.Contains(out, r.SourceTitle) {
			out = append(out, r.SourceTitle)
		}
	}
	return out, nil
}
