package webui

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSonarr is a Sonarr 5 that answers the calls the library actions make
// and records what was written.
type fakeSonarr struct {
	files   []map[string]any
	queue   []map[string]any
	history []map[string]any
	rows    []map[string]any
	bulk    []map[string]any
	cmds    []map[string]any
	eps     []map[string]any // GET /episode; two plain episodes when nil
	rels    []map[string]any // GET /release
	roots   []string
	status  string // GET /command/{id}
}

func (f *fakeSonarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	enc := json.NewEncoder(w)
	switch {
	case r.URL.Path == "/api/v5/system/status":
		_ = enc.Encode(map[string]string{"version": "5.0.0"})
	case r.URL.Path == "/api/v5/series/7":
		_ = enc.Encode(map[string]int{"id": 7, "tvdbId": 99})
	case r.URL.Path == "/api/v5/rootfolder":
		var out []map[string]string
		for _, p := range f.roots {
			out = append(out, map[string]string{"path": p})
		}
		_ = enc.Encode(out)
	case r.URL.Path == "/api/v5/episodefile" && r.Method == http.MethodGet:
		_ = enc.Encode(f.files)
	case r.URL.Path == "/api/v5/episodefile/bulk" && r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &f.bulk)
		w.WriteHeader(http.StatusAccepted)
	case r.URL.Path == "/api/v5/queue":
		_ = enc.Encode(map[string]any{"records": f.queue})
	case r.URL.Path == "/api/v5/episode" && f.eps != nil:
		_ = enc.Encode(f.eps)
	case r.URL.Path == "/api/v5/episode":
		_ = enc.Encode([]map[string]int{{"id": 1, "seasonNumber": 1}, {"id": 2, "seasonNumber": 2}})
	case r.URL.Path == "/api/v5/history":
		_ = enc.Encode(map[string]any{"records": f.history})
	case r.URL.Path == "/api/v5/release":
		_ = enc.Encode(f.rels)
	case r.URL.Path == "/api/v5/manualimport":
		_ = enc.Encode(f.rows)
	case strings.HasPrefix(r.URL.Path, "/api/v5/command/"):
		_ = enc.Encode(map[string]string{"status": f.status})
	case r.URL.Path == "/api/v5/command" && r.Method == http.MethodPost:
		var cmd map[string]any
		_ = json.NewDecoder(r.Body).Decode(&cmd)
		f.cmds = append(f.cmds, cmd)
		_ = enc.Encode(map[string]int{"id": 42})
	default:
		http.NotFound(w, r)
	}
}

func newTestLibrary(t *testing.T, fake *fakeSonarr, findings ...Line) *Library {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	st := NewStore("")
	st.s.Findings = findings
	return NewLibrary(Arr{URL: srv.URL, APIKey: "k"}, Arr{}, 0, st, slog.New(slog.DiscardHandler))
}

func finding(status, groups string, season int) Line {
	return Line{Msg: "library holds the SeaDex best", Attrs: map[string]any{
		"arr": "sonarr", "arr_id": int64(7), "scope": "season", "season": int64(season),
		"status": status, "recommended_groups": groups,
	}}
}

func TestFixFlagsFlagsOnlyTheBestGroupsLowFiles(t *testing.T) {
	fake := &fakeSonarr{files: []map[string]any{
		{"id": 1, "seasonNumber": 1, "releaseGroup": "NAN0", "customFormatScore": 0, "indexerFlags": 4},
		{"id": 2, "seasonNumber": 1, "releaseGroup": "Other", "customFormatScore": 0, "indexerFlags": 0},
		{"id": 3, "seasonNumber": 1, "releaseGroup": "nan0", "customFormatScore": 5000, "indexerFlags": 80},
		{"id": 4, "seasonNumber": 2, "releaseGroup": "NAN0", "customFormatScore": 0, "indexerFlags": 0},
	}}
	lib := newTestLibrary(t, fake, finding("at_best", "nan0", 1))
	res, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fixed != 1 || res.AlreadyOK != 1 || res.Skipped != 1 {
		t.Fatalf("result = %+v, want 1 fixed, 1 already ok, 1 skipped", res)
	}
	if len(fake.bulk) != 1 || fake.bulk[0]["id"] != float64(1) || fake.bulk[0]["indexerFlags"] != float64(4|sonarrBestFlags) {
		t.Fatalf("bulk update = %v, want file 1 with its flags plus Freeleech25+Scene", fake.bulk)
	}
}

func TestFixFlagsRefusesWithoutABestFinding(t *testing.T) {
	fake := &fakeSonarr{files: []map[string]any{
		{"id": 1, "seasonNumber": 1, "releaseGroup": "NAN0", "customFormatScore": 0, "indexerFlags": 0},
	}}
	lib := newTestLibrary(t, fake, finding("better_release", "nan0", 1))
	if _, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1}); err == nil {
		t.Fatal("FixFlags flagged files the scout does not say hold the best")
	}
	if _, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 8, Season: 1}); err == nil {
		t.Fatal("FixFlags flagged files with no finding at all")
	}
	if fake.bulk != nil {
		t.Fatalf("bulk update sent: %v", fake.bulk)
	}
}

func TestImportQueuedTakesOnlyLowEpisodesAndGrabFlags(t *testing.T) {
	ep := func(id, file int) map[string]any { return map[string]any{"id": id, "episodeFileId": file} }
	fake := &fakeSonarr{
		queue: []map[string]any{{"id": 5, "seriesId": 7, "downloadId": "ABC", "outputPath": "/data/pack", "sizeLeft": 0}},
		history: []map[string]any{
			{"eventType": "grabbed", "data": map[string]string{"indexerFlags": "Scene, Freeleech25"}},
		},
		files: []map[string]any{
			{"id": 100, "customFormatScore": 5000},
			{"id": 101, "customFormatScore": 0},
		},
		rows: []map[string]any{
			{"path": "/data/pack/01.mkv", "series": map[string]int{"id": 7}, "episodes": []any{ep(1, 100)},
				"rejections": []any{map[string]string{"reason": "Not a Custom Format upgrade"}}},
			{"path": "/data/pack/02.mkv", "series": map[string]int{"id": 7}, "episodes": []any{ep(2, 101)},
				"rejections": []any{map[string]string{"reason": "Episode 1x02 was unexpected considering the folder name"}}},
			{"path": "/data/pack/03.mkv", "series": map[string]int{"id": 7}, "episodes": []any{ep(3, 0)}},
			{"path": "/data/pack/04.mkv", "series": map[string]int{"id": 7}, "episodes": []any{ep(4, 101)},
				"rejections": []any{map[string]string{"reason": "Sample"}}},
			{"path": "/data/pack/NCOP.mkv"},
		},
	}
	lib := newTestLibrary(t, fake)
	if preview, err := lib.ImportQueued(t.Context(), "sonarr", 5, true); err != nil || preview.Files != 2 || len(fake.cmds) != 0 {
		t.Fatalf("dry run = %+v, %v, commands %v; want 2 files and nothing sent", preview, err, fake.cmds)
	}
	res, err := lib.ImportQueued(t.Context(), "sonarr", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Skipped != 3 || res.IndexerFlags != sonarrBestFlags || res.Command != 42 {
		t.Fatalf("result = %+v, want 2 files, 3 skipped, flags %d, command 42", res, sonarrBestFlags)
	}
	files := fake.cmds[0]["files"].([]any)
	var paths []string
	for _, f := range files {
		m := f.(map[string]any)
		paths = append(paths, m["path"].(string))
		if m["indexerFlags"] != float64(sonarrBestFlags) {
			t.Errorf("%s imported with flags %v, want the grab's", m["path"], m["indexerFlags"])
		}
	}
	if got := strings.Join(paths, ","); got != "/data/pack/02.mkv,/data/pack/03.mkv" {
		t.Fatalf("imported %s, want episodes 2 and 3 only", got)
	}
	if fake.cmds[0]["importMode"] != "auto" {
		t.Fatalf("importMode = %v, want auto (hardlink)", fake.cmds[0]["importMode"])
	}
	if res.Done {
		t.Fatal("a pack with files left to import was reported done")
	}
}

func TestImportQueuedReportsDoneOnlyWhenEveryMatchedFileIsImported(t *testing.T) {
	ep := func(id, file int) map[string]any { return map[string]any{"id": id, "episodeFileId": file} }
	good := map[string]any{"path": "/data/pack/01.mkv", "series": map[string]int{"id": 7}, "episodes": []any{ep(1, 100)}}
	fake := &fakeSonarr{
		queue:   []map[string]any{{"id": 5, "seriesId": 7, "downloadId": "ABC", "outputPath": "/data/pack", "sizeLeft": 0}},
		history: []map[string]any{{"eventType": "grabbed", "data": map[string]string{"indexerFlags": "Scene, Freeleech25"}}},
		files:   []map[string]any{{"id": 100, "customFormatScore": 5000}, {"id": 101, "customFormatScore": 0}},
	}
	lib := newTestLibrary(t, fake)
	for _, tc := range []struct {
		name string
		rows []map[string]any
		done bool
	}{
		{"imported plus extras", []map[string]any{good, {"path": "/data/pack/NCOP.mkv"}}, true},
		{"nothing matched", []map[string]any{{"path": "/data/pack/NCOP.mkv"}}, false},
		{"a low file is blocked", []map[string]any{good, {"path": "/data/pack/02.mkv", "series": map[string]int{"id": 7},
			"episodes": []any{ep(2, 101)}, "rejections": []any{map[string]string{"reason": "Sample"}}}}, false},
	} {
		fake.rows = tc.rows
		res, err := lib.ImportQueued(t.Context(), "sonarr", 5, true)
		if err != nil || res.Done != tc.done {
			t.Errorf("%s: done = %v, %v; want %v", tc.name, res.Done, err, tc.done)
		}
	}
}

func TestSearchPicksTheCommand(t *testing.T) {
	fake := &fakeSonarr{}
	lib := newTestLibrary(t, fake)
	for _, tc := range []struct {
		season int
		want   string
	}{{2, "SeasonSearch"}, {-1, "SeriesSearch"}} {
		if _, err := lib.Search(t.Context(), Target{Arr: "sonarr", ID: 7, Season: tc.season}); err != nil {
			t.Fatal(err)
		}
		if got := fake.cmds[len(fake.cmds)-1]["name"]; got != tc.want {
			t.Errorf("season %d sent %v, want %s", tc.season, got, tc.want)
		}
	}
	if _, err := lib.Search(t.Context(), Target{Arr: "radarr", ID: 1}); err == nil {
		t.Error("search on an arr that is not set up succeeded")
	}
}

func TestFixFlagsFlagsFilesImportedFromABestTorrent(t *testing.T) {
	fake := &fakeSonarr{
		files: []map[string]any{
			{"id": 1, "seasonNumber": 2, "releaseGroup": "", "customFormatScore": 0, "indexerFlags": 0},
			{"id": 2, "seasonNumber": 2, "releaseGroup": "", "customFormatScore": 0, "indexerFlags": 0},
			{"id": 3, "seasonNumber": 2, "releaseGroup": "", "customFormatScore": 0, "indexerFlags": 0},
		},
		history: []map[string]any{
			{"eventType": "downloadFolderImported", "downloadId": "ABC", "data": map[string]string{"fileId": "1"}},
			{"eventType": "downloadFolderImported", "downloadId": "DEF", "data": map[string]string{"fileId": "2"}},
			{"eventType": "grabbed", "downloadId": "ABC", "data": map[string]string{"fileId": "3"}},
		},
	}
	lib := newTestLibrary(t, fake) // no finding at all
	lib.Best = func(context.Context) (map[string][]int, error) {
		return map[string][]int{"abc": {99}, "def": {5}}, nil // DEF is best for another series
	}
	res, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fixed != 1 || res.Proven != 1 || res.Skipped != 2 {
		t.Fatalf("result = %+v, want file 1 fixed by proof and 2 skipped", res)
	}
	if len(fake.bulk) != 1 || fake.bulk[0]["id"] != float64(1) {
		t.Fatalf("bulk update = %v, want file 1 only", fake.bulk)
	}
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{}, nil }
	fake.bulk = nil
	if _, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 2}); err == nil || fake.bulk != nil {
		t.Fatalf("flagged without a finding or a proof: %v, %v", err, fake.bulk)
	}
}

// Irregular Witch S1: Erai-raws was grabbed as the best, then SeaDex named
// Kitsune. Only the flagged non-best files lose the flags; a file imported from
// a best torrent keeps them.
func TestFixFlagsClearsOnlyFlagsSeaDexNoLongerBacks(t *testing.T) {
	fake := &fakeSonarr{
		files: []map[string]any{
			{"id": 1, "seasonNumber": 1, "releaseGroup": "Erai-raws", "indexerFlags": 4 | sonarrBestFlags},
			{"id": 2, "seasonNumber": 1, "releaseGroup": "kitsune", "indexerFlags": sonarrBestFlags},
			{"id": 3, "seasonNumber": 1, "releaseGroup": "Other", "indexerFlags": 0},
			{"id": 4, "seasonNumber": 1, "releaseGroup": "", "indexerFlags": sonarrBestFlags},
			{"id": 5, "seasonNumber": 2, "releaseGroup": "Erai-raws", "indexerFlags": sonarrBestFlags},
		},
		history: []map[string]any{
			{"eventType": "downloadFolderImported", "downloadId": "ABC", "data": map[string]string{"fileId": "4"}},
		},
	}
	lib := newTestLibrary(t, fake, finding("better_release", "kitsune", 1))
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{"abc": {99}}, nil }
	res, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleared != 1 || res.Fixed != 0 || res.AlreadyOK != 2 || res.Skipped != 1 {
		t.Fatalf("result = %+v, want 1 cleared, the best and the proven kept, the unflagged one skipped", res)
	}
	if len(fake.bulk) != 1 || fake.bulk[0]["id"] != float64(1) || fake.bulk[0]["indexerFlags"] != float64(4) {
		t.Fatalf("bulk update = %v, want file 1 with only its own flags", fake.bulk)
	}
	if got := lib.store.betterReleaseTargets(); len(got) != 1 || got[0].Target != (Target{Arr: "sonarr", ID: 7, Season: 1}) {
		t.Fatalf("better release targets = %+v", got)
	}

	fake.bulk = nil
	lib.Best = func(context.Context) (map[string][]int, error) { return nil, errors.New("feed down") }
	if _, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1}); err == nil || fake.bulk != nil {
		t.Fatalf("cleared without the import proof: %v, %v", err, fake.bulk)
	}
	for _, status := range []string{"at_best", "theoretical_best", "no_best"} {
		lib := newTestLibrary(t, fake, finding(status, "kitsune", 1))
		if _, err := lib.FixFlags(t.Context(), Target{Arr: "sonarr", ID: 7, Season: 1}); fake.bulk != nil {
			t.Fatalf("%s: cleared flags without a better release: %v (%v)", status, fake.bulk, err)
		}
	}
}

// Without waiting for a scan, a better release clears the stale flags and puts
// one search in line; the next tick finds nothing left to clear.
func TestAutoFixClearsStaleFlagsAndSearchesOnce(t *testing.T) {
	fake := &fakeSonarr{files: []map[string]any{
		{"id": 1, "seasonNumber": 1, "releaseGroup": "Erai-raws", "indexerFlags": sonarrBestFlags},
	}}
	f := finding("better_release", "kitsune", 1)
	f.Attrs["title"] = "Irregular Witch"
	lib := newTestLibrary(t, fake, f)
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{}, nil }
	lib.autoFix(t.Context(), false)
	if len(fake.bulk) != 1 || fake.bulk[0]["indexerFlags"] != float64(0) {
		t.Fatalf("bulk update = %v, want file 1 unflagged", fake.bulk)
	}
	fake.files[0]["indexerFlags"] = 0
	lib.autoFix(t.Context(), false)
	if s := lib.Searches(); len(s) != 1 || s[0].Target != (Target{Arr: "sonarr", ID: 7, Season: 1}) || s[0].State != "waiting" {
		t.Fatalf("searches = %+v, want one waiting search for the season", s)
	}
}

func TestBestIndexTrustsOnlyPrivateAnimeBytesComments(t *testing.T) {
	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"hash": "AB1", "private": true, "comment": "https://animebytes.tv/torrent/11/group"},
			{"hash": "PUB", "private": false, "comment": "https://animebytes.tv/torrent/11/group"},
			{"hash": "ODD", "private": true, "comment": "see https://animebytes.tv/torrent/11/group"},
		})
	}))
	t.Cleanup(qb.Close)
	lib := newTestLibrary(t, &fakeSonarr{})
	lib.Qbit, _ = NewQbit(QbitConfig{URL: qb.URL})
	lib.Best = func(context.Context) (map[string][]int, error) { return map[string][]int{"ab:11": {1}}, nil }
	idx, err := lib.bestIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx["ab1"]; !ok || len(idx) != 1 {
		t.Fatalf("index = %v, want only the private AnimeBytes torrent", idx)
	}
}

func TestSearchesRunInLineAndReportTheGrab(t *testing.T) {
	fake := &fakeSonarr{status: "started"}
	lib := newTestLibrary(t, fake)
	jobs := []SearchJob{{Target: Target{Arr: "sonarr", ID: 7, Season: 1}}, {Target: Target{Arr: "sonarr", ID: 8, Season: 2}}}
	if n, err := lib.QueueSearches(jobs, false); err != nil || n != 2 {
		t.Fatalf("queued %d, %v; want 2", n, err)
	}
	if n, _ := lib.QueueSearches(jobs[:1], false); n != 0 {
		t.Fatal("the same target was put in line twice")
	}
	lib.stepSearches(t.Context())
	if len(fake.cmds) != 1 || fake.cmds[0]["seriesId"] != float64(7) {
		t.Fatalf("commands %v, want the oldest (series 7) only", fake.cmds)
	}
	lib.stepSearches(t.Context())
	if got := lib.Searches(); got[1].State != "started" || got[0].State != "waiting" || len(fake.cmds) != 1 {
		t.Fatalf("states %s, %s with %d commands; want started and still waiting", got[1].State, got[0].State, len(fake.cmds))
	}
	fake.status = "completed"
	fake.history = []map[string]any{
		{"eventType": "grabbed", "date": time.Now().UTC().Format(time.RFC3339), "sourceTitle": "[NAN0] Show S01", "episodeId": 1},
		{"eventType": "grabbed", "date": time.Now().UTC().Format(time.RFC3339), "sourceTitle": "RSS grab of S02", "episodeId": 2},
		{"eventType": "grabbed", "date": "2020-01-01T00:00:00Z", "sourceTitle": "old grab", "episodeId": 1},
	}
	lib.stepSearches(t.Context())
	got := lib.Searches()
	if got[1].State != "grabbed" || len(got[1].Grabbed) != 1 || got[1].Grabbed[0] != "[NAN0] Show S01" {
		t.Fatalf("finished search = %+v, want grabbed [NAN0] Show S01 only", got[1])
	}
	if len(fake.cmds) != 1 {
		t.Fatal("the next search started before the minute between searches")
	}
	if n := lib.CancelSearches(); n != 1 || lib.Searches()[0].State != "cancelled" {
		t.Fatalf("cancelled %d, want the waiting one", n)
	}
}

func TestRestartKeepsTheScanAndTheSearches(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir + "/webui-state.json")
	lib := NewLibrary(Arr{URL: "http://sonarr.invalid", APIKey: "k"}, Arr{}, 0, st, slog.New(slog.DiscardHandler))
	if _, err := lib.QueueSearches([]SearchJob{{Target: Target{Arr: "sonarr", ID: 7, Season: 1}}}, false); err != nil {
		t.Fatal(err)
	}
	lib.mu.Lock()
	lib.scan = ScoreScan{Running: true, FinishedAt: time.Now().UTC(), Clean: []string{"sonarr:7:1"}}
	lib.persistLocked()
	lib.mu.Unlock()
	again := NewLibrary(Arr{URL: "http://sonarr.invalid", APIKey: "k"}, Arr{}, 0, st, slog.New(slog.DiscardHandler))
	if s := again.Searches(); len(s) != 1 || s[0].State != "waiting" {
		t.Fatalf("searches after restart = %+v, want the one waiting", s)
	}
	if sc := again.Scan(); sc.Running || len(sc.Clean) != 1 || again.lastScan.IsZero() {
		t.Fatalf("scan after restart = %+v, want it finished, with its clean keys", sc)
	}
}

func TestGzipAPI(t *testing.T) {
	h := gzipAPI(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]int{"a": 1}) }))
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	zr, err := gzip.NewReader(rec.Body)
	if err != nil || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("not gzip: %v %v", rec.Header(), err)
	}
	if b, _ := io.ReadAll(zr); strings.TrimSpace(string(b)) != `{"a":1}` {
		t.Fatalf("body %q", b)
	}
}

func TestImportQueuedPlacesAnUnknownFilmBySeaDexHash(t *testing.T) {
	var cmds []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/api/v3/queue":
			_ = enc.Encode(map[string]any{"records": []map[string]any{{"id": 3, "title": "Film", "outputPath": "/d/f", "downloadId": "ABC"}}})
		case "/api/v3/history":
			_ = enc.Encode(map[string]any{"records": []any{}})
		case "/api/v3/manualimport":
			_ = enc.Encode([]map[string]any{
				{"path": "/d/f/film.mkv", "size": 10, "rejections": []map[string]string{{"reason": "Unknown Movie"}}},
				{"path": "/d/f/sample.mkv", "size": 1},
			})
		case "/api/v3/moviefile":
			_ = enc.Encode([]any{})
		case "/api/v3/command":
			var cmd map[string]any
			_ = json.NewDecoder(r.Body).Decode(&cmd)
			cmds = append(cmds, cmd)
			_ = enc.Encode(map[string]int{"id": 9})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	st := NewStore("")
	st.s.Findings = []Line{{Attrs: map[string]any{"arr": "radarr", "arr_id": float64(5), "info_hash": "abc"}}}
	lib := NewLibrary(Arr{}, Arr{URL: srv.URL, APIKey: "k"}, 0, st, slog.New(slog.DiscardHandler))
	res, err := lib.ImportQueued(t.Context(), "radarr", 3, false)
	if err != nil || res.Files != 1 || len(cmds) != 1 {
		t.Fatalf("result %+v, %v, %d commands; want the one film imported", res, err, len(cmds))
	}
	f := cmds[0]["files"].([]any)[0].(map[string]any)
	if f["movieId"] != float64(5) || f["path"] != "/d/f/film.mkv" || f["indexerFlags"] != float64(radarrBestFlags) {
		t.Fatalf("imported %v; want film.mkv as movie 5 with the best flags", f)
	}
}

func TestRestartDoesNotRepeatAnUpgradeStillFound(t *testing.T) {
	st := NewStore("")
	found := func(after bool) Line {
		return Line{Msg: "upgrade found", Attrs: map[string]any{"arr": "sonarr", "al_id": float64(1), "season": float64(1), "after_start": after}}
	}
	st.Observe(found(false))
	st.Observe(found(true)) // a restart finds it again
	if n := len(st.s.Events); n != 1 {
		t.Fatalf("%d events, want the first find only", n)
	}
	st.Observe(Line{Msg: "upgrade resolved", Attrs: map[string]any{"arr": "sonarr", "al_id": float64(1), "season": float64(1)}})
	st.Observe(found(true))
	if n := len(st.s.Events); n != 3 {
		t.Fatalf("%d events, want found, resolved, found again", n)
	}
	if logs := st.Logs(); len(logs) != 0 {
		t.Fatalf("activity lines reached the recent log: %v", logs)
	}
}

func TestABestGrabReplacesFilesScoredBestThatAreNot(t *testing.T) {
	fake := &fakeSonarr{
		queue:   []map[string]any{{"id": 5, "seriesId": 7, "outputPath": "/data/pack", "downloadId": "H", "sizeleft": 0}},
		history: []map[string]any{{"eventType": "grabbed", "data": map[string]string{"indexerFlags": "Scene, Freeleech25"}}},
		files:   []map[string]any{{"id": 100, "seasonNumber": 1, "customFormatScore": 5000}},
		rows: []map[string]any{{"path": "/data/pack/01.mkv", "series": map[string]int{"id": 7},
			"episodes": []any{map[string]int{"id": 1, "episodeFileId": 100, "seasonNumber": 1}}}},
	}
	lib := newTestLibrary(t, fake, Line{Attrs: map[string]any{"arr": "sonarr", "arr_id": float64(7), "season": float64(1), "scope": "season", "status": "better_release"}})
	res, err := lib.ImportQueued(t.Context(), "sonarr", 5, true)
	if err != nil || res.Done || res.Files != 1 {
		t.Fatalf("preview %+v, %v; want the best grab to replace the alt scored 5000", res, err)
	}
}

func TestAWrongMatchIsNotAlreadyImported(t *testing.T) {
	// Sonarr matched an S2 pack to S1 episodes that already score the
	// minimum; the grab was for other episodes, so nothing is "done".
	fake := &fakeSonarr{
		queue:   []map[string]any{{"id": 5, "seriesId": 7, "outputPath": "/data/pack", "downloadId": "H", "sizeleft": 0}},
		history: []map[string]any{{"eventType": "grabbed", "episodeId": 50, "data": map[string]string{"indexerFlags": "Scene, Freeleech25"}}},
		files:   []map[string]any{{"id": 100, "seasonNumber": 1, "customFormatScore": 5000}},
		rows: []map[string]any{{"path": "/data/pack/01.mkv", "series": map[string]int{"id": 7},
			"episodes": []any{map[string]int{"id": 1, "episodeFileId": 100, "seasonNumber": 1}}}},
	}
	lib := newTestLibrary(t, fake)
	res, err := lib.ImportQueued(t.Context(), "sonarr", 5, true)
	if err != nil || res.Done {
		t.Fatalf("preview %+v, %v; a match outside the grab must not read as already imported", res, err)
	}
}

func TestEpisodeVideoLeavesOutExtras(t *testing.T) {
	big := int64(500 << 20)
	for name, want := range map[string]bool{
		"Show S02/S02E04-The Island.mkv": true,
		"Show S02/S02OP-Baddest.mkv":     false,
		"Show/Extras/NCED1.mkv":          false,
		"Show/Menu 01.m2ts":              false,
		"Show/notes.txt":                 false,
	} {
		if got := episodeVideo(QbitFile{Name: name, Size: big}); got != want {
			t.Errorf("episodeVideo(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestImportQueuedMatchesUnmatchedPacksByNumber(t *testing.T) {
	big := int64(500 << 20)
	ep := func(id, s, e, abs, file int) map[string]any {
		return map[string]any{"id": id, "seasonNumber": s, "episodeNumber": e, "absoluteEpisodeNumber": abs, "episodeFileId": file}
	}
	row := func(name string) map[string]any {
		return map[string]any{"path": "/data/pack/" + name, "size": big,
			"rejections": []any{map[string]string{"reason": "Invalid season or episode"}}}
	}
	imported := func(fake *fakeSonarr) map[string]float64 {
		out := map[string]float64{}
		for _, f := range fake.cmds[len(fake.cmds)-1]["files"].([]any) {
			m := f.(map[string]any)
			out[m["path"].(string)[len("/data/pack/"):]] = m["episodeIds"].([]any)[0].(float64)
		}
		return out
	}
	queue := []map[string]any{{"id": 5, "seriesId": 7, "downloadId": "H", "outputPath": "/data/pack", "sizeLeft": 0}}
	grab := func(ids ...int) []map[string]any {
		var h []map[string]any
		for _, id := range ids {
			h = append(h, map[string]any{"eventType": "grabbed", "episodeId": id, "data": map[string]string{"indexerFlags": "Scene, Freeleech25"}})
		}
		return h
	}

	// Absolute numbers; episode 1 already scores the minimum.
	abs := &fakeSonarr{queue: queue, history: grab(12),
		files: []map[string]any{{"id": 100, "seasonNumber": 1, "customFormatScore": 5000}},
		eps:   []map[string]any{ep(11, 1, 1, 1, 100), ep(12, 2, 1, 2, 0), ep(13, 2, 2, 3, 0)},
		rows: []map[string]any{row("[BBT-RMX] Slam Dunk - 001 (BD 1080p) [87CC624A].mkv"),
			row("[BBT-RMX] Slam Dunk - 002 (BD 1080p) [E57A9D9D].mkv"), row("[BBT-RMX] Slam Dunk - 003v2 (BD 1080p).mkv"),
			row("[BBT-RMX] Slam Dunk - NCOP (BD 1080p).mkv")}}
	abs.rows[0]["quality"] = map[string]any{"quality": map[string]any{"id": 7, "name": "Bluray-1080p"}}
	abs.rows[1]["quality"] = map[string]any{"quality": map[string]any{"id": 0, "name": "Unknown"}}
	if _, err := newTestLibrary(t, abs).ImportQueued(t.Context(), "sonarr", 5, false); err != nil {
		t.Fatal(err)
	}
	if got := imported(abs); len(got) != 2 || got["[BBT-RMX] Slam Dunk - 002 (BD 1080p) [E57A9D9D].mkv"] != 12 ||
		got["[BBT-RMX] Slam Dunk - 003v2 (BD 1080p).mkv"] != 13 {
		t.Fatalf("absolute pack imported %v, want 002 as 12 and 003v2 as 13", got)
	}
	for _, f := range abs.cmds[0]["files"].([]any) {
		if g := f.(map[string]any)["releaseGroup"]; g != "BBT-RMX" {
			t.Errorf("release group %v, want BBT-RMX from the name", g)
		}
		if q := f.(map[string]any)["quality"].(map[string]any)["quality"].(map[string]any)["id"]; q != float64(7) {
			t.Errorf("quality id %v, want 7 borrowed from the pack", q)
		}
	}

	// The pack calls the season's second half S11E01-E02; Sonarr has S10E13-E14.
	renum := &fakeSonarr{queue: queue, history: grab(22, 21),
		eps:  []map[string]any{ep(20, 10, 12, 353, 0), ep(21, 10, 13, 354, 0), ep(22, 10, 14, 355, 0)},
		rows: []map[string]any{row("Gintama.S11E02.1080p-Sylvar.mkv"), row("Gintama.S11E01.1080p-Sylvar.mkv")}}
	if _, err := newTestLibrary(t, renum).ImportQueued(t.Context(), "sonarr", 5, false); err != nil {
		t.Fatal(err)
	}
	if got := imported(renum); got["Gintama.S11E01.1080p-Sylvar.mkv"] != 21 || got["Gintama.S11E02.1080p-Sylvar.mkv"] != 22 {
		t.Fatalf("renumbered pack imported %v, want S11E01 as 21 and S11E02 as 22", got)
	}

	// Counts disagree: no guess, nothing imported.
	renum.history, renum.cmds = grab(21), nil
	res, err := newTestLibrary(t, renum).ImportQueued(t.Context(), "sonarr", 5, true)
	if err != nil || res.Files != 0 {
		t.Fatalf("preview %+v, %v; want no guess when the grab's episodes and files disagree", res, err)
	}
}
