package compare

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// Black Clover's best names its files "Black Clover 017 (BD ...)": absolute
// numbers before the tags, which the coverage needs to read.
func TestEpisodeSpansReadsANumberBeforeTheTags(t *testing.T) {
	files := []seadex.File{
		{Name: "[CloverKnights] Black Clover 017 (BD 1080p HEVC Opus) [Dual Audio] [BC757F44].mkv", Length: 1},
		{Name: "[CloverKnights] Black Clover 002 (BD 1080p HEVC Opus) [Dual Audio] [1657406A].mkv", Length: 1},
		{Name: "[CloverKnights] Black Clover NCOP_01 (BD 1080p HEVC Opus) [Dual Audio] [CBEC1623].mkv", Length: 1},
	}
	spans, ok := episodeSpans(payload.Census(files), 1, false)
	if !ok || len(spans) != 2 || spans[0] != (payload.EpisodeSpan{Season: payload.AbsoluteSeason, First: 17, Last: 17}) {
		t.Fatalf("spans = %v, ok %v", spans, ok)
	}
}
