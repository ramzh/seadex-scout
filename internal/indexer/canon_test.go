package indexer

import "testing"

func TestCanonCopyServesTheSonarrTitle(t *testing.T) {
	it := item{Title: "[ARC] Golden Boy S01 (1995) [DVD 576p AC3] [Dual-Audio]", GUID: "g1"}
	v := curationVerdict{canon: "Golden Boy (1995)", rangeLabel: "S01", nonFilm: true, matched: true}
	c, ok := canonCopy(&it, v, requesterTV)
	if !ok || c.Title != "[ARC] Golden Boy (1995) S01 [DVD 576p AC3] [Dual-Audio]" || c.GUID != "g1#canon" {
		t.Fatalf("copy = %q %q %v", c.Title, c.GUID, ok)
	}
	// A split cour keeps its episode range.
	v.rangeLabel = "S01E13-E24"
	if c, _ := canonCopy(&it, v, requesterAny); c.Title != "[ARC] Golden Boy (1995) S01E13-E24 [DVD 576p AC3] [Dual-Audio]" {
		t.Fatalf("range copy = %q", c.Title)
	}
	for name, bad := range map[string]curationVerdict{
		"no canon":  {rangeLabel: "S01", nonFilm: true},
		"film":      {canon: "X (2000)", rangeLabel: "S01"},
		"no range":  {canon: "X (2000)", nonFilm: true},
		"contested": {canon: "", rangeLabel: "S01", nonFilm: true},
	} {
		if _, ok := canonCopy(&it, bad, requesterTV); ok {
			t.Errorf("%s: got a copy", name)
		}
	}
	if _, ok := canonCopy(&it, v, requesterMovies); ok {
		t.Error("a Radarr request got a canonical copy")
	}
}
