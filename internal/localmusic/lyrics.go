package localmusic

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const maxCoverFileBytes = 32 << 20 // 32 MiB cap on external folder artwork

var coverExts = []string{".jpg", ".jpeg", ".png", ".webp"}

// saveCoverBytes deduplicates image data by SHA-1 into coverDir and returns its coverID.
func saveCoverBytes(coverDir string, data []byte) (string, error) {
	if len(data) == 0 || coverDir == "" {
		return "", nil
	}
	sum := sha1.Sum(data)
	id := hex.EncodeToString(sum[:])
	dst := filepath.Join(coverDir, id)
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return id, nil
	}
	if err := os.MkdirAll(coverDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(coverDir, ".cover-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		if werr != nil {
			return "", werr
		}
		return "", cerr
	}
	if err := os.Rename(tmp, dst); err != nil {
		// On Windows, another worker hashing the identical cover concurrently may win the rename.
		_ = os.Remove(tmp)
		if st, statErr := os.Stat(dst); statErr == nil && st.Size() > 0 {
			return id, nil
		}
		return "", err
	}
	return id, nil
}

func isDiscSubfolder(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "disc") || strings.HasPrefix(lower, "cd")
}

type sidecarCoverInfo struct {
	path      string
	size      int64
	modTimeNs int64
}

func sameSidecarPath(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return canonicalPathKey(a) == canonicalPathKey(b)
}

type dirCoverEntry struct {
	name      string
	size      int64
	modTimeNs int64
}

type folderCoverResolver struct {
	coverDir  string
	mu        sync.Mutex
	dirFiles  map[string][]dirCoverEntry
	candCache map[string]sidecarCoverInfo
	idCache   map[string]string
}

func newFolderCoverResolver(coverDir string) *folderCoverResolver {
	return &folderCoverResolver{
		coverDir:  coverDir,
		dirFiles:  map[string][]dirCoverEntry{},
		candCache: map[string]sidecarCoverInfo{},
		idCache:   map[string]string{},
	}
}

func (r *folderCoverResolver) listDirCoversLocked(dir string) []dirCoverEntry {
	dk := canonicalPathKey(dir)
	if cached, ok := r.dirFiles[dk]; ok {
		return cached
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		r.dirFiles[dk] = nil
		return nil
	}
	var out []dirCoverEntry
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(ent.Name()))
		isImg := false
		for _, ce := range coverExts {
			if ext == ce {
				isImg = true
				break
			}
		}
		if !isImg {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.Size() <= 0 || info.Size() > maxCoverFileBytes {
			continue
		}
		out = append(out, dirCoverEntry{
			name:      ent.Name(),
			size:      info.Size(),
			modTimeNs: info.ModTime().UnixNano(),
		})
	}
	r.dirFiles[dk] = out
	return out
}

// candidate resolves the external cover image candidate (cover.*, folder.*,
// front.*, album.*, or <albumTitle>.*) for dir (and parent dir when dir matches
// disc*/cd*).
func (r *folderCoverResolver) candidate(dir, albumTitle string) sidecarCoverInfo {
	if r == nil || r.coverDir == "" || dir == "" {
		return sidecarCoverInfo{}
	}
	cacheKey := canonicalPathKey(dir) + "|" + strings.ToLower(strings.TrimSpace(albumTitle))

	r.mu.Lock()
	defer r.mu.Unlock()
	if cand, ok := r.candCache[cacheKey]; ok {
		return cand
	}

	searchDirs := []string{dir}
	if isDiscSubfolder(filepath.Base(dir)) {
		parent := filepath.Dir(dir)
		if parent != "" && parent != dir {
			searchDirs = append(searchDirs, parent)
		}
	}

	candidates := []string{"cover", "folder", "front", "album"}
	cleanAlb := strings.TrimSpace(albumTitle)
	if cleanAlb != "" && !containsFold(candidates, cleanAlb) {
		candidates = append(candidates, cleanAlb)
	}

	var found sidecarCoverInfo
	for _, sdir := range searchDirs {
		files := r.listDirCoversLocked(sdir)
		if len(files) == 0 {
			continue
		}
		for _, base := range candidates {
			for _, ext := range coverExts {
				want := base + ext
				for _, ent := range files {
					if strings.EqualFold(ent.name, want) {
						found = sidecarCoverInfo{
							path:      filepath.Clean(filepath.Join(sdir, ent.name)),
							size:      ent.size,
							modTimeNs: ent.modTimeNs,
						}
						r.candCache[cacheKey] = found
						return found
					}
				}
			}
		}
	}
	r.candCache[cacheKey] = found
	return found
}

func (r *folderCoverResolver) ingest(cand sidecarCoverInfo) string {
	if r == nil || r.coverDir == "" || cand.path == "" {
		return ""
	}
	pk := canonicalPathKey(cand.path)
	r.mu.Lock()
	if id, ok := r.idCache[pk]; ok {
		r.mu.Unlock()
		return id
	}
	r.mu.Unlock()

	data, err := os.ReadFile(cand.path)
	var id string
	if err == nil && len(data) > 0 && len(data) <= maxCoverFileBytes {
		id, _ = saveCoverBytes(r.coverDir, data)
	}

	r.mu.Lock()
	r.idCache[pk] = id
	r.mu.Unlock()
	return id
}

func (r *folderCoverResolver) resolveAndIngest(dir, albumTitle string) (string, sidecarCoverInfo) {
	cand := r.candidate(dir, albumTitle)
	if cand.path == "" {
		return "", sidecarCoverInfo{}
	}
	return r.ingest(cand), cand
}

var (
	lrcTagRe    = regexp.MustCompile(`\[(\d+):(\d+)(?:[.:](\d+))?\]`)
	lrcOffsetRe = regexp.MustCompile(`(?i)\[offset:\s*([-+]?\d+)\s*\]`)
)

type timedLRCLine struct {
	ms    int64
	order int
	text  string
}

// Lyrics loads .lrc or embedded lyrics for trackID, auto-decoding UTF-8 /
// GB18030 / Shift-JIS, clipping and shifting whole-disc CUE .lrc files to
// [ClipStartMs, ClipEndMs), and splitting bilingual duplicate timestamps into
// (lrc, trans).
func (l *Library) Lyrics(trackID string) (lrc, trans string) {
	tr, ok := l.TrackByID(trackID)
	if !ok || tr == nil {
		return "", ""
	}
	return resolveTrackLyrics(tr)
}

func resolveTrackLyrics(tr *LocalTrack) (lrc, trans string) {
	dir := tr.Dir
	if dir == "" && tr.Path != "" {
		dir = filepath.Dir(tr.Path)
	}
	audioBase := strings.TrimSuffix(filepath.Base(tr.Path), filepath.Ext(tr.Path))

	if tr.Spec.IsCUE {
		// 1. Per-track .lrc in the same directory (already relative to 00:00).
		var perTrackBases []string
		if tr.Title != "" {
			perTrackBases = append(perTrackBases, tr.Title)
			if tr.TrackNo > 0 {
				perTrackBases = append(perTrackBases,
					fmt.Sprintf("%02d - %s", tr.TrackNo, tr.Title),
					fmt.Sprintf("%02d. %s", tr.TrackNo, tr.Title),
				)
			}
		}
		for _, b := range perTrackBases {
			if raw, ok := readLRCByBase(dir, b); ok {
				lrc, trans = processLRCText(raw, false, 0, 0)
				if lrc != "" {
					return lrc, trans
				}
			}
		}

		// 2. Whole-disc .lrc (<cueBase>.lrc or <audioBase>.lrc), shifted & clipped.
		type lrcCandidate struct {
			dir  string
			base string
		}
		var wholeDiscCands []lrcCandidate
		addCand := func(cdir, cbase string) {
			if cdir == "" || cbase == "" {
				return
			}
			for _, ex := range wholeDiscCands {
				if canonicalPathKey(ex.dir) == canonicalPathKey(cdir) && strings.EqualFold(ex.base, cbase) {
					return
				}
			}
			wholeDiscCands = append(wholeDiscCands, lrcCandidate{dir: cdir, base: cbase})
		}

		if tr.CuePath != "" {
			cueDir := filepath.Dir(tr.CuePath)
			if cueDir == "" || cueDir == "." {
				cueDir = dir
			}
			cueBase := strings.TrimSuffix(filepath.Base(tr.CuePath), filepath.Ext(tr.CuePath))
			addCand(cueDir, cueBase)
			if tr.Path != "" {
				addCand(filepath.Dir(tr.Path), audioBase)
			}
			addCand(dir, audioBase)
		} else {
			for _, cb := range findCueBasesInDir(dir) {
				addCand(dir, cb)
			}
			addCand(dir, audioBase)
		}
		for _, cand := range wholeDiscCands {
			if raw, ok := readLRCByBase(cand.dir, cand.base); ok {
				lrc, trans = processLRCText(raw, true, tr.Spec.ClipStartMs, tr.Spec.ClipEndMs)
				if lrc != "" {
					return lrc, trans
				}
			}
		}
	} else if audioBase != "" {
		if raw, ok := readLRCByBase(dir, audioBase); ok {
			lrc, trans = processLRCText(raw, false, 0, 0)
			if lrc != "" {
				return lrc, trans
			}
		}
	}

	// Fallback to embedded lyrics.
	if emb := strings.TrimSpace(tr.EmbeddedLyrics); emb != "" {
		return processLRCText(emb, tr.Spec.IsCUE, tr.Spec.ClipStartMs, tr.Spec.ClipEndMs)
	}
	return "", ""
}

func findCueBasesInDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(ent.Name()), ".cue") {
			base := strings.TrimSuffix(ent.Name(), filepath.Ext(ent.Name()))
			if base != "" {
				out = append(out, base)
			}
		}
	}
	return out
}

func readLRCByBase(dir, base string) (string, bool) {
	if dir == "" || base == "" {
		return "", false
	}
	direct := filepath.Join(dir, base+".lrc")
	if b, err := os.ReadFile(direct); err == nil {
		if s := decodeTextAuto(b); s != "" {
			return s, true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	want := base + ".lrc"
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if strings.EqualFold(ent.Name(), want) {
			if b, err := os.ReadFile(filepath.Join(dir, ent.Name())); err == nil {
				if s := decodeTextAuto(b); s != "" {
					return s, true
				}
			}
		}
	}
	return "", false
}

func processLRCText(text string, clip bool, clipStartMs, clipEndMs int64) (string, string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if !lrcTagRe.MatchString(text) {
		// Plain untimed lyrics. Only return for non-clipped tracks or full-range CUE tracks.
		if clip && clipStartMs > 0 {
			return "", ""
		}
		return strings.TrimSpace(text), ""
	}

	var offsetMs int64
	if m := lrcOffsetRe.FindStringSubmatch(text); len(m) == 2 {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			offsetMs = v
		}
	}

	var parsed []timedLRCLine
	order := 0
	for _, raw := range strings.Split(text, "\n") {
		matches := lrcTagRe.FindAllStringSubmatchIndex(raw, -1)
		if len(matches) == 0 {
			continue
		}
		lyricText := strings.TrimSpace(raw[matches[len(matches)-1][1]:])
		for _, m := range matches {
			mm, _ := strconv.Atoi(raw[m[2]:m[3]])
			ss, _ := strconv.Atoi(raw[m[4]:m[5]])
			var fracMs int64
			if m[6] >= 0 {
				f := raw[m[6]:m[7]]
				switch len(f) {
				case 1:
					v, _ := strconv.Atoi(f)
					fracMs = int64(v) * 100
				case 2:
					v, _ := strconv.Atoi(f)
					fracMs = int64(v) * 10
				default:
					v, _ := strconv.Atoi(f[:3])
					fracMs = int64(v)
				}
			}
			atMs := int64(mm*60+ss)*1000 + fracMs - offsetMs
			if atMs < 0 {
				atMs = 0
			}
			if clip {
				if atMs < clipStartMs {
					continue
				}
				if clipEndMs > 0 && atMs >= clipEndMs {
					continue
				}
				atMs -= clipStartMs
			}
			parsed = append(parsed, timedLRCLine{
				ms:    atMs,
				order: order,
				text:  lyricText,
			})
			order++
		}
	}

	if len(parsed) == 0 {
		return "", ""
	}

	sort.SliceStable(parsed, func(i, j int) bool {
		if parsed[i].ms != parsed[j].ms {
			return parsed[i].ms < parsed[j].ms
		}
		return parsed[i].order < parsed[j].order
	})

	var (
		primaryLines []string
		transLines   []string
		i            = 0
	)
	for i < len(parsed) {
		j := i + 1
		for j < len(parsed) && parsed[j].ms == parsed[i].ms {
			j++
		}
		group := parsed[i:j]
		var nonEmpty []string
		for _, g := range group {
			if g.text != "" {
				nonEmpty = append(nonEmpty, g.text)
			}
		}
		tag := formatLRCTimeTag(parsed[i].ms)
		if len(nonEmpty) == 0 {
			primaryLines = append(primaryLines, tag)
		} else {
			primaryLines = append(primaryLines, tag+nonEmpty[0])
			if len(nonEmpty) >= 2 && nonEmpty[1] != nonEmpty[0] {
				transLines = append(transLines, tag+nonEmpty[1])
			}
		}
		i = j
	}

	return strings.Join(primaryLines, "\n"), strings.Join(transLines, "\n")
}

func formatLRCTimeTag(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	mins := ms / 60000
	rem := ms % 60000
	secs := rem / 1000
	millis := rem % 1000
	if millis%10 == 0 {
		return fmt.Sprintf("[%02d:%02d.%02d]", mins, secs, millis/10)
	}
	return fmt.Sprintf("[%02d:%02d.%03d]", mins, secs, millis)
}
