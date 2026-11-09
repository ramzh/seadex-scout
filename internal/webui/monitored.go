package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Arr is one Sonarr or Radarr instance the dashboard reads monitored flags
// from. An empty URL leaves it out.
type Arr struct {
	URL    string
	APIKey string
}

// monitoredEvery is how often the flags are read again, so a change made in
// Sonarr or Radarr shows on the dashboard without waiting for a SeaDex check.
const monitoredEvery = 15 * time.Minute

// maxArrListBytes bounds one series or movie list response.
const maxArrListBytes = 64 << 20

// MonitoredPoller keeps the set of unmonitored series, seasons and movies,
// read straight from the Sonarr and Radarr APIs: the arr client the scout uses
// does not carry the monitored flags.
type MonitoredPoller struct {
	sonarr, radarr Arr
	store          *Store
	client         *http.Client
	log            *slog.Logger
}

// NewMonitoredPoller returns a poller writing into store.
func NewMonitoredPoller(sonarr, radarr Arr, store *Store, log *slog.Logger) *MonitoredPoller {
	return &MonitoredPoller{sonarr: sonarr, radarr: radarr, store: store, log: log,
		client: &http.Client{Timeout: time.Minute}}
}

// Run reads the flags now and then every monitoredEvery until ctx ends.
func (p *MonitoredPoller) Run(ctx context.Context) {
	if p.sonarr.URL == "" && p.radarr.URL == "" {
		return
	}
	t := time.NewTicker(monitoredEvery)
	defer t.Stop()
	for {
		p.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Monitored is what the page reads: the keys of everything unmonitored,
// "sonarr:<id>" for a series, "sonarr:<id>:<season>" for a season and
// "radarr:<id>" for a movie.
type Monitored struct {
	At          time.Time `json:"at,omitzero"`
	Unmonitored []string  `json:"unmonitored"`
	Error       string    `json:"error,omitempty"`
}

// Refresh reads the monitored flags now: on the poller's own schedule, and
// when the dashboard's Refresh local library button asks. A failed read keeps
// the last flags. It does nothing when neither Sonarr nor Radarr is set up.
func (p *MonitoredPoller) Refresh(ctx context.Context) {
	if p.sonarr.URL == "" && p.radarr.URL == "" {
		return
	}
	var keys []string
	var errs []string
	if p.sonarr.URL != "" {
		k, err := p.sonarrUnmonitored(ctx)
		if err != nil {
			errs = append(errs, "sonarr: "+err.Error())
		}
		keys = append(keys, k...)
	}
	if p.radarr.URL != "" {
		k, err := p.radarrUnmonitored(ctx)
		if err != nil {
			errs = append(errs, "radarr: "+err.Error())
		}
		keys = append(keys, k...)
	}
	if len(errs) > 0 {
		p.log.Warn("dashboard could not read monitored flags; keeping the last ones", "error", strings.Join(errs, "; "))
		p.store.setMonitored(nil, strings.Join(errs, "; "))
		return
	}
	p.store.setMonitored(keys, "")
}

func (p *MonitoredPoller) sonarrUnmonitored(ctx context.Context) ([]string, error) {
	var series []struct {
		ID        int  `json:"id"`
		Monitored bool `json:"monitored"`
		Seasons   []struct {
			SeasonNumber int  `json:"seasonNumber"`
			Monitored    bool `json:"monitored"`
		} `json:"seasons"`
	}
	if err := p.getList(ctx, p.sonarr, "series", &series); err != nil {
		return nil, err
	}
	var keys []string
	for _, s := range series {
		id := strconv.Itoa(s.ID)
		if !s.Monitored {
			keys = append(keys, "sonarr:"+id)
		}
		for _, season := range s.Seasons {
			if !season.Monitored {
				keys = append(keys, "sonarr:"+id+":"+strconv.Itoa(season.SeasonNumber))
			}
		}
	}
	return keys, nil
}

func (p *MonitoredPoller) radarrUnmonitored(ctx context.Context) ([]string, error) {
	var movies []struct {
		ID        int  `json:"id"`
		Monitored bool `json:"monitored"`
	}
	if err := p.getList(ctx, p.radarr, "movie", &movies); err != nil {
		return nil, err
	}
	var keys []string
	for _, m := range movies {
		if !m.Monitored {
			keys = append(keys, "radarr:"+strconv.Itoa(m.ID))
		}
	}
	return keys, nil
}

// errNotFound marks an API version the server does not serve.
var errNotFound = errors.New("not found")

// getList reads resource from API v3, or from v5 on a server that has moved
// on (Sonarr 5), into out. The API key travels in a header, never the URL.
func (p *MonitoredPoller) getList(ctx context.Context, arr Arr, resource string, out any) error {
	err := p.getJSON(ctx, arr, "/api/v3/"+resource, out)
	if errors.Is(err, errNotFound) {
		err = p.getJSON(ctx, arr, "/api/v5/"+resource, out)
	}
	return err
}

func (p *MonitoredPoller) getJSON(ctx context.Context, arr Arr, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(arr.URL, "/")+path, http.NoBody)
	if err != nil {
		return errors.New("the configured URL is not valid")
	}
	req.Header.Set("X-Api-Key", arr.APIKey)
	req.Header.Set("Accept", "application/json")
	res, err := p.client.Do(req)
	if err != nil {
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) && uerr.Unwrap() != nil {
			return uerr.Unwrap() // drops the URL the client wraps the cause in
		}
		return errors.New("request failed")
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound:
		return errNotFound
	case res.StatusCode != http.StatusOK:
		return fmt.Errorf("%s answered %d", path, res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, maxArrListBytes)).Decode(out)
}

func (st *Store) setMonitored(keys []string, errText string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.monitored.Error = errText
	if errText != "" {
		return
	}
	if keys == nil {
		keys = []string{}
	}
	st.monitored.Unmonitored = keys
	st.monitored.At = st.now().UTC()
}

// markMonitored drops t (and, for a season, its series) from the unmonitored
// keys at once, so the page stops offering to monitor it before the next
// read of the flags.
func (st *Store) markMonitored(t Target) {
	st.mu.Lock()
	defer st.mu.Unlock()
	base := t.Arr + ":" + strconv.Itoa(t.ID)
	st.monitored.Unmonitored = slices.DeleteFunc(slices.Clone(st.monitored.Unmonitored), func(k string) bool {
		return k == base || k == t.key()
	})
}
