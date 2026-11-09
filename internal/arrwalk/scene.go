package arrwalk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/seadex-scout/internal/library"
)

// SeriesScene is what Sonarr's series list says about one series' titles: the
// alternate titles its scene mapping pins to one season, and the canonical
// title Sonarr's parser files as each season (library.Item.CanonTitles).
type SeriesScene struct {
	Titles []library.SceneTitle
	Canon  map[int]string
}

// SceneTitleSource reads, per Sonarr series id, the series' scene titles.
type SceneTitleSource func(ctx context.Context) (map[int]SeriesScene, error)

// stampSceneTitles sets each Sonarr item's SceneTitles and CanonTitles from
// the configured source. A failed read leaves them nil, which only disables
// the misfiled-title drop and the canonical copies in search results; nothing
// else reads them.
func (w *Walker) stampSceneTitles(ctx context.Context, items []library.Item) {
	if w.sceneTitles == nil {
		return
	}
	byID, err := w.sceneTitles(ctx)
	if err != nil {
		// A *url.Error carries the configured URL, which may embed credentials.
		w.log.Warn("sonarr scene titles not read; search results keep every alias", "error", httpx.LogSafeError(err))
		return
	}
	for i := range items {
		s := byID[items[i].ArrID]
		items[i].SceneTitles, items[i].CanonTitles = s.Titles, s.Canon
	}
}

// maxSceneBody bounds the series list read; a large library's list is a few MB.
const maxSceneBody = 64 << 20

type sonarrSeries struct {
	AlternateTitles []struct {
		Title             string `json:"title"`
		SeasonNumber      *int   `json:"seasonNumber"`
		SceneSeasonNumber *int   `json:"sceneSeasonNumber"`
	} `json:"alternateTitles"`
	Seasons []struct {
		Statistics struct {
			TotalEpisodeCount int `json:"totalEpisodeCount"`
		} `json:"statistics"`
		SeasonNumber int `json:"seasonNumber"`
	} `json:"seasons"`
	Title string `json:"title"`
	ID    int    `json:"id"`
	Year  int    `json:"year"`
}

// SonarrSceneTitles reads the scene seasons straight from Sonarr's series list,
// because arrapi flattens alternateTitles to bare strings and drops the season
// each one is mapped to. The season is the title's TVDB seasonNumber when Sonarr
// names one, else its sceneSeasonNumber (what Sonarr files the release under:
// the Maou Gakuin romaji title carries sceneSeasonNumber 2 only, and Sonarr's
// parse of "<that title> S01" returns season 2 episodes). A title mapped to
// every season (-1, or none) is not a scene title here.
//
// It also asks Sonarr's parser, once per series, season and title, whether
// "<title> (<year>) SNN" is that whole season of that series, and keeps the
// ones it is as canonical titles. The answers are remembered for the life of
// the process: a series' title, year and episode count rarely change, and a
// change makes a new question.
func SonarrSceneTitles(client *http.Client, baseURL, apiKey string) SceneTitleSource {
	base := strings.TrimRight(baseURL, "/") + "/api/v3"
	var mu sync.Mutex
	verdicts := map[string]bool{}
	get := func(ctx context.Context, path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, http.NoBody)
		if err != nil {
			return err
		}
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("sonarr %s: HTTP %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, maxSceneBody)).Decode(out)
	}
	// filesAs reports whether Sonarr parses title as exactly the episodes of
	// season of series id.
	filesAs := func(ctx context.Context, title string, id, season, episodes int) (bool, error) {
		var p struct {
			Series *struct {
				ID int `json:"id"`
			} `json:"series"`
			Episodes []struct {
				SeasonNumber int `json:"seasonNumber"`
			} `json:"episodes"`
		}
		if err := get(ctx, "/parse?title="+url.QueryEscape(title), &p); err != nil {
			return false, err
		}
		if p.Series == nil || p.Series.ID != id || len(p.Episodes) != episodes {
			return false, nil
		}
		for _, e := range p.Episodes {
			if e.SeasonNumber != season {
				return false, nil
			}
		}
		return true, nil
	}
	return func(ctx context.Context) (map[int]SeriesScene, error) {
		var series []sonarrSeries
		if err := get(ctx, "/series", &series); err != nil {
			return nil, fmt.Errorf("sonarr series list: %w", err)
		}
		mu.Lock()
		defer mu.Unlock()
		out := make(map[int]SeriesScene)
		for _, s := range series {
			var sc SeriesScene
			for _, a := range s.AlternateTitles {
				season := a.SceneSeasonNumber
				if a.SeasonNumber != nil {
					season = a.SeasonNumber
				}
				if t := strings.TrimSpace(a.Title); t != "" && season != nil && *season > 0 {
					sc.Titles = append(sc.Titles, library.SceneTitle{Title: t, Season: *season})
				}
			}
			title := strings.TrimSpace(s.Title)
			if s.Year > 0 && !strings.HasSuffix(title, fmt.Sprintf("(%d)", s.Year)) {
				title = fmt.Sprintf("%s (%d)", title, s.Year)
			}
			for _, se := range s.Seasons {
				n := se.Statistics.TotalEpisodeCount
				if se.SeasonNumber <= 0 || n <= 0 || title == "" || ctx.Err() != nil {
					continue
				}
				q := fmt.Sprintf("%s S%02d", title, se.SeasonNumber)
				key := fmt.Sprintf("%d|%d|%s", s.ID, n, q)
				ok, seen := verdicts[key]
				if !seen {
					var err error
					// A failed parse is not remembered, so the next walk asks again.
					if ok, err = filesAs(ctx, q, s.ID, se.SeasonNumber, n); err != nil {
						continue
					}
					verdicts[key] = ok
				}
				if ok {
					if sc.Canon == nil {
						sc.Canon = map[int]string{}
					}
					sc.Canon[se.SeasonNumber] = title
				}
			}
			out[s.ID] = sc
		}
		return out, nil
	}
}
