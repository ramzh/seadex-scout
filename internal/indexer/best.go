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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := atomicfile.ReadBoundedFile(ctx, f, maxFeedBytes)
	if err != nil {
		return nil, err
	}
	snap, err := unmarshalSnapshot(data)
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
