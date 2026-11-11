package webui

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// abTorrentPage reads the torrent id off an AnimeBytes release page
// ("https://animebytes.tv/torrents.php?id=16566&torrentid=1121459").
var abTorrentPage = regexp.MustCompile(`^https://animebytes\.tv/torrents\.php\?(?:.*&)?torrentid=(\d{1,12})(?:&|$)`)

// DirectGrab adds the SeaDex best of a Sonarr season qBittorrent does not
// hold under DirectCategory, for a season no search gets it for: Sonarr's
// scene mapping misreads every title the best is served under (Kyousougiga's
// S01 as specials, Grisaia no Rakuen's S02 as S00E07), so Sonarr rejects it or
// never asks. Sonarr does not watch the category, so the download stays
// untracked; once complete, autoSeeding imports it, its files placed by the
// AniList episode map. Only a best with a run on this season is added, since
// nothing else can place its files. It returns the release added, "" when
// the best is already in qBittorrent.
// ponytail: Sonarr seasons only; Radarr reads film names well enough.
func (l *Library) DirectGrab(ctx context.Context, t Target) (string, error) {
	if l.Qbit == nil || l.DirectCategory == "" {
		return "", errors.New("direct grabs are off")
	}
	if t.Arr != "sonarr" || t.Season < 0 {
		return "", errors.New("only a Sonarr season is grabbed directly")
	}
	f, ok := l.store.findingFor(t)
	if st := f.Str("status"); !ok || (st != "not_downloaded" && st != "better_release") {
		return "", errors.New("the season is not missing its SeaDex best")
	}
	hash := strings.ToLower(f.Str("info_hash"))
	abID := ""
	if m := abTorrentPage.FindStringSubmatch(f.Str("ab_url")); m != nil {
		abID = m[1]
	}
	nyaa, nyaaOK := NyaaTorrentURL(f.Str("nyaa_url"))
	runs := l.runs(ctx)
	run, ok := runs[hash]
	if !ok && abID != "" {
		run, ok = runs["ab:"+abID]
	}
	if !ok || run.Season != t.Season {
		return "", errors.New("the AniList map places no episodes of this best on the season")
	}
	if hash == "" && abID == "" {
		return "", errors.New("the best names no info hash or AnimeBytes torrent to tell whether qBittorrent holds it")
	}
	// One check-and-add at a time, so two searches finishing together add once.
	l.directMu.Lock()
	defer l.directMu.Unlock()
	torrents, err := l.Qbit.Torrents(ctx)
	if err != nil {
		return "", err
	}
	for _, q := range torrents {
		m := abTorrent.FindStringSubmatch(strings.TrimSpace(q.Comment))
		if hash != "" && strings.EqualFold(q.Hash, hash) || abID != "" && m != nil && q.Private && m[1] == abID {
			return "", nil
		}
	}
	// AnimeBytes first: the curation's tracker of choice, and a seed there
	// keeps the ratio.
	link, from := "", ""
	switch {
	case abID != "" && strings.TrimSpace(l.ABPasskey) != "":
		link, from = "https://animebytes.tv/torrent/"+abID+"/download/"+url.PathEscape(strings.TrimSpace(l.ABPasskey)), "AnimeBytes"
	case nyaaOK:
		link, from = nyaa, "Nyaa"
	default:
		return "", errors.New("the best has no download link the scout can build")
	}
	if err := l.Qbit.AddURL(ctx, link, l.DirectCategory); err != nil {
		return "", err
	}
	name := strings.TrimSpace(f.Str("recommended_group") + " " + f.Str("title"))
	l.log.Info("SeaDex best added to qBittorrent directly", "arr_id", t.ID, "season", t.Season,
		"release", name, "tracker", from, "category", l.DirectCategory)
	return name + " (" + from + ")", nil
}
