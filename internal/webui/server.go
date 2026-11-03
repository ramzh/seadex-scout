package webui

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

// csrfHeader must be present on every state-changing request. A browser sends
// a custom header cross-origin only after a CORS preflight, which this server
// never approves, so another site cannot trigger a report run.
const csrfHeader = "X-Seadex-Scout"

// Options configures the dashboard server.
type Options struct {
	Addr      string
	Username  string
	Password  string // empty turns authentication off
	ReportDir string
	Store     *Store
	Runner    *ReportRunner
	// Notifier sends the dashboard's test notification; nil when ntfy is off.
	Notifier interface{ SendTest(context.Context) error }
	// Qbit sends releases to qBittorrent; nil when it is not set up.
	Qbit *Qbit
	// Usenet searches Prowlarr and sends to SABnzbd; nil when not set up.
	Usenet *Usenet
	// Refresher runs the local library refresh; nil when it cannot run.
	Refresher *LibraryRefresher
	// Library acts on Sonarr and Radarr: search, flag fixes, scores, queue.
	Library *Library
	// Leftovers lists and deletes torrents no library file uses any more.
	Leftovers *Leftovers
	// Ctx bounds background jobs the page starts (the score scan).
	Ctx context.Context
	Log *slog.Logger
}

// NewServer returns the dashboard's HTTP server, not yet listening.
func NewServer(o Options) *http.Server {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // the embed directive guarantees the directory
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})

	app := http.NewServeMux()
	files := http.FileServerFS(static)
	app.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// The embedded files carry no modification time, so without this a
		// browser can keep serving the page from before an image update.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	app.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		snap := o.Store.Snapshot()
		if o.Qbit != nil {
			snap.Qbit = o.Qbit.Status()
		}
		if o.Usenet != nil {
			snap.Usenet = o.Usenet.Status()
		}
		if o.Library != nil {
			scan := o.Library.Scan()
			snap.ArrScored, snap.ProvenBest = scan.Clean, scan.ProvenBest
			snap.OnDisk = o.Library.OnDisk(r.Context())
		}
		writeJSON(w, snap)
	})
	app.HandleFunc("GET /api/logs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, o.Store.Logs())
	})
	app.HandleFunc("GET /api/reports", func(w http.ResponseWriter, _ *http.Request) {
		list, err := ListReports(o.ReportDir)
		if err != nil {
			o.Log.Warn("dashboard could not list reports", "error", err)
			http.Error(w, "could not read the report folder", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"reports": list, "job": o.Store.reportJob()})
	})
	app.HandleFunc("GET /api/reports/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		f, err := OpenReport(o.ReportDir, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if filepath.Ext(name) == ".md" {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		if r.URL.Query().Has("download") {
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		}
		http.ServeContent(w, r, "", info.ModTime(), f)
	})
	app.HandleFunc("POST /api/reports/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		if o.Runner == nil {
			http.Error(w, "reports cannot be started from here", http.StatusNotImplemented)
			return
		}
		switch err := o.Runner.Start(); {
		case errors.Is(err, ErrReportRunning):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case err != nil:
			o.Log.Warn("dashboard could not start a report", "error", err)
			http.Error(w, "could not start the report", http.StatusInternalServerError)
			return
		}
		o.Log.Info("report started from the dashboard")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, o.Store.reportJob())
	})
	app.HandleFunc("POST /api/qbit/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		if o.Qbit == nil {
			http.Error(w, "qBittorrent is not set up: set SEADEX_SCOUT_QBIT_URL", http.StatusNotImplemented)
			return
		}
		var in struct {
			URL string `json:"url"`
			Arr string `json:"arr"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		category, err := o.Qbit.AddNyaa(r.Context(), in.URL, in.Arr)
		if err != nil {
			o.Log.Warn("could not send a release to qBittorrent", "error", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		o.Log.Info("release sent to qBittorrent", "url", in.URL, "category", category)
		writeJSON(w, map[string]string{"category": category})
	})
	usenetOff := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return true
		}
		if o.Usenet == nil {
			http.Error(w, "Prowlarr and SABnzbd are not set up: set SEADEX_SCOUT_PROWLARR_URL, SEADEX_SCOUT_PROWLARR_KEY, SEADEX_SCOUT_SABNZBD_URL and SEADEX_SCOUT_SABNZBD_KEY", http.StatusNotImplemented)
			return true
		}
		return false
	}
	app.HandleFunc("POST /api/prowlarr/search", func(w http.ResponseWriter, r *http.Request) {
		if usenetOff(w, r) {
			return
		}
		var in struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		found, err := o.Usenet.Search(r.Context(), in.URL)
		if err != nil {
			o.Log.Warn("Prowlarr search from the dashboard failed", "error", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, found)
	})
	app.HandleFunc("POST /api/sabnzbd/add", func(w http.ResponseWriter, r *http.Request) {
		if usenetOff(w, r) {
			return
		}
		var in struct {
			Token string `json:"token"`
			ID    int    `json:"id"`
			Arr   string `json:"arr"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		category, err := o.Usenet.Send(r.Context(), in.Token, in.ID, in.Arr)
		if err != nil {
			o.Log.Warn("could not send a release to SABnzbd", "error", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		o.Log.Info("release sent to SABnzbd", "category", category)
		writeJSON(w, map[string]string{"category": category})
	})
	app.HandleFunc("POST /api/usenet/test", func(w http.ResponseWriter, r *http.Request) {
		if usenetOff(w, r) {
			return
		}
		prowlarr, sab, err := o.Usenet.Check(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]string{"prowlarr": prowlarr, "sabnzbd": sab})
	})
	app.HandleFunc("POST /api/library/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		if o.Refresher == nil {
			http.Error(w, "the library refresh is not available", http.StatusNotImplemented)
			return
		}
		if err := o.Refresher.Start(); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		o.Log.Info("local library refresh requested from the dashboard")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]bool{"started": true})
	})
	app.HandleFunc("POST /api/qbit/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		if o.Qbit == nil {
			http.Error(w, "qBittorrent is not set up: set SEADEX_SCOUT_QBIT_URL", http.StatusNotImplemented)
			return
		}
		version, err := o.Qbit.Check(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]string{"version": version})
	})
	app.HandleFunc("POST /api/notify/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		if o.Notifier == nil {
			http.Error(w, "ntfy is not set up: set SEADEX_SCOUT_NTFY_URL", http.StatusNotImplemented)
			return
		}
		if err := o.Notifier.SendTest(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]bool{"sent": true})
	})
	libraryRoutes(app, o)
	mux.Handle("/", basicAuth(o.Username, o.Password, app))

	return &http.Server{
		Addr:              o.Addr,
		Handler:           securityHeaders(gzipAPI(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// gzipAPI compresses the API's answers for a client that takes gzip: the
// state the page polls is over a megabyte of JSON that shrinks about tenfold.
func gzipAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close() //nolint:errcheck // the client sees a cut answer either way
		next.ServeHTTP(gzipWriter{ResponseWriter: w, gz: gz}, r)
	})
}

type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

func (g gzipWriter) WriteHeader(code int) {
	// The body is gzip whatever came before, http.Error included.
	g.Header().Del("Content-Length")
	g.Header().Set("Content-Encoding", "gzip")
	g.ResponseWriter.WriteHeader(code)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// basicAuth guards next with HTTP Basic auth when password is set. Both values
// are compared as hashes in constant time, so their length leaks nothing.
func basicAuth(username, password string, next http.Handler) http.Handler {
	if password == "" {
		return next
	}
	wantUser := sha256.Sum256([]byte(username))
	wantPass := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		gotUser := sha256.Sum256([]byte(u))
		gotPass := sha256.Sum256([]byte(p))
		userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
		passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
		if !ok || userOK&passOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="seadex-scout", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
