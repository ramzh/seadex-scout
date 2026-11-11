package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// kyousougiga is the P9 pack's rows as Sonarr reads them through its scene
// mapping: "- 01" to "- 05" as specials, the rest as S01E01-E05.
func kyousougiga(names ...string) []manualImportRow {
	var rows []manualImportRow
	for i, n := range names {
		var r manualImportRow
		_ = json.Unmarshal(fmt.Appendf(nil, `{"path":"/t/P9/%s","size":%d,"series":{"id":7},"episodes":[{"id":%d,"seasonNumber":0}],"rejections":[{"reason":"Not an upgrade for existing episode file(s)"}]}`,
			n, 1<<30, 900+i), &r)
		rows = append(rows, r)
	}
	return rows
}

func seasonEps(season, from, to int) []sonarrEpisode {
	var out []sonarrEpisode
	for n := from; n <= to; n++ {
		out = append(out, sonarrEpisode{ID: season*100 + n, SeasonNumber: season, EpisodeNumber: n})
	}
	return out
}

func placed(rows []manualImportRow) []int {
	var out []int
	for _, r := range rows {
		if len(r.Episodes) == 1 {
			out = append(out, r.Episodes[0].ID)
		}
	}
	return out
}

func TestPlaceByRun(t *testing.T) {
	ten := []string{"[P9] Kyousougiga - 01.mkv", "[P9] Kyousougiga - 02.mkv", "[P9] Kyousougiga - 03.mkv", "[P9] Kyousougiga - 04.mkv", "[P9] Kyousougiga - 05.mkv",
		"[P9] Kyousougiga - 06.mkv", "[P9] Kyousougiga - 07.mkv", "[P9] Kyousougiga - 08.mkv", "[P9] Kyousougiga - 09.mkv", "[P9] Kyousougiga - 10.mkv"}
	eps := append(seasonEps(1, 1, 24), seasonEps(0, 1, 9)...)

	rows := kyousougiga(ten...)
	if n := placeByRun(rows, EpisodeRun{Season: 1, First: 1, Last: 10}, 7, eps); n != 10 {
		t.Fatalf("placed %d rows, want 10", n)
	}
	if got := placed(rows); got[0] != 101 || got[9] != 110 || len(rows[0].Rejections) > 0 {
		t.Fatalf("episodes %v, rejections %v: want S01E01-E10 with Sonarr's match rejections dropped", got, rows[0].Rejections)
	}

	// A second cour numbered on from the first: its files are the run in order.
	cour := kyousougiga("S - 13.mkv", "S - 14.mkv", "S - 15.mkv")
	if n := placeByRun(cour, EpisodeRun{Season: 1, First: 13, Last: 15}, 7, eps); n != 3 || placed(cour)[0] != 113 {
		t.Fatalf("second cour placed %d %v, want S01E13-E15", n, placed(cour))
	}
	// The same cour numbered from 1 lands the same.
	cour = kyousougiga("S - 01.mkv", "S - 02.mkv", "S - 03.mkv")
	if n := placeByRun(cour, EpisodeRun{Season: 1, First: 13, Last: 15}, 7, eps); n != 3 || placed(cour)[2] != 115 {
		t.Fatalf("second cour from 1 placed %d %v, want S01E13-E15", n, placed(cour))
	}

	// Numbered past the entry (Durarara!!x2 Ten's "- 13" on as S02E01-E03).
	cour = kyousougiga("S - 13.mkv", "S - 14.mkv", "S - 15.mkv")
	if n := placeByRun(cour, EpisodeRun{Season: 1, First: 1, Last: 3}, 7, eps); n != 3 || placed(cour)[0] != 101 {
		t.Fatalf("numbered past the entry placed %d %v, want S01E01-E03", n, placed(cour))
	}
	// A one-episode entry's single file is placed whatever its name numbers
	// (an OVA as S00E07); two files for it place nothing.
	ova := kyousougiga("[Moxie] Ao no Exorcist - Kyoto Saga OVA.mkv")
	if n := placeByRun(ova, EpisodeRun{Season: 0, First: 7, Last: 7}, 7, eps); n != 1 || placed(ova)[0] != 7 {
		t.Fatalf("OVA placed %d %v, want S00E07", n, placed(ova))
	}
	if n := placeByRun(kyousougiga("OVA - 01-02.mkv"), EpisodeRun{Season: 0, First: 7, Last: 7}, 7, eps); n != 0 {
		t.Fatalf("a two-episode file for one special placed %d", n)
	}
	if n := placeByRun(kyousougiga("OVA - 01.mkv", "OVA - 02.mkv"), EpisodeRun{Season: 0, First: 7, Last: 7}, 7, eps); n != 0 {
		t.Fatalf("two files for one special placed %d", n)
	}
	// Only Sonarr's rejections of its own match go.
	kept := kyousougiga("S - 01.mkv")
	_ = json.Unmarshal([]byte(`[{"reason":"Not an upgrade for existing episode file(s)"},{"reason":"Sample"}]`), &kept[0].Rejections)
	if placeByRun(kept, EpisodeRun{Season: 1, First: 1, Last: 1}, 7, eps); len(kept[0].Rejections) != 1 || kept[0].Rejections[0].Reason != "Sample" {
		t.Fatalf("rejections %v, want only Sample kept", kept[0].Rejections)
	}

	for name, tc := range map[string]struct {
		rows []manualImportRow
		run  EpisodeRun
	}{
		"a repeated number":                   {kyousougiga("A - 01.mkv", "B - 01.mkv"), EpisodeRun{1, 1, 10}},
		"more files than the run":             {kyousougiga(ten...), EpisodeRun{1, 1, 5}},
		"numbers past the run, not all there": {kyousougiga("S - 13.mkv", "S - 14.mkv"), EpisodeRun{1, 13, 15}},
		"an episode Sonarr lacks":             {kyousougiga(ten...), EpisodeRun{1, 20, 29}},
		// A 00 recap beside 01-02 of a three-episode run would shift every file.
		"a 00 file": {kyousougiga("S - 00.mkv", "S - 01.mkv", "S - 02.mkv"), EpisodeRun{1, 1, 3}},
		// 02-04 for a three-episode run: neither from 1, the run's first, nor past it.
		"a block off by one":   {kyousougiga("S - 02.mkv", "S - 03.mkv", "S - 04.mkv"), EpisodeRun{1, 1, 3}},
		"a two-episode file":   {kyousougiga("S - 01-02.mkv", "S - 03.mkv"), EpisodeRun{1, 1, 3}},
		"a two-episode SxxExx": {kyousougiga("S.S01E01-E02.mkv", "S.S01E03.mkv"), EpisodeRun{1, 1, 3}},
	} {
		before := placed(tc.rows)
		if n := placeByRun(tc.rows, tc.run, 7, eps); n != 0 || fmt.Sprint(placed(tc.rows)) != fmt.Sprint(before) {
			t.Errorf("%s: placed %d, rows %v; want Sonarr's reading kept", name, n, placed(tc.rows))
		}
	}
}

// Kyousougiga: the P9 best seeds complete, Sonarr reads its files as
// specials, and the import puts them on S01 by the episode map.
func TestImportSeedingPlacesByTheEpisodeMap(t *testing.T) {
	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"hash": "P9HASH", "name": "[P9] Kyousougiga", "content_path": "/t/P9", "progress": 1},
		})
	}))
	t.Cleanup(qb.Close)
	var eps []map[string]any
	for n := 1; n <= 2; n++ {
		eps = append(eps, map[string]any{"id": 100 + n, "seasonNumber": 1, "episodeNumber": n}, map[string]any{"id": n, "seasonNumber": 0, "episodeNumber": n})
	}
	fake := &fakeSonarr{eps: eps, rows: []map[string]any{
		{"path": "/t/P9/[P9] Kyousougiga - 01.mkv", "size": 1 << 30, "series": map[string]int{"id": 7}, "episodes": []map[string]any{{"id": 1, "seasonNumber": 0}}},
		{"path": "/t/P9/[P9] Kyousougiga - 02.mkv", "size": 1 << 30, "series": map[string]int{"id": 7}, "episodes": []map[string]any{{"id": 2, "seasonNumber": 0}}},
	}}
	lib := newTestLibrary(t, fake)
	lib.Qbit, _ = NewQbit(QbitConfig{URL: qb.URL})
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{"p9hash": {99}}, nil }
	res, err := lib.ImportSeeding(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1})
	if err != nil || res.Files != 0 {
		t.Fatalf("without the map: %+v %v, want nothing imported (Sonarr reads specials)", res, err)
	}
	lib.Runs = func(context.Context) (map[string]EpisodeRun, error) {
		return map[string]EpisodeRun{"p9hash": {Season: 1, First: 1, Last: 2}}, nil
	}
	if res, err = lib.ImportSeeding(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1}); err != nil || res.Files != 2 {
		t.Fatalf("with the map: %+v %v, want 2 files", res, err)
	}
	// Untracked, so "auto" would move the files out of the torrent.
	if fake.cmds[0]["importMode"] != "copy" {
		t.Fatalf("importMode = %v, want copy (hardlink; never move a seeding torrent's files)", fake.cmds[0]["importMode"])
	}
	files := fake.cmds[0]["files"].([]any)
	if ids := files[0].(map[string]any)["episodeIds"]; fmt.Sprint(ids) != "[101]" {
		t.Fatalf("first file to %v, want S01E01 (101)", ids)
	}
}

// directQbit is qBittorrent for DirectGrab: its torrent list, and the adds.
type directQbit struct {
	mu       sync.Mutex
	torrents []map[string]any
	added    []string // "url|category"
}

func (d *directQbit) serve(t *testing.T) *Qbit {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode(d.torrents)
		case "/api/v2/torrents/createCategory":
		case "/api/v2/torrents/add":
			_ = r.ParseForm()
			d.added = append(d.added, r.PostForm.Get("urls")+"|"+r.PostForm.Get("category"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	q, _ := NewQbit(QbitConfig{URL: srv.URL})
	return q
}

func TestDirectGrab(t *testing.T) {
	f := finding("not_downloaded", "p9", 1)
	f.Attrs["info_hash"] = "D2A7"
	f.Attrs["ab_url"] = "https://animebytes.tv/torrents.php?id=16566&torrentid=1121459"
	f.Attrs["nyaa_url"] = "https://nyaa.si/view/1836182"
	lib := newTestLibrary(t, &fakeSonarr{}, f)
	d := &directQbit{}
	lib.Qbit = d.serve(t)
	target := Target{Arr: "sonarr", ID: 7, Season: 1}
	if _, err := lib.DirectGrab(t.Context(), target); err == nil {
		t.Fatal("grabbed with direct grabs off")
	}
	lib.DirectCategory = "anime-tv-imported"
	if _, err := lib.DirectGrab(t.Context(), target); err == nil || len(d.added) != 0 {
		t.Fatalf("grabbed a best with no episode map: %v %v", err, d.added)
	}
	lib.Runs = func(context.Context) (map[string]EpisodeRun, error) {
		return map[string]EpisodeRun{"d2a7": {Season: 1, First: 1, Last: 10}}, nil
	}
	if name, err := lib.DirectGrab(t.Context(), target); err != nil || name == "" || d.added[0] != "https://nyaa.si/download/1836182.torrent|anime-tv-imported" {
		t.Fatalf("no passkey: %q %v %v, want the Nyaa torrent", name, err, d.added)
	}
	lib.ABPasskey = "pk"
	if _, err := lib.DirectGrab(t.Context(), target); err != nil || !strings.HasPrefix(d.added[1], "https://animebytes.tv/torrent/1121459/download/pk|") {
		t.Fatalf("with a passkey: %v %v, want AnimeBytes first", err, d.added)
	}
	// Already in qBittorrent, by hash or by its AnimeBytes comment: nothing added.
	for _, held := range []map[string]any{
		{"hash": "d2a7"},
		{"hash": "OTHER", "private": true, "comment": "https://animebytes.tv/torrent/1121459/group"},
	} {
		d.torrents = []map[string]any{held}
		if name, err := lib.DirectGrab(t.Context(), target); err != nil || name != "" || len(d.added) != 2 {
			t.Fatalf("held %v: %q %v %v, want nothing added", held, name, err, d.added)
		}
	}
	if _, err := lib.DirectGrab(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 2}); err == nil {
		t.Fatal("grabbed for a season the finding is not about")
	}
}

// A specials season holds one best per OVA: every missing one is added, and
// one qBittorrent holds is left.
func TestDirectGrabAddsEverySpecial(t *testing.T) {
	a, b, c := finding("not_downloaded", "moxie", 0), finding("not_downloaded", "moxie", 0), finding("at_best", "moxie", 0)
	a.Attrs["info_hash"], a.Attrs["nyaa_url"] = "AA", "https://nyaa.si/view/11"
	b.Attrs["info_hash"], b.Attrs["nyaa_url"] = "BB", "https://nyaa.si/view/12"
	c.Attrs["info_hash"], c.Attrs["nyaa_url"] = "CC", "https://nyaa.si/view/13"
	lib := newTestLibrary(t, &fakeSonarr{}, a, b, c)
	d := &directQbit{}
	lib.Qbit, lib.DirectCategory = d.serve(t), "anime-tv-imported"
	lib.Runs = func(context.Context) (map[string]EpisodeRun, error) {
		return map[string]EpisodeRun{"aa": {Season: 0, First: 11, Last: 11}, "bb": {Season: 0, First: 12, Last: 12}, "cc": {Season: 0, First: 13, Last: 13}}, nil
	}
	target := Target{Arr: "sonarr", ID: 7, Season: 0}
	if name, err := lib.DirectGrab(t.Context(), target); err != nil || len(d.added) != 2 || !strings.Contains(name, "; ") {
		t.Fatalf("added %v (%q, %v), want both missing OVAs", d.added, name, err)
	}
	d.torrents = []map[string]any{{"hash": "aa"}}
	d.added = nil
	if _, err := lib.DirectGrab(t.Context(), target); err != nil || len(d.added) != 1 || d.added[0] != "https://nyaa.si/download/12.torrent|anime-tv-imported" {
		t.Fatalf("added %v (%v), want only the OVA qBittorrent lacks", d.added, err)
	}
}
