package localmusic

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ytb-music/internal/ytm"
)

const indexFormatVersion = 1

var supportedAudioExts = map[string]bool{
	".flac": true,
	".wav":  true,
	".aif":  true,
	".aiff": true,
	".aifc": true,
	".dsf":  true,
	".dff":  true,
	".m4a":  true,
	".mp4":  true,
	".mp3":  true,
	".ogg":  true,
	".wv":   true,
}

// onFileParsedHook is an optional test hook invoked when Scan actually parses
// an audio file from disk (rather than reusing its cached entry).
var onFileParsedHook func(path string)

// renameIndexFile is swapped in tests to simulate atomic rename failures.
var renameIndexFile = os.Rename

type cachedFile struct {
	Path          string     `json:"path"`
	Size          int64      `json:"size"`
	ModTimeNs     int64      `json:"modTimeNs"`
	AddedAt       int64      `json:"addedAt"`
	CoverTracked  bool       `json:"coverTracked,omitempty"`
	EmbeddedCover bool       `json:"embeddedCover,omitempty"`
	SidecarCover  string     `json:"sidecarCover,omitempty"`
	SidecarModNs  int64      `json:"sidecarModNs,omitempty"`
	SidecarSize   int64      `json:"sidecarSize,omitempty"`
	Track         LocalTrack `json:"track"`
}

type cachedCueFileRef struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	ModTimeNs int64  `json:"modTimeNs"`
}

type cachedCueTrackCover struct {
	EmbeddedCover bool   `json:"embeddedCover,omitempty"`
	AlbumTitle    string `json:"albumTitle,omitempty"`
	SidecarCover  string `json:"sidecarCover,omitempty"`
	SidecarModNs  int64  `json:"sidecarModNs,omitempty"`
	SidecarSize   int64  `json:"sidecarSize,omitempty"`
}

type cachedCue struct {
	Path         string                `json:"path"`
	Size         int64                 `json:"size"`
	ModTimeNs    int64                 `json:"modTimeNs"`
	AddedAt      int64                 `json:"addedAt"`
	CoverTracked bool                  `json:"coverTracked,omitempty"`
	RefTracked   bool                  `json:"refTracked,omitempty"`
	AllRefs      []string              `json:"allRefs,omitempty"`
	ResolvedRefs []cachedCueFileRef    `json:"resolvedRefs,omitempty"`
	Consumed     []cachedCueFileRef    `json:"consumed,omitempty"`
	TrackCovers  []cachedCueTrackCover `json:"trackCovers,omitempty"`
	Tracks       []LocalTrack          `json:"tracks,omitempty"`
}

func cloneCachedFile(cf cachedFile) cachedFile {
	cf.Track = cloneTrack(cf.Track)
	return cf
}

func cloneCachedCue(cc cachedCue) cachedCue {
	cc.AllRefs = slices.Clone(cc.AllRefs)
	cc.ResolvedRefs = slices.Clone(cc.ResolvedRefs)
	cc.Consumed = slices.Clone(cc.Consumed)
	cc.TrackCovers = slices.Clone(cc.TrackCovers)
	if cc.Tracks != nil {
		tracks := make([]LocalTrack, len(cc.Tracks))
		for i := range cc.Tracks {
			tracks[i] = cloneTrack(cc.Tracks[i])
		}
		cc.Tracks = tracks
	}
	return cc
}

type persistedIndex struct {
	Version    int                   `json:"version"`
	Folders    []string              `json:"folders"`
	Files      map[string]cachedFile `json:"files,omitempty"`
	Cues       map[string]cachedCue  `json:"cues,omitempty"`
	RoonGenres map[string][]string   `json:"roonGenres,omitempty"`
}

// Library manages local audiophile music folders, incremental metadata & cover
// caching, Roon genre merging, queries, and pure-Go track decoding.
type Library struct {
	indexPath string
	coverDir  string

	saveMu sync.Mutex
	scanMu sync.Mutex
	mu     sync.RWMutex

	revision   uint64
	folders    []string
	files      map[string]cachedFile
	cues       map[string]cachedCue
	roonGenres map[string][]string
	progress   ScanProgress

	tracks      []LocalTrack
	albums      []LocalAlbum
	artists     []LocalArtist
	genres      []GenreSummary
	tracksByID  map[string]int
	albumsByID  map[string]int
	artistsByID map[string]int
}

// NewLibrary creates a new local music library backed by indexPath and coverDir.
func NewLibrary(indexPath, coverDir string) *Library {
	return &Library{
		indexPath:   indexPath,
		coverDir:    coverDir,
		files:       map[string]cachedFile{},
		cues:        map[string]cachedCue{},
		roonGenres:  map[string][]string{},
		tracksByID:  map[string]int{},
		albumsByID:  map[string]int{},
		artistsByID: map[string]int{},
	}
}

// Load reads the persisted library index from disk and rebuilds in-memory views.
// A missing index file is treated as an empty library without error.
func (l *Library) Load() error {
	if l.indexPath == "" {
		return nil
	}
	b, err := os.ReadFile(l.indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var idx persistedIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.folders = l.folders[:0]
	for _, f := range idx.Folders {
		clean := filepath.Clean(strings.TrimSpace(f))
		if clean != "" && clean != "." && !containsPathKey(l.folders, clean) {
			l.folders = append(l.folders, clean)
		}
	}
	l.files = idx.Files
	if l.files == nil {
		l.files = map[string]cachedFile{}
	}
	l.cues = idx.Cues
	if l.cues == nil {
		l.cues = map[string]cachedCue{}
	}
	l.roonGenres = idx.RoonGenres
	if l.roonGenres == nil {
		l.roonGenres = map[string][]string{}
	}
	l.rebuildLocked()
	l.revision++
	return nil
}

// Revision returns a monotonic counter incremented whenever library state mutates.
func (l *Library) Revision() uint64 {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.revision
}

// Save atomically persists the library index to indexPath via a private temp file.
func (l *Library) Save() error {
	l.saveMu.Lock()
	defer l.saveMu.Unlock()

	l.mu.RLock()
	indexPath := l.indexPath
	if indexPath == "" {
		l.mu.RUnlock()
		return nil
	}
	snap := persistedIndex{
		Version:    indexFormatVersion,
		Folders:    slices.Clone(l.folders),
		Files:      l.files,
		Cues:       l.cues,
		RoonGenres: l.roonGenres,
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	l.mu.RUnlock()
	if err != nil {
		return err
	}
	return atomicWriteFile(indexPath, b)
}

func atomicWriteFile(dst string, data []byte) error {
	if dst == "" {
		return nil
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".local_library-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := renameIndexFile(tmpPath, dst); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Progress returns the most recent scan progress snapshot.
func (l *Library) Progress() ScanProgress {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.progress
}

// Folders returns the configured root music directories.
func (l *Library) Folders() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return slices.Clone(l.folders)
}

// AddFolder registers a single music directory, returning true if it was newly added.
func (l *Library) AddFolder(path string) bool {
	return l.AddFolders(path)
}

// AddFolders registers one or more music directories, returning true if any new
// directory was added.
func (l *Library) AddFolders(paths ...string) bool {
	l.mu.Lock()
	added := false
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		clean := filepath.Clean(p)
		if clean == "." || containsPathKey(l.folders, clean) {
			continue
		}
		l.folders = append(l.folders, clean)
		added = true
	}
	if added {
		l.revision++
	}
	l.mu.Unlock()
	if added {
		_ = l.Save()
	}
	return added
}

// RemoveFolder removes path from the watched folders and drops cached entries
// beneath it, returning true if path was present.
func (l *Library) RemoveFolder(path string) bool {
	clean := filepath.Clean(strings.TrimSpace(path))
	targetKey := canonicalPathKey(clean)

	l.mu.Lock()
	idx := -1
	for i, f := range l.folders {
		if canonicalPathKey(f) == targetKey {
			idx = i
			break
		}
	}
	if idx < 0 {
		l.mu.Unlock()
		return false
	}
	l.folders = slices.Delete(l.folders, idx, idx+1)

	for k, cf := range l.files {
		if isUnderAnyFolder(cf.Path, l.folders) {
			continue
		}
		delete(l.files, k)
	}
	for k, cc := range l.cues {
		if isUnderAnyFolder(cc.Path, l.folders) {
			continue
		}
		delete(l.cues, k)
	}
	l.rebuildLocked()
	l.revision++
	l.mu.Unlock()

	_ = l.Save()
	return true
}

func containsPathKey(list []string, candidate string) bool {
	ck := canonicalPathKey(candidate)
	for _, p := range list {
		if canonicalPathKey(p) == ck {
			return true
		}
	}
	return false
}

func isPathUnderRoot(p, root string) bool {
	pk := canonicalPathKey(p)
	rk := canonicalPathKey(root)
	if pk == rk {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(rk, sep) {
		rk += sep
	}
	return strings.HasPrefix(pk, rk)
}

func isUnderAnyFolder(p string, roots []string) bool {
	for _, r := range roots {
		if isPathUnderRoot(p, r) {
			return true
		}
	}
	return false
}

type walkedFile struct {
	path      string
	key       string
	size      int64
	modTimeNs int64
}

// Scan walks all configured folders, reuses unchanged (path, size, mtimeNs)
// cache entries unless a .cue or its referenced audio changed, parses new or
// modified files on bounded worker goroutines, rebuilds albums/artists/genres
// deterministically, and saves the library index.
func (l *Library) Scan(ctx context.Context, onProgress func(ScanProgress)) error {
	l.scanMu.Lock()
	defer l.scanMu.Unlock()

	l.mu.RLock()
	roots := slices.Clone(l.folders)
	prevFiles := make(map[string]cachedFile, len(l.files))
	for k, v := range l.files {
		prevFiles[k] = cloneCachedFile(v)
	}
	prevCues := make(map[string]cachedCue, len(l.cues))
	for k, v := range l.cues {
		prevCues[k] = cloneCachedCue(v)
	}
	l.mu.RUnlock()

	l.mu.Lock()
	l.progress = ScanProgress{Scanning: true}
	l.mu.Unlock()

	report := func(sp ScanProgress) {
		l.mu.Lock()
		l.progress = sp
		l.mu.Unlock()
		if onProgress != nil {
			onProgress(sp)
		}
	}
	defer func() {
		l.mu.Lock()
		l.progress.Scanning = false
		final := l.progress
		l.mu.Unlock()
		if onProgress != nil {
			onProgress(final)
		}
	}()

	type unavailableRoot struct {
		path string
		err  error
	}
	var (
		audioDisk        []walkedFile
		cueDisk          []walkedFile
		seenAudio        = map[string]walkedFile{}
		seenCue          = map[string]bool{}
		readableRoots    []string
		unavailableRoots []unavailableRoot
	)

	for _, root := range roots {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, err := os.Stat(root)
		if err != nil {
			unavailableRoots = append(unavailableRoots, unavailableRoot{
				path: root,
				err:  fmt.Errorf("localmusic: unavailable folder %s: %w", root, err),
			})
			continue
		}
		if !st.IsDir() {
			unavailableRoots = append(unavailableRoots, unavailableRoot{
				path: root,
				err:  fmt.Errorf("localmusic: path is not a folder %s", root),
			})
			continue
		}
		if _, err := os.ReadDir(root); err != nil {
			unavailableRoots = append(unavailableRoots, unavailableRoot{
				path: root,
				err:  fmt.Errorf("localmusic: unreadable folder %s: %w", root, err),
			})
			continue
		}

		rootKey := canonicalPathKey(root)
		var rootWalkErr error
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if canonicalPathKey(path) == rootKey {
					rootWalkErr = err
					return err
				}
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && (strings.HasPrefix(name, ".") || strings.EqualFold(name, "$RECYCLE.BIN") || strings.EqualFold(name, "System Volume Information")) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(name, "._") { // macOS AppleDouble resource fork
				return nil
			}
			ext := strings.ToLower(filepath.Ext(name))
			isCue := ext == ".cue"
			isAudio := supportedAudioExts[ext]
			if !isCue && !isAudio {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() <= 0 {
				return nil
			}
			clean := filepath.Clean(path)
			key := canonicalPathKey(clean)
			wf := walkedFile{
				path:      clean,
				key:       key,
				size:      info.Size(),
				modTimeNs: info.ModTime().UnixNano(),
			}
			if isCue {
				if !seenCue[key] {
					seenCue[key] = true
					cueDisk = append(cueDisk, wf)
				}
			} else {
				if _, dup := seenAudio[key]; !dup {
					seenAudio[key] = wf
					audioDisk = append(audioDisk, wf)
				}
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rootWalkErr != nil {
			unavailableRoots = append(unavailableRoots, unavailableRoot{
				path: root,
				err:  fmt.Errorf("localmusic: unreadable folder %s: %w", root, walkErr),
			})
			continue
		}
		readableRoots = append(readableRoots, root)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	slices.SortFunc(cueDisk, func(a, b walkedFile) int { return cmp.Compare(a.key, b.key) })
	slices.SortFunc(audioDisk, func(a, b walkedFile) int { return cmp.Compare(a.key, b.key) })

	covers := newFolderCoverResolver(l.coverDir)
	coverPresent := func(coverID string) bool {
		if coverID == "" || l.coverDir == "" {
			return true
		}
		st, err := os.Stat(filepath.Join(l.coverDir, coverID))
		return err == nil && st.Size() > 0
	}

	nowMs := time.Now().UnixMilli()
	nextCues := make(map[string]cachedCue, len(cueDisk))
	consumedByCue := map[string]bool{}

	for _, cw := range cueDisk {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if prev, ok := prevCues[cw.key]; ok &&
			prev.Size == cw.size &&
			prev.ModTimeNs == cw.modTimeNs &&
			prev.CoverTracked &&
			len(prev.AllRefs) > 0 &&
			len(prev.TrackCovers) == len(prev.Tracks) {

			refsUnchanged := prev.RefTracked || len(prev.Tracks) > 0
			expectedRefs := prev.ResolvedRefs
			if !prev.RefTracked {
				expectedRefs = prev.Consumed
			}
			prevRefMap := make(map[string]cachedCueFileRef, len(expectedRefs))
			for _, ref := range expectedRefs {
				prevRefMap[canonicalPathKey(ref.Path)] = ref
			}
			curResolved := make(map[string]cachedCueFileRef, len(prev.AllRefs))
			for _, litRef := range prev.AllRefs {
				rPath, rSize, rModNs, exists := resolveCueLiteralRef(cw.path, litRef, seenAudio)
				if !exists {
					continue
				}
				rk := canonicalPathKey(rPath)
				curResolved[rk] = cachedCueFileRef{Path: rPath, Size: rSize, ModTimeNs: rModNs}
			}
			if len(curResolved) != len(prevRefMap) {
				refsUnchanged = false
			} else {
				for rk, cur := range curResolved {
					old, exists := prevRefMap[rk]
					if !exists || old.Size != cur.Size || old.ModTimeNs != cur.ModTimeNs {
						refsUnchanged = false
						break
					}
				}
			}

			if refsUnchanged {
				for i := range prev.Tracks {
					if prev.TrackCovers[i].EmbeddedCover {
						if l.coverDir != "" && (prev.Tracks[i].CoverID == "" || !coverPresent(prev.Tracks[i].CoverID)) {
							refsUnchanged = false
							break
						}
					}
				}
			}

			if refsUnchanged {
				updatedCue := prev
				for i := range updatedCue.Tracks {
					tc := updatedCue.TrackCovers[i]
					if tc.EmbeddedCover {
						continue
					}
					covID, cand, same := covers.checkCachedOrResolve(
						updatedCue.Tracks[i].Dir, tc.AlbumTitle,
						tc.SidecarCover, tc.SidecarModNs, tc.SidecarSize, updatedCue.Tracks[i].CoverID,
						coverPresent,
					)
					if same {
						continue
					}
					updatedCue.Tracks[i].CoverID = covID
					updatedCue.TrackCovers[i] = cachedCueTrackCover{
						EmbeddedCover: false,
						AlbumTitle:    tc.AlbumTitle,
						SidecarCover:  cand.path,
						SidecarModNs:  cand.modTimeNs,
						SidecarSize:   cand.size,
					}
				}

				nextCues[cw.key] = updatedCue
				for _, ref := range updatedCue.Consumed {
					consumedByCue[canonicalPathKey(ref.Path)] = true
				}
				continue
			}
		}

		addedAt := cw.modTimeNs / 1e6
		if prev, ok := prevCues[cw.key]; ok && prev.AddedAt > 0 {
			addedAt = prev.AddedAt
		} else if addedAt <= 0 {
			addedAt = nowMs
		}

		loaded, err := loadCueTracks(cw.path, addedAt, covers)
		if err != nil || loaded == nil || (len(loaded.tracks) == 0 && len(loaded.allRefs) == 0) {
			continue
		}

		var resolvedRefs []cachedCueFileRef
		seenRes := map[string]bool{}
		for _, litRef := range loaded.allRefs {
			rPath, rSize, rModNs, exists := resolveCueLiteralRef(cw.path, litRef, seenAudio)
			if !exists {
				continue
			}
			rk := canonicalPathKey(rPath)
			if !seenRes[rk] {
				seenRes[rk] = true
				resolvedRefs = append(resolvedRefs, cachedCueFileRef{Path: rPath, Size: rSize, ModTimeNs: rModNs})
			}
		}

		var refs []cachedCueFileRef
		for _, cp := range loaded.consumed {
			ck := canonicalPathKey(cp)
			consumedByCue[ck] = true
			if wf, ok := seenAudio[ck]; ok {
				refs = append(refs, cachedCueFileRef{Path: cp, Size: wf.size, ModTimeNs: wf.modTimeNs})
			} else if st, err := os.Stat(cp); err == nil {
				refs = append(refs, cachedCueFileRef{Path: cp, Size: st.Size(), ModTimeNs: st.ModTime().UnixNano()})
			}
		}
		nextCues[cw.key] = cachedCue{
			Path:         cw.path,
			Size:         cw.size,
			ModTimeNs:    cw.modTimeNs,
			AddedAt:      addedAt,
			CoverTracked: true,
			RefTracked:   true,
			AllRefs:      loaded.allRefs,
			ResolvedRefs: resolvedRefs,
			Consumed:     refs,
			TrackCovers:  loaded.trackCovers,
			Tracks:       loaded.tracks,
		}
	}

	// Partition standalone audio files into cache hits and worker jobs.
	nextFiles := make(map[string]cachedFile, len(audioDisk))
	var toParse []walkedFile
	for _, aw := range audioDisk {
		if consumedByCue[aw.key] {
			continue
		}
		if prev, ok := prevFiles[aw.key]; ok && prev.Size == aw.size && prev.ModTimeNs == aw.modTimeNs && prev.CoverTracked {
			if prev.EmbeddedCover {
				if l.coverDir == "" || (prev.Track.CoverID != "" && coverPresent(prev.Track.CoverID)) {
					nextFiles[aw.key] = prev
					continue
				}
			} else {
				covID, cand, same := covers.checkCachedOrResolve(
					prev.Track.Dir, prev.Track.Album,
					prev.SidecarCover, prev.SidecarModNs, prev.SidecarSize, prev.Track.CoverID,
					coverPresent,
				)
				if same {
					nextFiles[aw.key] = prev
					continue
				}
				updated := prev
				updated.Track.CoverID = covID
				updated.SidecarCover = cand.path
				updated.SidecarModNs = cand.modTimeNs
				updated.SidecarSize = cand.size
				nextFiles[aw.key] = updated
				continue
			}
		}
		toParse = append(toParse, aw)
	}

	totalItems := len(cueDisk) + len(audioDisk)
	alreadyDone := len(cueDisk) + (len(audioDisk) - len(toParse))
	report(ScanProgress{
		Scanning: true,
		Scanned:  alreadyDone,
		Total:    totalItems,
	})

	if len(toParse) > 0 {
		type parseResult struct {
			ok    bool
			key   string
			entry cachedFile
			base  string
		}
		results := make([]parseResult, len(toParse))
		jobs := make(chan int, len(toParse))
		for i := range toParse {
			jobs <- i
		}
		close(jobs)

		workers := min(max(runtime.NumCPU(), 2), 8, len(toParse))
		var (
			wg       sync.WaitGroup
			progMu   sync.Mutex
			doneJobs int
		)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for idx := range jobs {
					if ctx.Err() != nil {
						return
					}
					wf := toParse[idx]
					if hook := onFileParsedHook; hook != nil {
						hook(wf.path)
					}
					meta, err := parseAudioFile(wf.path, wf.size)
					baseName := filepath.Base(wf.path)
					if err == nil && meta != nil {
						addedAt := wf.modTimeNs / 1e6
						if prev, ok := prevFiles[wf.key]; ok && prev.AddedAt > 0 {
							addedAt = prev.AddedAt
						} else if addedAt <= 0 {
							addedAt = nowMs
						}
						entry := l.buildCachedFileFromMeta(wf, meta, addedAt, covers)
						results[idx] = parseResult{
							ok:    true,
							key:   wf.key,
							base:  baseName,
							entry: entry,
						}
					}
					progMu.Lock()
					doneJobs++
					curScanned := alreadyDone + doneJobs
					progMu.Unlock()
					if ctx.Err() == nil {
						report(ScanProgress{
							Scanning: true,
							Scanned:  curScanned,
							Total:    totalItems,
							Current:  baseName,
						})
					}
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, r := range results {
			if r.ok {
				nextFiles[r.key] = r.entry
			}
		}
	}

	var activeRootErrs []error
	l.mu.Lock()
	if len(unavailableRoots) > 0 {
		var unavailPaths []string
		for _, ur := range unavailableRoots {
			if containsPathKey(l.folders, ur.path) {
				unavailPaths = append(unavailPaths, ur.path)
				activeRootErrs = append(activeRootErrs, ur.err)
			}
		}
		if len(unavailPaths) > 0 {
			for k, cf := range l.files {
				if _, already := nextFiles[k]; already {
					continue
				}
				if isUnderAnyFolder(cf.Path, unavailPaths) && !isUnderAnyFolder(cf.Path, readableRoots) {
					nextFiles[k] = cloneCachedFile(cf)
				}
			}
			for k, cc := range l.cues {
				if _, already := nextCues[k]; already {
					continue
				}
				if isUnderAnyFolder(cc.Path, unavailPaths) && !isUnderAnyFolder(cc.Path, readableRoots) {
					nextCues[k] = cloneCachedCue(cc)
				}
			}
		}
	}
	for k, cf := range nextFiles {
		if !isUnderAnyFolder(cf.Path, l.folders) {
			delete(nextFiles, k)
		}
	}
	for k, cc := range nextCues {
		if !isUnderAnyFolder(cc.Path, l.folders) {
			delete(nextCues, k)
		}
	}
	l.files = nextFiles
	l.cues = nextCues
	l.applyPersistedRoonGenresLocked()
	l.rebuildLocked()
	l.revision++
	l.progress = ScanProgress{
		Scanning: false,
		Scanned:  totalItems,
		Total:    totalItems,
	}
	l.mu.Unlock()

	saveErr := l.Save()
	if len(activeRootErrs) > 0 {
		if saveErr != nil {
			activeRootErrs = append(activeRootErrs, saveErr)
		}
		return errors.Join(activeRootErrs...)
	}
	return saveErr
}

var leadingTrackNumRe = regexp.MustCompile(`^\s*0*(\d{1,3})(?:[\s._-]+)(.+)$`)

func parseFilenameFallback(filename string) (trackNo int, title string) {
	stem := strings.TrimSpace(strings.TrimSuffix(filename, filepath.Ext(filename)))
	if m := leadingTrackNumRe.FindStringSubmatch(stem); len(m) == 3 {
		n, err := strconv.Atoi(m[1])
		rest := strings.TrimSpace(m[2])
		if err == nil && n > 0 && rest != "" {
			return n, rest
		}
	}
	return 0, stem
}

func (l *Library) buildCachedFileFromMeta(wf walkedFile, meta *parsedMeta, addedAt int64, covers *folderCoverResolver) cachedFile {
	dir := filepath.Clean(filepath.Dir(wf.path))
	fnTrackNo, fnTitle := parseFilenameFallback(filepath.Base(wf.path))

	title := meta.title
	if title == "" {
		title = fnTitle
	}
	trackNo := meta.trackNo
	if trackNo == 0 {
		trackNo = fnTrackNo
	}
	discNo := meta.discNo
	if discNo == 0 {
		discNo = parseDiscNumberFromDir(dir)
	}
	album := meta.album
	if album == "" {
		album = defaultAlbumFromDir(dir)
	}

	var (
		coverID       string
		embeddedCover bool
		sidecarInfo   sidecarCoverInfo
	)
	if len(meta.cover) > 0 {
		if cid, err := saveCoverBytes(l.coverDir, meta.cover); err == nil && cid != "" {
			coverID = cid
			embeddedCover = true
		}
	}
	if !embeddedCover {
		coverID, sidecarInfo = covers.resolveAndIngest(dir, album)
	}

	artists := slices.Clone(meta.artists)
	albumArtist := meta.albumArtist
	artistIDs := make([]string, len(artists))
	for i, a := range artists {
		artistIDs[i] = makeArtistID(a)
	}
	effAlbumArtist := albumArtist
	if effAlbumArtist == "" && len(artists) > 0 {
		effAlbumArtist = artists[0]
	}

	tr := LocalTrack{
		ID:             makeTrackID(wf.path, false, 0, 0),
		Path:           wf.path,
		Dir:            dir,
		Title:          title,
		Artists:        artists,
		Album:          album,
		AlbumArtist:    effAlbumArtist,
		AlbumID:        makeAlbumID(album, effAlbumArtist, albumGroupDir(dir)),
		ArtistIDs:      artistIDs,
		Genres:         slices.Clone(meta.genres),
		Composer:       meta.composer,
		Year:           meta.year,
		TrackNo:        trackNo,
		DiscNo:         discNo,
		DurationMs:     meta.durationMs,
		CoverID:        coverID,
		Spec:           meta.spec,
		GainDB:         meta.gainDB,
		AddedAt:        addedAt,
		Size:           wf.size,
		ModTimeNs:      wf.modTimeNs,
		EmbeddedLyrics: meta.lyrics,
	}
	return cachedFile{
		Path:          wf.path,
		Size:          wf.size,
		ModTimeNs:     wf.modTimeNs,
		AddedAt:       addedAt,
		CoverTracked:  true,
		EmbeddedCover: embeddedCover,
		SidecarCover:  sidecarInfo.path,
		SidecarModNs:  sidecarInfo.modTimeNs,
		SidecarSize:   sidecarInfo.size,
		Track:         tr,
	}
}

// albumGroupDir returns the logical album folder (grouping Disc 1 / Disc 2 under parent).
func albumGroupDir(dir string) string {
	if isDiscSubfolder(filepath.Base(dir)) {
		parent := filepath.Dir(dir)
		if parent != "" && parent != "." {
			return parent
		}
	}
	return dir
}

// rebuildLocked deterministically constructs l.tracks, l.albums, l.artists, and l.genres.
func (l *Library) rebuildLocked() {
	consumed := map[string]bool{}
	for _, cc := range l.cues {
		for _, ref := range cc.Consumed {
			consumed[canonicalPathKey(ref.Path)] = true
		}
	}

	rawTracks := make([]LocalTrack, 0, len(l.files)+len(l.cues)*8)
	for k, cf := range l.files {
		if consumed[k] {
			continue
		}
		rawTracks = append(rawTracks, cloneTrack(cf.Track))
	}
	for _, cc := range l.cues {
		for _, tr := range cc.Tracks {
			rawTracks = append(rawTracks, cloneTrack(tr))
		}
	}

	// Infer consistent AlbumArtist across folders where tracks share an Album title
	// but carry varying per-track guest credits without an explicit ALBUMARTIST tag.
	type folderAlbKey struct {
		dirKey string
		albKey string
	}
	folderGroups := map[folderAlbKey][]int{}
	for i := range rawTracks {
		t := &rawTracks[i]
		if t.Album == "" {
			t.Album = defaultAlbumFromDir(t.Dir)
		}
		k := folderAlbKey{
			dirKey: canonicalPathKey(albumGroupDir(t.Dir)),
			albKey: normMatchKey(t.Album),
		}
		folderGroups[k] = append(folderGroups[k], i)
	}

	for _, idxs := range folderGroups {
		if len(idxs) <= 1 {
			continue
		}
		var commonAA string
		multipleAA := false
		for _, i := range idxs {
			aa := strings.TrimSpace(rawTracks[i].AlbumArtist)
			if aa == "" && len(rawTracks[i].Artists) > 0 {
				aa = rawTracks[i].Artists[0]
			}
			if aa == "" {
				continue
			}
			if commonAA == "" {
				commonAA = aa
			} else if normMatchKey(commonAA) != normMatchKey(aa) {
				multipleAA = true
			}
		}
		if multipleAA {
			// Check if any track had an explicit compilation-wide AlbumArtist like "Various Artists".
			explicit := ""
			for _, i := range idxs {
				if strings.EqualFold(rawTracks[i].AlbumArtist, "Various Artists") {
					explicit = rawTracks[i].AlbumArtist
					break
				}
			}
			if explicit == "" {
				explicit = "Various Artists"
			}
			for _, i := range idxs {
				rawTracks[i].AlbumArtist = explicit
			}
		} else if commonAA != "" {
			for _, i := range idxs {
				if rawTracks[i].AlbumArtist == "" {
					rawTracks[i].AlbumArtist = commonAA
				}
			}
		}
	}

	// Finalize IDs on all tracks and sort deterministically.
	for i := range rawTracks {
		t := &rawTracks[i]
		t.Spec.computeFlags()
		grpDir := albumGroupDir(t.Dir)
		t.AlbumID = makeAlbumID(t.Album, t.AlbumArtist, grpDir)
		t.ArtistIDs = make([]string, len(t.Artists))
		for ai, aname := range t.Artists {
			t.ArtistIDs[ai] = makeArtistID(aname)
		}
	}

	slices.SortFunc(rawTracks, compareTracksOrder)

	// Group into albums.
	albumMap := map[string]*LocalAlbum{}
	var albumOrder []string
	for _, tr := range rawTracks {
		al := albumMap[tr.AlbumID]
		if al == nil {
			grpDir := albumGroupDir(tr.Dir)
			var artID string
			if tr.AlbumArtist != "" {
				artID = makeArtistID(tr.AlbumArtist)
			}
			al = &LocalAlbum{
				ID:          tr.AlbumID,
				Title:       tr.Album,
				AlbumArtist: tr.AlbumArtist,
				ArtistID:    artID,
				Dir:         grpDir,
			}
			albumMap[tr.AlbumID] = al
			albumOrder = append(albumOrder, tr.AlbumID)
		}
		al.Tracks = append(al.Tracks, tr)
		if al.CoverID == "" && tr.CoverID != "" {
			al.CoverID = tr.CoverID
		}
		if tr.Year > al.Year {
			al.Year = tr.Year
		}
		if tr.AddedAt > al.AddedAt {
			al.AddedAt = tr.AddedAt
		}
		for _, g := range tr.Genres {
			if g != "" && !containsFold(al.Genres, g) {
				al.Genres = append(al.Genres, g)
			}
		}
	}

	albums := make([]LocalAlbum, 0, len(albumOrder))
	for _, aid := range albumOrder {
		al := albumMap[aid]
		if rgs, ok := l.roonGenres[aid]; ok {
			for _, g := range rgs {
				if g != "" && !containsFold(al.Genres, g) {
					al.Genres = append(al.Genres, g)
				}
				for ti := range al.Tracks {
					if g != "" && !containsFold(al.Tracks[ti].Genres, g) {
						al.Tracks[ti].Genres = append(al.Tracks[ti].Genres, g)
					}
				}
			}
		}
		slices.SortFunc(al.Tracks, compareAlbumTrackOrder)
		discs := map[int]bool{}
		var bestSpec AudioSpec
		bestRank := -1
		anyCUE := false
		for i := range al.Tracks {
			d := max(al.Tracks[i].DiscNo, 1)
			discs[d] = true
			if al.CoverID != "" && al.Tracks[i].CoverID == "" {
				al.Tracks[i].CoverID = al.CoverID
			}
			sp := al.Tracks[i].Spec
			if sp.IsCUE {
				anyCUE = true
			}
			if r := specQualityRank(sp); r > bestRank {
				bestRank = r
				bestSpec = sp
			}
		}
		bestSpec.ClipStartMs = 0
		bestSpec.ClipEndMs = 0
		bestSpec.ClipStartFrames = 0
		bestSpec.ClipEndFrames = 0
		bestSpec.HasCueFrames = false
		bestSpec.IsCUE = anyCUE
		al.Spec = bestSpec
		al.DiscCount = max(len(discs), 1)
		albums = append(albums, *al)
	}

	// Default album order: most recently added, then title.
	sortAlbumsSlice(albums, "recent")

	// Rebuild canonical tracks slice with propagated album CoverID.
	allTracks := make([]LocalTrack, 0, len(rawTracks))
	for i := range albums {
		allTracks = append(allTracks, albums[i].Tracks...)
	}

	// Group into artists.
	type artistBuilder struct {
		ar       LocalArtist
		albumSet map[string]bool
		trackSet map[string]bool
	}
	artistMap := map[string]*artistBuilder{}
	var artistOrder []string
	ensureArtist := func(name string) *artistBuilder {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil
		}
		id := makeArtistID(name)
		b := artistMap[id]
		if b == nil {
			b = &artistBuilder{
				ar:       LocalArtist{ID: id, Name: name},
				albumSet: map[string]bool{},
				trackSet: map[string]bool{},
			}
			artistMap[id] = b
			artistOrder = append(artistOrder, id)
		}
		return b
	}

	for i := range albums {
		al := &albums[i]
		if al.AlbumArtist != "" && !strings.EqualFold(al.AlbumArtist, "Various Artists") {
			if b := ensureArtist(al.AlbumArtist); b != nil && !b.albumSet[al.ID] {
				b.albumSet[al.ID] = true
				b.ar.Albums = append(b.ar.Albums, cloneAlbumWithoutTracks(*al))
				if b.ar.CoverID == "" && al.CoverID != "" {
					b.ar.CoverID = al.CoverID
				}
				for _, g := range al.Genres {
					if !containsFold(b.ar.Genres, g) {
						b.ar.Genres = append(b.ar.Genres, g)
					}
				}
			}
		}
		for ti := range al.Tracks {
			tr := &al.Tracks[ti]
			names := tr.Artists
			if len(names) == 0 && tr.AlbumArtist != "" && !strings.EqualFold(tr.AlbumArtist, "Various Artists") {
				names = []string{tr.AlbumArtist}
			}
			for _, nm := range names {
				b := ensureArtist(nm)
				if b == nil {
					continue
				}
				if !b.albumSet[al.ID] {
					b.albumSet[al.ID] = true
					b.ar.Albums = append(b.ar.Albums, cloneAlbumWithoutTracks(*al))
				}
				if !b.trackSet[tr.ID] {
					b.trackSet[tr.ID] = true
					b.ar.Tracks = append(b.ar.Tracks, cloneTrack(*tr))
				}
				if b.ar.CoverID == "" && tr.CoverID != "" {
					b.ar.CoverID = tr.CoverID
				}
				for _, g := range tr.Genres {
					if !containsFold(b.ar.Genres, g) {
						b.ar.Genres = append(b.ar.Genres, g)
					}
				}
			}
		}
	}

	artists := make([]LocalArtist, 0, len(artistOrder))
	for _, aid := range artistOrder {
		b := artistMap[aid]
		slices.SortFunc(b.ar.Albums, func(a, c LocalAlbum) int {
			if a.Year != c.Year {
				return cmp.Compare(c.Year, a.Year)
			}
			return cmp.Compare(strings.ToLower(a.Title), strings.ToLower(c.Title))
		})
		b.ar.AlbumCount = len(b.ar.Albums)
		b.ar.TrackCount = len(b.ar.Tracks)
		artists = append(artists, b.ar)
	}
	slices.SortFunc(artists, func(a, b LocalArtist) int {
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})

	// Group into genres.
	type genreBuilder struct {
		gs       GenreSummary
		albumSet map[string]bool
	}
	genreMap := map[string]*genreBuilder{}
	var genreOrder []string
	for i := range albums {
		al := &albums[i]
		for _, g := range al.Genres {
			gk := strings.ToLower(strings.TrimSpace(g))
			if gk == "" {
				continue
			}
			gb := genreMap[gk]
			if gb == nil {
				gb = &genreBuilder{
					gs:       GenreSummary{Name: strings.TrimSpace(g)},
					albumSet: map[string]bool{},
				}
				genreMap[gk] = gb
				genreOrder = append(genreOrder, gk)
			}
			if !gb.albumSet[al.ID] {
				gb.albumSet[al.ID] = true
				gb.gs.AlbumCount++
				if al.CoverID != "" && len(gb.gs.Covers) < 4 {
					cu := CoverURL(al.CoverID)
					if !slices.Contains(gb.gs.Covers, cu) {
						gb.gs.Covers = append(gb.gs.Covers, cu)
					}
				}
			}
		}
		for ti := range al.Tracks {
			tr := &al.Tracks[ti]
			for _, g := range tr.Genres {
				gk := strings.ToLower(strings.TrimSpace(g))
				if gb := genreMap[gk]; gb != nil {
					gb.gs.TrackCount++
				}
			}
		}
	}

	genres := make([]GenreSummary, 0, len(genreOrder))
	for _, gk := range genreOrder {
		genres = append(genres, genreMap[gk].gs)
	}
	slices.SortFunc(genres, func(a, b GenreSummary) int {
		if a.AlbumCount != b.AlbumCount {
			return cmp.Compare(b.AlbumCount, a.AlbumCount)
		}
		if a.TrackCount != b.TrackCount {
			return cmp.Compare(b.TrackCount, a.TrackCount)
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})

	// Build fast ID lookup maps.
	tracksByID := make(map[string]int, len(allTracks))
	for i := range allTracks {
		tracksByID[allTracks[i].ID] = i
	}
	albumsByID := make(map[string]int, len(albums))
	for i := range albums {
		albumsByID[albums[i].ID] = i
	}
	artistsByID := make(map[string]int, len(artists))
	for i := range artists {
		artistsByID[artists[i].ID] = i
	}

	l.tracks = allTracks
	l.albums = albums
	l.artists = artists
	l.genres = genres
	l.tracksByID = tracksByID
	l.albumsByID = albumsByID
	l.artistsByID = artistsByID
}

func compareAlbumTrackOrder(a, b LocalTrack) int {
	da, db := max(a.DiscNo, 1), max(b.DiscNo, 1)
	if da != db {
		return cmp.Compare(da, db)
	}
	if a.TrackNo != b.TrackNo {
		if a.TrackNo == 0 {
			return 1
		}
		if b.TrackNo == 0 {
			return -1
		}
		return cmp.Compare(a.TrackNo, b.TrackNo)
	}
	if a.Spec.HasCueFrames && b.Spec.HasCueFrames && a.Spec.ClipStartFrames != b.Spec.ClipStartFrames {
		return cmp.Compare(a.Spec.ClipStartFrames, b.Spec.ClipStartFrames)
	}
	if a.Spec.ClipStartMs != b.Spec.ClipStartMs {
		return cmp.Compare(a.Spec.ClipStartMs, b.Spec.ClipStartMs)
	}
	if c := cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
		return c
	}
	return cmp.Compare(canonicalPathKey(a.Path), canonicalPathKey(b.Path))
}

func compareTracksOrder(a, b LocalTrack) int {
	if c := cmp.Compare(strings.ToLower(a.Album), strings.ToLower(b.Album)); c != 0 {
		return c
	}
	return compareAlbumTrackOrder(a, b)
}

func sortAlbumsSlice(albums []LocalAlbum, mode string) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "title":
		slices.SortFunc(albums, func(a, b LocalAlbum) int {
			if c := cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
	case "artist":
		slices.SortFunc(albums, func(a, b LocalAlbum) int {
			if c := cmp.Compare(strings.ToLower(a.AlbumArtist), strings.ToLower(b.AlbumArtist)); c != 0 {
				return c
			}
			if a.Year != b.Year {
				return cmp.Compare(b.Year, a.Year)
			}
			return cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
		})
	case "year":
		slices.SortFunc(albums, func(a, b LocalAlbum) int {
			if a.Year != b.Year {
				return cmp.Compare(b.Year, a.Year)
			}
			return cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
		})
	case "spec":
		slices.SortFunc(albums, func(a, b LocalAlbum) int {
			ra, rb := specQualityRank(a.Spec), specQualityRank(b.Spec)
			if ra != rb {
				return cmp.Compare(rb, ra)
			}
			return cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
		})
	default: // "recent"
		slices.SortFunc(albums, func(a, b LocalAlbum) int {
			if a.AddedAt != b.AddedAt {
				return cmp.Compare(b.AddedAt, a.AddedAt)
			}
			if c := cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
	}
}

func cloneTrack(t LocalTrack) LocalTrack {
	t.Artists = slices.Clone(t.Artists)
	t.ArtistIDs = slices.Clone(t.ArtistIDs)
	t.Genres = slices.Clone(t.Genres)
	return t
}

func cloneAlbum(a LocalAlbum) LocalAlbum {
	a.Genres = slices.Clone(a.Genres)
	tracks := make([]LocalTrack, len(a.Tracks))
	for i := range a.Tracks {
		tracks[i] = cloneTrack(a.Tracks[i])
	}
	a.Tracks = tracks
	return a
}

func cloneAlbumWithoutTracks(a LocalAlbum) LocalAlbum {
	a.Genres = slices.Clone(a.Genres)
	a.Tracks = nil
	return a
}

func cloneArtist(ar LocalArtist) LocalArtist {
	ar.Genres = slices.Clone(ar.Genres)
	albums := make([]LocalAlbum, len(ar.Albums))
	for i := range ar.Albums {
		albums[i] = cloneAlbum(ar.Albums[i])
	}
	ar.Albums = albums
	tracks := make([]LocalTrack, len(ar.Tracks))
	for i := range ar.Tracks {
		tracks[i] = cloneTrack(ar.Tracks[i])
	}
	ar.Tracks = tracks
	return ar
}

// --- Queries & Browsing ---

// CoverBytes loads raw cover image bytes for a localcover:// URL or bare coverID.
func (l *Library) CoverBytes(coverURLOrID string) ([]byte, error) {
	id := strings.TrimSpace(strings.TrimPrefix(coverURLOrID, CoverScheme))
	if id == "" || l.coverDir == "" {
		return nil, errors.New("localmusic: empty cover ID")
	}
	clean := filepath.Base(id)
	if clean != id || clean == "." || clean == ".." {
		return nil, errors.New("localmusic: invalid cover ID")
	}
	return os.ReadFile(filepath.Join(l.coverDir, clean))
}

// Stats returns aggregate counts across the local library.
func (l *Library) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()

	hiRes, dsd := 0, 0
	for i := range l.albums {
		if l.albums[i].Spec.IsHiRes {
			hiRes++
		}
		if l.albums[i].Spec.IsDSD {
			dsd++
		}
	}
	return Stats{
		Folders:     len(l.folders),
		Albums:      len(l.albums),
		Tracks:      len(l.tracks),
		Artists:     len(l.artists),
		Genres:      len(l.genres),
		HiResAlbums: hiRes,
		DSDAlbums:   dsd,
	}
}

func matchesSpecFocus(spec AudioSpec, focus string) bool {
	switch strings.ToLower(strings.TrimSpace(focus)) {
	case "", "all":
		return true
	case "hires", "hi-res":
		return spec.IsHiRes
	case "dsd":
		return spec.IsDSD
	case "lossless":
		return spec.IsLossless
	case "cue":
		return spec.IsCUE
	default:
		return true
	}
}

func textMatchesQuery(qRaw, qNorm string, fields ...string) bool {
	if qRaw == "" {
		return true
	}
	for _, f := range fields {
		if f == "" {
			continue
		}
		if strings.Contains(strings.ToLower(f), qRaw) {
			return true
		}
		if qNorm != "" && strings.Contains(normMatchKey(f), qNorm) {
			return true
		}
	}
	return false
}

// Albums returns albums matching filter f in the requested sort order.
func (l *Library) Albums(f AlbumFilter) []LocalAlbum {
	l.mu.RLock()
	defer l.mu.RUnlock()

	qRaw := strings.ToLower(strings.TrimSpace(f.Query))
	qNorm := normMatchKey(f.Query)
	wantGenre := strings.TrimSpace(f.Genre)

	out := make([]LocalAlbum, 0, len(l.albums))
	for i := range l.albums {
		al := &l.albums[i]
		if wantGenre != "" && !containsFold(al.Genres, wantGenre) {
			continue
		}
		if !matchesSpecFocus(al.Spec, f.Focus) {
			continue
		}
		if qRaw != "" {
			matched := textMatchesQuery(qRaw, qNorm, al.Title, al.AlbumArtist, al.Spec.Codec, al.Spec.Badge())
			if !matched {
				for _, g := range al.Genres {
					if textMatchesQuery(qRaw, qNorm, g) {
						matched = true
						break
					}
				}
			}
			if !matched {
				for ti := range al.Tracks {
					tr := &al.Tracks[ti]
					if textMatchesQuery(qRaw, qNorm, tr.Title, tr.Composer, strings.Join(tr.Artists, " ")) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, cloneAlbum(*al))
	}

	sortAlbumsSlice(out, f.Sort)
	return out
}

// AlbumByID looks up an album by ID.
func (l *Library) AlbumByID(id string) (*LocalAlbum, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	idx, ok := l.albumsByID[id]
	if !ok {
		return nil, false
	}
	al := cloneAlbum(l.albums[idx])
	return &al, true
}

// Artists returns artists matching query.
func (l *Library) Artists(query string) []LocalArtist {
	l.mu.RLock()
	defer l.mu.RUnlock()

	qRaw := strings.ToLower(strings.TrimSpace(query))
	qNorm := normMatchKey(query)
	out := make([]LocalArtist, 0, len(l.artists))
	for i := range l.artists {
		ar := &l.artists[i]
		if qRaw != "" && !textMatchesQuery(qRaw, qNorm, ar.Name, strings.Join(ar.Genres, " ")) {
			continue
		}
		out = append(out, cloneArtist(*ar))
	}
	return out
}

// ArtistByID looks up an artist by ID.
func (l *Library) ArtistByID(id string) (*LocalArtist, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	idx, ok := l.artistsByID[id]
	if !ok {
		return nil, false
	}
	ar := cloneArtist(l.artists[idx])
	return &ar, true
}

// Genres returns all genres in the local library ordered by album count.
func (l *Library) Genres() []GenreSummary {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]GenreSummary, len(l.genres))
	for i := range l.genres {
		out[i] = GenreSummary{
			Name:       l.genres[i].Name,
			AlbumCount: l.genres[i].AlbumCount,
			TrackCount: l.genres[i].TrackCount,
			Covers:     slices.Clone(l.genres[i].Covers),
		}
	}
	return out
}

// Tracks returns tracks matching query, genre, and focus ("", "hires", "dsd", "lossless", "cue").
func (l *Library) Tracks(query, genre, focus string) []LocalTrack {
	l.mu.RLock()
	defer l.mu.RUnlock()

	qRaw := strings.ToLower(strings.TrimSpace(query))
	qNorm := normMatchKey(query)
	wantGenre := strings.TrimSpace(genre)

	out := make([]LocalTrack, 0, len(l.tracks))
	for i := range l.tracks {
		tr := &l.tracks[i]
		if wantGenre != "" && !containsFold(tr.Genres, wantGenre) {
			continue
		}
		if !matchesSpecFocus(tr.Spec, focus) {
			continue
		}
		if qRaw != "" && !textMatchesQuery(
			qRaw, qNorm,
			tr.Title, tr.Album, tr.AlbumArtist, tr.Composer,
			strings.Join(tr.Artists, " "), strings.Join(tr.Genres, " "),
			tr.Spec.Codec, tr.Spec.Badge(),
		) {
			continue
		}
		out = append(out, cloneTrack(*tr))
	}
	return out
}

// TrackByID looks up a cloned track by ID without opening an audio decoder.
func (l *Library) TrackByID(id string) (*LocalTrack, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	idx, ok := l.tracksByID[id]
	if !ok {
		return nil, false
	}
	tr := cloneTrack(l.tracks[idx])
	return &tr, true
}

// BrowseFolder returns a folder-tree view of dir (or the configured root folders
// when dir == "").
func (l *Library) BrowseFolder(dir string) FolderNode {
	l.mu.RLock()
	defer l.mu.RUnlock()

	dir = strings.TrimSpace(dir)
	if dir == "" {
		node := FolderNode{}
		for _, root := range l.folders {
			entry := buildFolderEntryLocked(root, filepath.Base(root), l.tracks)
			if entry.Name == "" || entry.Name == "." || entry.Name == string(filepath.Separator) {
				entry.Name = root
			}
			node.Subdirs = append(node.Subdirs, entry)
		}
		return node
	}

	cleanDir := filepath.Clean(dir)
	if !isUnderAnyFolder(cleanDir, l.folders) {
		return FolderNode{Path: cleanDir, Name: filepath.Base(cleanDir)}
	}

	parent := filepath.Dir(cleanDir)
	for _, root := range l.folders {
		if canonicalPathKey(cleanDir) == canonicalPathKey(root) {
			parent = ""
			break
		}
	}

	dirKey := canonicalPathKey(cleanDir)
	sep := string(filepath.Separator)
	prefix := dirKey
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}

	type childAgg struct {
		path       string
		name       string
		trackCount int
		coverID    string
		grandDirs  map[string]bool
	}
	childrenMap := map[string]*childAgg{}
	var childKeys []string
	var directTracks []LocalTrack

	for i := range l.tracks {
		tr := &l.tracks[i]
		tk := canonicalPathKey(tr.Dir)
		if tk == dirKey {
			directTracks = append(directTracks, cloneTrack(*tr))
			continue
		}
		if !strings.HasPrefix(tk, prefix) {
			continue
		}
		// Compute immediate child directory name preserving original case from tr.Dir.
		relOrig := tr.Dir[len(cleanDir):]
		relOrig = strings.TrimLeft(relOrig, `/\`)
		if relOrig == "" {
			continue
		}
		parts := strings.Split(filepath.ToSlash(relOrig), "/")
		childName := parts[0]
		childPath := filepath.Join(cleanDir, childName)
		ck := canonicalPathKey(childPath)

		agg := childrenMap[ck]
		if agg == nil {
			agg = &childAgg{
				path:      childPath,
				name:      childName,
				grandDirs: map[string]bool{},
			}
			childrenMap[ck] = agg
			childKeys = append(childKeys, ck)
		}
		agg.trackCount++
		if agg.coverID == "" && tr.CoverID != "" {
			agg.coverID = tr.CoverID
		}
		if len(parts) >= 2 && parts[1] != "" {
			agg.grandDirs[strings.ToLower(parts[1])] = true
		}
	}

	subdirs := make([]FolderEntry, 0, len(childKeys))
	for _, ck := range childKeys {
		agg := childrenMap[ck]
		subdirs = append(subdirs, FolderEntry{
			Path:        agg.path,
			Name:        agg.name,
			SubdirCount: len(agg.grandDirs),
			TrackCount:  agg.trackCount,
			CoverID:     agg.coverID,
		})
	}
	slices.SortFunc(subdirs, func(a, b FolderEntry) int {
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	slices.SortFunc(directTracks, compareAlbumTrackOrder)

	name := filepath.Base(cleanDir)
	if name == "" || name == "." || name == sep {
		name = cleanDir
	}
	return FolderNode{
		Path:    cleanDir,
		Name:    name,
		Parent:  parent,
		Subdirs: subdirs,
		Tracks:  directTracks,
	}
}

func buildFolderEntryLocked(rootPath, displayName string, tracks []LocalTrack) FolderEntry {
	cleanRoot := filepath.Clean(rootPath)
	rk := canonicalPathKey(cleanRoot)
	sep := string(filepath.Separator)
	prefix := rk
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	childDirs := map[string]bool{}
	trackCount := 0
	var coverID string

	for i := range tracks {
		tr := &tracks[i]
		tk := canonicalPathKey(tr.Dir)
		if tk != rk && !strings.HasPrefix(tk, prefix) {
			continue
		}
		trackCount++
		if coverID == "" && tr.CoverID != "" {
			coverID = tr.CoverID
		}
		if tk != rk {
			rel := strings.TrimLeft(tr.Dir[len(cleanRoot):], `/\`)
			if rel != "" {
				first := strings.Split(filepath.ToSlash(rel), "/")[0]
				if first != "" {
					childDirs[strings.ToLower(first)] = true
				}
			}
		}
	}
	return FolderEntry{
		Path:        cleanRoot,
		Name:        displayName,
		SubdirCount: len(childDirs),
		TrackCount:  trackCount,
		CoverID:     coverID,
	}
}

// Search queries local tracks, albums, and artists for unified search results.
func (l *Library) Search(query string, limit int) ([]ytm.Track, []ytm.AlbumProfile, []ytm.ArtistProfile) {
	qRaw := strings.ToLower(strings.TrimSpace(query))
	if qRaw == "" {
		return nil, nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	qNorm := normMatchKey(query)

	l.mu.RLock()
	defer l.mu.RUnlock()

	type scoredIdx struct {
		idx   int
		score int
	}

	var trHits []scoredIdx
	for i := range l.tracks {
		tr := &l.tracks[i]
		sc := matchRelevanceScore(qRaw, qNorm, tr.Title, strings.Join(tr.Artists, " "), tr.Album, tr.Composer)
		if sc > 0 {
			trHits = append(trHits, scoredIdx{idx: i, score: sc})
		}
	}
	slices.SortFunc(trHits, func(a, b scoredIdx) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		return cmp.Compare(a.idx, b.idx)
	})
	trHits = trHits[:min(len(trHits), limit)]
	outTracks := make([]ytm.Track, len(trHits))
	for i, h := range trHits {
		outTracks[i] = l.tracks[h.idx].ToYTMTrack()
	}

	var alHits []scoredIdx
	for i := range l.albums {
		al := &l.albums[i]
		sc := matchRelevanceScore(qRaw, qNorm, al.Title, al.AlbumArtist, strings.Join(al.Genres, " "))
		if sc > 0 {
			alHits = append(alHits, scoredIdx{idx: i, score: sc})
		}
	}
	slices.SortFunc(alHits, func(a, b scoredIdx) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		return cmp.Compare(a.idx, b.idx)
	})
	alHits = alHits[:min(len(alHits), limit)]
	outAlbums := make([]ytm.AlbumProfile, len(alHits))
	for i, h := range alHits {
		outAlbums[i] = l.albums[h.idx].ToAlbumProfile()
	}

	var arHits []scoredIdx
	for i := range l.artists {
		ar := &l.artists[i]
		sc := matchRelevanceScore(qRaw, qNorm, ar.Name, strings.Join(ar.Genres, " "))
		if sc > 0 {
			arHits = append(arHits, scoredIdx{idx: i, score: sc})
		}
	}
	slices.SortFunc(arHits, func(a, b scoredIdx) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		return cmp.Compare(a.idx, b.idx)
	})
	arHits = arHits[:min(len(arHits), limit)]
	outArtists := make([]ytm.ArtistProfile, len(arHits))
	for i, h := range arHits {
		outArtists[i] = l.artists[h.idx].ToArtistProfile()
	}

	return outTracks, outAlbums, outArtists
}

func matchRelevanceScore(qRaw, qNorm, primary string, secondary ...string) int {
	pLow := strings.ToLower(primary)
	pNorm := normMatchKey(primary)
	switch {
	case pLow == qRaw || (qNorm != "" && pNorm == qNorm):
		return 100
	case strings.HasPrefix(pLow, qRaw) || (qNorm != "" && strings.HasPrefix(pNorm, qNorm)):
		return 80
	case strings.Contains(pLow, qRaw) || (qNorm != "" && strings.Contains(pNorm, qNorm)):
		return 60
	}
	for _, s := range secondary {
		if s == "" {
			continue
		}
		sLow := strings.ToLower(s)
		sNorm := normMatchKey(s)
		if sLow == qRaw || (qNorm != "" && sNorm == qNorm) {
			return 50
		}
		if strings.Contains(sLow, qRaw) || (qNorm != "" && strings.Contains(sNorm, qNorm)) {
			return 35
		}
	}
	return 0
}

// --- Roon Genre Merge ---

// ApplyRoonGenres normalizes album and artist names (case-, punctuation-,
// whitespace-, and CJK/Kana-insensitively) to merge Roon genres onto matching
// local albums and their tracks, persists the updated index, and returns the
// number of matched local albums.
func (l *Library) ApplyRoonGenres(items []RoonAlbumGenre) int {
	if len(items) == 0 {
		return 0
	}

	type prepRoon struct {
		albNorm      string
		albStripNorm string
		artNorm      string
		rawArtist    string
		genres       []string
	}
	prepped := make([]prepRoon, 0, len(items))
	for _, it := range items {
		albNorm := normMatchKey(it.Album)
		if albNorm == "" {
			continue
		}
		var cleanGenres []string
		for _, g := range it.Genres {
			g = strings.TrimSpace(g)
			if g != "" && !containsFold(cleanGenres, g) {
				cleanGenres = append(cleanGenres, g)
			}
		}
		if len(cleanGenres) == 0 {
			continue
		}
		prepped = append(prepped, prepRoon{
			albNorm:      albNorm,
			albStripNorm: normMatchKey(stripEditionNotes(it.Album)),
			artNorm:      normMatchKey(it.Artist),
			rawArtist:    strings.TrimSpace(it.Artist),
			genres:       cleanGenres,
		})
	}
	if len(prepped) == 0 {
		return 0
	}

	l.mu.Lock()
	matchedAlbums := map[string][]string{}
	for i := range l.albums {
		al := &l.albums[i]
		localAlbNorm := normMatchKey(al.Title)
		localAlbStrip := normMatchKey(stripEditionNotes(al.Title))
		if localAlbNorm == "" {
			continue
		}
		for _, pr := range prepped {
			albMatch := localAlbNorm == pr.albNorm ||
				(localAlbStrip != "" && pr.albStripNorm != "" && localAlbStrip == pr.albStripNorm)
			if !albMatch {
				continue
			}
			if !roonArtistMatchesAlbum(pr.rawArtist, pr.artNorm, al) {
				continue
			}
			merged := matchedAlbums[al.ID]
			for _, g := range pr.genres {
				if !containsFold(merged, g) {
					merged = append(merged, g)
				}
			}
			matchedAlbums[al.ID] = merged
		}
	}

	if len(matchedAlbums) == 0 {
		l.mu.Unlock()
		return 0
	}

	for albID, genres := range matchedAlbums {
		existing := l.roonGenres[albID]
		for _, g := range genres {
			if !containsFold(existing, g) {
				existing = append(existing, g)
			}
		}
		l.roonGenres[albID] = existing
	}

	l.applyPersistedRoonGenresLocked()
	l.rebuildLocked()
	l.revision++
	l.mu.Unlock()

	_ = l.Save()
	return len(matchedAlbums)
}

func roonArtistMatchesAlbum(roonRaw, roonNorm string, al *LocalAlbum) bool {
	if roonNorm == "" || strings.EqualFold(roonRaw, "Various Artists") {
		return true
	}
	localAA := strings.TrimSpace(al.AlbumArtist)
	if localAA == "" || strings.EqualFold(localAA, "Various Artists") {
		return true
	}
	localNorm := normMatchKey(localAA)
	if artistNormCompatible(localNorm, roonNorm) {
		return true
	}
	for i := range al.Tracks {
		for _, ta := range al.Tracks[i].Artists {
			if artistNormCompatible(normMatchKey(ta), roonNorm) {
				return true
			}
		}
	}
	return false
}

func artistNormCompatible(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, rb := len([]rune(a)), len([]rune(b))
	if min(ra, rb) >= 3 && (strings.Contains(a, b) || strings.Contains(b, a)) {
		return true
	}
	return false
}

func (l *Library) applyPersistedRoonGenresLocked() {
	if len(l.roonGenres) == 0 {
		return
	}
	for k, cf := range l.files {
		grpDir := albumGroupDir(cf.Track.Dir)
		albID := cf.Track.AlbumID
		if albID == "" {
			albID = makeAlbumID(cf.Track.Album, cf.Track.AlbumArtist, grpDir)
		}
		if rgs, ok := l.roonGenres[albID]; ok {
			var missing []string
			for _, g := range rgs {
				if !containsFold(cf.Track.Genres, g) {
					missing = append(missing, g)
				}
			}
			if len(missing) > 0 {
				cf.Track = cloneTrack(cf.Track)
				cf.Track.Genres = append(cf.Track.Genres, missing...)
				l.files[k] = cf
			}
		}
	}
	for k, cc := range l.cues {
		cloned := false
		for i := range cc.Tracks {
			tr := &cc.Tracks[i]
			grpDir := albumGroupDir(tr.Dir)
			albID := tr.AlbumID
			if albID == "" {
				albID = makeAlbumID(tr.Album, tr.AlbumArtist, grpDir)
			}
			if rgs, ok := l.roonGenres[albID]; ok {
				for _, g := range rgs {
					if !containsFold(tr.Genres, g) {
						if !cloned {
							tracksCp := make([]LocalTrack, len(cc.Tracks))
							for ti := range cc.Tracks {
								tracksCp[ti] = cloneTrack(cc.Tracks[ti])
							}
							cc.Tracks = tracksCp
							tr = &cc.Tracks[i]
							cloned = true
						}
						tr.Genres = append(tr.Genres, g)
					}
				}
			}
		}
		if cloned {
			l.cues[k] = cc
		}
	}
}
