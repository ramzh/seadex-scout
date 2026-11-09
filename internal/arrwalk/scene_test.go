package arrwalk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Golden Boy's canonical title is kept because Sonarr files it as the whole
// season; Kyousougiga's is not, because Sonarr files it as nothing.
func TestSonarrSceneTitlesKeepsOnlyParsedCanonTitles(t *testing.T) {
	parses := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/api/v3/series":
			_ = enc.Encode([]map[string]any{
				{"id": 1, "title": "Golden Boy", "year": 1995, "seasons": []map[string]any{
					{"seasonNumber": 0, "statistics": map[string]int{"totalEpisodeCount": 2}},
					{"seasonNumber": 1, "statistics": map[string]int{"totalEpisodeCount": 6}}}},
				{"id": 2, "title": "Kyousougiga", "year": 2013, "alternateTitles": []map[string]any{
					{"title": "Kyousougiga", "sceneSeasonNumber": 2}},
					"seasons": []map[string]any{{"seasonNumber": 1, "statistics": map[string]int{"totalEpisodeCount": 10}}}},
			})
		case "/api/v3/parse":
			parses++
			eps := []map[string]int{}
			id := 0
			if strings.HasPrefix(r.URL.Query().Get("title"), "Golden Boy (1995) S01") {
				id = 1
				for range 6 {
					eps = append(eps, map[string]int{"seasonNumber": 1})
				}
			} else {
				id = 2
			}
			_ = enc.Encode(map[string]any{"series": map[string]int{"id": id}, "episodes": eps})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	src := SonarrSceneTitles(srv.Client(), srv.URL, "k")
	got, err := src(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Sonarr names Kyousougiga but files no episode for "(2013) S01": the
	// title is kept for episode-range copies only.
	if got[1].Canon[1] != "Golden Boy (1995)" || got[2].Canon[1] != "~Kyousougiga (2013)" {
		t.Fatalf("canon = %v / %v", got[1].Canon, got[2].Canon)
	}
	if len(got[2].Titles) != 1 || got[2].Titles[0].Season != 2 {
		t.Fatalf("scene titles = %v", got[2].Titles)
	}
	first := parses
	if _, err := src(t.Context()); err != nil || parses != first {
		t.Fatalf("parsed again: %d then %d", first, parses)
	}
}
