package indexer

import "testing"

// Sonarr pins the romaji title of Maou Gakuin no Futekigousha to season 2; the
// S1 entry's owner carries it as misfiled.
func TestMisfiledTitle(t *testing.T) {
	misfiled := cleanTitles([]string{
		"Maou Gakuin no Futekigousha - Shijou Saikyou no Maou no Shiso, Tensei shite Shison-tachi no Gakkou e Kayou",
		"Maou Gakuin no Futekigousha II",
		"The Misfit of Demon King Academy II",
	})
	tests := []struct {
		title string
		want  bool
	}{
		// The S1 pack under the S2-mapped romaji alias (AB 955364).
		{"[Asakura] Maou Gakuin no Futekigousha ~Shijou Saikyou no Maou no Shiso, Tensei Shite Shison-tachi no Gakkou e Kayou~ S01 [v2] [Blu-ray][MKV][h265 10-bit][1080p]", true},
		// Its other aliases are served.
		{"[Asakura] The Misfit of Demon King Academy S01 [v2] [Blu-ray][MKV][h265 10-bit][1080p]", false},
		{"[Asakura] The Misfit of Demon King Academy: History's Strongest Demon King Reincarnates and Goes to School with His Descendants S01 [v2]", false},
		// A longer title that only starts with a misfiled one is another series part.
		{"[-ZR-] Maou Gakuin no Futekigousha ~Shijou Saikyou no Maou no Shiso, Tensei shite Shison-tachi no Gakkou e Kayou~ Extra S01", false},
		{"[Group] The Misfit of Demon King Academy II S01", true},
		{"[Group] Misfit of Demon King Academy II 2023 S01", true},
	}
	for _, tt := range tests {
		if got := misfiledTitle(tt.title, misfiled); got != tt.want {
			t.Errorf("misfiledTitle(%q) = %v, want %v", tt.title, got, tt.want)
		}
	}
	if misfiledTitle(tests[0].title, "") {
		t.Error("no misfiled titles must drop nothing")
	}
}

// A title is misfiled only when every owner says so.
func TestMisfiledVoteIntersects(t *testing.T) {
	var v misfiledVote
	v.add("a,b,c")
	v.add("b,c,d")
	if v.titles != "b,c" {
		t.Fatalf("intersection = %q, want b,c", v.titles)
	}
	v.add("")
	if v.titles != "" {
		t.Fatalf("an owner with none must empty it, got %q", v.titles)
	}
}
