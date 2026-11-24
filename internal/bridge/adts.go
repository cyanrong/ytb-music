package bridge

import (
	"encoding/binary"
	"errors"
	"io"
	"iter"
)

type packetLoc struct {
	offset int64
	size   uint32
}

// aacADTSReader extracts raw AAC frames from either a standard MP4 (`stbl`) or
// a fragmented DASH MP4 (`moof`/`trun`/`mdat`, as YouTube serves itag 140/141),
// supports O(1)/indexed sample seeking, and wraps each frame in an ADTS header.
type aacADTSReader struct {
	r                 io.ReaderAt
	size              int64
	profile           int // 0-based ADTS profile (AAC-LC = 1)
	freqIdx           int
	chanCfg           int
	sampleRate        int
	audioTrackID      uint32
	defaultSampleSize uint32

	fragmented bool
	scanOffset int64
	allScanned bool
	packets    []packetLoc
	pktCursor  int
}

var adtsSampleRates = [16]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

const maxMP4Samples = 1 << 22

func openAACADTSReader(r io.ReaderAt, size int64) (*aacADTSReader, error) {
	var hdr [16]byte
	var (
		moov    []byte
		moovEnd int64
	)
	for off := int64(0); off+8 <= size; {
		boxType, boxSize, headerLen, err := readMP4BoxHeader(r, off, size, hdr[:])
		if err != nil || boxSize <= 0 || off+boxSize > size {
			break
		}
		if boxType == "moov" {
			bodyLen := boxSize - headerLen
			if bodyLen <= 0 || bodyLen > 256<<20 {
				return nil, errors.New("bridge: invalid MP4 moov size")
			}
			moov = make([]byte, bodyLen)
			if _, err := readFullAt(r, moov, off+headerLen); err != nil {
				return nil, err
			}
			moovEnd = off + boxSize
			break
		}
		off += boxSize
	}
	if len(moov) == 0 {
		return nil, errors.New("bridge: missing MP4 moov header")
	}

	mvex := mp4Child(moov, "mvex")
	for typ, trak := range mp4Boxes(moov) {
		if typ != "trak" {
			continue
		}
		mdia := mp4Child(trak, "mdia")
		hdlr := mp4Child(mdia, "hdlr")
		if len(hdlr) < 12 || string(hdlr[8:12]) != "soun" {
			continue
		}
		stbl := mp4Child(mdia, "minf", "stbl")
		profile, freqIdx, chanCfg, ok := parseAACSampleEntry(mp4Child(stbl, "stsd"))
		if !ok {
			continue
		}

		var trackID uint32
		if tkhd := mp4Child(trak, "tkhd"); len(tkhd) >= 16 {
			at := 12
			if tkhd[0] == 1 {
				at = 20
			}
			if len(tkhd) >= at+4 {
				trackID = binary.BigEndian.Uint32(tkhd[at : at+4])
			}
		}

		fragmented := mvex != nil && !hasMP4Samples(stbl)
		var defaultSampleSize uint32
		if fragmented {
			for mtyp, trex := range mp4Boxes(mvex) {
				if mtyp == "trex" && len(trex) >= 20 && binary.BigEndian.Uint32(trex[4:8]) == trackID {
					defaultSampleSize = binary.BigEndian.Uint32(trex[16:20])
					break
				}
			}
		}

		ar := &aacADTSReader{
			r:                 r,
			size:              size,
			profile:           profile,
			freqIdx:           freqIdx,
			chanCfg:           chanCfg,
			sampleRate:        adtsSampleRates[freqIdx],
			audioTrackID:      trackID,
			defaultSampleSize: defaultSampleSize,
			fragmented:        fragmented,
			scanOffset:        moovEnd,
		}
		if !fragmented {
			locs, err := indexTrakStbl(stbl)
			if err != nil {
				return nil, err
			}
			ar.packets = locs
			ar.allScanned = true
		}
		return ar, nil
	}

	return nil, errors.New("bridge: stream is not AAC-in-MP4")
}

func (ar *aacADTSReader) SampleRate() int { return ar.sampleRate }

func (ar *aacADTSReader) SilentFrame() []byte {
	return silentADTSForFreqIdx(ar.freqIdx)
}

// SeekToSample positions the ADTS cursor at the 1024-sample AAC packet containing sample.
func (ar *aacADTSReader) SeekToSample(sample int64) error {
	if sample < 0 {
		sample = 0
	}
	targetIdx := int(sample / 1024)
	for !ar.allScanned && len(ar.packets) <= targetIdx {
		if err := ar.indexNextFragment(); err != nil {
			if errors.Is(err, io.EOF) {
				ar.allScanned = true
				break
			}
			return err
		}
	}
	ar.pktCursor = min(targetIdx, len(ar.packets))
	return nil
}

// NextFrame returns the next ADTS-wrapped AAC frame (1024 PCM samples).
func (ar *aacADTSReader) NextFrame() ([]byte, error) {
	for !ar.allScanned && len(ar.packets) <= ar.pktCursor {
		if err := ar.indexNextFragment(); err != nil {
			if errors.Is(err, io.EOF) {
				ar.allScanned = true
				break
			}
			return nil, err
		}
	}
	if ar.pktCursor >= len(ar.packets) {
		return nil, io.EOF
	}
	loc := ar.packets[ar.pktCursor]
	ar.pktCursor++
	raw := make([]byte, loc.size)
	if _, err := readFullAt(ar.r, raw, loc.offset); err != nil {
		return nil, err
	}
	return wrapADTS(ar.profile, ar.freqIdx, ar.chanCfg, raw), nil
}

func wrapADTS(profile, freqIdx, chanCfg int, raw []byte) []byte {
	frameLen := len(raw) + 7
	out := make([]byte, frameLen)
	out[0] = 0xFF
	out[1] = 0xF1 // MPEG-4, Layer 0, no CRC
	out[2] = byte(((profile & 0x03) << 6) | ((freqIdx & 0x0F) << 2) | ((chanCfg >> 2) & 0x01))
	out[3] = byte(((chanCfg & 0x03) << 6) | ((frameLen >> 11) & 0x03))
	out[4] = byte((frameLen >> 3) & 0xFF)
	out[5] = byte(((frameLen & 0x07) << 5) | 0x1F)
	out[6] = 0xFC
	copy(out[7:], raw)
	return out
}

var silentAACPayload = []byte{0x21, 0x00, 0x49, 0x90, 0x02, 0x19, 0x00, 0x23, 0x80}

var silentADTSFrame44kStereo = silentADTSForFreqIdx(4)

func silentADTSForFreqIdx(freqIdx int) []byte {
	if freqIdx < 0 || freqIdx >= 13 {
		freqIdx = 4
	}
	return wrapADTS(1, freqIdx, 2, silentAACPayload)
}

type rawFragPacket struct {
	offset   int64
	size     uint32
	explicit bool
}

func (ar *aacADTSReader) indexNextFragment() error {
	var hdr [16]byte
	var moofOff, moofHdrLen, moofEnd int64
	for {
		if ar.size > 0 && ar.scanOffset >= ar.size {
			return io.EOF
		}
		boxType, boxSize, headerLen, err := readMP4BoxHeader(ar.r, ar.scanOffset, ar.size, hdr[:])
		if err != nil {
			return err
		}
		nextOff := ar.scanOffset + boxSize
		if boxType == "moof" {
			moofOff = ar.scanOffset
			moofHdrLen = headerLen
			moofEnd = nextOff
			ar.scanOffset = nextOff
			break
		}
		ar.scanOffset = nextOff
	}

	rawPkts, needMdatOffset, err := parseMoofPackets(ar.r, moofOff, moofHdrLen, moofEnd, ar.audioTrackID, ar.defaultSampleSize)
	if err != nil {
		return err
	}
	if len(rawPkts) == 0 {
		return nil
	}

	var mdatPayloadPos int64
	if needMdatOffset {
		for {
			if ar.size > 0 && ar.scanOffset >= ar.size {
				return io.ErrUnexpectedEOF
			}
			boxType, boxSize, headerLen, err := readMP4BoxHeader(ar.r, ar.scanOffset, ar.size, hdr[:])
			if err != nil {
				return err
			}
			if boxType == "mdat" {
				mdatPayloadPos = ar.scanOffset + headerLen
				ar.scanOffset += boxSize
				break
			}
			ar.scanOffset += boxSize
		}
	} else {
		for ar.size <= 0 || ar.scanOffset < ar.size {
			boxType, boxSize, _, err := readMP4BoxHeader(ar.r, ar.scanOffset, ar.size, hdr[:])
			if err != nil {
				break
			}
			if boxType == "moof" {
				break
			}
			ar.scanOffset += boxSize
			if boxType == "mdat" {
				break
			}
		}
	}

	for _, p := range rawPkts {
		off := p.offset
		if !p.explicit {
			off = mdatPayloadPos + (p.offset - moofOff)
		}
		if p.size > 0 && p.size <= 1<<20 && off >= 0 {
			ar.packets = append(ar.packets, packetLoc{offset: off, size: p.size})
		}
	}
	return nil
}

func parseMoofPackets(r io.ReaderAt, moofOff, moofHdrLen, moofEnd int64, audioTrackID, trexDefaultSampleSize uint32) (pkts []rawFragPacket, needMdatOffset bool, err error) {
	bodyLen := moofEnd - (moofOff + moofHdrLen)
	if bodyLen <= 0 || bodyLen > 64<<20 {
		return nil, false, errors.New("bridge: invalid MP4 moof size")
	}
	moofBody := make([]byte, bodyLen)
	if _, err := readFullAt(r, moofBody, moofOff+moofHdrLen); err != nil {
		return nil, false, err
	}

	for typ, traf := range mp4Boxes(moofBody) {
		if typ != "traf" {
			continue
		}
		tfhd := mp4Child(traf, "tfhd")
		if len(tfhd) < 8 {
			continue
		}
		tfFlags := uint32(tfhd[1])<<16 | uint32(tfhd[2])<<8 | uint32(tfhd[3])
		trackID := binary.BigEndian.Uint32(tfhd[4:8])
		if audioTrackID != 0 && trackID != audioTrackID {
			continue
		}

		baseOffset := moofOff
		hasBaseOffset := false
		idx := 8
		if tfFlags&0x000001 != 0 && idx+8 <= len(tfhd) {
			baseOffset = int64(binary.BigEndian.Uint64(tfhd[idx : idx+8]))
			hasBaseOffset = true
			idx += 8
		}
		if tfFlags&0x000002 != 0 {
			idx += 4
		}
		if tfFlags&0x000008 != 0 {
			idx += 4
		}
		defaultSampleSize := trexDefaultSampleSize
		if tfFlags&0x000010 != 0 && idx+4 <= len(tfhd) {
			defaultSampleSize = binary.BigEndian.Uint32(tfhd[idx : idx+4])
		}

		curOff := baseOffset
		explicitOff := hasBaseOffset
		for cType, trun := range mp4Boxes(traf) {
			if cType != "trun" || len(trun) < 8 {
				continue
			}
			trFlags := uint32(trun[1])<<16 | uint32(trun[2])<<8 | uint32(trun[3])
			count := int(binary.BigEndian.Uint32(trun[4:8]))
			if count < 0 || count > maxMP4Samples {
				return nil, false, errors.New("bridge: invalid MP4 trun sample count")
			}
			tIdx := 8
			if trFlags&0x000001 != 0 && tIdx+4 <= len(trun) {
				rel := int64(int32(binary.BigEndian.Uint32(trun[tIdx : tIdx+4])))
				curOff = baseOffset + rel
				explicitOff = true
				tIdx += 4
			}
			if trFlags&0x000004 != 0 {
				tIdx += 4
			}
			hasDur := trFlags&0x000100 != 0
			hasSize := trFlags&0x000200 != 0
			hasFlags := trFlags&0x000400 != 0
			hasCTS := trFlags&0x000800 != 0
			for range count {
				if hasDur {
					tIdx += 4
				}
				sz := defaultSampleSize
				if hasSize && tIdx+4 <= len(trun) {
					sz = binary.BigEndian.Uint32(trun[tIdx : tIdx+4])
					tIdx += 4
				}
				if hasFlags {
					tIdx += 4
				}
				if hasCTS {
					tIdx += 4
				}
				if !explicitOff {
					needMdatOffset = true
				}
				pkts = append(pkts, rawFragPacket{
					offset:   curOff,
					size:     sz,
					explicit: explicitOff,
				})
				curOff += int64(sz)
			}
		}
	}
	return pkts, needMdatOffset, nil
}

func mp4Boxes(b []byte) iter.Seq2[string, []byte] {
	return func(yield func(string, []byte) bool) {
		for len(b) >= 8 {
			size, head := uint64(binary.BigEndian.Uint32(b[:4])), uint64(8)
			typ := string(b[4:8])
			switch size {
			case 0:
				size = uint64(len(b))
			case 1:
				if len(b) < 16 {
					return
				}
				size, head = binary.BigEndian.Uint64(b[8:16]), 16
			}
			if size < head || size > uint64(len(b)) {
				return
			}
			if !yield(typ, b[head:size]) {
				return
			}
			b = b[size:]
		}
	}
}

func mp4Child(b []byte, path ...string) []byte {
	for _, want := range path {
		var found []byte
		for typ, body := range mp4Boxes(b) {
			if typ == want {
				found = body
				break
			}
		}
		if found == nil {
			return nil
		}
		b = found
	}
	return b
}

func parseAACSampleEntry(stsd []byte) (profile, freqIdx, chanCfg int, ok bool) {
	if len(stsd) < 8+36 {
		return 0, 0, 0, false
	}
	rem := stsd[8:]
	for len(rem) >= 36 {
		eSize := int(binary.BigEndian.Uint32(rem[:4]))
		if eSize < 36 || eSize > len(rem) {
			break
		}
		entry := rem[:eSize]
		rem = rem[eSize:]
		if string(entry[4:8]) != "mp4a" {
			continue
		}
		inner := entry[36:]
		switch binary.BigEndian.Uint16(entry[16:18]) {
		case 1:
			inner = inner[min(16, len(inner)):]
		case 2:
			inner = inner[min(36, len(inner)):]
		}
		if wave := mp4Child(inner, "wave"); wave != nil {
			inner = wave
		}
		esds := mp4Child(inner, "esds")
		if len(esds) < 4 {
			continue
		}
		asc, ascOK := parseESDSConfig(esds[4:])
		if !ascOK || len(asc) < 2 {
			continue
		}
		aot := int(asc[0] >> 3)
		fIdx := int((asc[0]&0x07)<<1 | (asc[1] >> 7))
		cCfg := int((asc[1] >> 3) & 0x0f)
		if aot == 5 || aot == 29 {
			aot = 2
		}
		if fIdx >= 13 || adtsSampleRates[fIdx] == 0 || cCfg == 0 {
			continue
		}
		return max(0, min(3, aot-1)), fIdx, cCfg, true
	}
	return 0, 0, 0, false
}

func parseESDSConfig(b []byte) ([]byte, bool) {
	descriptor := func(in []byte) (tag byte, body, rest []byte, ok bool) {
		if len(in) < 2 {
			return 0, nil, nil, false
		}
		tag, in = in[0], in[1:]
		n := 0
		for i := 0; i < 4 && len(in) > 0; i++ {
			c := in[0]
			in = in[1:]
			n = n<<7 | int(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
		if n > len(in) {
			return 0, nil, nil, false
		}
		return tag, in[:n], in[n:], true
	}
	tag, es, _, ok := descriptor(b)
	if !ok || tag != 3 || len(es) < 3 {
		return nil, false
	}
	flags := es[2]
	es = es[3:]
	if flags&0x80 != 0 {
		es = es[min(2, len(es)):]
	}
	if flags&0x40 != 0 && len(es) > 0 {
		es = es[min(1+int(es[0]), len(es)):]
	}
	if flags&0x20 != 0 {
		es = es[min(2, len(es)):]
	}
	tag, dc, _, ok := descriptor(es)
	if !ok || tag != 4 || len(dc) < 13 {
		return nil, false
	}
	switch dc[0] {
	case 0x40, 0x66, 0x67, 0x68:
	default:
		return nil, false
	}
	tag, asc, _, ok := descriptor(dc[13:])
	if !ok || tag != 5 || len(asc) < 2 {
		return nil, false
	}
	return asc, true
}

func hasMP4Samples(stbl []byte) bool {
	if stsz := mp4Child(stbl, "stsz"); len(stsz) >= 12 {
		return binary.BigEndian.Uint32(stsz[8:12]) > 0
	}
	if stz2 := mp4Child(stbl, "stz2"); len(stz2) >= 12 {
		return binary.BigEndian.Uint32(stz2[8:12]) > 0
	}
	return false
}

func indexTrakStbl(stbl []byte) ([]packetLoc, error) {
	var sizes []uint32
	if stsz := mp4Child(stbl, "stsz"); len(stsz) >= 12 {
		fixed := binary.BigEndian.Uint32(stsz[4:8])
		cnt := int(binary.BigEndian.Uint32(stsz[8:12]))
		if cnt <= 0 || cnt > maxMP4Samples || (fixed == 0 && len(stsz) < 12+cnt*4) {
			return nil, errors.New("bridge: incomplete MP4 sample table")
		}
		sizes = make([]uint32, cnt)
		for i := range cnt {
			if fixed != 0 {
				sizes[i] = fixed
			} else {
				sizes[i] = binary.BigEndian.Uint32(stsz[12+i*4 : 16+i*4])
			}
		}
	} else if stz2 := mp4Child(stbl, "stz2"); len(stz2) >= 12 {
		field := int(stz2[7])
		cnt := int(binary.BigEndian.Uint32(stz2[8:12]))
		if cnt <= 0 || cnt > maxMP4Samples || (field != 4 && field != 8 && field != 16) || len(stz2) < 12+(cnt*field+7)/8 {
			return nil, errors.New("bridge: incomplete MP4 sample table")
		}
		sizes = make([]uint32, cnt)
		b := stz2[12:]
		for i := range cnt {
			switch field {
			case 4:
				sizes[i] = uint32(b[i/2]>>(4*(1-i%2))) & 0x0f
			case 8:
				sizes[i] = uint32(b[i])
			case 16:
				sizes[i] = uint32(binary.BigEndian.Uint16(b[2*i : 2*i+2]))
			}
		}
	} else {
		return nil, errors.New("bridge: incomplete MP4 sample table")
	}

	if stts := mp4Child(stbl, "stts"); len(stts) >= 8 {
		entryCnt := int(binary.BigEndian.Uint32(stts[4:8]))
		if entryCnt > 0 && len(stts) >= 8+entryCnt*8 {
			var totalStts int64
			for i := range entryCnt {
				totalStts += int64(binary.BigEndian.Uint32(stts[8+i*8 : 12+i*8]))
			}
			if totalStts > 0 && totalStts < int64(len(sizes)) {
				sizes = sizes[:totalStts]
			}
		}
	}

	var chunks []int64
	if stco := mp4Child(stbl, "stco"); len(stco) >= 8 {
		cnt := int(binary.BigEndian.Uint32(stco[4:8]))
		if cnt <= 0 || cnt > maxMP4Samples || len(stco) < 8+cnt*4 {
			return nil, errors.New("bridge: incomplete MP4 sample table")
		}
		chunks = make([]int64, cnt)
		for i := range cnt {
			chunks[i] = int64(binary.BigEndian.Uint32(stco[8+i*4 : 12+i*4]))
		}
	} else if co64 := mp4Child(stbl, "co64"); len(co64) >= 8 {
		cnt := int(binary.BigEndian.Uint32(co64[4:8]))
		if cnt <= 0 || cnt > maxMP4Samples || len(co64) < 8+cnt*8 {
			return nil, errors.New("bridge: incomplete MP4 sample table")
		}
		chunks = make([]int64, cnt)
		for i := range cnt {
			chunks[i] = int64(binary.BigEndian.Uint64(co64[8+i*8 : 16+i*8]))
		}
	} else {
		return nil, errors.New("bridge: incomplete MP4 sample table")
	}

	stscRaw := mp4Child(stbl, "stsc")
	if len(stscRaw) < 8 {
		return nil, errors.New("bridge: incomplete MP4 sample table")
	}
	stscCnt := int(binary.BigEndian.Uint32(stscRaw[4:8]))
	if stscCnt <= 0 || stscCnt > maxMP4Samples || len(stscRaw) < 8+stscCnt*12 {
		return nil, errors.New("bridge: incomplete MP4 sample table")
	}
	stsc := make([][3]uint32, stscCnt)
	for i := range stscCnt {
		stsc[i] = [3]uint32{
			binary.BigEndian.Uint32(stscRaw[8+i*12 : 12+i*12]),
			binary.BigEndian.Uint32(stscRaw[12+i*12 : 16+i*12]),
			binary.BigEndian.Uint32(stscRaw[16+i*12 : 20+i*12]),
		}
	}

	locs := make([]packetLoc, 0, len(sizes))
	sampleIdx := 0
	for chunkIdx, chunkOff := range chunks {
		chunkNum := uint32(chunkIdx + 1)
		spc := stsc[0][1]
		for _, entry := range stsc {
			if chunkNum >= entry[0] {
				spc = entry[1]
			} else {
				break
			}
		}
		off := chunkOff
		for range int(spc) {
			if sampleIdx >= len(sizes) {
				break
			}
			sz := sizes[sampleIdx]
			sampleIdx++
			if sz > 0 {
				locs = append(locs, packetLoc{offset: off, size: sz})
			}
			off += int64(sz)
		}
	}
	if len(locs) == 0 {
		return nil, errors.New("bridge: incomplete MP4 sample table")
	}
	return locs, nil
}

func readMP4BoxHeader(r io.ReaderAt, off, limit int64, hdr []byte) (boxType string, boxSize, headerLen int64, err error) {
	if _, err := readFullAt(r, hdr[:8], off); err != nil {
		return "", 0, 0, err
	}
	sz := uint64(binary.BigEndian.Uint32(hdr[:4]))
	boxType = string(hdr[4:8])
	headerLen = 8
	if sz == 1 {
		if _, err := readFullAt(r, hdr[8:16], off+8); err != nil {
			return "", 0, 0, err
		}
		sz = binary.BigEndian.Uint64(hdr[8:16])
		headerLen = 16
	} else if sz == 0 {
		if limit > off {
			sz = uint64(limit - off)
		} else {
			return "", 0, 0, io.EOF
		}
	}
	if int64(sz) < headerLen {
		return "", 0, 0, errors.New("bridge: invalid MP4 box size")
	}
	return boxType, int64(sz), headerLen, nil
}

func readFullAt(r io.ReaderAt, dst []byte, off int64) (int, error) {
	total := 0
	for total < len(dst) {
		n, err := r.ReadAt(dst[total:], off+int64(total))
		total += n
		if err != nil {
			if total == len(dst) {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}
