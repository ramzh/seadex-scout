package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestLeftoversKeepWhatMustNotBeDeleted checks the delete gate: only a torrent
// with no file in the library, from a public tracker, sharing no files and
// complete on disk may be deleted, and Delete re-checks before it does.
func TestLeftoversKeepWhatMustNotBeDeleted(t *testing.T) {
	dir := t.TempDir()
	media, torrents := filepath.Join(dir, "media"), filepath.Join(dir, "torrents")
	write := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(torrents, "inlib", "a.mkv"))
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(torrents, "inlib", "a.mkv"), filepath.Join(media, "a.mkv")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"plain", "ab", "shared"} {
		write(filepath.Join(torrents, n, "b.mkv"))
	}
	tor := func(hash, name string, extra map[string]any) map[string]any {
		m := map[string]any{"hash": hash, "name": name, "category": "anime-tv", "progress": 1.0, "size": 10,
			"save_path": torrents, "content_path": filepath.Join(torrents, name)}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	list := []map[string]any{
		tor("h1", "inlib", nil),
		tor("h2", "plain", nil),
		tor("h3", "ab", map[string]any{"comment": "https://animebytes.tv/torrent/1/group"}),
		tor("h4", "shared", nil),
		tor("h5", "shared", map[string]any{"name": "shared copy"}),
		tor("h6", "gone", nil),
		tor("h7", "other", map[string]any{"category": "stash"}),
	}
	files := map[string]string{"h1": "inlib/a.mkv", "h2": "plain/b.mkv", "h3": "ab/b.mkv", "h4": "shared/b.mkv", "h5": "shared/b.mkv", "h6": "gone/c.mkv", "h7": "plain/b.mkv"}
	var deleted []string
	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode(list)
		case "/api/v2/torrents/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": files[r.URL.Query().Get("hash")], "size": 1}})
		case "/api/v2/torrents/delete":
			_ = r.ParseForm()
			deleted = append(deleted, r.PostForm.Get("hashes")+":"+r.PostForm.Get("deleteFiles"))
		}
	}))
	t.Cleanup(qb.Close)
	q, err := NewQbit(QbitConfig{URL: qb.URL})
	if err != nil {
		t.Fatal(err)
	}
	lib := newTestLibrary(t, &fakeSonarr{roots: []string{media}})
	o := NewLeftovers(lib, q, []string{"anime-tv"})

	found, checked, err := o.find(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range found {
		got[f.Hash] = f.Keep
	}
	want := map[string]string{"h2": "", "h3": "private tracker", "h4": "shares files with another torrent",
		"h5": "shares files with another torrent", "h6": "files missing"}
	if checked != 6 || len(got) != len(want) {
		t.Fatalf("checked %d, found %v; want 6 checked and %v", checked, got, want)
	}
	for h, k := range want {
		if keep, ok := got[h]; !ok || keep != k {
			t.Errorf("%s: keep %q (found %v), want %q", h, keep, ok, k)
		}
	}
	for _, h := range []string{"h1", "h3", "h4", "h6", "h7"} {
		if _, err := o.Delete(t.Context(), h); err == nil {
			t.Errorf("Delete(%s) succeeded", h)
		}
	}
	if _, err := o.Delete(t.Context(), "h2"); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "h2:true" {
		t.Fatalf("deleted %v, want only h2 with its files", deleted)
	}
}
