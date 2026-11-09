package indexer

import (
	"regexp"
	"strings"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/release"
)

// canonFragment marks the guid of a search result's canonical copy, so it is
// never taken for the original or for a film twin.
const canonFragment = "canon"

var (
	leadingGroup = regexp.MustCompile(`^\s*\[[^\]]+\]\s*`)
	trailingTags = regexp.MustCompile(`(?:\s*\[[^\]]*\])+\s*$`)
	// namedEpisodes is an episode or a number range in a title: "E27-E51",
	// "S01E05", "01-12", "01~12".
	namedEpisodes = regexp.MustCompile(`(?i)(?:\bS\d{1,2})?E\d{1,4}\b|\b\d{1,4}\s*[-~]\s*\d{1,4}\b`)
)

// canonCopy is a series search result again under the title Sonarr's own
// parser files as its season (library.Item.CanonTitles), with its range label,
// group and trailing tags: "[ARC] Golden Boy (1995) S01 [DVD 576p AC3]". The
// original stays, so an alias Sonarr reads well is not lost; the copy only
// covers the aliases it misreads (another series' alias, an unknown title).
// Sonarr grabs one torrent once, whichever copy it ranks first. Every holder
// must agree on the title (the canon vote), and a film, a Radarr request, or a
// result with no served range gets none.
// ponytail: search results only; the RSS journal keeps its own titles.
func canonCopy(it *item, v curationVerdict, class requester) (item, bool) {
	if v.canon == "" || !v.nonFilm || class == requesterMovies || !servedSeason.MatchString(v.rangeLabel) {
		return item{}, false
	}
	canon, rangedOnly := strings.CutPrefix(v.canon, library.CanonRangedOnly)
	if rangedOnly && !strings.Contains(v.rangeLabel, "E") {
		return item{}, false
	}
	// A bare season label beside episodes the title names itself would claim
	// the whole season for part of it (Soul Eater's figged pack, "S01 E27-E51").
	if !strings.Contains(v.rangeLabel, "E") && namedEpisodes.MatchString(it.Title) {
		return item{}, false
	}
	title := leadingGroup.FindString(it.Title) + canon + " " + v.rangeLabel + trailingTags.FindString(it.Title)
	title = arrReadableRevision(strings.TrimSpace(title))
	if v.rev > 1 {
		title = revisedTitle(title, release.Revision{Version: v.rev, Marker: release.RevisionVersion})
	}
	if title == it.Title || it.guid() == "" {
		return item{}, false
	}
	c := *it
	c.Title = title
	c.GUID = it.guid() + "#" + canonFragment
	return c, true
}
