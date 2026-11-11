package webui

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// abTorrentPage reads the torrent id off an AnimeBytes release page
// ("https://animebytes.tv/torrents.php?id=16566&torrentid=1121459").
var abTorrentPage = regexp.MustCompile(`^https://animebytes\.tv/torrents\.php\?(?:.*&)?torrentid=(\d{1,12})(?:&|$)`)

// DirectGrab adds the SeaDex bests of a Sonarr season qBittorrent does not
// hold under DirectCategory, for a season no search gets them for: Sonarr's
// scene mapping misreads every title the best is served under (Kyousougiga's
// S01 as specials, Grisaia no Rakuen's S02 as S00E07), so Sonarr rejects it or
// never asks. Sonarr does not watch the category, so the download stays
// untracked; once complete, autoSeeding imports it, its files placed by the
// AniList episode map. Only a best with a run on this season is added, since
// nothing else can place its files. A season holds several bests when it is
// specials (Blue Exorcist's two OVAs are S00E11 and S00E12), so every missing
// one is added. It returns the releases added, "" when qBittorrent already
// holds them all, and an error only when none could be added.
// ponytail: Sonarr seasons only; Radarr reads film names well enough.
func (l *Library) DirectGrab(ctx context.Context, t Target) (string, error) {
	if l.Qbit == nil || l.DirectCategory == "" {
		return "", errors.New("direct grabs are off")
	}
	if t.Arr != "sonarr" || t.Season < 0 {
		return "", errors.New("only a Sonarr season is grabbed directly")
	}
	type best struct {
		f          Line
		hash, abID string
	}
	var want []best
	runs := l.runs(ctx)
	for _, f := range l.store.seasonFindings(t) {
		if st := f.Str("status"); st != "not_downloaded" && st != "better_release" {
			continue
		}
		b := best{f: f, hash: strings.ToLower(f.Str("info_hash"))}
		if m := abTorrentPage.FindStringSubmatch(f.Str("ab_url")); m != nil {
			b.abID = m[1]
		}
		run, ok := runs[b.hash]
		if !ok && b.abID != "" {
			run, ok = runs["ab:"+b.abID]
		}
		// No hash or AnimeBytes id: nothing tells whether qBittorrent holds it.
		dup := slices.ContainsFunc(want, func(w best) bool { return b.hash != "" && w.hash == b.hash || b.abID != "" && w.abID == b.abID })
		if ok && run.Season == t.Season && (b.hash != "" || b.abID != "") && !dup {
			want = append(want, b)
		}
	}
	if len(want) == 0 {
		return "", errors.New("the season misses no SeaDex best the AniList map places on it")
	}
	// One check-and-add at a time, so two searches finishing together add once.
	l.directMu.Lock()
	defer l.directMu.Unlock()
	torrents, err := l.Qbit.Torrents(ctx)
	if err != nil {
		return "", err
	}
	var added []string
	var failed error
	for _, b := range want {
		if slices.ContainsFunc(torrents, func(q QbitTorrent) bool {
			m := abTorrent.FindStringSubmatch(strings.TrimSpace(q.Comment))
			return b.hash != "" && strings.EqualFold(q.Hash, b.hash) || b.abID != "" && m != nil && q.Private && m[1] == b.abID
		}) {
			continue
		}
		// AnimeBytes first: the curation's tracker of choice, and a seed there
		// keeps the ratio.
		link, from := "", ""
		nyaa, nyaaOK := NyaaTorrentURL(b.f.Str("nyaa_url"))
		switch {
		case b.abID != "" && strings.TrimSpace(l.ABPasskey) != "":
			link, from = "https://animebytes.tv/torrent/"+b.abID+"/download/"+url.PathEscape(strings.TrimSpace(l.ABPasskey)), "AnimeBytes"
		case nyaaOK:
			link, from = nyaa, "Nyaa"
		default:
			failed = errors.New("the best has no download link the scout can build")
			continue
		}
		if err := l.Qbit.AddURL(ctx, link, l.DirectCategory); err != nil {
			failed = err
			continue
		}
		name := strings.TrimSpace(b.f.Str("recommended_group") + " " + b.f.Str("title"))
		l.log.Info("SeaDex best added to qBittorrent directly", "arr_id", t.ID, "season", t.Season,
			"release", name, "tracker", from, "category", l.DirectCategory)
		added = append(added, name+" ("+from+")")
	}
	// One best not added fails the call only when none was: the caller
	// reports what was added either way.
	if len(added) == 0 && failed != nil {
		return "", failed
	}
	return strings.Join(added, "; "), nil
}
