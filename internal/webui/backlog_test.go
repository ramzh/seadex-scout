package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A monitored season whose best is not downloaded is searched once; the next
// tick leaves it alone until the best changes.
func TestAutoBacklogQueuesEachMissingBestOnce(t *testing.T) {
	missing := finding("not_downloaded", "escanor", 1)
	missing.Attrs["release_url"] = "https://nyaa.si/view/1"
	missing.Attrs["title"] = "+Tic Elder Sister"
	held := finding("at_best", "nan0", 2)
	lib := newTestLibrary(t, &fakeSonarr{}, missing, held)
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 0 {
		t.Fatalf("searched before the monitored flags were read: %+v", s)
	}
	lib.store.monitored.At = time.Now()
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 1 || s[0].Target != (Target{Arr: "sonarr", ID: 7, Season: 1}) {
		t.Fatalf("searches = %+v, want one for season 1", s)
	}
	lib.ClearSearches()
	lib.CancelSearches()
	lib.searches = nil
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 0 {
		t.Fatalf("searched again with the same best: %+v", s)
	}
	lib.store.s.Findings[0].Attrs["release_url"] = "https://nyaa.si/view/2"
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 1 {
		t.Fatalf("a new best was not searched: %+v", s)
	}
}

// A better release is searched too: autoFix only searches one whose stale
// flags it cleared.
func TestAutoBacklogSearchesBetterRelease(t *testing.T) {
	lib := newTestLibrary(t, &fakeSonarr{}, finding("better_release", "reza", 0))
	lib.store.monitored.At = time.Now()
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 1 || s[0].Target != (Target{Arr: "sonarr", ID: 7, Season: 0}) {
		t.Fatalf("searches = %+v, want one for season 0", s)
	}
}

func TestAutoBacklogSkipsUnmonitored(t *testing.T) {
	lib := newTestLibrary(t, &fakeSonarr{}, finding("not_downloaded", "escanor", 1))
	lib.store.monitored = Monitored{At: time.Now(), Unmonitored: []string{"sonarr:7:1"}}
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 0 {
		t.Fatalf("searched an unmonitored season: %+v", s)
	}
}

// The reasons come from the releases that reach the minimum score only.
func TestAfterNothingNamesTheSeaDexRejections(t *testing.T) {
	rej := func(msg string) map[string]any {
		return map[string]any{"rejections": []map[string]string{{"reason": "x", "message": msg}}}
	}
	fake := &fakeSonarr{rels: []map[string]any{
		{"customFormatScore": 5000, "decision": rej("Not an upgrade")},
		{"customFormatScore": 5000, "decision": rej("Not an upgrade")},
		{"customFormatScore": 5000, "decision": rej("Matches another series")},
		{"customFormatScore": 0, "decision": rej("Below minimum score")},
	}}
	lib := newTestLibrary(t, fake)
	got := lib.afterNothing(t.Context(), lib.sonarr, Target{Arr: "sonarr", ID: 7, Season: 1}).why
	if !strings.HasPrefix(got, "3 SeaDex releases rejected: Not an upgrade (2); Matches another series (1)") {
		t.Fatalf("why = %q", got)
	}
	fake.rels = fake.rels[3:]
	if got := lib.afterNothing(t.Context(), lib.sonarr, Target{Arr: "sonarr", ID: 7, Season: 1}).why; !strings.HasPrefix(got, "no release reaching score 5000 among 1") {
		t.Fatalf("why = %q", got)
	}
}

// Misfit S2: the scout holds -ZR- and an untagged E01 that carries the best
// flags. Only the untagged file loses them; an alt group the scout also lists
// keeps them.
func TestFixFlagsClearsExtraGroupsOfAnAtBestSeason(t *testing.T) {
	fake := &fakeSonarr{files: []map[string]any{
		{"id": 1, "seasonNumber": 2, "releaseGroup": "", "indexerFlags": sonarrBestFlags},
		{"id": 2, "seasonNumber": 2, "releaseGroup": "ZR", "indexerFlags": sonarrBestFlags},
		{"id": 3, "seasonNumber": 2, "releaseGroup": "Alt", "indexerFlags": sonarrBestFlags},
	}}
	f := finding("at_best", "zr", 2)
	f.Attrs["current_group"] = "nogrp,zr,alt"
	f.Attrs["alt_groups"] = "alt"
	lib := newTestLibrary(t, fake, f)
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{}, nil }
	if got := lib.store.betterReleaseTargets(); len(got) != 1 {
		t.Fatalf("targets = %+v, want season 2", got)
	}
	res, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 2})
	if err != nil || res.Cleared != 1 || len(fake.bulk) != 1 || fake.bulk[0]["id"] != float64(1) {
		t.Fatalf("res = %+v, err %v, bulk %v; want only the untagged file cleared", res, err, fake.bulk)
	}
}

// Haganai S00E02: a better release for one special clears only that episode's
// file, not the other specials'.
func TestFixFlagsEpisodeFindingCoversOnlyItsEpisodes(t *testing.T) {
	fake := &fakeSonarr{
		files: []map[string]any{
			{"id": 10, "seasonNumber": 0, "releaseGroup": "Coalgirls", "indexerFlags": sonarrBestFlags},
			{"id": 11, "seasonNumber": 0, "releaseGroup": "Coalgirls", "indexerFlags": sonarrBestFlags},
		},
		eps: []map[string]any{
			{"id": 1, "seasonNumber": 0, "episodeNumber": 1, "episodeFileId": 10},
			{"id": 2, "seasonNumber": 0, "episodeNumber": 2, "episodeFileId": 11},
		},
	}
	f := finding("better_release", "reza", 0)
	f.Attrs["scope"], f.Attrs["episodes"] = "episodes", "S00E02"
	lib := newTestLibrary(t, fake, f)
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{}, nil }
	res, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 0})
	if err != nil || res.Cleared != 1 || len(fake.bulk) != 1 || fake.bulk[0]["id"] != float64(11) {
		t.Fatalf("res = %+v, err %v, bulk %v; want only S00E02's file cleared", res, err, fake.bulk)
	}
}

func TestEpisodeSpan(t *testing.T) {
	got := episodeSpan("S00E11-E14, S00E16-E17")
	if len(got) != 6 || got[0] != [2]int{0, 11} || got[5] != [2]int{0, 17} {
		t.Fatalf("span = %v", got)
	}
}

// Black Clover: the best stops at episode 51, so the alt's file for a later
// episode earns the flags; once the best covers it, the flags come off again.
func TestFixFlagsAltCoversAnEpisodeNoBestHas(t *testing.T) {
	fake := &fakeSonarr{files: []map[string]any{
		{"id": 1, "seasonNumber": 1, "releaseGroup": "CloverKnights", "customFormatScore": 5000, "indexerFlags": sonarrBestFlags},
		{"id": 2, "seasonNumber": 1, "releaseGroup": "iAHD", "customFormatScore": 0, "indexerFlags": 64},
		{"id": 3, "seasonNumber": 1, "releaseGroup": "Other", "customFormatScore": 0, "indexerFlags": 0},
	}}
	f := finding("at_best", "cloverknights", 1)
	f.Attrs["alt_groups"] = "iahd"
	f.Attrs["current_group"] = "cloverknights,iahd,other"
	f.Attrs["uncovered_files"] = "2,3"
	lib := newTestLibrary(t, fake, f)
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{}, nil }
	res, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1})
	if err != nil || res.AltCovers != 1 || len(fake.bulk) != 1 || fake.bulk[0]["id"] != float64(2) {
		t.Fatalf("res = %+v, err %v, bulk %v; want iAHD's file flagged", res, err, fake.bulk)
	}
	// SeaDex's best now covers episode 2's file: its alt flags are stale.
	fake.files[1]["indexerFlags"], fake.files[1]["customFormatScore"] = 64|sonarrBestFlags, 5000
	fake.bulk = nil
	lib.store.s.Findings[0].Attrs["uncovered_files"] = "3"
	res, err = lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1})
	if err != nil || res.Cleared != 1 || len(fake.bulk) != 1 || fake.bulk[0]["indexerFlags"] != float64(0) {
		t.Fatalf("res = %+v, err %v, bulk %v; want iAHD's flags cleared", res, err, fake.bulk)
	}
}

// An entry with a theoretical best only: its alt is the best there is.
func TestFixFlagsAltOfANoBestEntry(t *testing.T) {
	fake := &fakeSonarr{files: []map[string]any{
		{"id": 1, "seasonNumber": 1, "releaseGroup": "CTR", "customFormatScore": 0, "indexerFlags": 0},
	}}
	f := finding("theoretical_best", "", 1)
	f.Attrs["alt_groups"] = "ctr"
	lib := newTestLibrary(t, fake, f)
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{}, nil }
	res, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1})
	if err != nil || res.AltCovers != 1 {
		t.Fatalf("res = %+v, err %v; want the alt file flagged", res, err)
	}
}

// An entry with no obtainable best has nothing to search for.
func TestAutoBacklogSkipsAnEntryWithNoBestGroup(t *testing.T) {
	lib := newTestLibrary(t, &fakeSonarr{}, finding("not_downloaded", "", 2))
	lib.store.monitored.At = time.Now()
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 0 {
		t.Fatalf("searched an entry with no best: %+v", s)
	}
}

// A whole-series search gets no 400 from Sonarr: it is not asked.
func TestAfterNothingSeriesSearch(t *testing.T) {
	lib := newTestLibrary(t, &fakeSonarr{})
	if v := lib.afterNothing(t.Context(), lib.sonarr, Target{Arr: "sonarr", ID: 7, Season: -1}); !strings.HasPrefix(v.why, "a whole-series search") {
		t.Fatalf("why = %q", v.why)
	}
}

// There's No Freaking Way S1: the arr approves a SeaDex release its automatic
// search skipped; the scout grabs the best-scored approved one.
func TestAfterNothingGrabsAnApprovedSeaDexRelease(t *testing.T) {
	approved := func(guid string, score int) map[string]any {
		return map[string]any{"customFormatScore": score, "downloadAllowed": true,
			"decision": map[string]any{"approved": true},
			"release":  map[string]any{"guid": guid, "indexerId": 3, "title": "[Headpatter] " + guid}}
	}
	fake := &fakeSonarr{rels: []map[string]any{approved("a", 5000), approved("b", 5001), approved("c", 0)}}
	lib := newTestLibrary(t, fake)
	v := lib.afterNothing(t.Context(), lib.sonarr, Target{Arr: "sonarr", ID: 7, Season: 1})
	if v.grabbed != "[Headpatter] b" || len(fake.grabs) != 1 || fake.grabs[0]["guid"] != "b" || fake.grabs[0]["indexerId"] != float64(3) {
		t.Fatalf("verdict %+v, grabs %v", v, fake.grabs)
	}
}

// BLUE LOCK S2: monitoring a season sets its flag and its episodes', drops it
// from the unmonitored keys and searches it first.
func TestMonitorSeasonAndSearch(t *testing.T) {
	fake := &fakeSonarr{
		series: map[string]any{"id": 7, "monitored": true, "seasons": []map[string]any{
			{"seasonNumber": 1, "monitored": true}, {"seasonNumber": 2, "monitored": false}}},
		eps: []map[string]any{{"id": 11, "seasonNumber": 1}, {"id": 21, "seasonNumber": 2}, {"id": 22, "seasonNumber": 2}},
	}
	lib := newTestLibrary(t, fake)
	lib.store.monitored = Monitored{At: time.Now(), Unmonitored: []string{"sonarr:7:2"}}
	if err := lib.Monitor(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 2}, "BLUE LOCK"); err != nil {
		t.Fatal(err)
	}
	put, _ := fake.puts["/api/v5/series/7"].(map[string]any)
	seasons, _ := put["seasons"].([]any)
	if len(seasons) != 2 || seasons[1].(map[string]any)["monitored"] != true {
		t.Fatalf("series PUT = %v", put)
	}
	mon, _ := fake.puts["/api/v5/episode/monitor"].(map[string]any)
	if ids, _ := mon["episodeIds"].([]any); len(ids) != 2 || mon["monitored"] != true {
		t.Fatalf("episode monitor = %v", mon)
	}
	if len(lib.store.monitored.Unmonitored) != 0 {
		t.Fatalf("still unmonitored: %v", lib.store.monitored.Unmonitored)
	}
	if s := lib.Searches(); len(s) != 1 || s[0].Season != 2 {
		t.Fatalf("searches = %+v", s)
	}
}

// Misfit S2: E01 lost the best flags, the -ZR- pack is already seeding, so a
// search can only hit qBittorrent's 409. The pack's E01 is imported instead;
// E02, already at the minimum, is left alone.
func TestImportSeedingTakesLowEpisodesFromABestTorrent(t *testing.T) {
	lib, fake := seedingFixture(t)
	res, err := lib.ImportSeeding(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Torrent != "[-ZR-] Misfit S02" || len(fake.cmds) != 1 {
		t.Fatalf("res = %+v, cmds %v", res, fake.cmds)
	}
	files, _ := fake.cmds[0]["files"].([]any)
	f0, _ := files[0].(map[string]any)
	if fake.cmds[0]["name"] != "ManualImport" || len(files) != 1 || f0["indexerFlags"] != float64(sonarrBestFlags) || f0["downloadId"] != nil {
		t.Fatalf("import command = %v", fake.cmds[0])
	}
}

// The tick imports from a seeding best without waiting for a search (SAO S4:
// the search re-grabbed the pack qBittorrent held, so nothing followed it).
func TestAutoSeedingImportsMissingTargets(t *testing.T) {
	lib, fake := seedingFixture(t)
	lib.store.s.Findings = []Line{finding("not_downloaded", "zr", 2)}
	lib.autoSeeding(t.Context())
	if len(fake.cmds) != 0 {
		t.Fatalf("imported before the monitored flags were read: %v", fake.cmds)
	}
	lib.store.monitored.At = time.Now()
	lib.autoSeeding(t.Context())
	if len(fake.cmds) != 1 {
		t.Fatalf("cmds = %v, want one import", fake.cmds)
	}
}

// seedingFixture is Misfit S2: -ZR-'s pack seeding complete, E01 below the
// minimum score, E02 at it, and a library file Sonarr also lists.
func seedingFixture(t *testing.T) (*Library, *fakeSonarr) {
	t.Helper()
	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"hash": "ZRHASH", "name": "[-ZR-] Misfit S02", "content_path": "/data/torrents/anime/tv/Misfit S02", "progress": 1},
			{"hash": "OTHER", "name": "something else", "content_path": "/data/x", "progress": 1},
		})
	}))
	t.Cleanup(qb.Close)
	ep := func(id, file int) map[string]any {
		return map[string]any{"id": id, "episodeFileId": file, "seasonNumber": 2}
	}
	fake := &fakeSonarr{
		files: []map[string]any{
			{"id": 101, "seasonNumber": 2, "releaseGroup": "", "customFormatScore": 0},
			{"id": 102, "seasonNumber": 2, "releaseGroup": "ZR", "customFormatScore": 5000},
		},
		rows: []map[string]any{
			{"path": "/data/torrents/anime/tv/Misfit S02/E01.mkv", "series": map[string]int{"id": 7}, "episodes": []map[string]any{ep(1, 101)}},
			{"path": "/data/torrents/anime/tv/Misfit S02/E02.mkv", "series": map[string]int{"id": 7}, "episodes": []map[string]any{ep(2, 102)}},
		},
	}
	lib := newTestLibrary(t, fake)
	lib.Qbit, _ = NewQbit(QbitConfig{URL: qb.URL})
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{"zrhash": {99}}, nil }
	// A file outside the torrent (the library's own, as Sonarr returns with a
	// seriesId) is never imported.
	fake.rows = append(fake.rows, map[string]any{"path": "/data/media/anime/tv/Misfit/Season 02/E01.mkv",
		"series": map[string]int{"id": 7}, "episodes": []map[string]any{ep(1, 101)}})
	return lib, fake
}

func TestDigestSaysWhatHappened(t *testing.T) {
	lib := newTestLibrary(t, &fakeSonarr{})
	now := time.Now().UTC()
	lib.searches = []SearchJob{
		{Target: Target{Arr: "sonarr", ID: 1, Season: 1}, Title: "86", State: "grabbed", Grabbed: []string{"[koala] 86 S01"}, Finished: now},
		{Target: Target{Arr: "sonarr", ID: 2, Season: 2}, Title: "Bungo", State: "nothing", Message: "3 SeaDex releases rejected: x (3)", Finished: now},
		{Target: Target{Arr: "sonarr", ID: 3, Season: 1}, Title: "Old", State: "grabbed", Finished: now.Add(-48 * time.Hour)},
	}
	lib.fixStats = FlagFix{Fixed: 4, AltCovers: 3, Cleared: 1}
	text, ok := lib.Digest(now.Add(-24 * time.Hour))
	for _, want := range []string{"Grabbed or imported (1)", "86 S1: [koala] 86 S01", "Searched, nothing grabbed (1)", "Bungo S2: 3 SeaDex", "4 files given the best flags (3 as the alt", "1 stale ones cleared"} {
		if !ok || !strings.Contains(text, want) {
			t.Fatalf("digest lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Old") {
		t.Fatalf("digest has a search older than a day:\n%s", text)
	}
	if _, ok := lib.Digest(now.Add(time.Minute)); ok {
		t.Fatal("a second digest with nothing new is not empty")
	}
}

// Durarara!!x2 Part 3: a file this queued download already put in place is
// never imported from it again, whatever the scout says of the season.
func TestImportQueuedSkipsFilesFromTheSameDownload(t *testing.T) {
	ep := func(id, file int) map[string]any { return map[string]any{"id": id, "episodeFileId": file} }
	fake := &fakeSonarr{
		queue: []map[string]any{{"id": 5, "seriesId": 7, "downloadId": "ABC", "outputPath": "/data/pack", "sizeLeft": 0}},
		history: []map[string]any{
			{"eventType": "grabbed", "downloadId": "ABC", "data": map[string]string{"indexerFlags": "Scene, Freeleech25"}},
			{"eventType": "downloadFolderImported", "downloadId": "ABC", "data": map[string]string{"fileId": "101"}},
		},
		files: []map[string]any{{"id": 101, "customFormatScore": 0}},
		rows: []map[string]any{
			{"path": "/data/pack/26.mkv", "series": map[string]int{"id": 7}, "episodes": []any{ep(1, 101)}},
			{"path": "/data/pack/27.mkv", "series": map[string]int{"id": 7}, "episodes": []any{ep(2, 0)}},
		},
	}
	lib := newTestLibrary(t, fake)
	res, err := lib.ImportQueued(t.Context(), "sonarr", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := fake.cmds[0]["files"].([]any)
	if res.Files != 1 || len(files) != 1 || files[0].(map[string]any)["path"] != "/data/pack/27.mkv" {
		t.Fatalf("result %+v, files %v; want only the episode with no file", res, files)
	}
}

// Durarara!!x2 Part 3: Sonarr matches the "- 26" to "- 37" files to S02E01-E12,
// which the library holds; the grab was for E25-E36. The files move to those
// episodes in order, and the OVA's file, in Specials, keeps Sonarr's match.
func TestMatchByNumberMovesFilesMatchedOutsideTheGrab(t *testing.T) {
	var eps []map[string]any
	for e := 1; e <= 36; e++ {
		eps = append(eps, map[string]any{"id": 200 + e, "seasonNumber": 2, "episodeNumber": e, "episodeFileId": map[bool]int{true: 900 + e, false: 0}[e <= 12]})
	}
	eps = append(eps, map[string]any{"id": 301, "seasonNumber": 0, "episodeNumber": 3, "episodeFileId": 0})
	fake := &fakeSonarr{eps: eps}
	lib := newTestLibrary(t, fake)
	ep := func(id, season int) map[string]any { return map[string]any{"id": id, "seasonNumber": season} }
	var raw []map[string]any
	for k := 0; k < 3; k++ {
		raw = append(raw, map[string]any{"path": fmt.Sprintf("/d/Durarara!!x2 Ketsu - %02d (BD 1080p) [Kametsu].mkv", 26+k), "size": 1 << 30,
			"series": map[string]int{"id": 7}, "episodes": []any{ep(201+k, 2)}})
	}
	raw = append(raw, map[string]any{"path": "/d/Durarara!!x2 OVA - 03 (BD 1080p) [Kametsu].mkv", "size": 1 << 30,
		"series": map[string]int{"id": 7}, "episodes": []any{ep(301, 0)}})
	body, _ := json.Marshal(raw)
	var rows []manualImportRow
	_ = json.Unmarshal(body, &rows)
	grabbed := map[int]bool{225: true, 226: true, 227: true}
	if err := lib.matchByNumber(t.Context(), lib.sonarr, 7, rows, grabbed); err != nil {
		t.Fatal(err)
	}
	for k, want := range []int{225, 226, 227, 301} {
		if got := rows[k].Episodes[0].ID; got != want {
			t.Fatalf("row %d matched to %d, want %d", k, got, want)
		}
	}
}

// Ketsu's files carry season-relative numbers with no season: "- 25" is
// S02E25, the episode the grab was for, not absolute 25 (S02E01).
func TestMatchByNumberPrefersTheGrabbedSeasonRelativeEpisode(t *testing.T) {
	var eps []map[string]any
	for e := 1; e <= 24; e++ {
		eps = append(eps, map[string]any{"id": 100 + e, "seasonNumber": 1, "episodeNumber": e, "absoluteEpisodeNumber": e})
	}
	for e := 1; e <= 36; e++ {
		eps = append(eps, map[string]any{"id": 200 + e, "seasonNumber": 2, "episodeNumber": e, "absoluteEpisodeNumber": 24 + e})
	}
	lib := newTestLibrary(t, &fakeSonarr{eps: eps})
	rows := []manualImportRow{{Path: "/d/[Kametsu] Durarara!!x2 - 25 - Ketsu (BD 1080p) [4948569E].mkv", Size: 1 << 30}}
	if err := lib.matchByNumber(t.Context(), lib.sonarr, 7, rows, map[int]bool{225: true}); err != nil {
		t.Fatal(err)
	}
	if len(rows[0].Episodes) != 1 || rows[0].Episodes[0].ID != 225 {
		t.Fatalf("matched %+v, want S02E25 (225)", rows[0].Episodes)
	}
	// Without a grab to go by, the absolute number holds.
	rows = []manualImportRow{{Path: rows[0].Path, Size: 1 << 30}}
	if err := lib.matchByNumber(t.Context(), lib.sonarr, 7, rows, nil); err != nil {
		t.Fatal(err)
	}
	if len(rows[0].Episodes) != 1 || rows[0].Episodes[0].ID != 201 {
		t.Fatalf("matched %+v, want absolute 25 = S02E01 (201)", rows[0].Episodes)
	}
}
