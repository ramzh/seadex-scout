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
	for _, q := range seeding {
		var rows []manualImportRow
		v := url.Values{"folder": {q.ContentPath}, "seriesId": {strconv.Itoa(t.ID)}, "filterExistingFiles": {"false"}}
		if err := a.call(ctx, http.MethodGet, "/manualimport?"+v.Encode(), nil, &rows); err != nil {
			return res, err
		}
		var take []map[string]any
		for _, r := range rows {
			if r.Series == nil || r.Series.ID != t.ID || len(r.Episodes) == 0 {
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
			take = append(take, map[string]any{
				"path": r.Path, "folderName": r.FolderName, "seriesId": t.ID, "episodeIds": ids,
				"quality": r.Quality, "languages": r.Languages, "releaseGroup": r.ReleaseGroup,
				"releaseType": r.ReleaseType, "indexerFlags": bestFlags(t.Arr), "downloadId": strings.ToUpper(q.Hash),
			})
		}
		if len(take) == 0 {
			continue
		}
		var cmd struct {
			ID int `json:"id"`
		}
		if err := a.call(ctx, http.MethodPost, "/command",
			map[string]any{"name": "ManualImport", "files": take, "importMode": "auto"}, &cmd); err != nil {
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
