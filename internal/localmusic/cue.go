package localmusic

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var cueFallbackExts = []string{
	".flac", ".wav", ".aiff", ".aif", ".aifc", ".wv", ".dsf", ".dff", ".m4a", ".mp4", ".mp3", ".ogg",
}

type cueSheet struct {
	title       string
	performer   string
	songwriter  string
	genres      []string
	year        int
	discNo      int
	albumGainDB float64
	hasAlbGain  bool
	files       []cueFileBlock
}

type cueFileBlock struct {
	rawName string
	tracks  []cueTrackEntry
}

type cueTrackEntry struct {
	number      int
	mode        string
	title       string
	performer   string
	songwriter  string
	trackGainDB float64
	hasTrkGain  bool
	index00Ms   int64
	hasIndex00  bool
	index01Ms   int64
	hasIndex01  bool
}

var (
	cueTrackRe = regexp.MustCompile(`(?i)^TRACK\s+(\d+)\s+(\S+)`)
	cueIndexRe = regexp.MustCompile(`(?i)^INDEX\s+(\d+)\s+(\d+):(\d+):(\d+)`)
	discDirRe  = regexp.MustCompile(`(?i)^(?:disc|cd)[\s_-]*0*(\d+)$`)
)

// parseCueBytes parses raw CUE sheet bytes (auto-detecting UTF-8, UTF-16,
// GB18030, or Shift-JIS).
func parseCueBytes(data []byte) (*cueSheet, error) {
	text := decodeTextAuto(data)
	if text == "" {
		return nil, errors.New("localmusic: empty CUE sheet")
	}

	cs := &cueSheet{}
	var curFile *cueFileBlock
	var curTrack *cueTrackEntry

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "REM "):
			rem := strings.TrimSpace(line[4:])
			parseCueRem(rem, cs, curTrack)

		case strings.HasPrefix(upper, "FILE "):
			ref := extractCueFileArg(strings.TrimSpace(line[5:]))
			if ref != "" {
				cs.files = append(cs.files, cueFileBlock{rawName: ref})
				curFile = &cs.files[len(cs.files)-1]
				curTrack = nil
			}

		case strings.HasPrefix(upper, "TRACK "):
			if m := cueTrackRe.FindStringSubmatch(line); len(m) == 3 {
				if curFile == nil {
					cs.files = append(cs.files, cueFileBlock{})
					curFile = &cs.files[len(cs.files)-1]
				}
				num, _ := strconv.Atoi(m[1])
				curFile.tracks = append(curFile.tracks, cueTrackEntry{
					number: num,
					mode:   strings.ToUpper(m[2]),
				})
				curTrack = &curFile.tracks[len(curFile.tracks)-1]
			}

		case strings.HasPrefix(upper, "TITLE "):
			val := unquoteCueArg(strings.TrimSpace(line[6:]))
			if curTrack != nil {
				curTrack.title = val
			} else if cs.title == "" {
				cs.title = val
			}

		case strings.HasPrefix(upper, "PERFORMER "):
			val := unquoteCueArg(strings.TrimSpace(line[10:]))
			if curTrack != nil {
				curTrack.performer = val
			} else if cs.performer == "" {
				cs.performer = val
			}

		case strings.HasPrefix(upper, "SONGWRITER "):
			val := unquoteCueArg(strings.TrimSpace(line[11:]))
			if curTrack != nil {
				curTrack.songwriter = val
			} else if cs.songwriter == "" {
				cs.songwriter = val
			}

		case strings.HasPrefix(upper, "INDEX "):
			if curTrack != nil {
				if m := cueIndexRe.FindStringSubmatch(line); len(m) == 5 {
					idxNum, _ := strconv.Atoi(m[1])
					mm, _ := strconv.Atoi(m[2])
					ss, _ := strconv.Atoi(m[3])
					ff, _ := strconv.Atoi(m[4])
					ms := int64(mm*60+ss)*1000 + int64(ff)*1000/75
					switch idxNum {
					case 0:
						curTrack.index00Ms = ms
						curTrack.hasIndex00 = true
					case 1:
						curTrack.index01Ms = ms
						curTrack.hasIndex01 = true
					}
				}
			}
		}
	}

	if len(cs.files) == 0 {
		return nil, errors.New("localmusic: CUE sheet contains no FILE entries")
	}
	return cs, nil
}

func parseCueRem(rem string, cs *cueSheet, curTrack *cueTrackEntry) {
	fields := strings.Fields(rem)
	if len(fields) < 2 {
		return
	}
	key := strings.ToUpper(fields[0])
	val := unquoteCueArg(strings.TrimSpace(rem[len(fields[0]):]))
	if val == "" {
		return
	}
	switch key {
	case "GENRE", "STYLE":
		for _, g := range expandGenreValue(val) {
			if g != "" && !containsFold(cs.genres, g) {
				cs.genres = append(cs.genres, g)
			}
		}
	case "DATE", "YEAR", "ORIGINALDATE":
		if y := parseYear(val); y > 0 && cs.year == 0 {
			cs.year = y
		}
	case "DISCNUMBER", "DISC":
		if d := parseIndexPair(val); d > 0 && cs.discNo == 0 {
			cs.discNo = d
		}
	case "COMPOSER":
		if curTrack != nil && curTrack.songwriter == "" {
			curTrack.songwriter = val
		} else if cs.songwriter == "" {
			cs.songwriter = val
		}
	case "REPLAYGAIN_ALBUM_GAIN":
		if g, ok := parseGainDB(val); ok {
			cs.albumGainDB = g
			cs.hasAlbGain = true
		}
	case "REPLAYGAIN_TRACK_GAIN":
		if curTrack != nil {
			if g, ok := parseGainDB(val); ok {
				curTrack.trackGainDB = g
				curTrack.hasTrkGain = true
			}
		}
	}
}

func unquoteCueArg(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func extractCueFileArg(rest string) string {
	if len(rest) == 0 {
		return ""
	}
	if rest[0] == '"' {
		if end := strings.IndexByte(rest[1:], '"'); end >= 0 {
			return rest[1 : 1+end]
		}
		// Fallback to last quote when filename contains quotes.
		if end := strings.LastIndexByte(rest[1:], '"'); end >= 0 {
			return rest[1 : 1+end]
		}
	}
	// Unquoted filename before trailing file type token (e.g. WAVE, MP3).
	parts := strings.Fields(rest)
	if len(parts) > 1 {
		return strings.Join(parts[:len(parts)-1], " ")
	}
	return rest
}

// resolveCueAudioFile resolves a CUE FILE reference relative to cuePath's
// directory, falling back to sibling lossless/audio extensions when the
// referenced file was transcoded (e.g. CDImage.wav -> CDImage.flac).
func resolveCueAudioFile(cuePath, refName string) (string, os.FileInfo, bool) {
	cueDir := filepath.Dir(cuePath)
	rel := filepath.FromSlash(strings.ReplaceAll(refName, "\\", "/"))
	exact := filepath.Join(cueDir, rel)
	if st, err := os.Stat(exact); err == nil && !st.IsDir() {
		return filepath.Clean(exact), st, true
	}

	searchDir := filepath.Dir(exact)
	wantBase := strings.TrimSuffix(filepath.Base(exact), filepath.Ext(exact))
	if found, st, ok := findAudioByBase(searchDir, wantBase); ok {
		return found, st, true
	}

	cueBase := strings.TrimSuffix(filepath.Base(cuePath), filepath.Ext(cuePath))
	if !strings.EqualFold(cueBase, wantBase) {
		if found, st, ok := findAudioByBase(cueDir, cueBase); ok {
			return found, st, true
		}
	}
	return "", nil, false
}

func findAudioByBase(dir, base string) (string, os.FileInfo, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, false
	}
	for _, ext := range cueFallbackExts {
		want := base + ext
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			if strings.EqualFold(ent.Name(), want) {
				full := filepath.Join(dir, ent.Name())
				if st, err := ent.Info(); err == nil {
					return filepath.Clean(full), st, true
				}
			}
		}
	}
	return "", nil, false
}

func parseDiscNumberFromDir(dir string) int {
	if m := discDirRe.FindStringSubmatch(strings.TrimSpace(filepath.Base(dir))); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// loadCueTracks parses cuePath, resolves its referenced audio files, extracts
// audio specs and covers, and returns the sliced CUE tracks plus the list of
// referenced audio file paths consumed by the CUE sheet.
func loadCueTracks(cuePath, coverDir string, addedAt int64, dirCovers map[string]string) ([]LocalTrack, []string, error) {
	raw, err := os.ReadFile(cuePath)
	if err != nil {
		return nil, nil, err
	}
	cs, err := parseCueBytes(raw)
	if err != nil {
		return nil, nil, err
	}

	cueDir := filepath.Clean(filepath.Dir(cuePath))
	discNo := cs.discNo
	if discNo == 0 {
		discNo = parseDiscNumberFromDir(cueDir)
	}

	var (
		tracks   []LocalTrack
		consumed []string
		seenCons = map[string]bool{}
	)

	for _, fb := range cs.files {
		if fb.rawName == "" || len(fb.tracks) == 0 {
			continue
		}
		audioPath, audioInfo, ok := resolveCueAudioFile(cuePath, fb.rawName)
		if !ok {
			continue
		}
		ckey := canonicalPathKey(audioPath)
		if !seenCons[ckey] {
			seenCons[ckey] = true
			consumed = append(consumed, audioPath)
		}

		audioMeta, err := parseAudioFile(audioPath, audioInfo.Size())
		if err != nil {
			continue
		}

		albumTitle := cs.title
		if albumTitle == "" {
			albumTitle = audioMeta.album
		}
		if albumTitle == "" {
			albumTitle = defaultAlbumFromDir(cueDir)
		}

		albumArtist := cs.performer
		if albumArtist == "" {
			albumArtist = audioMeta.albumArtist
		}

		var coverID string
		if len(audioMeta.cover) > 0 {
			coverID, _ = saveCoverBytes(coverDir, audioMeta.cover)
		}
		if coverID == "" {
			coverID = findFolderCover(cueDir, albumTitle, coverDir, dirCovers)
		}

		year := cs.year
		if year == 0 {
			year = audioMeta.year
		}
		genres := append([]string(nil), cs.genres...)
		for _, g := range audioMeta.genres {
			if !containsFold(genres, g) {
				genres = append(genres, g)
			}
		}
		effDiscNo := discNo
		if effDiscNo == 0 {
			effDiscNo = audioMeta.discNo
		}

		for i, te := range fb.tracks {
			if te.mode != "AUDIO" {
				continue
			}
			var startMs int64
			if te.hasIndex01 {
				startMs = te.index01Ms
			} else if te.hasIndex00 {
				startMs = te.index00Ms
			}

			var endMs int64
			if i+1 < len(fb.tracks) {
				nxt := fb.tracks[i+1]
				if nxt.mode != "AUDIO" && nxt.hasIndex00 {
					endMs = nxt.index00Ms
				} else if nxt.hasIndex01 {
					endMs = nxt.index01Ms
				} else if nxt.hasIndex00 {
					endMs = nxt.index00Ms
				}
			} else if audioMeta.durationMs > 0 {
				endMs = audioMeta.durationMs
			}

			var durMs int64
			if endMs > startMs {
				durMs = endMs - startMs
			}

			spec := audioMeta.spec
			spec.IsCUE = true
			spec.ClipStartMs = startMs
			spec.ClipEndMs = endMs

			title := te.title
			if title == "" {
				title = fmt.Sprintf("Track %02d", te.number)
			}

			var artists []string
			if te.performer != "" {
				artists = splitTagValues(te.performer)
			} else if albumArtist != "" {
				artists = splitTagValues(albumArtist)
			} else if len(audioMeta.artists) > 0 {
				artists = append([]string(nil), audioMeta.artists...)
			}
			effAlbumArtist := albumArtist
			if effAlbumArtist == "" && len(artists) > 0 {
				effAlbumArtist = artists[0]
			}

			composer := te.songwriter
			if composer == "" {
				composer = cs.songwriter
			}
			if composer == "" {
				composer = audioMeta.composer
			}

			gainDB := audioMeta.gainDB
			if cs.hasAlbGain {
				gainDB = cs.albumGainDB
			}
			if te.hasTrkGain {
				gainDB = te.trackGainDB
			}

			artistIDs := make([]string, len(artists))
			for ai, a := range artists {
				artistIDs[ai] = makeArtistID(a)
			}
			albID := makeAlbumID(albumTitle, effAlbumArtist, albumGroupDir(cueDir))

			tracks = append(tracks, LocalTrack{
				ID:             makeTrackID(audioPath, true, te.number, startMs),
				Path:           audioPath,
				Dir:            cueDir,
				Title:          title,
				Artists:        artists,
				Album:          albumTitle,
				AlbumArtist:    effAlbumArtist,
				AlbumID:        albID,
				ArtistIDs:      artistIDs,
				Genres:         append([]string(nil), genres...),
				Composer:       composer,
				Year:           year,
				TrackNo:        te.number,
				DiscNo:         effDiscNo,
				DurationMs:     durMs,
				CoverID:        coverID,
				Spec:           spec,
				GainDB:         gainDB,
				AddedAt:        addedAt,
				Size:           audioInfo.Size(),
				ModTimeNs:      audioInfo.ModTime().UnixNano(),
				EmbeddedLyrics: audioMeta.lyrics,
			})
		}
	}

	return tracks, consumed, nil
}

func defaultAlbumFromDir(dir string) string {
	base := filepath.Base(dir)
	if discDirRe.MatchString(base) {
		parent := filepath.Base(filepath.Dir(dir))
		if parent != "" && parent != "." && parent != string(filepath.Separator) {
			return parent
		}
	}
	return base
}
