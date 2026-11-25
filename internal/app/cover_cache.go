package app

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

var localThumbDir = sync.OnceValue(func() string {
	dir, err := mygo.App.Path(mygo.PathCache)
	if err != nil || dir == "" {
		return ""
	}
	dir = filepath.Join(dir, "local_cover_thumbs")
	if os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	return dir
})

func parseLocalCoverURL(rawURL string) (id string, bucketPx int) {
	rest := strings.TrimSpace(strings.TrimPrefix(rawURL, localmusic.CoverScheme))
	var query string
	if qIdx := strings.IndexByte(rest, '?'); qIdx >= 0 {
		query = rest[qIdx+1:]
		rest = rest[:qIdx]
	}
	id = filepath.Base(strings.TrimSpace(rest))
	if id == "." || id == ".." || id == string(filepath.Separator) {
		id = ""
	}
	for part := range strings.SplitSeq(query, "&") {
		if after, ok := strings.CutPrefix(part, "w="); ok {
			if px, err := strconv.Atoi(after); err == nil && px > 0 {
				switch {
				case px <= 200:
					bucketPx = 200
				case px <= 440:
					bucketPx = 400
				default:
					bucketPx = 800
				}
			}
		}
	}
	return id, bucketPx
}

func isHexSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func scaleAspectRGBA(img image.Image, tw, th int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := range th {
		y0 := b.Min.Y + y*sh/th
		y1 := max(y0+1, b.Min.Y+(y+1)*sh/th)
		stepY := max(1, (y1-y0)/3)
		for x := range tw {
			x0 := b.Min.X + x*sw/tw
			x1 := max(x0+1, b.Min.X+(x+1)*sw/tw)
			stepX := max(1, (x1-x0)/3)
			var r, g, bl, a, cnt uint32
			for yy := y0; yy < y1; yy += stepY {
				for xx := x0; xx < x1; xx += stepX {
					cr, cg, cb, ca := img.At(xx, yy).RGBA()
					r += cr >> 8
					g += cg >> 8
					bl += cb >> 8
					a += ca >> 8
					cnt++
				}
			}
			out.SetRGBA(x, y, color.RGBA{
				R: uint8(r / cnt),
				G: uint8(g / cnt),
				B: uint8(bl / cnt),
				A: uint8(a / cnt),
			})
		}
	}
	return out
}

// resizeLocalCover downscales multi-megabyte audiophile cover scans into
// compact thumbnails (~25-40 KB) so large album walls stay resident in artCache
// without eviction or reload flicker.
func resizeLocalCover(raw []byte, maxDim int) []byte {
	if maxDim <= 0 || len(raw) == 0 {
		return raw
	}
	still := stillCover(raw)
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(still)); err == nil {
		if cfg.Width <= maxDim && cfg.Height <= maxDim && len(still) <= 96<<10 {
			return still
		}
	}
	img, _, err := image.Decode(bytes.NewReader(still))
	if err != nil {
		return raw
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return raw
	}
	tw, th := w, h
	if w > maxDim || h > maxDim {
		if w >= h {
			tw = maxDim
			th = max(1, int(int64(h)*int64(maxDim)/int64(w)))
		} else {
			th = maxDim
			tw = max(1, int(int64(w)*int64(maxDim)/int64(h)))
		}
	}
	scaled := scaleAspectRGBA(img, tw, th)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, scaled, &jpeg.Options{Quality: 90}); err != nil {
		return raw
	}
	if out.Len() >= len(raw) && w <= maxDim && h <= maxDim {
		return raw
	}
	return out.Bytes()
}

// fetchCover returns the picture at url: kept on disk, or fetched and kept.
func fetchCover(url string) ([]byte, error) {
	if localmusic.IsLocalCover(url) {
		id, bucketPx := parseLocalCoverURL(url)
		if id == "" {
			return nil, errors.New("local cover not found")
		}
		var thumbPath string
		if bucketPx > 0 && isHexSHA1(id) {
			if tdir := localThumbDir(); tdir != "" {
				thumbPath = filepath.Join(tdir, fmt.Sprintf("%s_%d.jpg", id, bucketPx))
				if b, err := os.ReadFile(thumbPath); err == nil && len(b) > 0 {
					return b, nil
				}
			}
		}

		var raw []byte
		localCoverMu.RLock()
		loader := localCoverLoader
		localCoverMu.RUnlock()
		if loader != nil {
			if b, err := loader(localmusic.CoverScheme + id); err == nil && len(b) > 0 {
				raw = b
			}
		}
		if len(raw) == 0 {
			if cacheDir, err := mygo.App.Path(mygo.PathCache); err == nil && cacheDir != "" {
				if b, err := os.ReadFile(filepath.Join(cacheDir, "local_covers", id)); err == nil && len(b) > 0 {
					raw = b
				}
			}
		}
		if len(raw) == 0 {
			return nil, errors.New("local cover not found")
		}
		if bucketPx <= 0 {
			return raw, nil
		}
		resized := resizeLocalCover(raw, bucketPx)
		if thumbPath != "" && len(resized) > 0 && &resized[0] != &raw[0] {
			go func(dst string, data []byte) {
				if tmp, err := os.CreateTemp(filepath.Dir(dst), ".thumb-*.jpg"); err == nil {
					tmpName := tmp.Name()
					_, werr := tmp.Write(data)
					cerr := tmp.Close()
					if werr == nil && cerr == nil {
						_ = os.Rename(tmpName, dst)
					} else {
						_ = os.Remove(tmpName)
					}
				}
			}(thumbPath, resized)
		}
		return resized, nil
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
