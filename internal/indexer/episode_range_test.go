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

// barePack is the CloverKnights layout: "Black Clover 017 (BD ...)" plus
// creditless openings and endings, some in an NC folder.
func barePack() *seadex.Torrent {
	t := &seadex.Torrent{ReleaseGroup: "CloverKnights"}
	for n := 1; n <= 51; n++ {
		t.Files = append(t.Files, seadex.File{Name: fmt.Sprintf("[CloverKnights] Black Clover %03d (BD 1080p HEVC Opus) [Dual Audio] [ABCDEF%02d].mkv", n, n), Length: 1 << 30})
	}
	for _, extra := range []string{"NC/[CloverKnights] Black Clover NCED_01 (BD 1080p HEVC Opus) [Dual Audio] [B3BADC0F].mkv",
		"[CloverKnights] Black Clover NCOP_04 (BD 1080p HEVC Opus) [Dual Audio] [2A2A862F].mkv"} {
		t.Files = append(t.Files, seadex.File{Name: extra, Length: 1 << 28})
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
		{"bare numbers with creditless extras", barePack(),
			EntryInfo{Title: "Black Clover", Season: 1, SeasonKnown: true, EpisodeRun: epSeq(1, 170), EpisodeRunSeason: 1},
			"Black Clover S01E01-E51"},
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
		{"a part numbered by the series", rangePack("sam", "Dr. STONE - Science Future", 25, 37),
			EntryInfo{Title: "Dr. STONE", Season: 4, SeasonKnown: true, EpisodeRun: epSeq(25, 37), EpisodeRunSeason: 4},
			"Dr. STONE S04E25-E37"},
		{"a gap in the run keeps the season label", rangePack("G", "Show", 1, 3),
			EntryInfo{Title: "Show", Season: 1, SeasonKnown: true, EpisodeRun: []int{1, 2, 5, 6}, EpisodeRunSeason: 1},
			"Show S01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := episodeMarker(tc.pack, &tc.meta); tc.meta.Title+" "+got != tc.want {
				t.Errorf("title head = %q, want %q", tc.meta.Title+" "+got, tc.want)
			}
		})
	}
}

func TestWithRangeSwapsTheBareSeasonToken(t *testing.T) {
	for _, tc := range []struct{ title, label, want string }{
		{"[CloverKnights] Black Clover S01 [Blu-ray][MKV][1080p]", "S01E01-E51", "[CloverKnights] Black Clover S01E01-E51 [Blu-ray][MKV][1080p]"},
		{"[sam] Dr. STONE S04", "S04E01-E12", "[sam] Dr. STONE S04E01-E12"},
		{"[G] Show S02 [BD]", "S01E13-E25", "[G] Show S01E13-E25 [BD]"},  // a second cour the tracker calls S02
		{"[G] Show S01 [BD]", "S02", "[G] Show S02 [BD]"},                // a second season the tracker calls S01
		{"[G] Show S02 [BD]", "S02", "[G] Show S02 [BD]"},                // same season
		{"[G] Show S01E05 [BD]", "S01E01-E12", "[G] Show S01E05 [BD]"},   // already episodic
		{"[G] Show S01 S01 [BD]", "S01E01-E12", "[G] Show S01 S01 [BD]"}, // ambiguous
		{"[G] Show S01 [BD]", "", "[G] Show S01 [BD]"},                   // no range
	} {
		if got := withRange(tc.title, tc.label); got != tc.want {
			t.Errorf("withRange(%q, %q) = %q, want %q", tc.title, tc.label, got, tc.want)
		}
	}
}
