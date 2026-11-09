package webui

import (
	"strings"
	"testing"
	"time"
)

// A monitored season whose best is not downloaded is searched once; the next
// tick leaves it alone until the best changes.
func TestAutoBacklogQueuesEachMissingBestOnce(t *testing.T) {
	missing := finding("not_downloaded", "escanor", 1)
	missing.Attrs["release_url"] = "https://nyaa.si/view/1"
	missing.Attrs["title"] = "+Tic Elder Sister"
	held := finding("at_best", "nan0", 2)
	lib := newTestLibrary(t, &fakeSonarr{}, missing, held)
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 0 {
		t.Fatalf("searched before the monitored flags were read: %+v", s)
	}
	lib.store.monitored.At = time.Now()
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 1 || s[0].Target != (Target{Arr: "sonarr", ID: 7, Season: 1}) {
		t.Fatalf("searches = %+v, want one for season 1", s)
	}
	lib.ClearSearches()
	lib.CancelSearches()
	lib.searches = nil
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 0 {
		t.Fatalf("searched again with the same best: %+v", s)
	}
	lib.store.s.Findings[0].Attrs["release_url"] = "https://nyaa.si/view/2"
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 1 {
		t.Fatalf("a new best was not searched: %+v", s)
	}
}

func TestAutoBacklogSkipsUnmonitored(t *testing.T) {
	lib := newTestLibrary(t, &fakeSonarr{}, finding("not_downloaded", "escanor", 1))
	lib.store.monitored = Monitored{At: time.Now(), Unmonitored: []string{"sonarr:7:1"}}
	lib.autoBacklog()
	if s := lib.Searches(); len(s) != 0 {
		t.Fatalf("searched an unmonitored season: %+v", s)
	}
}

// The reasons come from the releases that reach the minimum score only.
func TestWhyNothingNamesTheSeaDexRejections(t *testing.T) {
	rej := func(msg string) map[string]any {
		return map[string]any{"rejections": []map[string]string{{"reason": "x", "message": msg}}}
	}
	fake := &fakeSonarr{rels: []map[string]any{
		{"customFormatScore": 5000, "decision": rej("Not an upgrade")},
		{"customFormatScore": 5000, "decision": rej("Not an upgrade")},
		{"customFormatScore": 5000, "decision": rej("Matches another series")},
		{"customFormatScore": 0, "decision": rej("Below minimum score")},
	}}
	lib := newTestLibrary(t, fake)
	got := lib.whyNothing(t.Context(), lib.sonarr, Target{Arr: "sonarr", ID: 7, Season: 1})
	if !strings.HasPrefix(got, "3 SeaDex releases rejected: Not an upgrade (2); Matches another series (1)") {
		t.Fatalf("why = %q", got)
	}
	fake.rels = fake.rels[3:]
	if got := lib.whyNothing(t.Context(), lib.sonarr, Target{Arr: "sonarr", ID: 7, Season: 1}); !strings.HasPrefix(got, "no release reaching score 5000 among 1") {
		t.Fatalf("why = %q", got)
	}
}
