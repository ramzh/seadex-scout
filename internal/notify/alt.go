package notify

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/release"
)

// altKVs renders an upgrade's headline alt release for the built-in
// dashboard: its group, its links split the way the best release's are, and
// its sizes, each only when known. Nothing is rendered when the entry lists
// no obtainable alt. These attributes are diagnostic, outside the log
// contract the alert rules read.
func altKVs(f *compare.Finding) []any {
	a := &f.Alt
	if a.Group == "" {
		return nil
	}
	publicLink, abLink := trackerURLs(a.Links)
	kvs := []any{
		"alt_group", capAttr(a.Group),
		"alt_seadex_tags", altTags(a),
		"alt_nyaa_url", capURLAttr(publicLink.nyaaURL()),
		"alt_public_url", capURLAttr(publicLink.otherURL()),
		"alt_public_tracker", capAlertTextAttr(publicLink.otherTracker()),
		"alt_ab_url", capURLAttr(abLink.url),
		"alt_ab_tracker", capAlertTextAttr(abLink.abTracker()),
	}
	if a.ReleaseBytes > 0 {
		kvs = append(kvs, "alt_recommended_bytes", a.ReleaseBytes)
		if a.CurrentBytes > 0 {
			kvs = append(kvs, "alt_size_change_bytes", a.ReleaseBytes-a.CurrentBytes)
		}
	}
	return kvs
}

// forkKVs is what this fork adds to every finding line for the dashboard:
// the sizes on a row upstream does not size, the size on disk, the entry's
// incomplete flag, why no best is named, and the headline alt.
func forkKVs(f *compare.Finding) []any {
	var kvs []any
	if !isUpgrade(f) {
		kvs = append(kvs, sizeKVs(f)...)
	}
	if f.ArrID > 0 {
		kvs = append(kvs, "arr_id", f.ArrID)
	}
	if f.LibraryBytes > 0 {
		kvs = append(kvs, "library_bytes", f.LibraryBytes)
	}
	if f.CoverageKnown {
		ids := make([]string, len(f.Uncovered))
		for i, id := range f.Uncovered {
			ids[i] = strconv.Itoa(id)
		}
		// Empty when the best covers every file; absent when unknown.
		kvs = append(kvs, "uncovered_files", strings.Join(ids, ","))
	}
	if f.EntryIncomplete {
		kvs = append(kvs, "entry_incomplete", true)
	}
	if f.NoBestReason != "" {
		kvs = append(kvs, "no_best_reason", f.NoBestReason)
	}
	if notes := strings.TrimSpace(f.Notes); notes != "" {
		kvs = append(kvs, "seadex_notes", reboundTo(capAttr(notes), maxNotesBytes))
	}
	kvs = append(kvs, altKVs(f)...)
	if opts := optionsAttr(f.BestOptions, "best"); opts != "" {
		kvs = append(kvs, "best_options", opts)
	}
	if opts := optionsAttr(f.AltOptions, "alt"); opts != "" {
		kvs = append(kvs, "alt_options", opts)
	}
	return kvs
}

// optionJSON is one release in best_options or alt_options, as the
// dashboard reads it.
type optionJSON struct {
	Group         string `json:"group"`
	Tags          string `json:"tags"`
	NyaaURL       string `json:"nyaa_url,omitempty"`
	PublicURL     string `json:"public_url,omitempty"`
	PublicTracker string `json:"public_tracker,omitempty"`
	ABURL         string `json:"ab_url,omitempty"`
	ABTracker     string `json:"ab_tracker,omitempty"`
	Bytes         int64  `json:"bytes,omitempty"`
	Change        *int64 `json:"change,omitempty"`
	Headline      bool   `json:"headline,omitempty"`
}

// optionsAttr renders a finding's releases as a JSON list for the dashboard,
// or "" when there is nothing past the headline the row already shows.
func optionsAttr(opts []compare.ReleaseOption, rung string) string {
	extra := false
	for i := range opts {
		extra = extra || !opts[i].Headline
	}
	if !extra {
		return ""
	}
	out := make([]optionJSON, 0, len(opts))
	for i := range opts {
		o := &opts[i]
		publicLink, abLink := trackerURLs(o.Links)
		j := optionJSON{
			Group: capAttr(o.Group), Tags: releaseTags(rung, o),
			NyaaURL: capURLAttr(publicLink.nyaaURL()), PublicURL: capURLAttr(publicLink.otherURL()),
			PublicTracker: capAlertTextAttr(publicLink.otherTracker()),
			ABURL:         capURLAttr(abLink.url), ABTracker: capAlertTextAttr(abLink.abTracker()),
			Headline: o.Headline,
		}
		if o.ReleaseBytes > 0 {
			j.Bytes = o.ReleaseBytes
			if o.CurrentBytes > 0 {
				change := o.ReleaseBytes - o.CurrentBytes
				j.Change = &change
			}
		}
		out = append(out, j)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(raw)
}

// releaseTags is an option's info line: its rung, kind, resolution and
// dual audio, as altTags renders the headline alt's.
func releaseTags(rung string, a *compare.AltRelease) string {
	tags := []string{rung}
	if a.Kind != "" && a.Kind != string(release.KindUnknown) {
		tags = append(tags, a.Kind)
	}
	if a.Resolution != "" {
		tags = append(tags, a.Resolution)
	}
	if a.DualAudio {
		tags = append(tags, "dual-audio")
	}
	return strings.Join(tags, " · ")
}

// maxNotesBytes bounds the SeaDex notes a finding line carries: they ride
// every line of every check, and the dashboard only shows them on hover.
const maxNotesBytes = 1500

// altTags is the alt's counterpart of seadexTags: "alt", then its kind,
// resolution and dual audio.
func altTags(a *compare.AltRelease) string { return releaseTags("alt", a) }

// boundRetainedAlt is boundRetained for the alt release's and the release
// options' untrusted strings.
func boundRetainedAlt(f *compare.Finding) {
	for _, opts := range []*[]compare.ReleaseOption{&f.BestOptions, &f.AltOptions} {
		*opts = capRetainedList(*opts)
		for i := range *opts {
			o := &(*opts)[i]
			o.Group = capAttr(o.Group)
			o.Links = capRetainedList(o.Links)
			for j := range o.Links {
				o.Links[j].Tracker = capRetainedElem(o.Links[j].Tracker)
				o.Links[j].URL = capRetainedElem(o.Links[j].URL)
			}
		}
	}
	f.Notes = reboundTo(capAttr(f.Notes), maxNotesBytes)
	f.Alt.Group = capAttr(f.Alt.Group)
	f.Alt.Links = capRetainedList(f.Alt.Links)
	for i := range f.Alt.Links {
		f.Alt.Links[i].Tracker = capRetainedElem(f.Alt.Links[i].Tracker)
		f.Alt.Links[i].URL = capRetainedElem(f.Alt.Links[i].URL)
	}
}
