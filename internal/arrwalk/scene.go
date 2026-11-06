package arrwalk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cplieger/seadex-scout/internal/library"
)

// SceneTitleSource reads, per Sonarr series id, the alternate titles Sonarr's
// scene mapping pins to one season.
type SceneTitleSource func(ctx context.Context) (map[int][]library.SceneTitle, error)

// stampSceneTitles sets each Sonarr item's SceneTitles from the configured
// source. A failed read leaves them nil, which only disables the misfiled-title
// drop in search results; nothing else reads them.
func (w *Walker) stampSceneTitles(ctx context.Context, items []library.Item) {
	if w.sceneTitles == nil {
		return
	}
	byID, err := w.sceneTitles(ctx)
	if err != nil {
		w.log.Warn("sonarr scene titles not read; search results keep every alias", "error", err)
		return
	}
	for i := range items {
		items[i].SceneTitles = byID[items[i].ArrID]
	}
}

// maxSceneBody bounds the series list read; a large library's list is a few MB.
const maxSceneBody = 64 << 20

// SonarrSceneTitles reads the scene seasons straight from Sonarr's series list,
// because arrapi flattens alternateTitles to bare strings and drops the season
// each one is mapped to. The season is the title's TVDB seasonNumber when Sonarr
// names one, else its sceneSeasonNumber (what Sonarr files the release under:
// the Maou Gakuin romaji title carries sceneSeasonNumber 2 only, and Sonarr's
// parse of "<that title> S01" returns season 2 episodes). A title mapped to
// every season (-1, or none) is not a scene title here.
func SonarrSceneTitles(client *http.Client, baseURL, apiKey string) SceneTitleSource {
	return func(ctx context.Context) (map[int][]library.SceneTitle, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v3/series", http.NoBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("sonarr series list: HTTP %d", resp.StatusCode)
		}
		var series []struct {
			AlternateTitles []struct {
				Title             string `json:"title"`
				SeasonNumber      *int   `json:"seasonNumber"`
				SceneSeasonNumber *int   `json:"sceneSeasonNumber"`
			} `json:"alternateTitles"`
			ID int `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxSceneBody)).Decode(&series); err != nil {
			return nil, fmt.Errorf("sonarr series list: %w", err)
		}
		out := make(map[int][]library.SceneTitle)
		for _, s := range series {
			for _, a := range s.AlternateTitles {
				season := a.SceneSeasonNumber
				if a.SeasonNumber != nil {
					season = a.SeasonNumber
				}
				if t := strings.TrimSpace(a.Title); t != "" && season != nil && *season > 0 {
					out[s.ID] = append(out[s.ID], library.SceneTitle{Title: t, Season: *season})
				}
			}
		}
		return out, nil
	}
}
