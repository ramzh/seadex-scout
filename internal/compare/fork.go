package compare

import (
	"path"
	"regexp"
	"slices"
	"strconv"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// This file holds the fork's Extended mode (Config.Extended), which reports
// every entry the built-in dashboard lists rather than only the exceptions
// upstream alerts on. Without Extended, Compare behaves exactly as upstream,
// apart from the headline alt on upgrades (alt.go).

// No-best reasons for Finding.NoBestReason.
const (
	noBestTheoretical  = "theoretical"
	noBestIncomplete   = "incomplete"
	noBestUnobtainable = "unobtainable"
)

// extendedRow reports an outcome upstream stays silent on: no file on disk
// (StatusNotDownloaded) or already at SeaDex's best (StatusAtBest). A withheld
// comparison, and a no-file item whose walk failed, stay silent.
func (c *Comparer) extendedRow(m *match.Match, d *align.Decision, recommended []candidate, recGroups []string) *Finding {
	if !c.extended || d.Outcome == align.OutcomeWithheld {
		return nil
	}
	if d.Outcome == align.OutcomeNoFile && (m.Item == nil || m.Item.Failed) {
		return nil
	}
	base := baseFinding(m, d)
	_, base.AltGroups = c.listedGroups(&m.Entry)
	base.EntryIncomplete = m.Entry.Incomplete
	if len(recommended) > 0 {
		fillBest(&base, recommended, recGroups)
	} else {
		base.NoBestReason = noBestReason(&m.Entry)
	}
	status := StatusAtBest
	if d.Outcome == align.OutcomeNoFile {
		status = StatusNotDownloaded
	}
	return c.enrich(m, d, recommended, finalize(&base, status))
}

// noBestRow replaces emptyResult in Extended mode: an entry with no best the
// operator can obtain is still reported, so its alt shows, and it is marked as
// held at an alt when every group on disk is one SeaDex lists as an alt.
func noBestRow(entry *seadex.Entry, base *Finding, d *align.Decision) *Finding {
	base.EntryIncomplete = entry.Incomplete
	base.NoBestReason = noBestReason(entry)
	status := StatusNoBest
	switch base.NoBestReason {
	case noBestTheoretical:
		status = StatusTheoretical
	case noBestIncomplete:
		status = StatusIncomplete
	}
	if heldAlt(d.Groups, base.AltGroups) {
		base.Tier = TierAlt
	}
	return finalize(base, status)
}

func noBestReason(entry *seadex.Entry) string {
	switch classify.Fallback(entry) {
	case classify.FallbackTheoretical:
		return noBestTheoretical
	case classify.FallbackIncomplete:
		return noBestIncomplete
	default:
		return noBestUnobtainable
	}
}

// heldAlt reports whether the unit holds files and every group of them is
// one of the entry's alt groups.
func heldAlt(held, alt []string) bool {
	if len(held) == 0 || len(alt) == 0 {
		return false
	}
	for _, g := range held {
		if !slices.Contains(alt, release.NormalizeGroup(g)) {
			return false
		}
	}
	return true
}

// keepUpgrade undoes upstream's downgrade of an upgrade to StatusIncomplete
// for an incomplete entry in Extended mode: the best release is still the one
// to get, so the row stays an upgrade and EntryIncomplete carries the flag.
func (c *Comparer) keepUpgrade(entry *seadex.Entry, f *Finding, upgrade Status) *Finding {
	if !c.extended || f == nil {
		return f
	}
	f.EntryIncomplete = entry.Incomplete
	if f.Status == StatusIncomplete && entry.Incomplete {
		f.Status = upgrade
	}
	return f
}

// enrich adds what the dashboard shows beside a row. Without Extended it is
// fillAlt alone. In Extended mode it adds the size on disk, the best release's
// sizes when pool is given and they are not set yet, and the headline alt on
// every row.
func (c *Comparer) enrich(m *match.Match, d *align.Decision, pool []candidate, f *Finding) *Finding {
	if f == nil {
		return nil
	}
	if !c.extended {
		return c.fillAlt(m, d, f)
	}
	f.LibraryBytes = libraryBytes(m.Item, d)
	f.Uncovered, f.CoverageKnown = uncoveredFiles(m, d, c.recommended(&m.Entry))
	if m.Item != nil {
		f.ArrID = m.Item.ArrID
	}
	f.Notes = m.Entry.Notes
	if f.ReleaseBytes == 0 && len(pool) > 0 {
		if set, ok := downloadSet(pool); ok {
			if total, ok := downloadBytes(set); ok {
				f.ReleaseBytes = total
				if current, ok := replacedBytes(m, d, set); ok {
					f.CurrentBytes = current
				}
			}
		}
	}
	f = c.addAlt(m, d, f)
	f.BestOptions = c.releaseOptions(m, d, c.recommended(&m.Entry),
		optionKey{release.NormalizeGroup(f.RecommendedGroup), f.Kind, f.Resolution, f.DualAudio})
	f.AltOptions = c.releaseOptions(m, d, c.alternatives(&m.Entry),
		optionKey{release.NormalizeGroup(f.Alt.Group), f.Alt.Kind, f.Alt.Resolution, f.Alt.DualAudio})
	return f
}

// libraryBytes is the size of the files the decided unit holds: a movie's
// file, a season's files, the files of the episodes an episodes scope names,
// or every file of a whole series. It is zero when nothing is held or any
// size is unknown.
func libraryBytes(item *library.Item, d *align.Decision) int64 {
	if item == nil {
		return 0
	}
	files := map[int]bool{}
	switch d.Kind {
	case align.ScopeMovie, align.ScopeWholeSeries:
		for id := range item.FileBytes {
			files[id] = true
		}
	case align.ScopeSeason:
		for _, ep := range item.SeasonEpisodes[d.Season] {
			files[ep.File] = true
		}
	case align.ScopeEpisodes:
		for _, e := range d.Episodes {
			if ep, ok := item.SeasonEpisodes[d.Season][e]; ok {
				files[ep.File] = true
			}
		}
	default:
		return 0
	}
	var total int64
	for id := range files {
		size := item.FileBytes[id]
		if id <= 0 || size <= 0 {
			return 0
		}
		sum, ok := addBytes(total, size)
		if !ok {
			return 0
		}
		total = sum
	}
	return total
}

// Episode names payload.Span does not read, for sizing and coverage only:
// "S01.E05" (a separator between season and episode), a bare "Ep05", "Ep 05"
// or "Episode 05", and a number right before the bracketed tags, "Black
// Clover 017 (BD 1080p ...)" (the indexer's bareNumber). The shared parser is
// left alone because the indexer reads releases with it too.
var (
	separatedEpisode = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])S(\d{1,2})[ ._]E(\d{1,4})(?:v\d+)?(?:[^a-z0-9]|$)`)
	bareEpisode      = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])Ep(?:isode)?[ ._]?(\d{1,4})(?:v\d+)?(?:[^a-z0-9]|$)`)
	bareTagged       = regexp.MustCompile(`\s(\d{2,4})(?:v\d+)?\s*[\[(]`)
)

// episodeSpans is payload.Spans with a fallback for the names above. A bare
// "Ep05" reads as absolute episode 5, or as episode 5 of season when
// relative is set.
func episodeSpans(files []seadex.File, season int, relative bool) ([]payload.EpisodeSpan, bool) {
	if spans, ok := payload.Spans(files); ok {
		return spans, true
	}
	if len(files) == 0 {
		return nil, false
	}
	spans := make([]payload.EpisodeSpan, 0, len(files))
	for i := range files {
		if span, ok := payload.Span(payload.StripExt(path.Base(files[i].Name))); ok {
			spans = append(spans, span)
			continue
		}
		base := payload.StripExt(path.Base(files[i].Name))
		if m := lastMatch(separatedEpisode, base); m != nil {
			s, _ := strconv.Atoi(m[1])
			e, _ := strconv.Atoi(m[2])
			spans = append(spans, payload.EpisodeSpan{Season: s, First: e, Last: e})
			continue
		}
		m := lastMatch(bareEpisode, base)
		if m == nil {
			m = lastMatch(bareTagged, base)
		}
		if m != nil {
			e, _ := strconv.Atoi(m[1])
			s := payload.AbsoluteSeason
			if relative {
				s = season
			}
			spans = append(spans, payload.EpisodeSpan{Season: s, First: e, Last: e})
			continue
		}
		return nil, false
	}
	return spans, true
}

// lastMatch is the submatches of re's last match in s, or nil: an episode
// marker follows the title, which may hold a look-alike.
func lastMatch(re *regexp.Regexp, s string) []string {
	all := re.FindAllStringSubmatch(s, -1)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// settleUnverifiable decides, in Extended mode, a comparison upstream leaves
// undecided because unknown group evidence on one side could hide a match:
// most often a SeaDex release with no group (NOGRP). Groups the operator
// holds that SeaDex names are still evidence: when every held group is a
// named best group, the unit is at SeaDex's best, and when every one is a
// named alt group, it is held at an alt, an optional upgrade like any other.
// An untagged unit on the operator's side stays undecided.
func (c *Comparer) settleUnverifiable(d *align.Decision, recGroups []string, f *Finding) *Finding {
	if !c.extended || f == nil {
		return f
	}
	if heldNamed(d.Groups, recGroups) {
		f.Status = StatusAtBest
		return f
	}
	if heldNamed(d.Groups, f.AltGroups) {
		f.Tier = TierAlt
	}
	return f
}

// heldNamed reports whether the unit holds files and every group of them is
// one of groups, by name: an untagged group (NOGRP) on either side matches
// nothing, since it names no release.
func heldNamed(held, groups []string) bool {
	if len(held) == 0 {
		return false
	}
	nogrp := release.NormalizeGroup(release.NoGroup)
	for _, g := range held {
		n := release.NormalizeGroup(g)
		if n == nogrp || !slices.Contains(groups, n) {
			return false
		}
	}
	return true
}
