package webui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Leftover torrents: downloads none of whose files are in the library any
// more (no file shares an inode with a file under a Sonarr or Radarr root
// folder). They only take space. The scout reads the paths qBittorrent and
// the arrs report, so it needs the same data mount they have (read-only is
// enough to list; deleting goes through qBittorrent).

// LeftoverTorrent is one torrent the leftover scan judged.
type LeftoverTorrent struct {
	Hash     string `json:"hash"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Size     int64  `json:"size"`
	AddedOn  int64  `json:"added_on"`
	Files    int    `json:"files"`
	Missing  int    `json:"missing"` // files not on disk
	// Keep says why the page offers no delete: "private tracker", "shares
	// files with another torrent", "files missing". Empty means it is safe to
	// delete with its data.
	Keep string `json:"keep,omitempty"`
}

// LeftoverScan is the newest leftover scan.
type LeftoverScan struct {
	Running    bool              `json:"running"`
	FinishedAt time.Time         `json:"finished_at,omitzero"`
	Error      string            `json:"error,omitempty"`
	Checked    int               `json:"checked"`
	Torrents   []LeftoverTorrent `json:"torrents"`
}

// Leftovers finds and removes leftover torrents.
type Leftovers struct {
	lib        *Library
	qbit       *Qbit
	categories []string

	mu   sync.Mutex
	scan LeftoverScan
}

// NewLeftovers returns the leftover view for torrents in the categories, or
// nil without qBittorrent.
func NewLeftovers(lib *Library, q *Qbit, categories []string) *Leftovers {
	if q == nil || lib == nil {
		return nil
	}
	return &Leftovers{lib: lib, qbit: q, categories: categories}
}

// Scan returns the newest scan.
func (o *Leftovers) Scan() LeftoverScan {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.scan
	s.Torrents = slices.Clone(s.Torrents)
	return s
}

// Start runs a scan in the background.
func (o *Leftovers) Start(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.scan.Running {
		return errors.New("a leftover scan is already running")
	}
	o.scan.Running = true
	go func() {
		out, checked, err := o.find(ctx, "")
		o.mu.Lock()
		defer o.mu.Unlock()
		o.scan = LeftoverScan{FinishedAt: time.Now().UTC(), Checked: checked, Torrents: out}
		if err != nil {
			o.scan.Error = err.Error()
		}
		o.lib.log.Info("leftover torrent scan complete", "checked", checked, "leftovers", len(out), "error", o.scan.Error)
	}()
	return nil
}

type inode struct{ dev, ino uint64 }

func inodeOf(fi fs.FileInfo) (inode, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return inode{}, false
	}
	return inode{uint64(st.Dev), st.Ino}, true
}

// libraryInodes walks every Sonarr and Radarr root folder.
func (o *Leftovers) libraryInodes(ctx context.Context) (map[inode]bool, error) {
	seen := map[inode]bool{}
	roots := 0
	for _, a := range []*arrAPI{o.lib.sonarr, o.lib.radarr} {
		if a == nil {
			continue
		}
		var folders []struct {
			Path string `json:"path"`
		}
		if err := a.call(ctx, http.MethodGet, "/rootfolder", nil, &folders); err != nil {
			return nil, fmt.Errorf("%s root folders: %w", a.name, err)
		}
		for _, f := range folders {
			if _, err := os.Stat(f.Path); err != nil {
				return nil, fmt.Errorf("the scout cannot read %s's root folder %s; mount the data folder at the same path the arrs use", a.name, f.Path)
			}
			roots++
			err := filepath.WalkDir(f.Path, func(_ string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !d.Type().IsRegular() {
					return ctx.Err()
				}
				if fi, err := d.Info(); err == nil {
					if n, ok := inodeOf(fi); ok {
						seen[n] = true
					}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	if roots == 0 {
		return nil, errors.New("no root folder to compare against")
	}
	return seen, nil
}

// find lists the leftover torrents (only the one with hash when it is set),
// and how many torrents it checked.
func (o *Leftovers) find(ctx context.Context, hash string) ([]LeftoverTorrent, int, error) {
	lib, err := o.libraryInodes(ctx)
	if err != nil {
		return nil, 0, err
	}
	torrents, err := o.qbit.Torrents(ctx)
	if err != nil {
		return nil, 0, err
	}
	shared := map[string]int{}
	for _, t := range torrents {
		shared[t.ContentPath]++
	}
	var out []LeftoverTorrent
	checked := 0
	for _, t := range torrents {
		if ctx.Err() != nil {
			return nil, checked, ctx.Err()
		}
		if (hash != "" && !strings.EqualFold(t.Hash, hash)) || !slices.Contains(o.categories, t.Category) || t.Progress < 1 {
			continue
		}
		checked++
		files, err := o.qbit.Files(ctx, t.Hash)
		if err != nil {
			return nil, checked, err
		}
		lt := LeftoverTorrent{Hash: t.Hash, Name: t.Name, Category: t.Category, Size: t.Size, AddedOn: t.AddedOn, Files: len(files)}
		inLibrary := false
		for _, f := range files {
			fi, err := os.Stat(filepath.Join(t.SavePath, f.Name))
			if err != nil {
				lt.Missing++
				continue
			}
			if n, ok := inodeOf(fi); ok && lib[n] {
				inLibrary = true
				break
			}
		}
		if inLibrary || len(files) == 0 {
			continue
		}
		switch {
		case t.Private || strings.Contains(t.Tracker, "animebytes") || strings.Contains(t.Comment, "animebytes"):
			lt.Keep = "private tracker"
		case shared[t.ContentPath] > 1:
			lt.Keep = "shares files with another torrent"
		case lt.Missing > 0:
			lt.Keep = "files missing"
		}
		out = append(out, lt)
	}
	slices.SortFunc(out, func(x, y LeftoverTorrent) int { return cmp.Compare(y.Size, x.Size) })
	return out, checked, nil
}

// Delete removes a leftover torrent with its data, after checking again that
// it is still a leftover the page may delete.
func (o *Leftovers) Delete(ctx context.Context, hash string) (LeftoverTorrent, error) {
	if hash == "" {
		return LeftoverTorrent{}, errors.New("no torrent named")
	}
	found, _, err := o.find(ctx, hash)
	if err != nil {
		return LeftoverTorrent{}, err
	}
	if len(found) != 1 {
		return LeftoverTorrent{}, errors.New("that torrent is not a leftover (now in the library, gone, or not finished)")
	}
	if found[0].Keep != "" {
		return found[0], fmt.Errorf("that torrent is kept: %s", found[0].Keep)
	}
	if err := o.qbit.Delete(ctx, found[0].Hash, true); err != nil {
		return found[0], err
	}
	o.mu.Lock()
	o.scan.Torrents = slices.DeleteFunc(o.scan.Torrents, func(t LeftoverTorrent) bool { return t.Hash == found[0].Hash })
	o.mu.Unlock()
	return found[0], nil
}
