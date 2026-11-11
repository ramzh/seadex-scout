package indexer

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/cplieger/atomicfile/v4"
)

// BestReleases reads the persisted feed at path and returns every release the
// curation marks as a SeaDex best, keyed by its Nyaa info hash (lower case)
// and by its tracker key ("ab:<torrent id>", "nyaa:<id>"), each with the TVDB
// ids of the entries that call it best (0 for an entry with none). The
// dashboard uses it to prove a library file came from a best release.
func BestReleases(ctx context.Context, path string) (map[string][]int, error) {
	snap, err := readSnapshot(ctx, path)
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	add := func(k string, tvdb int) {
		if k != "" && !slices.Contains(out[k], tvdb) {
			out[k] = append(out[k], tvdb)
		}
	}
	for _, rels := range snap.Owners {
		for _, r := range rels {
			if r.IsBest {
				add(strings.ToLower(r.Hash), r.TvdbID)
				add(r.Key, r.TvdbID)
			}
		}
	}
	return out, nil
}

// BestRuns reads the persisted feed at path and returns the episode run
// (ownedRelease.Run) of every best release, keyed like BestReleases. A
// release whose owners give different runs (a torrent two entries share) has
// none: its files cannot be placed by one entry's episodes.
func BestRuns(ctx context.Context, path string) (map[string]string, error) {
	snap, err := readSnapshot(ctx, path)
	if err != nil {
		return nil, err
	}
	out, split := map[string]string{}, map[string]bool{}
	add := func(k, run string) {
		if k == "" || split[k] {
			return
		}
		if have, ok := out[k]; ok && have != run {
			delete(out, k)
			split[k] = true
			return
		}
		out[k] = run
	}
	for _, rels := range snap.Owners {
		for _, r := range rels {
			if r.IsBest {
				add(strings.ToLower(r.Hash), r.Run)
				add(r.Key, r.Run)
			}
		}
	}
	for k, run := range out {
		if run == "" {
			delete(out, k)
		}
	}
	return out, nil
}

func readSnapshot(ctx context.Context, path string) (snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return snapshot{}, err
	}
	defer f.Close()
	data, err := atomicfile.ReadBoundedFile(ctx, f, maxFeedBytes)
	if err != nil {
		return snapshot{}, err
	}
	return unmarshalSnapshot(data)
}
