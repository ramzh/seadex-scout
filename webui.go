package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/seadex-scout/internal/config"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/scout"
	"github.com/cplieger/seadex-scout/internal/webui"
)

// The built-in dashboard is configured from the environment rather than from
// config.yaml, so the upstream config schema (which rejects unknown keys) stays
// unchanged.
const (
	envWebUIPort     = "SEADEX_SCOUT_WEBUI_PORT"
	envWebUIBind     = "SEADEX_SCOUT_WEBUI_BIND"
	envWebUIUsername = "SEADEX_SCOUT_WEBUI_USERNAME"
	envWebUIPassword = "SEADEX_SCOUT_WEBUI_PASSWORD"

	envNtfyURL         = "SEADEX_SCOUT_NTFY_URL"
	envNtfyToken       = "SEADEX_SCOUT_NTFY_TOKEN"
	envNtfyUsername    = "SEADEX_SCOUT_NTFY_USERNAME"
	envNtfyPassword    = "SEADEX_SCOUT_NTFY_PASSWORD"
	envNtfyEvents      = "SEADEX_SCOUT_NTFY_EVENTS"
	envNtfyOptional    = "SEADEX_SCOUT_NTFY_OPTIONAL"
	envNtfyUnmonitored = "SEADEX_SCOUT_NTFY_UNMONITORED"
	envDashboardURL    = "SEADEX_SCOUT_DASHBOARD_URL"

	// envMinScore is the custom format score a SeaDex best file reaches; the
	// Library tab lists files below it.
	envMinScore = "SEADEX_SCOUT_MIN_SCORE"
	// envAutoFix turns on the automatic flag fix and queue import.
	envAutoFix = "SEADEX_SCOUT_AUTO_FIX"
	// envLeftoverCategories lists the qBittorrent categories the leftover
	// torrent view checks (comma-separated).
	envLeftoverCategories     = "SEADEX_SCOUT_LEFTOVER_CATEGORIES"
	defaultLeftoverCategories = "anime-tv,anime-tv-imported,anime-film,anime-film-imported"

	envQbitURL            = "SEADEX_SCOUT_QBIT_URL"
	envQbitUsername       = "SEADEX_SCOUT_QBIT_USERNAME"
	envQbitPassword       = "SEADEX_SCOUT_QBIT_PASSWORD"
	envQbitCategorySonarr = "SEADEX_SCOUT_QBIT_CATEGORY_SONARR"
	envQbitCategoryRadarr = "SEADEX_SCOUT_QBIT_CATEGORY_RADARR"

	envProwlarrURL       = "SEADEX_SCOUT_PROWLARR_URL"
	envProwlarrKey       = "SEADEX_SCOUT_PROWLARR_KEY" // shared with the indexer's ${VAR}
	envSabURL            = "SEADEX_SCOUT_SABNZBD_URL"
	envSabKey            = "SEADEX_SCOUT_SABNZBD_KEY"
	envSabCategorySonarr = "SEADEX_SCOUT_SABNZBD_CATEGORY_SONARR"
	envSabCategoryRadarr = "SEADEX_SCOUT_SABNZBD_CATEGORY_RADARR"

	defaultWebUIPort = "9119"
	webUIStateFile   = "webui-state.json"
	webUISaveEvery   = time.Minute
	webUIStopTimeout = 5 * time.Second
)

// buildVersion names the build, such as "main@<commit>"; the image build
// sets it with -ldflags -X, and a local build leaves "dev".
var buildVersion = "dev"

// activeWebUI is set by main in daemon mode when the dashboard is on; run
// starts it. Every other mode leaves it nil.
var activeWebUI *webUIRuntime

type webUIRuntime struct {
	store *webui.Store
	addr  string

	// refresh runs the scout's local library refresh. It is set once the
	// scout and the cycle lock exist, after the server started.
	refreshMu sync.Mutex
	refresh   func(context.Context) error
}

// setRefresh makes the dashboard's Refresh local library button work.
func (w *webUIRuntime) setRefresh(run func(context.Context) error) {
	w.refreshMu.Lock()
	w.refresh = run
	w.refreshMu.Unlock()
}

func (w *webUIRuntime) runRefresh(ctx context.Context) error {
	w.refreshMu.Lock()
	run := w.refresh
	w.refreshMu.Unlock()
	if run == nil {
		return errors.New("seadex-scout is still starting; try again in a moment")
	}
	return run(ctx)
}

// installWebUI wraps the default logger so the dashboard sees every log line
// from here on, and loads the dashboard's saved state from beside the config
// file. It returns nil when SEADEX_SCOUT_WEBUI_PORT turns the dashboard off.
func installWebUI(configPath string) *webUIRuntime {
	port := strings.TrimSpace(cmp.Or(os.Getenv(envWebUIPort), defaultWebUIPort))
	switch strings.ToLower(port) {
	case "off", "0", "false", "disabled":
		slog.Info("dashboard disabled", "env", envWebUIPort)
		return nil
	}
	addr := net.JoinHostPort(strings.TrimSpace(os.Getenv(envWebUIBind)), port)

	store := webui.NewStore(filepath.Join(filepath.Dir(configPath), webUIStateFile))
	store.SetVersion(shortVersion(buildVersion))
	loadErr := store.Load()
	slog.SetDefault(slog.New(webui.NewHandler(slog.Default().Handler(), store)))
	if loadErr != nil {
		slog.Warn("dashboard could not read its saved state; starting empty", "error", loadErr)
	}
	return &webUIRuntime{store: store, addr: addr}
}

// start serves the dashboard and saves its state every minute until ctx ends.
// The returned func stops the server and makes a last save. A port that cannot
// be bound is logged and the scout keeps running without its dashboard.
func (w *webUIRuntime) start(ctx context.Context, cfg *config.Config) func() {
	log := slog.Default()
	var runner *webui.ReportRunner
	if exe, err := os.Executable(); err == nil {
		runner = webui.NewReportRunner(ctx, exe, w.store)
	} else {
		log.Warn("dashboard cannot start reports: the executable path is unknown", "error", err)
	}
	var sonarr, radarr webui.Arr
	if cfg.SonarrEnabled() {
		sonarr = webui.Arr{URL: cfg.SonarrURL, APIKey: cfg.SonarrAPIKey}
	}
	if cfg.RadarrEnabled() {
		radarr = webui.Arr{URL: cfg.RadarrURL, APIKey: cfg.RadarrAPIKey}
	}
	poller := webui.NewMonitoredPoller(sonarr, radarr, w.store, log)
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		poller.Run(ctx)
	}()
	// Refresh local library re-reads the monitored flags first, so one click
	// picks up both a change of monitoring and newly imported files. The flags
	// are read even when the library refresh itself cannot run.
	refreshAll := func(ctx context.Context) error {
		poller.Refresh(ctx)
		return w.runRefresh(ctx)
	}

	notifier := startNtfy(ctx, w.store, log)

	minScore, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(envMinScore)))
	library := webui.NewLibrary(sonarr, radarr, minScore, w.store, log)
	qbit := newQbit(log)
	library.Qbit = qbit
	library.Best = func(ctx context.Context) (map[string][]int, error) {
		return indexer.BestReleases(ctx, config.DefaultIndexerFeedPath)
	}
	library.Auto, _ = strconv.ParseBool(strings.TrimSpace(os.Getenv(envAutoFix)))
	if library.Auto {
		log.Info("automatic flag fix and queue import on", "env", envAutoFix)
	}
	if notifier != nil {
		notifier.SetDigest(library.Digest)
	}
	refresher := webui.NewLibraryRefresher(ctx, refreshAll, w.store)
	library.Refresh = refresher.Start
	go library.Run(ctx)
	var categories []string
	for _, c := range strings.Split(cmp.Or(os.Getenv(envLeftoverCategories), defaultLeftoverCategories), ",") {
		if c = strings.TrimSpace(c); c != "" {
			categories = append(categories, c)
		}
	}
	leftovers := webui.NewLeftovers(library, qbit, categories)
	if leftovers != nil {
		// The list lives in memory only, so a restart fills it again.
		_ = leftovers.Start(ctx)
	}

	srv := webui.NewServer(webui.Options{
		Addr:      w.addr,
		Username:  cmp.Or(os.Getenv(envWebUIUsername), "admin"),
		Password:  os.Getenv(envWebUIPassword),
		ReportDir: cfg.ReportDir,
		Store:     w.store,
		Runner:    runner,
		Notifier:  notifier,
		Qbit:      qbit,
		Usenet:    newUsenet(log),
		Refresher: refresher,
		Library:   library,
		Leftovers: leftovers,
		Ctx:       ctx,
		Log:       log,
	})

	served := make(chan struct{})
	go func() {
		defer close(served)
		ln, err := net.Listen("tcp", w.addr)
		if err != nil {
			log.Error("dashboard could not listen; the scout keeps running without it",
				"addr", w.addr, "error", err)
			return
		}
		log.Info("dashboard listening", "addr", ln.Addr().String(),
			"auth", os.Getenv(envWebUIPassword) != "")
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("dashboard stopped", "error", err)
		}
	}()

	quit, saved := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(saved)
		t := time.NewTicker(webUISaveEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-quit:
				return
			case <-t.C:
				if err := w.store.Save(); err != nil {
					log.Warn("dashboard could not save its state", "error", err)
				}
			}
		}
	}()

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), webUIStopTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		close(quit)
		<-served
		<-saved
		<-polled
		if err := w.store.Save(); err != nil {
			log.Warn("dashboard could not save its state", "error", err)
		}
	}
}

// startNtfy starts ntfy notifications when SEADEX_SCOUT_NTFY_URL is set, and
// returns the notifier for the dashboard's test button (nil when off). A bad
// setting is logged and leaves notifications off.
func startNtfy(ctx context.Context, store *webui.Store, log *slog.Logger) *webui.Notifier {
	topic := strings.TrimSpace(os.Getenv(envNtfyURL))
	if topic == "" {
		return nil
	}
	events := webui.AllEvents
	if raw := strings.TrimSpace(os.Getenv(envNtfyEvents)); raw != "" {
		events = nil
		for _, e := range strings.Split(raw, ",") {
			if e = strings.ToLower(strings.TrimSpace(e)); slices.Contains(webui.AllEvents, e) {
				events = append(events, e)
			} else if e != "" {
				log.Warn("ignoring an unknown ntfy event name", "env", envNtfyEvents, "event", e,
					"valid", strings.Join(webui.AllEvents, ","))
			}
		}
	}
	n, err := webui.NewNotifier(webui.NtfyConfig{
		TopicURL:     topic,
		Token:        os.Getenv(envNtfyToken),
		Username:     os.Getenv(envNtfyUsername),
		Password:     os.Getenv(envNtfyPassword),
		Events:       events,
		Optional:     envTrue(envNtfyOptional),
		Unmonitored:  envTrue(envNtfyUnmonitored),
		DashboardURL: strings.TrimSpace(os.Getenv(envDashboardURL)),
	}, store, log)
	if err != nil {
		log.Error("ntfy notifications are off", "env", envNtfyURL, "error", err)
		return nil
	}
	log.Info("ntfy notifications on", "events", strings.Join(events, ","))
	go n.Run(ctx)
	return n
}

func envTrue(name string) bool {
	v, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return v
}

// newQbit sets up the Send to qBittorrent buttons when SEADEX_SCOUT_QBIT_URL
// is set, or returns nil. A bad URL is logged and leaves them off.
func newQbit(log *slog.Logger) *webui.Qbit {
	addr := strings.TrimSpace(os.Getenv(envQbitURL))
	if addr == "" {
		return nil
	}
	q, err := webui.NewQbit(webui.QbitConfig{
		URL:      addr,
		Username: os.Getenv(envQbitUsername),
		Password: os.Getenv(envQbitPassword),
		Categories: map[string]string{
			"sonarr": strings.TrimSpace(os.Getenv(envQbitCategorySonarr)),
			"radarr": strings.TrimSpace(os.Getenv(envQbitCategoryRadarr)),
		},
	})
	if err != nil {
		log.Error("Send to qBittorrent is off", "env", envQbitURL, "error", err)
		return nil
	}
	log.Info("Send to qBittorrent on")
	return q
}

// newUsenet sets up the → Prowlarr buttons when both Prowlarr and SABnzbd
// are set, or returns nil. A partial or bad setting is logged.
func newUsenet(log *slog.Logger) *webui.Usenet {
	prowlarr, sab := strings.TrimSpace(os.Getenv(envProwlarrURL)), strings.TrimSpace(os.Getenv(envSabURL))
	if prowlarr == "" && sab == "" {
		return nil
	}
	u, err := webui.NewUsenet(webui.UsenetConfig{
		ProwlarrURL: prowlarr,
		ProwlarrKey: strings.TrimSpace(os.Getenv(envProwlarrKey)),
		SabURL:      sab,
		SabKey:      strings.TrimSpace(os.Getenv(envSabKey)),
		Categories: map[string]string{
			"sonarr": strings.TrimSpace(os.Getenv(envSabCategorySonarr)),
			"radarr": strings.TrimSpace(os.Getenv(envSabCategoryRadarr)),
		},
	})
	if err != nil {
		log.Error("the Prowlarr buttons are off", "error", err)
		return nil
	}
	log.Info("Prowlarr search and SABnzbd sending on")
	return u
}

// errCheckRunning means a scheduled check holds the cycle lock.
var errCheckRunning = errors.New("a SeaDex check is running right now; try again when it finishes")

// localRefresh runs the scout's local library refresh under the cycle lock the
// scheduled checks take, so the two never overlap, and records its health the
// way a check does.
func localRefresh(ex *scheduler.Exclusive, sc *scout.Scout, marker *health.Marker) func(context.Context) error {
	return func(ctx context.Context) error {
		var refreshErr error
		outcome, err := ex.RunOrSkip(func() error {
			healthy, err := sc.RefreshLocal(ctx)
			if err != nil {
				refreshErr = err
				return nil
			}
			if !healthy && ctx.Err() == nil {
				refreshErr = errors.New("Sonarr or Radarr could not be read; see the log")
			}
			if err := marker.SetChecked(healthy); err != nil {
				slog.Error("library refresh could not record health", "error", err)
			}
			return nil
		})
		switch {
		case outcome == scheduler.OutcomeSkipped:
			return errCheckRunning
		case err != nil:
			return err
		}
		return refreshErr
	}
}

// shortVersion trims the commit in "branch@<40-hex commit>" to 7 characters.
func shortVersion(v string) string {
	if i := strings.LastIndex(v, "@"); i >= 0 && len(v)-(i+1) > 7 {
		return v[:i+1+7]
	}
	return v
}
