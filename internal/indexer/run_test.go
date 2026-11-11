package indexer

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

func TestRunLabelRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		season int
		run    []int
		want   string
	}{
		{1, epSeq(1, 10), "1:1-10"},
		{2, epSeq(13, 24), "2:13-24"},
		{1, []int{1, 2, 5}, ""}, // a gap
		{0, epSeq(1, 3), ""},    // specials are the twin's job
		{1, nil, ""},
	} {
		got := runLabel(tc.season, tc.run)
		if got != tc.want {
			t.Errorf("runLabel(%d, %v) = %q, want %q", tc.season, tc.run, got, tc.want)
		}
		if s, f, l, ok := ParseRun(got); ok != (got != "") || ok && (s != tc.season || f != tc.run[0] || l != tc.run[len(tc.run)-1]) {
			t.Errorf("ParseRun(%q) = %d %d %d %v", got, s, f, l, ok)
		}
	}
	for _, bad := range []string{"1:5-3", "0:1-2", "1:01-2", "1:1-2x", "x"} {
		if _, _, _, ok := ParseRun(bad); ok {
			t.Errorf("ParseRun(%q) accepted", bad)
		}
	}
}

// Only a best carries its entry's run: an alt is never imported by the map.
func TestOwnershipCarriesTheBestsRun(t *testing.T) {
	entry := seadex.Entry{AniListID: 19703, Torrents: []seadex.Torrent{
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/1", InfoHash: "d2a717dcf5f1f7d1ac3474e3d98d9e4ac67b7392", IsBest: true},
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/2", InfoHash: "e2a717dcf5f1f7d1ac3474e3d98d9e4ac67b7392"},
	}}
	info := func(int) EntryInfo {
		return EntryInfo{Title: "Kyousougiga", Season: 1, SeasonKnown: true, Kind: KindSeries, EpisodeRun: epSeq(1, 10), EpisodeRunSeason: 1}
	}
	got := ownershipOf([]seadex.Entry{entry}, info)["19703"]
	if len(got) != 2 || got[0].Run != "1:1-10" || got[1].Run != "" {
		t.Fatalf("owned = %+v, want the best alone with run 1:1-10", got)
	}
}
