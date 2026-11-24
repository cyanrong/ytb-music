package app

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"

	"ytb-music/internal/localmusic"
	"ytb-music/internal/ytm"
)

// localCoverLoader loads localcover:// artwork bytes (set by setLocalLibrary
// and replaceable in tests).
var (
	localCoverMu     sync.RWMutex
	localCoverLoader func(url string) ([]byte, error)
)

func setLocalCoverLoader(fn func(url string) ([]byte, error)) {
	localCoverMu.Lock()
	localCoverLoader = fn
	localCoverMu.Unlock()
}

// Covers come from the disk once they came from the network: the home page
// opens with its pictures, without asking YouTube's image servers for them
// again. Those shown longest ago go once the covers take coverCacheMax.

const coverCacheMax = 300 << 20

var coverDir = sync.OnceValue(func() string {
	dir, err := mygo.App.Path(mygo.PathCache)
	if err != nil {
		return ""
	}
	dir = filepath.Join(dir, "covers")
	if os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	go pruneCovers(dir)
	return dir
})

// fetchCover returns the picture at url: kept on disk, or fetched and kept.
func fetchCover(url string) ([]byte, error) {
	if localmusic.IsLocalCover(url) {
		localCoverMu.RLock()
		loader := localCoverLoader
		localCoverMu.RUnlock()
		if loader != nil {
			if b, err := loader(url); err == nil && len(b) > 0 {
				return b, nil
			}
		}
		if cacheDir, err := mygo.App.Path(mygo.PathCache); err == nil && cacheDir != "" {
			id := filepath.Base(strings.TrimSpace(strings.TrimPrefix(url, localmusic.CoverScheme)))
			if id != "" && id != "." && id != ".." {
				if b, err := os.ReadFile(filepath.Join(cacheDir, "local_covers", id)); err == nil && len(b) > 0 {
					return b, nil
				}
			}
		}
		return nil, errors.New("local cover not found")
	}
	dir := coverDir()
	if dir == "" {
		return ytm.Fetch(url)
	}
	sum := sha1.Sum([]byte(url))
	path := filepath.Join(dir, hex.EncodeToString(sum[:]))
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		now := time.Now()
		os.Chtimes(path, now, now) // shown now: the last to go
		return b, nil
	}
	b, err := ytm.Fetch(url)
	if err != nil {
		return nil, err
	}
	go func() {
		if os.WriteFile(path+".part", b, 0o600) == nil {
			os.Rename(path+".part", path)
		}
	}()
	return b, nil
}

// pruneCovers drops the covers shown longest ago until the rest take no
// more than coverCacheMax.
func pruneCovers(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type cover struct {
		path string
		size int64
		used time.Time
	}
	var all []cover
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		all = append(all, cover{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	slices.SortFunc(all, func(a, b cover) int { return a.used.Compare(b.used) })
	for _, c := range all {
		if total <= coverCacheMax {
			return
		}
		os.Remove(c.path)
		total -= c.size
	}
}
