package webui

import (
	"context"
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
