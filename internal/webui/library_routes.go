package webui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// libraryRoutes serves the library actions. Each state-changing route needs
// the CSRF header, like the rest of the dashboard.
func libraryRoutes(app *http.ServeMux, o Options) {
	ready := func(w http.ResponseWriter, r *http.Request, post bool) bool {
		if post && r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return false
		}
		if o.Library == nil {
			http.Error(w, "Sonarr and Radarr are not set up", http.StatusNotImplemented)
			return false
		}
		return true
	}
	fail := func(w http.ResponseWriter, what string, err error) {
		o.Log.Warn("dashboard "+what+" failed", "error", err)
		status := http.StatusBadGateway
		if errors.Is(err, errArrOff) {
			status = http.StatusNotImplemented
		}
		http.Error(w, err.Error(), status)
	}
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(v); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return false
		}
		return true
	}

	app.HandleFunc("GET /api/library", func(w http.ResponseWriter, r *http.Request) {
		if ready(w, r, false) {
			writeJSON(w, o.Library.Scan())
		}
	})
	app.HandleFunc("POST /api/library/scan", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w, r, true) {
			return
		}
		if err := o.Library.StartScan(cmp.Or(o.Ctx, context.Background())); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, o.Library.Scan())
	})
	app.HandleFunc("POST /api/arr/search", func(w http.ResponseWriter, r *http.Request) {
		var t Target
		if !ready(w, r, true) || !decode(w, r, &t) {
			return
		}
		id, err := o.Library.Search(r.Context(), t)
		if err != nil {
			fail(w, "search", err)
			return
		}
		o.Log.Info("search started from the dashboard", "arr", t.Arr, "arr_id", t.ID, "season", t.Season, "command", id)
		writeJSON(w, map[string]int{"command": id})
	})
	app.HandleFunc("POST /api/arr/fixflags", func(w http.ResponseWriter, r *http.Request) {
		var t Target
		if !ready(w, r, true) || !decode(w, r, &t) {
			return
		}
		res, err := o.Library.FixFlags(r.Context(), t)
		if err != nil {
			fail(w, "flag fix", err)
			return
		}
		o.Log.Info("indexer flags fixed from the dashboard", "arr", t.Arr, "arr_id", t.ID, "season", t.Season,
			"fixed", res.Fixed, "already_ok", res.AlreadyOK, "skipped", res.Skipped)
		writeJSON(w, res)
	})
	app.HandleFunc("GET /api/queue", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w, r, false) {
			return
		}
		items, err := o.Library.Queue(r.Context())
		if err != nil {
			fail(w, "queue read", err)
			return
		}
		writeJSON(w, map[string]any{"items": items})
	})
	app.HandleFunc("POST /api/queue/import", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID  int  `json:"id"`
			Dry bool `json:"dry"`
		}
		if !ready(w, r, true) || !decode(w, r, &in) {
			return
		}
		res, err := o.Library.ImportQueued(r.Context(), in.ID, in.Dry)
		if err != nil {
			fail(w, "import", err)
			return
		}
		if in.Dry {
			writeJSON(w, res)
			return
		}
		o.Log.Info("queued download imported from the dashboard", "queue_id", in.ID, "files", res.Files,
			"skipped", res.Skipped, "indexer_flags", res.IndexerFlags, "command", res.Command)
		writeJSON(w, res)
	})
	app.HandleFunc("POST /api/queue/remove", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Arr string `json:"arr"`
			ID  int    `json:"id"`
		}
		if !ready(w, r, true) || !decode(w, r, &in) {
			return
		}
		if err := o.Library.RemoveFromQueue(r.Context(), in.Arr, in.ID); err != nil {
			fail(w, "queue removal", err)
			return
		}
		o.Log.Info("download removed from the queue from the dashboard; the torrent is kept", "arr", in.Arr, "queue_id", in.ID)
		writeJSON(w, map[string]bool{"removed": true})
	})

	leftReady := func(w http.ResponseWriter, r *http.Request, post bool) bool {
		if !ready(w, r, post) {
			return false
		}
		if o.Leftovers == nil {
			http.Error(w, "qBittorrent is not set up", http.StatusNotImplemented)
			return false
		}
		return true
	}
	app.HandleFunc("GET /api/leftovers", func(w http.ResponseWriter, r *http.Request) {
		if leftReady(w, r, false) {
			writeJSON(w, o.Leftovers.Scan())
		}
	})
	app.HandleFunc("POST /api/leftovers/scan", func(w http.ResponseWriter, r *http.Request) {
		if !leftReady(w, r, true) {
			return
		}
		if err := o.Leftovers.Start(cmp.Or(o.Ctx, context.Background())); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, o.Leftovers.Scan())
	})
	app.HandleFunc("POST /api/leftovers/delete", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Hash string `json:"hash"`
		}
		if !leftReady(w, r, true) || !decode(w, r, &in) {
			return
		}
		t, err := o.Leftovers.Delete(r.Context(), in.Hash)
		if err != nil {
			fail(w, "leftover delete", err)
			return
		}
		o.Log.Info("leftover torrent deleted with its data from the dashboard", "torrent", t.Name, "size", t.Size, "category", t.Category)
		writeJSON(w, t)
	})
}
