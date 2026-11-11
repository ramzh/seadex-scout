package webui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// SeedingImport is what ImportSeeding did.
type SeedingImport struct {
	Torrent string   `json:"torrent,omitempty"`
	Files   int      `json:"files"`
	Command int      `json:"command,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

// ImportSeeding imports a Sonarr season's missing or low-scoring episodes from
// a SeaDex best torrent qBittorrent already holds. A search cannot: Sonarr
// grabs the same torrent again and qBittorrent refuses it with 409 Conflict
// (Misfit S2's -ZR- pack after its untagged E01 lost the best flags, Nanbaka
// S1's ALiEN pack). The torrent must be complete and one the curation marks as
// best for this series (bestIndex); its files go in as hardlinks with the best
// flags, the way Import does for a queued download. Radarr is left to its own
// search.
// ponytail: Sonarr only; a film already seeding is rare enough to import by hand.
func (l *Library) ImportSeeding(ctx context.Context, t Target) (SeedingImport, error) {
	var res SeedingImport
	if t.Arr != "sonarr" || t.Season < 0 {
		return res, errors.New("only a Sonarr season imports from a seeding torrent")
	}
	if l.Qbit == nil {
		return res, errors.New("qBittorrent is not set up")
	}
	a, err := l.arr(t.Arr)
	if err != nil {
		return res, err
	}
	idx, err := l.bestIndex(ctx)
	if len(idx) == 0 {
		if err == nil {
			err = errors.New("no SeaDex best torrent is known")
		}
		return res, err
	}
	tvdb, err := l.tvdbOf(ctx, a, t.ID)
	if err != nil {
		return res, err
	}
	torrents, err := l.Qbit.Torrents(ctx)
	if err != nil {
		return res, err
	}
	var seeding []QbitTorrent
	for _, q := range torrents {
		if tvdbs, ok := idx[strings.ToLower(q.Hash)]; ok && q.Progress >= 1 && slices.Contains(tvdbs, tvdb) {
			seeding = append(seeding, q)
		}
	}
	if len(seeding) == 0 {
		return res, nil
	}
	files, err := l.files(ctx, a, t.ID)
	if err != nil {
		return res, err
	}
	score := map[int]int{}
	for _, f := range files {
		score[f.ID] = f.CustomFormatScore
	}
	reasons := map[string]int{}
	runs := l.runs(ctx)
	var eps []sonarrEpisode
	for _, q := range seeding {
		var rows []manualImportRow
		// No seriesId: with one, Sonarr scans the series' library folder, not
		// the torrent, and "imports" the library's own files onto themselves
		// (Haganai S00E02 was deleted as an upgrade of itself).
		v := url.Values{"folder": {q.ContentPath}, "filterExistingFiles": {"false"}}
		if err := a.call(ctx, http.MethodGet, "/manualimport?"+v.Encode(), nil, &rows); err != nil {
			return res, err
		}
		if run, ok := runOf(q, runs); ok && run.Season == t.Season {
			if eps == nil {
				if err := a.call(ctx, http.MethodGet, "/episode?seriesId="+strconv.Itoa(t.ID), nil, &eps); err != nil {
					return res, err
				}
			}
			if n := placeByRun(rows, run, t.ID, eps); n > 0 {
				l.log.Debug("seeding best placed by the AniList episode map", "torrent", q.Name, "files", n, "season", run.Season)
			}
		}
		var take []map[string]any
		for _, r := range rows {
			if r.Series == nil || r.Series.ID != t.ID || len(r.Episodes) == 0 || !inside(r.Path, q.ContentPath) {
				continue
			}
			want, other := false, false
			ids := make([]int, 0, len(r.Episodes))
			for _, e := range r.Episodes {
				ids = append(ids, e.ID)
				other = other || e.SeasonNumber != t.Season
				want = want || e.EpisodeFileID == 0 || score[e.EpisodeFileID] < l.minScore
			}
			if other || !want {
				continue
			}
			blocked := ""
			for _, rj := range r.Rejections {
				if !importableRejection(rj.Reason) {
					blocked = rj.Reason
					break
				}
			}
			if blocked != "" {
				reasons[blocked]++
				continue
			}
			// No downloadId: Sonarr 5's ManualImport dereferences the tracked
			// download's ImportItem after importing, which is null for a
			// download it already finished with; the command fails after the
			// files moved (DanMachi S4: E02-E11 in, best flags lost). An
			// untracked import keeps the flags.
			take = append(take, map[string]any{
				"path": r.Path, "folderName": r.FolderName, "seriesId": t.ID, "episodeIds": ids,
				"quality": r.Quality, "languages": r.Languages, "releaseGroup": r.ReleaseGroup,
				"releaseType": r.ReleaseType, "indexerFlags": bestFlags(t.Arr),
			})
		}
		if len(take) == 0 {
			continue
		}
		var cmd struct {
			ID int `json:"id"`
		}
		if err := a.call(ctx, http.MethodPost, "/command",
			// "copy", never "auto": with no downloadId Sonarr does not know the
			// files are a torrent's, and "auto" MOVES them out of it (Kyousougiga
			// and six other packs emptied, 2026-11-11). "copy" hardlinks when
			// Sonarr's "Use Hardlinks instead of Copy" is on.
			map[string]any{"name": "ManualImport", "files": take, "importMode": "copy"}, &cmd); err != nil {
			return res, err
		}
		res.Torrent, res.Files, res.Command = q.Name, len(take), cmd.ID
		l.log.Info("imported from a SeaDex best torrent already seeding", "arr", t.Arr, "arr_id", t.ID, "season", t.Season,
			"torrent", q.Name, "files", len(take))
		l.forget(t)
		return res, nil
	}
	for reason, n := range reasons {
		res.Reasons = append(res.Reasons, fmt.Sprintf("%d × %s", n, reason))
	}
	slices.Sort(res.Reasons)
	return res, nil
}

// autoSeeding runs ImportSeeding on every monitored Sonarr season the backlog
// lists, each tick, so a best qBittorrent already holds fills its episodes
// without a search. A search that "grabs" a pack qBittorrent already has is
// followed by nothing: Sonarr imported SAO S4 and DanMachi S4 part 1's E01
// from the grab's name and called the rest unexpected.
// ponytail: one manualimport scan per seeding best per target per tick; cache
// the empty answers if the series count grows large.
func (l *Library) autoSeeding(ctx context.Context) {
	if l.Qbit == nil {
		return
	}
	for _, j := range l.store.missingTargets() {
		if ctx.Err() != nil {
			return
		}
		if j.Arr != "sonarr" || j.Season < 0 {
			continue
		}
		if _, err := l.ImportSeeding(ctx, j.Target); err != nil {
			l.log.Debug("seeding import skipped", "arr_id", j.ID, "season", j.Season, "error", err)
		}
	}
}

// inside reports whether path is root or a file under it: only the torrent's
// own files may be imported.
func inside(path, root string) bool {
	root = strings.TrimRight(root, "/")
	return root != "" && (path == root || strings.HasPrefix(path, root+"/"))
}

// EpisodeRun is where a SeaDex best's entry episodes land in Sonarr, from the
// AniList-to-TVDB episode map: entry episode n is episode First+n-1 of Season.
type EpisodeRun struct{ Season, First, Last int }

// sonarrEpisode is one row of Sonarr's /episode list.
type sonarrEpisode struct {
	ID            int `json:"id"`
	SeasonNumber  int `json:"seasonNumber"`
	EpisodeNumber int `json:"episodeNumber"`
	EpisodeFileID int `json:"episodeFileId"`
}

// runs reads the episode runs of the SeaDex bests; none when unavailable.
func (l *Library) runs(ctx context.Context) map[string]EpisodeRun {
	if l.Runs == nil {
		return nil
	}
	runs, err := l.Runs(ctx)
	if err != nil {
		l.log.Debug("episode runs not read", "error", err)
	}
	return runs
}

// runOf finds a torrent's run by its info hash, or for an AnimeBytes torrent
// by the torrent id in its comment, as bestIndex does.
func runOf(q QbitTorrent, runs map[string]EpisodeRun) (EpisodeRun, bool) {
	if run, ok := runs[strings.ToLower(q.Hash)]; ok {
		return run, true
	}
	if m := abTorrent.FindStringSubmatch(strings.TrimSpace(q.Comment)); m != nil && q.Private {
		run, ok := runs["ab:"+m[1]]
		return run, ok
	}
	return EpisodeRun{}, false
}

// placeByRun matches a SeaDex best's episode files to the episodes the
// AniList map says its entry is, overriding Sonarr's reading of the names:
// Sonarr's scene mapping can file them elsewhere (Kyousougiga's "- 01" to
// S00E02, Grisaia no Rakuen's S02 to S00E07). The numbers in the names count
// the entry's episodes from 1, or the files are exactly the entry's episodes
// in order, numbered from the run's own first episode or from past the entry
// (a second cour numbered "13"-"24"). A repeated number, a 0, a file holding
// several episodes, a number the run cannot place, or an episode Sonarr does
// not have leaves every row as Sonarr read it: a guess shifted by one would
// put every file on the wrong episode. Sonarr's rejections of its own match
// go; the others (a sample, a locked file) stay. It returns how many rows it
// placed.
func placeByRun(rows []manualImportRow, run EpisodeRun, series int, eps []sonarrEpisode) int {
	n := run.Last - run.First + 1
	nums := map[int]int{} // row -> the number in its name
	seen := map[int]bool{}
	lo, hi := 0, 0
	for i, r := range rows {
		if !episodeVideo(QbitFile{Name: cmp.Or(r.RelativePath, filepath.Base(r.Path)), Size: r.Size}) {
			continue
		}
		name := filepath.Base(r.Path)
		v, ok := fileNumber(name)
		if !ok || v < 1 || seen[v] || severalEpisodes(name) {
			return 0
		}
		seen[v], nums[i] = true, v
		if len(nums) == 1 || v < lo {
			lo = v
		}
		hi = max(hi, v)
	}
	if len(nums) == 0 || len(nums) > n {
		return 0
	}
	first := 1 // the number entry episode 1 carries
	if hi > n {
		if hi-lo+1 != n || len(nums) != n || (lo != run.First && lo <= n) {
			return 0
		}
		first = lo
	}
	byNumber := map[int]sonarrEpisode{}
	for _, e := range eps {
		if e.SeasonNumber == run.Season {
			byNumber[e.EpisodeNumber] = e
		}
	}
	place := map[int]sonarrEpisode{}
	for i, v := range nums {
		e, ok := byNumber[run.First+v-first]
		if !ok {
			return 0
		}
		place[i] = e
	}
	for i, e := range place {
		r := &rows[i]
		r.Series = &struct {
			ID int `json:"id"`
		}{series}
		r.Episodes = []struct {
			ID            int `json:"id"`
			EpisodeFileID int `json:"episodeFileId"`
			SeasonNumber  int `json:"seasonNumber"`
		}{{e.ID, e.EpisodeFileID, e.SeasonNumber}}
		r.Rejections = slices.DeleteFunc(r.Rejections, func(rj struct {
			Reason string `json:"reason"`
		}) bool {
			return matchRejection(rj.Reason)
		})
		r.ReleaseType = "singleEpisode"
	}
	return len(place)
}

// multiEpisode is a name holding several episodes: "- 01-02", "S01E01-E02".
var multiEpisode = regexp.MustCompile(`(?i)\bS\d{1,2}E\d{1,4}\s*-?\s*E\d{1,4}\b|[ _.-]\d{1,4}\s*[-~]\s*\d{1,4}(?:v\d+)?(?:[ _.]|$)`)

func severalEpisodes(name string) bool {
	stem := bracketed.ReplaceAllString(strings.TrimSuffix(name, filepath.Ext(name)), " ")
	return multiEpisode.MatchString(stem + " ")
}

// matchRejection is a Sonarr rejection of the episodes it matched a file to,
// which a placement by the map replaces: unknown or unexpected episodes, and
// "not an upgrade" of the file on the episode it wrongly chose.
func matchRejection(reason string) bool {
	r := strings.ToLower(reason)
	for _, w := range []string{"episode", "upgrade", "series", "season", "unexpected"} {
		if strings.Contains(r, w) {
			return true
		}
	}
	return false
}
