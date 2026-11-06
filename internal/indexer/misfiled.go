package indexer

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// A tracker lists a release under several title aliases, and Sonarr's scene
// mapping can pin one of them to a season other than the release's own: the
// romaji title of Maou Gakuin no Futekigousha is mapped to season 2, so
// Sonarr files Asakura's S1 pack under that alias as S2 (and its +1 revision
// score outranks the real S2 best). A search result under such an alias is not
// served; the release's other aliases still are, so it stays grabbable for its
// own season. Every owner of the release must agree a title is misfiled
// (misfiledVote), so a batch two seasons share never loses an alias one of
// them is filed under.

// cleanArticles are the words Sonarr's title cleaning drops (CleanSeriesTitle).
var cleanArticles = regexp.MustCompile(`\b(?:a|an|the|and|or|of)\b`)

// cleanTitle reduces a title to Sonarr-comparable form: lower case, articles
// dropped, letters and digits only.
func cleanTitle(s string) string {
	s = cleanArticles.ReplaceAllString(strings.ToLower(s), " ")
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s)
}

// cleanTitles is the sorted, de-duplicated, comma-joined cleaned form of titles,
// "" for none.
func cleanTitles(titles []string) string {
	var out []string
	for _, t := range titles {
		if c := cleanTitle(t); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// misfiledVote folds owners' misfiled sets by intersection: a title is misfiled
// only if every owner says so. An owner with none empties it.
type misfiledVote struct {
	titles string
	seen   bool
}

func (v *misfiledVote) add(titles string) {
	if !v.seen {
		v.titles, v.seen = titles, true
		return
	}
	v.titles = intersectTitles(v.titles, titles)
}

func intersectTitles(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	bs := strings.Split(b, ",")
	var out []string
	for t := range strings.SplitSeq(a, ",") {
		if slices.Contains(bs, t) {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// leadingGroups matches the bracketed group tags a tracker title opens with.
var leadingGroups = regexp.MustCompile(`^\s*(?:[\[(][^\])]*[\])]\s*)+`)

// afterSeries is what Sonarr reads right after a series title: a season or
// episode token, a number (a year or an absolute episode), or nothing.
var afterSeries = regexp.MustCompile(`^(?:s\d|e\d|\d|$)`)

// misfiledTitle reports whether title is served under one of misfiled (cleaned,
// comma-joined): its series part, cleaned, is exactly such a title.
func misfiledTitle(title, misfiled string) bool {
	if misfiled == "" {
		return false
	}
	c := cleanTitle(leadingGroups.ReplaceAllString(title, ""))
	for m := range strings.SplitSeq(misfiled, ",") {
		if rest, ok := strings.CutPrefix(c, m); ok && afterSeries.MatchString(rest) {
			return true
		}
	}
	return false
}
