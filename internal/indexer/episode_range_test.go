package indexer

import (
	"fmt"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

func rangePack(group, title string, first, last int) *seadex.Torrent {
	t := &seadex.Torrent{ReleaseGroup: group}
	for n := first; n <= last; n++ {
		t.Files = append(t.Files, seadex.File{Name: fmt.Sprintf("[%s] %s - %02d [1080p].mkv", group, title, n), Length: 1 << 30})
	}
	return t
}

func epSeq(from, to int) []int {
	var out []int
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

func TestSynthesizedPackTitleCarriesItsEpisodeRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		pack *seadex.Torrent
		meta EntryInfo
		want string
	}{
		{"part of a long season", rangePack("CloverKnights", "Black Clover", 1, 51),
			EntryInfo{Title: "Black Clover", Season: 1, SeasonKnown: true, EpisodeRun: epSeq(1, 170), EpisodeRunSeason: 1},
			"Black Clover S01E01-E51"},
		{"a later cour inside one TVDB season", rangePack("NAN0", "Owari no Seraph Nagoya", 1, 12),
			EntryInfo{Title: "Seraph of the End", Season: 1, SeasonKnown: true, EpisodeRun: epSeq(13, 24), EpisodeRunSeason: 1},
			"Seraph of the End S01E13-E24"},
		{"the whole entry from episode 1 stays a season pack", rangePack("G", "Show", 1, 12),
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true, EpisodeRun: epSeq(1, 12), EpisodeRunSeason: 1},
			"Show S01"},
		{"no run keeps the season label", rangePack("G", "Show", 1, 5),
			EntryInfo{Title: "Show", Season: 2, SeasonKnown: true},
			"Show S02"},
		{"a run on another season is not used", rangePack("G", "Show", 1, 5),
			EntryInfo{Title: "Show", Season: 2, SeasonKnown: true, EpisodeRun: epSeq(1, 12), EpisodeRunSeason: 1},
			"Show S02"},
		{"a span past the run keeps the season label", rangePack("G", "Show", 1, 30),
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true, EpisodeRun: epSeq(13, 24), EpisodeRunSeason: 1},
			"Show S01"},
		{"a gap in the run keeps the season label", rangePack("G", "Show", 1, 3),
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true, EpisodeRun: []int{1, 2, 5, 6}, EpisodeRunSeason: 1},
			"Show S01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !isPack(tc.pack) {
				t.Fatalf("fixture is not a pack")
			}
			if got := episodeMarker(tc.pack, &tc.meta); tc.meta.Title+" "+got != tc.want {
				t.Errorf("title head = %q, want %q", tc.meta.Title+" "+got, tc.want)
			}
		})
	}
}
