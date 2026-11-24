package bridge

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/egoist/mygo/audio/mp4"
)

type packetLoc struct {
	offset int64
	size   uint32
}

// aacADTSReader extracts raw AAC frames from either a standard MP4 (`stbl`) or
// a fragmented DASH MP4 (`moof`/`trun`/`mdat`, as YouTube serves itag 140/141),
// supports O(1)/indexed sample seeking, and wraps each frame in an ADTS header.
type aacADTSReader struct {
	r          io.ReaderAt
	size       int64
	profile    int // 0-based ADTS profile (AAC-LC = 1)
	freqIdx    int
	chanCfg    int
	sampleRate int

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

func openAACADTSReader(r io.ReaderAt, size int64) (*aacADTSReader, error) {
	tr, err := mp4.ReadTrack(r, size)
	if err != nil {
		return nil, err
	}
	if tr.Codec != "mp4a" || len(tr.Config) < 2 {
		return nil, errors.New("bridge: stream is not AAC-in-MP4")
	}
	aot := int(tr.Config[0] >> 3)
	freqIdx := int((tr.Config[0]&0x07)<<1 | (tr.Config[1] >> 7))
	chanCfg := int((tr.Config[1] >> 3) & 0x0f)
	if aot == 5 || aot == 29 {
		aot = 2
	}
	if freqIdx >= 13 || adtsSampleRates[freqIdx] == 0 || chanCfg == 0 {
		return nil, errors.New("bridge: unsupported AudioSpecificConfig")
	}
	ar := &aacADTSReader{
		r:          r,
		size:       size,
		profile:    max(0, min(3, aot-1)),
		freqIdx:    freqIdx,
		chanCfg:    chanCfg,
		sampleRate: adtsSampleRates[freqIdx],
		fragmented: tr.Fragmented,
	}
	if !tr.Fragmented {
		locs, err := indexNonFragmentedAAC(r, size)
		if err != nil {
			return nil, err
		}
		ar.packets = locs
		ar.allScanned = true
	}
	return ar, nil
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

func (ar *aacADTSReader) indexNextFragment() error {
	var hdr [16]byte
	var moofOff, moofEnd int64
	for {
		if ar.size > 0 && ar.scanOffset >= ar.size {
			return io.EOF
		}
		boxType, boxSize, _, err := readMP4BoxHeader(ar.r, ar.scanOffset, ar.size, hdr[:])
		if err != nil {
			return err
		}
		nextOff := ar.scanOffset + boxSize
		if boxType == "moof" {
			moofOff = ar.scanOffset
			moofEnd = nextOff
			ar.scanOffset = nextOff
			break
		}
		ar.scanOffset = nextOff
	}

	sizes, dataOffset, hasDataOffset, err := parseMoofTrun(ar.r, moofOff, moofEnd)
	if err != nil {
		return err
	}
	if len(sizes) == 0 {
		return nil
	}

	payloadPos := moofOff + dataOffset
	if !hasDataOffset {
		for {
			if ar.size > 0 && ar.scanOffset >= ar.size {
				return io.ErrUnexpectedEOF
			}
			boxType, boxSize, headerLen, err := readMP4BoxHeader(ar.r, ar.scanOffset, ar.size, hdr[:])
			if err != nil {
				return err
			}
			if boxType == "mdat" {
				payloadPos = ar.scanOffset + headerLen
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
			ar.scanOffset += boxSize
			if boxType == "mdat" {
				break
			}
		}
	}

	off := payloadPos
	for _, sz := range sizes {
		if sz > 0 && sz <= 1<<20 {
			ar.packets = append(ar.packets, packetLoc{offset: off, size: sz})
		}
		off += int64(sz)
	}
	return nil
}

func parseMoofTrun(r io.ReaderAt, moofOff, moofEnd int64) (sizes []uint32, dataOffset int64, hasDataOffset bool, err error) {
	var hdr [16]byte
	pos := moofOff + 8
	for pos+8 <= moofEnd {
		boxType, boxSize, headerLen, berr := readMP4BoxHeader(r, pos, moofEnd, hdr[:])
		if berr != nil {
			return nil, 0, false, berr
		}
		boxEnd := pos + boxSize
		if boxType == "traf" {
			tpos := pos + headerLen
			var defaultSampleSize uint32
			for tpos+8 <= boxEnd {
				cType, cSize, cHdr, cerr := readMP4BoxHeader(r, tpos, boxEnd, hdr[:])
				if cerr != nil {
					return nil, 0, false, cerr
				}
				cBody := make([]byte, cSize-cHdr)
				if _, cerr := readFullAt(r, cBody, tpos+cHdr); cerr != nil {
					return nil, 0, false, cerr
				}
				switch cType {
				case "tfhd":
					if len(cBody) >= 8 {
						flags := uint32(cBody[1])<<16 | uint32(cBody[2])<<8 | uint32(cBody[3])
						idx := 8
						if flags&0x000001 != 0 {
							idx += 8
						}
						if flags&0x000002 != 0 {
							idx += 4
						}
						if flags&0x000008 != 0 {
							idx += 4
						}
						if flags&0x000010 != 0 && idx+4 <= len(cBody) {
							defaultSampleSize = binary.BigEndian.Uint32(cBody[idx : idx+4])
						}
					}
				case "trun":
					if len(cBody) >= 8 {
						flags := uint32(cBody[1])<<16 | uint32(cBody[2])<<8 | uint32(cBody[3])
						count := int(binary.BigEndian.Uint32(cBody[4:8]))
						idx := 8
						if flags&0x000001 != 0 && idx+4 <= len(cBody) {
							dataOffset = int64(int32(binary.BigEndian.Uint32(cBody[idx : idx+4])))
							hasDataOffset = true
							idx += 4
						}
						if flags&0x000004 != 0 {
							idx += 4
						}
						hasDur := flags&0x000100 != 0
						hasSize := flags&0x000200 != 0
						hasFlags := flags&0x000400 != 0
						hasCTS := flags&0x000800 != 0
						for range count {
							if hasDur {
								idx += 4
							}
							sz := defaultSampleSize
							if hasSize && idx+4 <= len(cBody) {
								sz = binary.BigEndian.Uint32(cBody[idx : idx+4])
								idx += 4
							}
							if hasFlags {
								idx += 4
							}
							if hasCTS {
								idx += 4
							}
							sizes = append(sizes, sz)
						}
					}
				}
				tpos += cSize
			}
		}
		pos = boxEnd
	}
	return sizes, dataOffset, hasDataOffset, nil
}

func indexNonFragmentedAAC(r io.ReaderAt, size int64) ([]packetLoc, error) {
	var hdr [16]byte
	var walk func(start, end int64) (stsz []uint32, chunks []int64, stsc [][3]uint32)
	walk = func(start, end int64) (stsz []uint32, chunks []int64, stsc [][3]uint32) {
		for pos := start; pos+8 <= end; {
			bt, bs, hl, err := readMP4BoxHeader(r, pos, end, hdr[:])
			if err != nil || bs <= 0 {
				break
			}
			switch bt {
			case "moov", "trak", "mdia", "minf", "stbl":
				s1, c1, sc1 := walk(pos+hl, pos+bs)
				if len(s1) > 0 {
					stsz = s1
				}
				if len(c1) > 0 {
					chunks = c1
				}
				if len(sc1) > 0 {
					stsc = sc1
				}
			case "stsz":
				b := make([]byte, bs-hl)
				if _, err := readFullAt(r, b, pos+hl); err == nil && len(b) >= 12 {
					fixed := binary.BigEndian.Uint32(b[4:8])
					cnt := int(binary.BigEndian.Uint32(b[8:12]))
					stsz = make([]uint32, cnt)
					for i := range cnt {
						if fixed != 0 {
							stsz[i] = fixed
						} else if 12+(i+1)*4 <= len(b) {
							stsz[i] = binary.BigEndian.Uint32(b[12+i*4:])
						}
					}
				}
			case "stco":
				b := make([]byte, bs-hl)
				if _, err := readFullAt(r, b, pos+hl); err == nil && len(b) >= 8 {
					cnt := int(binary.BigEndian.Uint32(b[4:8]))
					chunks = make([]int64, 0, cnt)
					for i := range cnt {
						if 8+(i+1)*4 <= len(b) {
							chunks = append(chunks, int64(binary.BigEndian.Uint32(b[8+i*4:])))
						}
					}
				}
			case "co64":
				b := make([]byte, bs-hl)
				if _, err := readFullAt(r, b, pos+hl); err == nil && len(b) >= 8 {
					cnt := int(binary.BigEndian.Uint32(b[4:8]))
					chunks = make([]int64, 0, cnt)
					for i := range cnt {
						if 8+(i+1)*8 <= len(b) {
							chunks = append(chunks, int64(binary.BigEndian.Uint64(b[8+i*8:])))
						}
					}
				}
			case "stsc":
				b := make([]byte, bs-hl)
				if _, err := readFullAt(r, b, pos+hl); err == nil && len(b) >= 8 {
					cnt := int(binary.BigEndian.Uint32(b[4:8]))
					for i := range cnt {
						if 8+(i+1)*12 <= len(b) {
							stsc = append(stsc, [3]uint32{
								binary.BigEndian.Uint32(b[8+i*12:]),
								binary.BigEndian.Uint32(b[12+i*12:]),
								binary.BigEndian.Uint32(b[16+i*12:]),
							})
						}
					}
				}
			}
			pos += bs
		}
		return
	}
	stsz, chunks, stsc := walk(0, size)
	if len(stsz) == 0 || len(chunks) == 0 || len(stsc) == 0 {
		return nil, errors.New("bridge: incomplete MP4 sample table")
	}
	locs := make([]packetLoc, 0, len(stsz))
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
			if sampleIdx >= len(stsz) {
				break
			}
			sz := stsz[sampleIdx]
			sampleIdx++
			if sz > 0 {
				locs = append(locs, packetLoc{offset: off, size: sz})
			}
			off += int64(sz)
		}
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
