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
	part := item{Title: "[figged] Soul Eater S01 E27-E51 (BluRay 720p AVC Opus) [Dual-Audio]", GUID: "g2"}
	if _, ok := canonCopy(&part, curationVerdict{canon: "Soul Eater (2008)", rangeLabel: "S01", nonFilm: true}, requesterTV); ok {
		t.Error("a part of the season got a whole-season copy")
	}
	// Durarara!! S2: a split season's title serves ranged copies only.
	dur := item{Title: "[Kametsu] Durarara S3 S02E13-E24 [Blu-ray]", GUID: "g3"}
	ranged := curationVerdict{canon: "~Durarara!! (2010)", rangeLabel: "S02E13-E24", nonFilm: true}
	if c, ok := canonCopy(&dur, ranged, requesterTV); !ok || c.Title != "[Kametsu] Durarara!! (2010) S02E13-E24 [Blu-ray]" {
		t.Fatalf("ranged copy = %q %v", c.Title, ok)
	}
	ranged.rangeLabel = "S02"
	if _, ok := canonCopy(&dur, ranged, requesterTV); ok {
		t.Error("a ranged-only title got a whole-season copy")
	}
	if _, ok := canonCopy(&it, v, requesterMovies); ok {
		t.Error("a Radarr request got a canonical copy")
	}
}
