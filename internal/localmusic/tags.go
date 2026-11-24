package localmusic

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const maxMetaChunkBytes = 64 << 20 // 64 MiB cap on single metadata/cover block

type parsedMeta struct {
	title        string
	artists      []string
	album        string
	albumArtist  string
	genres       []string
	composer     string
	year         int
	trackNo      int
	discNo       int
	durationMs   int64
	gainDB       float64
	hasTrackGain bool
	hasAlbumGain bool
	lyrics       string
	cover        []byte
	coverType    int // 3 = front cover
	spec         AudioSpec
}

func (m *parsedMeta) setCover(data []byte, picType int) {
	if len(data) == 0 {
		return
	}
	if len(m.cover) == 0 || (m.coverType != 3 && picType == 3) {
		m.cover = data
		m.coverType = picType
	}
}

func (m *parsedMeta) addArtist(vals ...string) {
	for _, v := range vals {
		for _, part := range splitTagValues(v) {
			if part != "" && !containsFold(m.artists, part) {
				m.artists = append(m.artists, part)
			}
		}
	}
}

func (m *parsedMeta) addGenre(vals ...string) {
	for _, v := range vals {
		for _, part := range expandGenreValue(v) {
			if part != "" && !containsFold(m.genres, part) {
				m.genres = append(m.genres, part)
			}
		}
	}
}

func (m *parsedMeta) setGain(raw string, isTrack bool) {
	g, ok := parseGainDB(raw)
	if !ok {
		return
	}
	if isTrack {
		m.gainDB = g
		m.hasTrackGain = true
	} else if !m.hasTrackGain && !m.hasAlbumGain {
		m.gainDB = g
		m.hasAlbumGain = true
	}
}

func containsFold(list []string, s string) bool {
	for _, e := range list {
		if strings.EqualFold(e, s) {
			return true
		}
	}
	return false
}

func splitTagValues(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var parts []string
	if strings.Contains(s, ";") {
		parts = strings.Split(s, ";")
	} else if strings.Contains(s, " / ") {
		parts = strings.Split(s, " / ")
	} else {
		parts = []string{s}
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var (
	yearRe    = regexp.MustCompile(`\b(1[6-9]\d{2}|20\d{2}|21\d{2})\b`)
	id3v1Gen  = regexp.MustCompile(`^\((\d+)\)(.*)$`)
	gainNumRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)
)

func parseYear(s string) int {
	m := yearRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0
	}
	y, _ := strconv.Atoi(m[1])
	return y
}

func parseIndexPair(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if idx := strings.IndexByte(s, '/'); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	n, _ := strconv.Atoi(s)
	if n < 0 {
		return 0
	}
	return n
}

func parseGainDB(s string) (float64, bool) {
	m := gainNumRe.FindString(s)
	if m == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(m, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

var id3v1Genres = [...]string{
	"Blues", "Classic Rock", "Country", "Dance", "Disco", "Funk", "Grunge", "Hip-Hop",
	"Jazz", "Metal", "New Age", "Oldies", "Other", "Pop", "R&B", "Rap",
	"Reggae", "Rock", "Techno", "Industrial", "Alternative", "Ska", "Death Metal", "Pranks",
	"Soundtrack", "Euro-Techno", "Ambient", "Trip-Hop", "Vocal", "Jazz+Funk", "Fusion", "Trance",
	"Classical", "Instrumental", "Acid", "House", "Game", "Sound Clip", "Gospel", "Noise",
	"AlternRock", "Bass", "Soul", "Punk", "Space", "Meditative", "Instrumental Pop", "Instrumental Rock",
	"Ethnic", "Gothic", "Darkwave", "Techno-Industrial", "Electronic", "Pop-Folk", "Eurodance", "Dream",
	"Southern Rock", "Comedy", "Cult", "Gangsta", "Top 40", "Christian Rap", "Pop/Funk", "Jungle",
	"Native American", "Cabaret", "New Wave", "Psychadelic", "Rave", "Showtunes", "Trailer", "Lo-Fi",
	"Tribal", "Acid Punk", "Acid Jazz", "Polka", "Retro", "Musical", "Rock & Roll", "Hard Rock",
}

func expandGenreValue(s string) []string {
	var out []string
	for _, raw := range splitTagValues(s) {
		if m := id3v1Gen.FindStringSubmatch(raw); len(m) == 3 {
			idx, err := strconv.Atoi(m[1])
			rest := strings.TrimSpace(m[2])
			if rest != "" {
				out = append(out, rest)
				continue
			}
			if err == nil && idx >= 0 && idx < len(id3v1Genres) {
				out = append(out, id3v1Genres[idx])
				continue
			}
		}
		if idx, err := strconv.Atoi(raw); err == nil && idx >= 0 && idx < len(id3v1Genres) {
			out = append(out, id3v1Genres[idx])
			continue
		}
		out = append(out, raw)
	}
	return out
}

// parseAudioFile parses audio specification, tags, and embedded cover art from path.
func parseAudioFile(path string, fileSize int64) (*parsedMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if fileSize <= 0 {
		if st, err := f.Stat(); err == nil {
			fileSize = st.Size()
		}
	}

	ext := strings.ToLower(filepath.Ext(path))
	meta := &parsedMeta{}

	var parseErr error
	switch ext {
	case ".flac":
		parseErr = parseFLAC(f, fileSize, meta)
	case ".wav":
		parseErr = parseWAV(f, fileSize, meta)
	case ".aif", ".aiff", ".aifc":
		parseErr = parseAIFF(f, fileSize, meta)
	case ".dsf":
		parseErr = parseDSF(f, fileSize, meta)
	case ".dff":
		parseErr = parseDFF(f, fileSize, meta)
	case ".m4a", ".mp4":
		parseErr = parseMP4(f, fileSize, meta)
	case ".mp3":
		parseErr = parseMP3(f, fileSize, meta)
	case ".ogg":
		parseErr = parseOGG(f, fileSize, meta)
	case ".wv":
		parseErr = parseWavPack(f, fileSize, meta)
	default:
		return nil, errors.New("localmusic: unsupported audio format " + ext)
	}
	if parseErr != nil {
		return nil, parseErr
	}

	meta.spec.computeFlags()
	if meta.spec.BitrateKbps <= 0 && fileSize > 0 && meta.durationMs > 0 {
		meta.spec.BitrateKbps = int((fileSize * 8) / meta.durationMs)
	}
	return meta, nil
}

func mulDiv1000(count, rate uint64) int64 {
	if count == 0 || rate == 0 {
		return 0
	}
	whole := count / rate
	rem := count % rate
	if whole > math.MaxInt64/1000 {
		return math.MaxInt64
	}
	ms := whole*1000 + (rem*1000)/rate
	if ms > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(ms)
}

// --- FLAC ---

func parseFLAC(f *os.File, fileSize int64, m *parsedMeta) error {
	off := int64(0)
	var head [10]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return err
	}
	if string(head[:3]) == "ID3" {
		tagSize := synchsafe32(head[6:10])
		if tagSize > 0 && 10+int64(tagSize) <= fileSize && tagSize <= maxMetaChunkBytes {
			buf := make([]byte, 10+tagSize)
			if _, err := f.ReadAt(buf, 0); err == nil {
				parseID3v2(buf, m)
			}
		}
		off = 10 + int64(tagSize)
	}

	var magic [4]byte
	if _, err := f.ReadAt(magic[:], off); err != nil || string(magic[:]) != "fLaC" {
		return errors.New("localmusic: invalid FLAC header")
	}
	off += 4
	m.spec.Codec = "FLAC"
	m.spec.IsLossless = true

	for off+4 <= fileSize {
		var bh [4]byte
		if _, err := f.ReadAt(bh[:], off); err != nil {
			break
		}
		last := bh[0]&0x80 != 0
		blockType := bh[0] & 0x7F
		blockLen := int64(bh[1])<<16 | int64(bh[2])<<8 | int64(bh[3])
		off += 4
		if blockLen < 0 || off+blockLen > fileSize {
			break
		}

		switch blockType {
		case 0: // STREAMINFO (34 bytes)
			if blockLen >= 34 {
				var si [34]byte
				if _, err := f.ReadAt(si[:], off); err == nil {
					v := binary.BigEndian.Uint64(si[10:18])
					sr := int((v >> 44) & 0xFFFFF)
					ch := int(((v >> 41) & 0x07) + 1)
					bps := int(((v >> 36) & 0x1F) + 1)
					samples := int64(v & 0xFFFFFFFFF)
					m.spec.SampleRate = sr
					m.spec.Channels = ch
					m.spec.BitDepth = bps
					if sr > 0 && samples > 0 {
						m.durationMs = samples * 1000 / int64(sr)
					}
				}
			}
		case 4: // VORBIS_COMMENT
			if blockLen > 0 && blockLen <= maxMetaChunkBytes {
				buf := make([]byte, blockLen)
				if _, err := f.ReadAt(buf, off); err == nil {
					parseVorbisComment(buf, m)
				}
			}
		case 6: // PICTURE
			if blockLen > 0 && blockLen <= maxMetaChunkBytes {
				buf := make([]byte, blockLen)
				if _, err := f.ReadAt(buf, off); err == nil {
					parseFLACPicture(buf, m)
				}
			}
		}

		off += blockLen
		if last {
			break
		}
	}
	return nil
}

func parseVorbisComment(b []byte, m *parsedMeta) {
	if len(b) < 8 {
		return
	}
	vendorLen := uint64(binary.LittleEndian.Uint32(b[0:4]))
	if vendorLen > uint64(len(b)-8) {
		return
	}
	pos := 4 + int(vendorLen)
	count := binary.LittleEndian.Uint32(b[pos : pos+4])
	pos += 4
	for i := uint32(0); i < count && pos+4 <= len(b); i++ {
		cLen := uint64(binary.LittleEndian.Uint32(b[pos : pos+4]))
		pos += 4
		if cLen > uint64(len(b)-pos) {
			break
		}
		line := string(b[pos : pos+int(cLen)])
		pos += int(cLen)
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(line[:eq]))
		val := strings.TrimSpace(line[eq+1:])
		if val == "" {
			continue
		}
		applyVorbisKeyVal(key, val, m)
	}
}

func applyVorbisKeyVal(key, val string, m *parsedMeta) {
	switch key {
	case "TITLE":
		if m.title == "" {
			m.title = val
		}
	case "ARTIST", "PERFORMER":
		m.addArtist(val)
	case "ALBUM":
		if m.album == "" {
			m.album = val
		}
	case "ALBUMARTIST", "ALBUM ARTIST":
		if m.albumArtist == "" {
			m.albumArtist = val
		}
	case "GENRE", "STYLE":
		m.addGenre(val)
	case "DATE", "YEAR", "ORIGINALDATE":
		if y := parseYear(val); y > 0 && (m.year == 0 || key != "ORIGINALDATE") {
			if m.year == 0 {
				m.year = y
			}
		}
	case "COMPOSER":
		if m.composer == "" {
			m.composer = val
		}
	case "TRACKNUMBER", "TRACK":
		if m.trackNo == 0 {
			m.trackNo = parseIndexPair(val)
		}
	case "DISCNUMBER", "DISC":
		if m.discNo == 0 {
			m.discNo = parseIndexPair(val)
		}
	case "REPLAYGAIN_TRACK_GAIN":
		m.setGain(val, true)
	case "REPLAYGAIN_ALBUM_GAIN":
		m.setGain(val, false)
	case "LYRICS", "UNSYNCEDLYRICS":
		if m.lyrics == "" {
			m.lyrics = val
		}
	case "METADATA_BLOCK_PICTURE":
		if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(val)); err == nil {
			parseFLACPicture(raw, m)
		}
	}
}

func parseFLACPicture(b []byte, m *parsedMeta) {
	if len(b) < 32 {
		return
	}
	picType := int(binary.BigEndian.Uint32(b[0:4]))
	mimeLen := uint64(binary.BigEndian.Uint32(b[4:8]))
	if mimeLen > uint64(len(b)-32) {
		return
	}
	mime := string(b[8 : 8+int(mimeLen)])
	pos := 8 + int(mimeLen)
	descLen := uint64(binary.BigEndian.Uint32(b[pos : pos+4]))
	pos += 4
	if len(b)-pos < 20 || descLen > uint64(len(b)-pos-20) {
		return
	}
	pos += int(descLen) + 16 // skip width, height, depth, colors
	dataLen := uint64(binary.BigEndian.Uint32(b[pos : pos+4]))
	pos += 4
	if dataLen == 0 || dataLen > uint64(len(b)-pos) || mime == "-->" {
		return
	}
	m.setCover(bytes.Clone(b[pos:pos+int(dataLen)]), picType)
}

// --- WAV ---

func parseWAV(f *os.File, fileSize int64, m *parsedMeta) error {
	var riff [12]byte
	if _, err := f.ReadAt(riff[:], 0); err != nil {
		return err
	}
	magic := string(riff[0:4])
	if (magic != "RIFF" && magic != "RF64") || string(riff[8:12]) != "WAVE" {
		return errors.New("localmusic: invalid WAV header")
	}
	m.spec.Codec = "WAV"
	m.spec.IsLossless = true

	var (
		byteRate    uint32
		blockAlign  uint16
		rf64DataLen uint64
		haveFmt     bool
	)
	off := int64(12)
	for off+8 <= fileSize {
		var chdr [8]byte
		if _, err := f.ReadAt(chdr[:], off); err != nil {
			break
		}
		cid := string(chdr[0:4])
		csize32 := binary.LittleEndian.Uint32(chdr[4:8])
		off += 8
		rem := uint64(fileSize - off)

		if cid == "ds64" && csize32 >= 28 && rem >= 28 {
			var ds [28]byte
			if _, err := f.ReadAt(ds[:], off); err == nil {
				rf64DataLen = binary.LittleEndian.Uint64(ds[8:16])
			}
		}

		actualU := uint64(csize32)
		if cid == "data" && csize32 == 0xFFFFFFFF && rf64DataLen > 0 {
			actualU = rf64DataLen
		}
		if actualU > rem {
			actualU = rem
		}
		actualSize := int64(actualU)

		switch cid {
		case "fmt ":
			if actualSize >= 16 && actualSize <= 4096 {
				b := make([]byte, actualSize)
				if _, err := f.ReadAt(b, off); err == nil {
					tag := binary.LittleEndian.Uint16(b[0:2])
					ch := int(binary.LittleEndian.Uint16(b[2:4]))
					sr := binary.LittleEndian.Uint32(b[4:8])
					byteRate = binary.LittleEndian.Uint32(b[8:12])
					blockAlign = binary.LittleEndian.Uint16(b[12:14])
					bps := int(binary.LittleEndian.Uint16(b[14:16]))
					if tag == 0xFFFE && len(b) >= 40 {
						validBits := int(binary.LittleEndian.Uint16(b[18:20]))
						if validBits > 0 && validBits <= bps {
							bps = validBits
						}
					}
					if sr > 0 && sr <= maxDSDSampleRate && ch > 0 {
						m.spec.Channels = ch
						m.spec.SampleRate = int(sr)
						m.spec.BitDepth = bps
						haveFmt = true
					}
				}
			}
		case "data":
			if haveFmt && m.spec.SampleRate > 0 {
				if blockAlign > 0 {
					frames := actualU / uint64(blockAlign)
					m.durationMs = mulDiv1000(frames, uint64(m.spec.SampleRate))
				} else if byteRate > 0 {
					m.durationMs = mulDiv1000(actualU, uint64(byteRate))
				}
			}
		case "LIST":
			if actualSize >= 4 && actualSize <= maxMetaChunkBytes {
				b := make([]byte, actualSize)
				if _, err := f.ReadAt(b, off); err == nil && string(b[0:4]) == "INFO" {
					parseWAVInfo(b[4:], m)
				}
			}
		case "id3 ", "ID3 ":
			if actualSize >= 10 && actualSize <= maxMetaChunkBytes {
				b := make([]byte, actualSize)
				if _, err := f.ReadAt(b, off); err == nil {
					parseID3v2(b, m)
				}
			}
		}

		step := actualU
		if step&1 == 1 && step < rem {
			step++
		}
		off += int64(step)
	}
	if !haveFmt {
		return errors.New("localmusic: WAV missing fmt chunk")
	}
	return nil
}

func parseWAVInfo(b []byte, m *parsedMeta) {
	pos := 0
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		sz := uint64(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		pos += 8
		rem := uint64(len(b) - pos)
		if sz > rem {
			break
		}
		val := decodeTextAuto(b[pos : pos+int(sz)])
		step := sz
		if step&1 == 1 && step < rem {
			step++
		}
		pos += int(step)
		if val == "" {
			continue
		}
		switch id {
		case "INAM":
			if m.title == "" {
				m.title = val
			}
		case "IART":
			if len(m.artists) == 0 {
				m.addArtist(val)
			}
		case "IPRD":
			if m.album == "" {
				m.album = val
			}
		case "IGNR":
			m.addGenre(val)
		case "ICRD":
			if m.year == 0 {
				m.year = parseYear(val)
			}
		case "ITRK", "IPRT":
			if m.trackNo == 0 {
				m.trackNo = parseIndexPair(val)
			}
		}
	}
}

// --- AIFF / AIFC ---

func parseAIFF(f *os.File, fileSize int64, m *parsedMeta) error {
	var form [12]byte
	if _, err := f.ReadAt(form[:], 0); err != nil {
		return err
	}
	ft := string(form[8:12])
	if string(form[0:4]) != "FORM" || (ft != "AIFF" && ft != "AIFC") {
		return errors.New("localmusic: invalid AIFF header")
	}
	m.spec.Codec = "AIFF"
	m.spec.IsLossless = true

	haveComm := false
	off := int64(12)
	for off+8 <= fileSize {
		var chdr [8]byte
		if _, err := f.ReadAt(chdr[:], off); err != nil {
			break
		}
		cid := string(chdr[0:4])
		csizeU := uint64(binary.BigEndian.Uint32(chdr[4:8]))
		off += 8
		rem := uint64(fileSize - off)
		if csizeU > rem {
			csizeU = rem
		}
		csize := int64(csizeU)

		switch cid {
		case "COMM":
			if csize >= 18 && csize <= 1024 {
				b := make([]byte, csize)
				if _, err := f.ReadAt(b, off); err == nil {
					ch := int(int16(binary.BigEndian.Uint16(b[0:2])))
					frames := uint64(binary.BigEndian.Uint32(b[2:6]))
					sampleSize := int(int16(binary.BigEndian.Uint16(b[6:8])))
					rateF := parseIEEE80(b[8:18])
					if ch > 0 && sampleSize > 0 && !math.IsNaN(rateF) && !math.IsInf(rateF, 0) && rateF >= 1 && rateF <= maxDSDSampleRate {
						sr := int(math.Round(rateF))
						m.spec.Channels = ch
						m.spec.BitDepth = sampleSize
						m.spec.SampleRate = sr
						if sr > 0 && frames > 0 {
							m.durationMs = mulDiv1000(frames, uint64(sr))
						}
						haveComm = true
					}
				}
			}
		case "NAME":
			if csize > 0 && csize <= 4096 && m.title == "" {
				b := make([]byte, csize)
				if _, err := f.ReadAt(b, off); err == nil {
					m.title = decodeTextAuto(b)
				}
			}
		case "AUTH":
			if csize > 0 && csize <= 4096 && len(m.artists) == 0 {
				b := make([]byte, csize)
				if _, err := f.ReadAt(b, off); err == nil {
					m.addArtist(decodeTextAuto(b))
				}
			}
		case "ID3 ", "id3 ":
			if csize >= 10 && csize <= maxMetaChunkBytes {
				b := make([]byte, csize)
				if _, err := f.ReadAt(b, off); err == nil {
					parseID3v2(b, m)
				}
			}
		}

		step := csizeU
		if step&1 == 1 && step < rem {
			step++
		}
		off += int64(step)
	}
	if !haveComm {
		return errors.New("localmusic: AIFF missing COMM chunk")
	}
	return nil
}

// parseIEEE80 decodes an 80-bit big-endian IEEE 754 extended float (as used in AIFF COMM).
func parseIEEE80(b []byte) float64 {
	if len(b) < 10 {
		return 0
	}
	sign := b[0]&0x80 != 0
	exp := int(uint16(b[0]&0x7F)<<8 | uint16(b[1]))
	mant := binary.BigEndian.Uint64(b[2:10])
	if exp == 0 && mant == 0 {
		return 0
	}
	if exp == 0x7FFF {
		return 0
	}
	val := math.Ldexp(float64(mant), exp-16383-63)
	if sign {
		return -val
	}
	return val
}

// --- DSF & DFF (DSD64 - DSD512) ---

func parseDSF(f *os.File, fileSize int64, m *parsedMeta) error {
	if fileSize < 28+52 {
		return errors.New("localmusic: invalid DSF header")
	}
	var dsdHdr [28]byte
	if _, err := f.ReadAt(dsdHdr[:], 0); err != nil {
		return err
	}
	if string(dsdHdr[0:4]) != "DSD " {
		return errors.New("localmusic: invalid DSF header")
	}
	dsdChunkU := binary.LittleEndian.Uint64(dsdHdr[4:12])
	if dsdChunkU < 28 || dsdChunkU > uint64(fileSize) {
		return errors.New("localmusic: invalid DSF header")
	}
	dsdChunkSize := int64(dsdChunkU)
	if fileSize-dsdChunkSize < 52 {
		return errors.New("localmusic: DSF missing fmt chunk")
	}
	metaOffU := binary.LittleEndian.Uint64(dsdHdr[20:28])

	var fmtHdr [52]byte
	if _, err := f.ReadAt(fmtHdr[:], dsdChunkSize); err != nil {
		return err
	}
	if string(fmtHdr[0:4]) != "fmt " {
		return errors.New("localmusic: DSF missing fmt chunk")
	}
	fmtChunkU := binary.LittleEndian.Uint64(fmtHdr[4:12])
	channelsU := binary.LittleEndian.Uint32(fmtHdr[24:28])
	sampleRateU := binary.LittleEndian.Uint32(fmtHdr[28:32])
	sampleCount := binary.LittleEndian.Uint64(fmtHdr[36:44])
	blockSizeU := binary.LittleEndian.Uint32(fmtHdr[44:48])
	if channelsU < 1 || channelsU > 64 || sampleRateU < 1 || sampleRateU > maxDSDSampleRate {
		return errors.New("localmusic: invalid DSF format parameters")
	}
	channels := int(channelsU)
	sampleRate := int(sampleRateU)

	// Bound sampleCount by actual data chunk payload when present on disk.
	if fmtChunkU >= 52 && fmtChunkU <= uint64(fileSize-dsdChunkSize) {
		dataHdrOff := dsdChunkSize + int64(fmtChunkU)
		if fileSize-dataHdrOff >= 12 {
			var dataHdr [12]byte
			if _, err := f.ReadAt(dataHdr[:], dataHdrOff); err == nil && string(dataHdr[0:4]) == "data" {
				dataChunkU := binary.LittleEndian.Uint64(dataHdr[4:12])
				if dataChunkU >= 12 {
					availU := min(dataChunkU-12, uint64(fileSize-(dataHdrOff+12)))
					var maxSamples uint64
					if blockSizeU >= 1 && blockSizeU <= 65536 {
						groupU := uint64(channels) * uint64(blockSizeU)
						maxSamples = (availU / groupU) * uint64(blockSizeU) * 8
					} else {
						maxSamples = (availU / uint64(channels)) * 8
					}
					if sampleCount == 0 || sampleCount > maxSamples {
						sampleCount = maxSamples
					}
				}
			}
		}
	}

	m.spec.Codec = dsdCodecName(sampleRate)
	m.spec.SampleRate = sampleRate
	m.spec.BitDepth = 1
	m.spec.Channels = channels
	m.spec.BitrateKbps = int((uint64(sampleRate) * uint64(channels)) / 1000)
	m.spec.IsDSD = true
	m.spec.IsLossless = true
	m.spec.IsHiRes = true
	if sampleCount > 0 {
		m.durationMs = mulDiv1000(sampleCount, uint64(sampleRate))
	}

	if metaOffU > 0 && fileSize >= 10 && metaOffU <= uint64(fileSize-10) {
		metaOff := int64(metaOffU)
		var id3h [10]byte
		if _, err := f.ReadAt(id3h[:], metaOff); err == nil && string(id3h[0:3]) == "ID3" {
			tagLen := int64(10 + synchsafe32(id3h[6:10]))
			if tagLen > fileSize-metaOff {
				tagLen = fileSize - metaOff
			}
			if tagLen >= 10 && tagLen <= maxMetaChunkBytes {
				buf := make([]byte, tagLen)
				if _, err := f.ReadAt(buf, metaOff); err == nil {
					parseID3v2(buf, m)
				}
			}
		}
	}
	return nil
}

func parseDFF(f *os.File, fileSize int64, m *parsedMeta) error {
	var frm [16]byte
	if _, err := f.ReadAt(frm[:], 0); err != nil {
		return err
	}
	if string(frm[0:4]) != "FRM8" || string(frm[12:16]) != "DSD " {
		return errors.New("localmusic: invalid DSDIFF header")
	}

	var dsdDataBytes uint64
	off := int64(16)
	for off+12 <= fileSize {
		var chdr [12]byte
		if _, err := f.ReadAt(chdr[:], off); err != nil {
			break
		}
		cid := string(chdr[0:4])
		csizeU := binary.BigEndian.Uint64(chdr[4:12])
		off += 12
		rem := uint64(fileSize - off)
		if csizeU > rem {
			csizeU = rem
		}
		csize := int64(csizeU)

		switch cid {
		case "PROP":
			if csize >= 4 && csize <= maxMetaChunkBytes {
				b := make([]byte, csize)
				if _, err := f.ReadAt(b, off); err == nil && string(b[0:4]) == "SND " {
					parseDFFProp(b[4:], m)
				}
			}
		case "DSD ":
			dsdDataBytes = csizeU
		case "DIIN":
			if csize > 0 && csize <= 1<<20 {
				b := make([]byte, csize)
				if _, err := f.ReadAt(b, off); err == nil {
					parseDFFDiin(b, m)
				}
			}
		case "ID3 ", "id3 ":
			if csize >= 10 && csize <= maxMetaChunkBytes {
				b := make([]byte, csize)
				if _, err := f.ReadAt(b, off); err == nil {
					parseID3v2(b, m)
				}
			}
		}
		step := csizeU
		if step&1 == 1 && step < rem {
			step++
		}
		off += int64(step)
	}

	if m.spec.SampleRate <= 0 || m.spec.Channels <= 0 {
		return errors.New("localmusic: DSDIFF missing FS or CHNL property")
	}
	m.spec.Codec = dsdCodecName(m.spec.SampleRate)
	m.spec.BitDepth = 1
	m.spec.BitrateKbps = int((uint64(m.spec.SampleRate) * uint64(m.spec.Channels)) / 1000)
	m.spec.IsDSD = true
	m.spec.IsLossless = true
	m.spec.IsHiRes = true
	if dsdDataBytes > 0 {
		chU := uint64(m.spec.Channels)
		samplesPerCh := (dsdDataBytes/chU)*8 + ((dsdDataBytes%chU)*8)/chU
		m.durationMs = mulDiv1000(samplesPerCh, uint64(m.spec.SampleRate))
	}
	return nil
}

func parseDFFProp(b []byte, m *parsedMeta) {
	pos := 0
	for pos+12 <= len(b) {
		id := string(b[pos : pos+4])
		sz := binary.BigEndian.Uint64(b[pos+4 : pos+12])
		pos += 12
		rem := uint64(len(b) - pos)
		if sz > rem {
			break
		}
		sub := b[pos : pos+int(sz)]
		step := sz
		if step&1 == 1 && step < rem {
			step++
		}
		pos += int(step)
		switch id {
		case "FS  ":
			if len(sub) >= 4 {
				sr := binary.BigEndian.Uint32(sub[0:4])
				if sr <= maxDSDSampleRate {
					m.spec.SampleRate = int(sr)
				}
			}
		case "CHNL":
			if len(sub) >= 2 {
				m.spec.Channels = int(binary.BigEndian.Uint16(sub[0:2]))
			}
		case "ID3 ", "id3 ":
			if len(sub) >= 10 {
				parseID3v2(sub, m)
			}
		}
	}
}

func parseDFFDiin(b []byte, m *parsedMeta) {
	pos := 0
	for pos+12 <= len(b) {
		id := string(b[pos : pos+4])
		sz := binary.BigEndian.Uint64(b[pos+4 : pos+12])
		pos += 12
		rem := uint64(len(b) - pos)
		if sz > rem {
			break
		}
		sub := b[pos : pos+int(sz)]
		step := sz
		if step&1 == 1 && step < rem {
			step++
		}
		pos += int(step)
		switch id {
		case "DITI":
			if len(sub) >= 4 && m.title == "" {
				n := uint64(binary.BigEndian.Uint32(sub[0:4]))
				if n > 0 && n <= uint64(len(sub)-4) {
					m.title = decodeTextAuto(sub[4 : 4+int(n)])
				}
			}
		case "DIAR":
			if len(sub) >= 4 && len(m.artists) == 0 {
				n := uint64(binary.BigEndian.Uint32(sub[0:4]))
				if n > 0 && n <= uint64(len(sub)-4) {
					m.addArtist(decodeTextAuto(sub[4 : 4+int(n)]))
				}
			}
		}
	}
}

// --- M4A / MP4 (AAC & ALAC) ---

func parseMP4(f *os.File, fileSize int64, m *parsedMeta) error {
	var h [16]byte
	var moov []byte
	for off := int64(0); off+8 <= fileSize; {
		if _, err := f.ReadAt(h[:8], off); err != nil {
			break
		}
		nU := uint64(binary.BigEndian.Uint32(h[0:4]))
		head := uint64(8)
		typ := string(h[4:8])
		rem := uint64(fileSize - off)
		switch nU {
		case 0:
			nU = rem
		case 1:
			if rem < 16 {
				nU = 0
			} else if _, err := f.ReadAt(h[8:16], off+8); err == nil {
				nU = binary.BigEndian.Uint64(h[8:16])
				head = 16
			} else {
				nU = 0
			}
		}
		if nU < head || nU > rem {
			break
		}
		if typ == "moov" {
			bodyLen := nU - head
			if bodyLen > maxMetaChunkBytes {
				return errors.New("localmusic: MP4 moov box too large")
			}
			moov = make([]byte, int(bodyLen))
			if _, err := f.ReadAt(moov, off+int64(head)); err != nil {
				return err
			}
			break
		}
		off += int64(nU)
	}
	if moov == nil {
		return errors.New("localmusic: MP4 missing moov box")
	}

	walkMP4Boxes(moov, func(typ string, body []byte) {
		switch typ {
		case "mvhd":
			parseMP4Mvhd(body, m)
		case "trak":
			parseMP4Trak(body, m)
		case "udta":
			parseMP4Udta(body, m)
		}
	})

	if m.spec.Codec == "" {
		m.spec.Codec = "AAC"
	}
	return nil
}

func walkMP4Boxes(b []byte, fn func(typ string, body []byte)) {
	for len(b) >= 8 {
		sz := uint64(binary.BigEndian.Uint32(b[0:4]))
		head := uint64(8)
		typ := string(b[4:8])
		switch sz {
		case 0:
			sz = uint64(len(b))
		case 1:
			if len(b) < 16 {
				return
			}
			sz = binary.BigEndian.Uint64(b[8:16])
			head = 16
		}
		if sz < head || sz > uint64(len(b)) {
			return
		}
		fn(typ, b[head:sz])
		b = b[sz:]
	}
}

func mp4Child(b []byte, path ...string) []byte {
	cur := b
	for _, want := range path {
		var found []byte
		walkMP4Boxes(cur, func(typ string, body []byte) {
			if found == nil && typ == want {
				found = body
			}
		})
		if found == nil {
			return nil
		}
		cur = found
	}
	return cur
}

func parseMP4Mvhd(b []byte, m *parsedMeta) {
	if len(b) < 20 {
		return
	}
	var timescale uint32
	var dur uint64
	if b[0] == 1 {
		if len(b) < 32 {
			return
		}
		timescale = binary.BigEndian.Uint32(b[20:24])
		dur = binary.BigEndian.Uint64(b[24:32])
	} else {
		timescale = binary.BigEndian.Uint32(b[12:16])
		dur = uint64(binary.BigEndian.Uint32(b[16:20]))
	}
	if timescale > 0 && dur > 0 {
		m.durationMs = mulDiv1000(dur, uint64(timescale))
	}
}

func parseMP4Trak(trak []byte, m *parsedMeta) {
	mdia := mp4Child(trak, "mdia")
	if mdia == nil {
		return
	}
	hdlr := mp4Child(mdia, "hdlr")
	if len(hdlr) < 12 || string(hdlr[8:12]) != "soun" {
		return
	}
	if m.durationMs == 0 {
		if mdhd := mp4Child(mdia, "mdhd"); len(mdhd) >= 20 {
			var ts uint32
			var dur uint64
			if mdhd[0] == 1 && len(mdhd) >= 32 {
				ts = binary.BigEndian.Uint32(mdhd[20:24])
				dur = binary.BigEndian.Uint64(mdhd[24:32])
			} else {
				ts = binary.BigEndian.Uint32(mdhd[12:16])
				dur = uint64(binary.BigEndian.Uint32(mdhd[16:20]))
			}
			if ts > 0 && dur > 0 {
				m.durationMs = mulDiv1000(dur, uint64(ts))
			}
		}
	}

	stsd := mp4Child(mdia, "minf", "stbl", "stsd")
	if len(stsd) < 16 {
		return
	}
	walkMP4Boxes(stsd[8:], func(entryType string, entry []byte) {
		if m.spec.Codec != "" || len(entry) < 28 {
			return
		}
		qtVer := binary.BigEndian.Uint16(entry[8:10])
		channels := int(binary.BigEndian.Uint16(entry[16:18]))
		sampleSize := int(binary.BigEndian.Uint16(entry[18:20]))
		sampleRate := int(binary.BigEndian.Uint32(entry[24:28]) >> 16)
		childOff := 28
		if qtVer == 1 && len(entry) >= 44 {
			childOff = 44
		} else if qtVer == 2 && len(entry) >= 64 {
			childOff = 64
		}

		switch entryType {
		case "alac":
			m.spec.Codec = "ALAC"
			m.spec.IsLossless = true
			m.spec.Channels = channels
			m.spec.BitDepth = sampleSize
			m.spec.SampleRate = sampleRate
			if childOff < len(entry) {
				alacCfg := mp4Child(entry[childOff:], "alac")
				if alacCfg == nil {
					alacCfg = mp4Child(entry[childOff:], "wave", "alac")
				}
				// FullBox: 4 bytes version/flags + 24 bytes ALACSpecificConfig.
				if len(alacCfg) >= 28 {
					m.spec.BitDepth = int(alacCfg[9])
					m.spec.Channels = int(alacCfg[13])
					m.spec.SampleRate = int(binary.BigEndian.Uint32(alacCfg[24:28]))
				}
			}
		case "mp4a":
			m.spec.Codec = "AAC"
			m.spec.IsLossless = false
			m.spec.Channels = channels
			m.spec.SampleRate = sampleRate
			m.spec.BitDepth = 0
			if childOff < len(entry) {
				if esds := mp4Child(entry[childOff:], "esds"); len(esds) > 4 {
					parseMP4Esds(esds[4:], m)
				}
			}
		case "fLaC":
			m.spec.Codec = "FLAC"
			m.spec.IsLossless = true
			m.spec.Channels = channels
			m.spec.BitDepth = sampleSize
			m.spec.SampleRate = sampleRate
		}
	})
}

var mp4SampleRates = [...]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350,
}

func parseMP4Esds(b []byte, m *parsedMeta) {
	for i := 0; i < len(b); {
		tag := b[i]
		i++
		length := 0
		for k := 0; k < 4 && i < len(b); k++ {
			c := b[i]
			i++
			length = (length << 7) | int(c&0x7F)
			if c&0x80 == 0 {
				break
			}
		}
		if length < 0 || length > len(b)-i {
			return
		}
		payload := b[i : i+length]
		switch tag {
		case 0x03: // ES_Descriptor
			if len(payload) >= 3 {
				skip := 3
				if payload[2]&0x80 != 0 && skip < len(payload) {
					skip += 2
				}
				if payload[2]&0x40 != 0 && skip < len(payload) {
					skip += 1 + int(payload[skip])
				}
				if payload[2]&0x20 != 0 && skip < len(payload) {
					skip += 2
				}
				if skip < len(payload) {
					parseMP4Esds(payload[skip:], m)
				}
			}
			return
		case 0x04: // DecoderConfigDescriptor
			if len(payload) >= 13 {
				avgBr := uint64(binary.BigEndian.Uint32(payload[9:13]))
				if avgBr > 0 {
					m.spec.BitrateKbps = int((avgBr + 500) / 1000)
				}
				parseMP4Esds(payload[13:], m)
			}
			return
		case 0x05: // AudioSpecificConfig
			if len(payload) >= 2 {
				aot := (payload[0] >> 3) & 0x1F
				bitPos := 5
				if aot == 31 && len(payload) >= 3 {
					bitPos = 11
				}
				srIdx := readBits(payload, bitPos, 4)
				bitPos += 4
				if srIdx == 15 {
					sr := readBits(payload, bitPos, 24)
					if sr > 0 {
						m.spec.SampleRate = sr
					}
					bitPos += 24
				} else if srIdx >= 0 && srIdx < len(mp4SampleRates) {
					m.spec.SampleRate = mp4SampleRates[srIdx]
				}
				chCfg := readBits(payload, bitPos, 4)
				if chCfg > 0 && chCfg <= 7 {
					if chCfg == 7 {
						m.spec.Channels = 8
					} else {
						m.spec.Channels = chCfg
					}
				}
			}
			return
		}
		i += length
	}
}

func readBits(b []byte, bitOff, count int) int {
	val := 0
	for k := range count {
		idx := (bitOff + k) / 8
		if idx >= len(b) {
			return -1
		}
		shift := 7 - ((bitOff + k) % 8)
		val = (val << 1) | int((b[idx]>>shift)&1)
	}
	return val
}

func parseMP4Udta(udta []byte, m *parsedMeta) {
	meta := mp4Child(udta, "meta")
	if len(meta) < 8 {
		return
	}
	// 'meta' is normally a FullBox with 4 bytes version/flags before child boxes.
	metaChildren := meta
	if len(meta) >= 12 {
		possibleType := string(meta[8:12])
		if possibleType == "hdlr" || possibleType == "ilst" || possibleType == "keys" || possibleType == "free" {
			metaChildren = meta[4:]
		}
	}
	ilst := mp4Child(metaChildren, "ilst")
	if ilst == nil {
		return
	}

	walkMP4Boxes(ilst, func(itemType string, itemBody []byte) {
		if itemType == "----" {
			var mean, name string
			var dataVal []byte
			walkMP4Boxes(itemBody, func(sub string, sb []byte) {
				switch sub {
				case "mean":
					if len(sb) > 4 {
						mean = string(sb[4:])
					}
				case "name":
					if len(sb) > 4 {
						name = strings.ToUpper(strings.TrimSpace(string(sb[4:])))
					}
				case "data":
					if len(sb) > 8 {
						dataVal = sb[8:]
					}
				}
			})
			_ = mean
			if len(dataVal) > 0 {
				txt := decodeTextAuto(dataVal)
				switch name {
				case "REPLAYGAIN_TRACK_GAIN":
					m.setGain(txt, true)
				case "REPLAYGAIN_ALBUM_GAIN":
					m.setGain(txt, false)
				}
			}
			return
		}

		walkMP4Boxes(itemBody, func(sub string, sb []byte) {
			if sub != "data" || len(sb) < 8 {
				return
			}
			dataType := binary.BigEndian.Uint32(sb[0:4])
			val := sb[8:]
			if len(val) == 0 {
				return
			}
			txt := func() string {
				if dataType == 2 {
					return cleanDecodedString(decodeUTF16Bytes(val, binary.BigEndian))
				}
				return decodeTextAuto(val)
			}

			switch itemType {
			case "\xa9nam":
				if m.title == "" {
					m.title = txt()
				}
			case "\xa9ART":
				m.addArtist(txt())
			case "aART":
				if m.albumArtist == "" {
					m.albumArtist = txt()
				}
			case "\xa9alb":
				if m.album == "" {
					m.album = txt()
				}
			case "\xa9gen":
				m.addGenre(txt())
			case "gnre":
				if len(val) >= 2 {
					idx := int(binary.BigEndian.Uint16(val[0:2])) - 1
					if idx >= 0 && idx < len(id3v1Genres) {
						m.addGenre(id3v1Genres[idx])
					}
				}
			case "\xa9day":
				if m.year == 0 {
					m.year = parseYear(txt())
				}
			case "\xa9wrt":
				if m.composer == "" {
					m.composer = txt()
				}
			case "trkn":
				if len(val) >= 4 && m.trackNo == 0 {
					m.trackNo = int(binary.BigEndian.Uint16(val[2:4]))
				}
			case "disk":
				if len(val) >= 4 && m.discNo == 0 {
					m.discNo = int(binary.BigEndian.Uint16(val[2:4]))
				}
			case "\xa9lyr":
				if m.lyrics == "" {
					m.lyrics = txt()
				}
			case "covr":
				m.setCover(bytes.Clone(val), 3)
			}
		})
	})
}

// --- ID3v2 / ID3v1 / MP3 ---

func synchsafe32(b []byte) int {
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

func removeUnsync(b []byte) []byte {
	if !bytes.Contains(b, []byte{0xFF, 0x00}) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xFF && i+1 < len(b) && b[i+1] == 0x00 {
			i++
		}
	}
	return out
}

func parseID3v2(tag []byte, m *parsedMeta) {
	if len(tag) < 10 || string(tag[0:3]) != "ID3" {
		return
	}
	ver := tag[3]
	flags := tag[5]
	bodyLen := synchsafe32(tag[6:10])
	if 10+bodyLen > len(tag) {
		bodyLen = len(tag) - 10
	}
	if bodyLen <= 0 {
		return
	}
	body := tag[10 : 10+bodyLen]
	if (ver == 2 || ver == 3) && flags&0x80 != 0 {
		body = removeUnsync(body)
	}

	pos := 0
	if (ver == 3 || ver == 4) && flags&0x40 != 0 && len(body) >= 4 {
		var extU uint64
		if ver == 4 {
			extU = uint64(synchsafe32(body[0:4]))
		} else {
			extU = uint64(binary.BigEndian.Uint32(body[0:4])) + 4
		}
		if extU > 0 && extU < uint64(len(body)) {
			pos = int(extU)
		}
	}

	if ver == 2 {
		for pos+6 <= len(body) {
			fid := string(body[pos : pos+3])
			if body[pos] == 0 {
				break
			}
			fsize := uint64(body[pos+3])<<16 | uint64(body[pos+4])<<8 | uint64(body[pos+5])
			pos += 6
			if fsize == 0 || fsize > uint64(len(body)-pos) {
				break
			}
			parseID3v22Frame(fid, body[pos:pos+int(fsize)], m)
			pos += int(fsize)
		}
		return
	}

	for pos+10 <= len(body) {
		if body[pos] == 0 {
			break
		}
		fid := string(body[pos : pos+4])
		var fsize uint64
		if ver == 4 {
			fsize = uint64(synchsafe32(body[pos+4 : pos+8]))
		} else {
			fsize = uint64(binary.BigEndian.Uint32(body[pos+4 : pos+8]))
		}
		flag2 := body[pos+9]
		pos += 10
		if fsize == 0 || fsize > uint64(len(body)-pos) {
			break
		}
		fdata := body[pos : pos+int(fsize)]
		pos += int(fsize)

		if ver == 4 {
			if flag2&0x02 != 0 {
				fdata = removeUnsync(fdata)
			}
			if flag2&0x01 != 0 {
				if len(fdata) < 4 {
					continue
				}
				fdata = fdata[4:]
			}
		} else if ver == 3 {
			// Skip zlib-compressed or encrypted frames cleanly.
			if flag2&0xC0 != 0 {
				continue
			}
		}
		parseID3v23Frame(fid, fdata, m)
	}
}

func decodeID3Strings(fdata []byte) []string {
	if len(fdata) < 1 {
		return nil
	}
	enc := fdata[0]
	raw := fdata[1:]
	if len(raw) == 0 {
		return nil
	}
	switch enc {
	case 0: // ISO-8859-1 (with UTF-8 / GB18030 / Shift-JIS auto-fallback)
		var out []string
		for _, seg := range bytes.Split(raw, []byte{0}) {
			if len(seg) == 0 {
				continue
			}
			if s := decodeTextAuto(seg); s != "" {
				out = append(out, s)
			}
		}
		return out
	case 1: // UTF-16 with BOM
		return splitID3UTF16(raw, true)
	case 2: // UTF-16BE without BOM
		return splitID3UTF16(raw, false)
	case 3: // UTF-8
		var out []string
		for _, seg := range bytes.Split(raw, []byte{0}) {
			if len(seg) == 0 {
				continue
			}
			if s := cleanDecodedString(string(seg)); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		if s := decodeTextAuto(raw); s != "" {
			return []string{s}
		}
		return nil
	}
}

func splitID3UTF16(raw []byte, withBOM bool) []string {
	var out []string
	order := binary.ByteOrder(binary.LittleEndian)
	if !withBOM {
		order = binary.BigEndian
	}
	start := 0
	for i := 0; i+1 < len(raw); i += 2 {
		if raw[i] == 0 && raw[i+1] == 0 {
			if s := decodeOneUTF16Seg(raw[start:i], withBOM, order); s != "" {
				out = append(out, s)
			}
			start = i + 2
		}
	}
	if start < len(raw) {
		if s := decodeOneUTF16Seg(raw[start:], withBOM, order); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func decodeOneUTF16Seg(seg []byte, withBOM bool, defaultOrder binary.ByteOrder) string {
	if len(seg) < 2 {
		return ""
	}
	order := defaultOrder
	if withBOM {
		if seg[0] == 0xFF && seg[1] == 0xFE {
			order = binary.LittleEndian
			seg = seg[2:]
		} else if seg[0] == 0xFE && seg[1] == 0xFF {
			order = binary.BigEndian
			seg = seg[2:]
		}
	}
	return cleanDecodedString(decodeUTF16Bytes(seg, order))
}

func parseID3v22Frame(fid string, fdata []byte, m *parsedMeta) {
	switch fid {
	case "TT2":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.title == "" {
			m.title = vals[0]
		}
	case "TP1":
		m.addArtist(decodeID3Strings(fdata)...)
	case "TP2":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.albumArtist == "" {
			m.albumArtist = vals[0]
		}
	case "TAL":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.album == "" {
			m.album = vals[0]
		}
	case "TCO":
		m.addGenre(decodeID3Strings(fdata)...)
	case "TYE":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.year == 0 {
			m.year = parseYear(vals[0])
		}
	case "TCM":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.composer == "" {
			m.composer = vals[0]
		}
	case "TRK":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.trackNo == 0 {
			m.trackNo = parseIndexPair(vals[0])
		}
	case "TPA":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.discNo == 0 {
			m.discNo = parseIndexPair(vals[0])
		}
	case "ULT":
		if lyr := parseID3USLT(fdata); lyr != "" && m.lyrics == "" {
			m.lyrics = lyr
		}
	case "PIC":
		// encoding(1) + format(3) + picType(1) + desc(null-term) + data
		if len(fdata) > 6 {
			enc := fdata[0]
			picType := int(fdata[4])
			rest := fdata[5:]
			_, img := skipID3NullTerm(rest, enc)
			m.setCover(bytes.Clone(img), picType)
		}
	}
}

func parseID3v23Frame(fid string, fdata []byte, m *parsedMeta) {
	switch fid {
	case "TIT2":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.title == "" {
			m.title = vals[0]
		}
	case "TPE1":
		m.addArtist(decodeID3Strings(fdata)...)
	case "TPE2":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.albumArtist == "" {
			m.albumArtist = vals[0]
		}
	case "TALB":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.album == "" {
			m.album = vals[0]
		}
	case "TCON":
		m.addGenre(decodeID3Strings(fdata)...)
	case "TDRC", "TYER", "TDOR":
		if vals := decodeID3Strings(fdata); len(vals) > 0 {
			if y := parseYear(vals[0]); y > 0 && (m.year == 0 || fid != "TDOR") {
				if m.year == 0 {
					m.year = y
				}
			}
		}
	case "TCOM":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.composer == "" {
			m.composer = vals[0]
		}
	case "TRCK":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.trackNo == 0 {
			m.trackNo = parseIndexPair(vals[0])
		}
	case "TPOS":
		if vals := decodeID3Strings(fdata); len(vals) > 0 && m.discNo == 0 {
			m.discNo = parseIndexPair(vals[0])
		}
	case "TXXX":
		vals := decodeID3Strings(fdata)
		if len(vals) >= 2 {
			desc := strings.ToUpper(strings.TrimSpace(vals[0]))
			val := vals[1]
			switch desc {
			case "REPLAYGAIN_TRACK_GAIN":
				m.setGain(val, true)
			case "REPLAYGAIN_ALBUM_GAIN":
				m.setGain(val, false)
			case "STYLE", "GENRE":
				m.addGenre(val)
			case "LYRICS":
				if m.lyrics == "" {
					m.lyrics = val
				}
			}
		}
	case "USLT":
		if lyr := parseID3USLT(fdata); lyr != "" && m.lyrics == "" {
			m.lyrics = lyr
		}
	case "APIC":
		// encoding(1) + mime(null-term ASCII) + picType(1) + desc(null-term enc) + data
		if len(fdata) > 4 {
			enc := fdata[0]
			mimeEnd := bytes.IndexByte(fdata[1:], 0)
			if mimeEnd < 0 {
				return
			}
			mime := string(fdata[1 : 1+mimeEnd])
			pos := 1 + mimeEnd + 1
			if pos >= len(fdata) || mime == "-->" {
				return
			}
			picType := int(fdata[pos])
			pos++
			_, img := skipID3NullTerm(fdata[pos:], enc)
			m.setCover(bytes.Clone(img), picType)
		}
	}
}

func skipID3NullTerm(b []byte, enc byte) (head, rest []byte) {
	if enc == 1 || enc == 2 {
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return b[:i], b[i+2:]
			}
		}
		return b, nil
	}
	if idx := bytes.IndexByte(b, 0); idx >= 0 {
		return b[:idx], b[idx+1:]
	}
	return b, nil
}

func parseID3USLT(fdata []byte) string {
	if len(fdata) < 5 {
		return ""
	}
	enc := fdata[0]
	// fdata[1:4] is 3-byte ISO-639-2 language code.
	_, textBytes := skipID3NullTerm(fdata[4:], enc)
	if len(textBytes) == 0 {
		return ""
	}
	withEnc := make([]byte, 1+len(textBytes))
	withEnc[0] = enc
	copy(withEnc[1:], textBytes)
	vals := decodeID3Strings(withEnc)
	return strings.Join(vals, "\n")
}

func parseID3v1(f *os.File, fileSize int64, m *parsedMeta) int64 {
	if fileSize < 128 {
		return 0
	}
	var v1 [128]byte
	if _, err := f.ReadAt(v1[:], fileSize-128); err != nil || string(v1[0:3]) != "TAG" {
		return 0
	}
	if m.title == "" {
		m.title = decodeTextAuto(v1[3:33])
	}
	if len(m.artists) == 0 {
		m.addArtist(decodeTextAuto(v1[33:63]))
	}
	if m.album == "" {
		m.album = decodeTextAuto(v1[63:93])
	}
	if m.year == 0 {
		m.year = parseYear(decodeTextAuto(v1[93:97]))
	}
	if m.trackNo == 0 && v1[125] == 0 && v1[126] > 0 {
		m.trackNo = int(v1[126])
	}
	if len(m.genres) == 0 && int(v1[127]) < len(id3v1Genres) {
		m.addGenre(id3v1Genres[v1[127]])
	}
	return 128
}

var (
	mpegSampleRates = [4][3]int{
		{11025, 12000, 8000},  // MPEG 2.5
		{0, 0, 0},             // reserved
		{22050, 24000, 16000}, // MPEG 2
		{44100, 48000, 32000}, // MPEG 1
	}
	mpegBitratesV1 = [3][15]int{
		// Layer III
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
		// Layer II
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		// Layer I
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
	}
	mpegBitratesV2 = [3][15]int{
		// Layer III / II
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		// Layer I
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
	}
)

func parseMP3(f *os.File, fileSize int64, m *parsedMeta) error {
	m.spec.Codec = "MP3"
	m.spec.IsLossless = false
	m.spec.BitDepth = 0

	audioStart := int64(0)
	var head [10]byte
	if _, err := f.ReadAt(head[:], 0); err == nil && string(head[0:3]) == "ID3" {
		tagSize := int64(10 + synchsafe32(head[6:10]))
		if tagSize > fileSize {
			tagSize = fileSize
		}
		if tagSize >= 10 && tagSize <= maxMetaChunkBytes {
			buf := make([]byte, tagSize)
			if _, err := f.ReadAt(buf, 0); err == nil {
				parseID3v2(buf, m)
			}
		}
		audioStart = tagSize
	}

	tailBytes := parseID3v1(f, fileSize, m)
	tailBytes += parseAPEv2Tail(f, fileSize-tailBytes, m)

	scanLen := min(fileSize-audioStart-tailBytes, int64(128<<10))
	if scanLen < 4 {
		return errors.New("localmusic: MP3 file too short")
	}
	scanBuf := make([]byte, scanLen)
	n, _ := f.ReadAt(scanBuf, audioStart)
	scanBuf = scanBuf[:n]

	foundFrame := false
	for i := 0; i+4 <= len(scanBuf); i++ {
		if scanBuf[i] != 0xFF || scanBuf[i+1]&0xE0 != 0xE0 {
			continue
		}
		verIdx := int((scanBuf[i+1] >> 3) & 0x03)
		layerIdx := int((scanBuf[i+1] >> 1) & 0x03) // 1=L3, 2=L2, 3=L1
		brIdx := int((scanBuf[i+2] >> 4) & 0x0F)
		srIdx := int((scanBuf[i+2] >> 2) & 0x03)
		pad := int((scanBuf[i+2] >> 1) & 0x01)
		chMode := int((scanBuf[i+3] >> 6) & 0x03)
		if verIdx == 1 || layerIdx == 0 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
			continue
		}
		sr := mpegSampleRates[verIdx][srIdx]
		var kbps int
		if verIdx == 3 {
			kbps = mpegBitratesV1[layerIdx-1][brIdx]
		} else {
			kbps = mpegBitratesV2[layerIdx-1][brIdx]
		}
		if sr <= 0 || kbps <= 0 {
			continue
		}
		samplesPerFrame := 1152
		var frameSize int
		switch layerIdx {
		case 3: // Layer I
			samplesPerFrame = 384
			frameSize = (12*kbps*1000/sr + pad) * 4
		case 2: // Layer II
			samplesPerFrame = 1152
			frameSize = 144*kbps*1000/sr + pad
		case 1: // Layer III
			if verIdx == 3 {
				samplesPerFrame = 1152
				frameSize = 144*kbps*1000/sr + pad
			} else {
				samplesPerFrame = 576
				frameSize = 72*kbps*1000/sr + pad
			}
		}

		// Verify next frame sync if within scanBuf to avoid false positives in padding.
		if i+frameSize+2 <= len(scanBuf) {
			if scanBuf[i+frameSize] != 0xFF || scanBuf[i+frameSize+1]&0xE0 != 0xE0 {
				continue
			}
		}

		channels := 2
		if chMode == 3 {
			channels = 1
		}
		m.spec.SampleRate = sr
		m.spec.Channels = channels
		m.spec.BitrateKbps = kbps

		frameBytes := scanBuf[i:min(i+frameSize, len(scanBuf))]
		audioBytes := max(fileSize-(audioStart+int64(i))-tailBytes, 0)

		// Check Xing / Info VBR header.
		xingOff := 4 + 32
		if verIdx == 3 && channels == 1 {
			xingOff = 4 + 17
		} else if verIdx != 3 {
			if channels == 1 {
				xingOff = 4 + 9
			} else {
				xingOff = 4 + 17
			}
		}
		parsedVBR := false
		if xingOff+8 <= len(frameBytes) {
			tag := string(frameBytes[xingOff : xingOff+4])
			if tag == "Xing" || tag == "Info" {
				vbrFlags := binary.BigEndian.Uint32(frameBytes[xingOff+4 : xingOff+8])
				p := xingOff + 8
				var totalFrames, totalBytes uint32
				if vbrFlags&0x01 != 0 && p+4 <= len(frameBytes) {
					totalFrames = binary.BigEndian.Uint32(frameBytes[p : p+4])
					p += 4
				}
				if vbrFlags&0x02 != 0 && p+4 <= len(frameBytes) {
					totalBytes = binary.BigEndian.Uint32(frameBytes[p : p+4])
				}
				if totalFrames > 0 {
					m.durationMs = int64(totalFrames) * int64(samplesPerFrame) * 1000 / int64(sr)
					bytesForRate := audioBytes
					if totalBytes > 0 {
						bytesForRate = int64(totalBytes)
					}
					if m.durationMs > 0 && bytesForRate > 0 {
						m.spec.BitrateKbps = int((bytesForRate * 8) / m.durationMs)
					}
					parsedVBR = true
				}
			}
		}
		// Check Fraunhofer VBRI header at offset 36.
		if !parsedVBR && 36+18 <= len(frameBytes) && string(frameBytes[36:40]) == "VBRI" {
			totalBytes := binary.BigEndian.Uint32(frameBytes[46:50])
			totalFrames := binary.BigEndian.Uint32(frameBytes[50:54])
			if totalFrames > 0 {
				m.durationMs = int64(totalFrames) * int64(samplesPerFrame) * 1000 / int64(sr)
				if m.durationMs > 0 && totalBytes > 0 {
					m.spec.BitrateKbps = int((int64(totalBytes) * 8) / m.durationMs)
				}
				parsedVBR = true
			}
		}
		if !parsedVBR && kbps > 0 && audioBytes > 0 {
			m.durationMs = (audioBytes * 8) / int64(kbps)
		}
		foundFrame = true
		break
	}
	if !foundFrame {
		return errors.New("localmusic: no valid MPEG audio frame found")
	}
	return nil
}

// --- OGG Vorbis ---

func parseOGG(f *os.File, fileSize int64, m *parsedMeta) error {
	m.spec.Codec = "OGG"
	m.spec.IsLossless = false
	m.spec.BitDepth = 0

	// Reassemble first two Vorbis header packets from initial Ogg pages.
	var curPkt []byte
	pktsRead := 0
	off := int64(0)
	var phdr [27]byte
	for off+27 <= fileSize && pktsRead < 2 && off < 16<<20 {
		if _, err := f.ReadAt(phdr[:], off); err != nil || string(phdr[0:4]) != "OggS" {
			break
		}
		numSegs := int(phdr[26])
		if off+27+int64(numSegs) > fileSize {
			break
		}
		segTable := make([]byte, numSegs)
		if _, err := f.ReadAt(segTable, off+27); err != nil {
			break
		}
		dataOff := off + 27 + int64(numSegs)
		pageBodyLen := 0
		for _, s := range segTable {
			pageBodyLen += int(s)
		}
		if dataOff+int64(pageBodyLen) > fileSize || len(curPkt)+pageBodyLen > maxMetaChunkBytes {
			break
		}
		pageBody := make([]byte, pageBodyLen)
		if _, err := f.ReadAt(pageBody, dataOff); err != nil {
			break
		}
		p := 0
		for _, segLen := range segTable {
			curPkt = append(curPkt, pageBody[p:p+int(segLen)]...)
			p += int(segLen)
			if segLen < 255 {
				handleOggHeaderPacket(curPkt, m)
				curPkt = curPkt[:0]
				pktsRead++
				if pktsRead >= 2 {
					break
				}
			}
		}
		off = dataOff + int64(pageBodyLen)
	}

	if m.spec.SampleRate <= 0 {
		return errors.New("localmusic: invalid Ogg Vorbis stream")
	}

	// Read trailing 64 KiB to find last Ogg page granule position.
	tailLen := min(fileSize, int64(64<<10))
	tail := make([]byte, tailLen)
	if n, err := f.ReadAt(tail, fileSize-tailLen); err == nil && n >= 27 {
		tail = tail[:n]
		for i := len(tail) - 27; i >= 0; i-- {
			if tail[i] == 'O' && tail[i+1] == 'g' && tail[i+2] == 'g' && tail[i+3] == 'S' {
				granule := binary.LittleEndian.Uint64(tail[i+6 : i+14])
				if granule > 0 && granule != math.MaxUint64 {
					m.durationMs = mulDiv1000(granule, uint64(m.spec.SampleRate))
					break
				}
			}
		}
	}
	return nil
}

func handleOggHeaderPacket(pkt []byte, m *parsedMeta) {
	if len(pkt) >= 30 && pkt[0] == 0x01 && string(pkt[1:7]) == "vorbis" {
		srU := binary.LittleEndian.Uint32(pkt[12:16])
		if srU > 0 && srU <= 768000 {
			m.spec.Channels = int(pkt[11])
			m.spec.SampleRate = int(srU)
		}
		brNom := int(int32(binary.LittleEndian.Uint32(pkt[20:24])))
		if brNom > 0 {
			m.spec.BitrateKbps = (brNom + 500) / 1000
		}
	} else if len(pkt) >= 7 && pkt[0] == 0x03 && string(pkt[1:7]) == "vorbis" {
		parseVorbisComment(pkt[7:], m)
	}
}

// --- WavPack (.wv) & APEv2 ---

var wvSampleRates = [15]int{
	6000, 8000, 9600, 11025, 12000, 16000, 22050, 24000,
	32000, 44100, 48000, 64000, 88200, 96000, 192000,
}

func parseWavPack(f *os.File, fileSize int64, m *parsedMeta) error {
	off := int64(0)
	var head [10]byte
	if _, err := f.ReadAt(head[:], 0); err == nil && string(head[0:3]) == "ID3" {
		off = int64(10 + synchsafe32(head[6:10]))
		if off > fileSize {
			return errors.New("localmusic: invalid WavPack header")
		}
	}

	if fileSize-off < 32 {
		return errors.New("localmusic: invalid WavPack header")
	}
	var wvh [32]byte
	if _, err := f.ReadAt(wvh[:], off); err != nil || string(wvh[0:4]) != "wvpk" {
		return errors.New("localmusic: invalid WavPack header")
	}

	ckSize := int64(binary.LittleEndian.Uint32(wvh[4:8]))
	totalSamplesLow := uint64(binary.LittleEndian.Uint32(wvh[12:16]))
	totalSamples := int64(-1)
	if totalSamplesLow != 0xFFFFFFFF {
		totalSamples = int64(totalSamplesLow) | (int64(wvh[10]) << 32)
	}
	flags := binary.LittleEndian.Uint32(wvh[24:28])
	bytesPerSample := int((flags & 0x03) + 1)
	mono := (flags & 0x04) != 0
	hybrid := (flags & 0x08) != 0
	dsdFlag := (flags & 0x80000000) != 0
	srIdx := int((flags >> 23) & 0x0F)

	sr := 0
	if srIdx < 15 {
		sr = wvSampleRates[srIdx]
	} else if ckSize > 24 {
		// Scan WavPack metadata sub-blocks in first block for ID_SAMPLE_RATE (0x27).
		subLen := min(ckSize-24, int64(4096), fileSize-(off+32))
		if subLen > 0 {
			sub := make([]byte, subLen)
			if _, err := f.ReadAt(sub, off+32); err == nil {
				sr = findWavPackCustomRate(sub)
			}
		}
	}

	m.spec.Codec = "WV"
	m.spec.Channels = 2
	if mono {
		m.spec.Channels = 1
	}
	m.spec.SampleRate = sr
	m.spec.BitDepth = bytesPerSample * 8
	m.spec.IsLossless = !hybrid
	if dsdFlag {
		m.spec.BitDepth = 1
		m.spec.IsDSD = true
		m.spec.IsLossless = true
		m.spec.IsHiRes = true
	}
	if sr > 0 && totalSamples > 0 {
		m.durationMs = mulDiv1000(uint64(totalSamples), uint64(sr))
	}

	id3Tail := parseID3v1(f, fileSize, m)
	parseAPEv2Tail(f, fileSize-id3Tail, m)
	return nil
}

func findWavPackCustomRate(b []byte) int {
	pos := 0
	for pos+2 <= len(b) {
		id := b[pos]
		large := id&0x80 != 0
		odd := id&0x40 != 0
		fnID := id & 0x3F
		pos++
		var wordSize int
		if large {
			if pos+3 > len(b) {
				break
			}
			wordSize = int(b[pos]) | int(b[pos+1])<<8 | int(b[pos+2])<<16
			pos += 3
		} else {
			wordSize = int(b[pos])
			pos++
		}
		byteLen := wordSize * 2
		if byteLen > len(b)-pos {
			break
		}
		payload := b[pos : pos+byteLen]
		if odd && len(payload) > 0 {
			payload = payload[:len(payload)-1]
		}
		if fnID == 0x27 && len(payload) >= 3 {
			srU := uint32(payload[0]) | uint32(payload[1])<<8 | uint32(payload[2])<<16
			if len(payload) >= 4 {
				srU |= uint32(payload[3]) << 24
			}
			if srU <= maxDSDSampleRate {
				return int(srU)
			}
			return 0
		}
		pos += byteLen
	}
	return 0
}

func parseAPEv2Tail(f io.ReaderAt, endOff int64, m *parsedMeta) int64 {
	if endOff < 32 {
		return 0
	}
	var footer [32]byte
	if _, err := f.ReadAt(footer[:], endOff-32); err != nil || string(footer[0:8]) != "APETAGEX" {
		return 0
	}
	tagSize := int64(binary.LittleEndian.Uint32(footer[12:16]))
	itemCount := binary.LittleEndian.Uint32(footer[16:20])
	flags := binary.LittleEndian.Uint32(footer[20:24])
	if tagSize < 32 || tagSize > endOff || tagSize > maxMetaChunkBytes {
		return 0
	}
	itemsStart := endOff - tagSize
	itemsBytesLen := tagSize - 32
	if itemsBytesLen <= 0 {
		return tagSize
	}
	buf := make([]byte, itemsBytesLen)
	if _, err := f.ReadAt(buf, itemsStart); err != nil {
		return tagSize
	}

	pos := 0
	for i := uint32(0); i < itemCount && pos+9 <= len(buf); i++ {
		valLen := uint64(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		itemFlags := binary.LittleEndian.Uint32(buf[pos+4 : pos+8])
		pos += 8
		nulIdx := bytes.IndexByte(buf[pos:], 0)
		if nulIdx <= 0 {
			break
		}
		key := strings.ToUpper(strings.TrimSpace(string(buf[pos : pos+nulIdx])))
		pos += nulIdx + 1
		if valLen > uint64(len(buf)-pos) {
			break
		}
		val := buf[pos : pos+int(valLen)]
		pos += int(valLen)

		itemType := (itemFlags >> 1) & 0x03
		if itemType == 1 { // binary (e.g. Cover Art)
			if strings.HasPrefix(key, "COVER ART") {
				picType := 0
				if strings.Contains(key, "FRONT") {
					picType = 3
				}
				img := val
				if fnEnd := bytes.IndexByte(val, 0); fnEnd >= 0 && fnEnd+1 < len(val) {
					img = val[fnEnd+1:]
				}
				m.setCover(bytes.Clone(img), picType)
			}
			continue
		}
		if itemType != 0 {
			continue
		}
		for _, seg := range bytes.Split(val, []byte{0}) {
			txt := decodeTextAuto(seg)
			if txt == "" {
				continue
			}
			applyAPEv2KeyVal(key, txt, m)
		}
	}

	totalConsumed := tagSize
	// If tag also has a 32-byte header before items (bit 31 of flags).
	if flags&0x80000000 != 0 && itemsStart >= 32 {
		totalConsumed += 32
	}
	return totalConsumed
}

func applyAPEv2KeyVal(key, val string, m *parsedMeta) {
	switch key {
	case "TITLE":
		if m.title == "" {
			m.title = val
		}
	case "ARTIST":
		m.addArtist(val)
	case "ALBUM":
		if m.album == "" {
			m.album = val
		}
	case "ALBUM ARTIST", "ALBUMARTIST":
		if m.albumArtist == "" {
			m.albumArtist = val
		}
	case "GENRE", "STYLE":
		m.addGenre(val)
	case "YEAR", "DATE":
		if m.year == 0 {
			m.year = parseYear(val)
		}
	case "COMPOSER":
		if m.composer == "" {
			m.composer = val
		}
	case "TRACK", "TRACKNUMBER":
		if m.trackNo == 0 {
			m.trackNo = parseIndexPair(val)
		}
	case "DISC", "DISCNUMBER":
		if m.discNo == 0 {
			m.discNo = parseIndexPair(val)
		}
	case "REPLAYGAIN_TRACK_GAIN":
		m.setGain(val, true)
	case "REPLAYGAIN_ALBUM_GAIN":
		m.setGain(val, false)
	case "LYRICS", "UNSYNCEDLYRICS":
		if m.lyrics == "" {
			m.lyrics = val
		}
	}
}
