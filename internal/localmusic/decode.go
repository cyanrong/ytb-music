package localmusic

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/audio"
	_ "github.com/egoist/mygo/audio/aac"
	_ "github.com/egoist/mygo/audio/flac"
	_ "github.com/egoist/mygo/audio/mp3"
	_ "github.com/egoist/mygo/audio/mp4"
	_ "github.com/egoist/mygo/audio/vorbis"
)

var dsdByteAvg [256]float32

func init() {
	for i := range 256 {
		dsdByteAvg[i] = (float32(bits.OnesCount8(uint8(i))) - 4.0) * 0.25
	}
}

// OpenDecoder looks up trackID in the library and opens a pure-Go audio.Decoder
// (which also implements io.Closer), wrapping CUE tracks in a frame-accurate
// clipDecoder.
func (l *Library) OpenDecoder(trackID string) (audio.Decoder, *LocalTrack, error) {
	tr, ok := l.TrackByID(trackID)
	if !ok || tr == nil {
		return nil, nil, errors.New("localmusic: track not found: " + trackID)
	}
	dec, err := OpenTrackDecoder(tr)
	if err != nil {
		return nil, nil, err
	}
	return dec, tr, nil
}

// OpenTrackDecoder opens a pure-Go audio.Decoder (also implementing io.Closer)
// directly from a LocalTrack descriptor.
func OpenTrackDecoder(tr *LocalTrack) (audio.Decoder, error) {
	if tr == nil || tr.Path == "" {
		return nil, errors.New("localmusic: empty track path")
	}
	f, err := os.Open(tr.Path)
	if err != nil {
		return nil, err
	}

	var dec audio.Decoder
	ext := strings.ToLower(filepath.Ext(tr.Path))
	switch ext {
	case ".aif", ".aiff", ".aifc":
		dec, err = newAIFFDecoder(f)
	case ".dsf":
		dec, err = newDSFDecoder(f)
	case ".dff":
		dec, err = newDFFDecoder(f)
	default:
		var inner audio.Decoder
		inner, _, err = audio.NewDecoder(f)
		if err == nil {
			dec = &fileDecoder{Decoder: inner, f: f}
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	if tr.Spec.IsCUE || tr.Spec.HasCueFrames || tr.Spec.ClipStartMs > 0 || tr.Spec.ClipEndMs > 0 || tr.Spec.ClipStartFrames > 0 || tr.Spec.ClipEndFrames > 0 {
		clipped, err := newClipDecoder(dec, tr.Spec)
		if err != nil {
			if c, ok := dec.(io.Closer); ok {
				_ = c.Close()
			}
			return nil, err
		}
		return clipped, nil
	}
	return dec, nil
}

// fileDecoder wraps a standard audio.Decoder together with its underlying *os.File.
type fileDecoder struct {
	audio.Decoder
	f *os.File
}

func (d *fileDecoder) Close() error {
	if d.f != nil {
		err := d.f.Close()
		d.f = nil
		return err
	}
	return nil
}

// --- Pure-Go AIFF / AIFC Decoder ---

type aiffEncoding int

const (
	aiffPCMBE aiffEncoding = iota
	aiffPCMLE              // AIFC "sowt"
	aiffPCMU8              // AIFC "raw "
	aiffFloat32BE
	aiffFloat64BE
)

type aiffDecoder struct {
	f          *os.File
	br         *bufio.Reader
	rate       int
	channels   int
	sampleBits int
	sampleByte int
	enc        aiffEncoding
	start      int64
	length     int64
	pos        int64
	buf        []byte
}

func newAIFFDecoder(f *os.File) (*aiffDecoder, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := st.Size()
	var form [12]byte
	if _, err := f.ReadAt(form[:], 0); err != nil {
		return nil, errors.New("localmusic: invalid AIFF file")
	}
	ft := string(form[8:12])
	if string(form[0:4]) != "FORM" || (ft != "AIFF" && ft != "AIFC") {
		return nil, errors.New("localmusic: invalid AIFF FORM header")
	}

	d := &aiffDecoder{f: f, enc: aiffPCMBE, length: -1}
	var (
		haveComm bool
		haveSSND bool
		ssndEnd  int64
	)
	off := int64(12)
	for off+8 <= fileSize && (!haveComm || !haveSSND) {
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
			if csize < 18 || csize > 1024 {
				return nil, errors.New("localmusic: invalid AIFF COMM chunk")
			}
			b := make([]byte, csize)
			if _, err := f.ReadAt(b, off); err != nil {
				return nil, err
			}
			d.channels = int(int16(binary.BigEndian.Uint16(b[0:2])))
			d.length = int64(binary.BigEndian.Uint32(b[2:6]))
			d.sampleBits = int(int16(binary.BigEndian.Uint16(b[6:8])))
			rateF := parseIEEE80(b[8:18])
			if !math.IsNaN(rateF) && !math.IsInf(rateF, 0) && rateF >= 1 && rateF <= maxDSDSampleRate {
				d.rate = int(math.Round(rateF))
			}
			if ft == "AIFC" && len(b) >= 22 {
				comp := string(b[18:22])
				switch comp {
				case "NONE", "twos", "in24", "in32":
					d.enc = aiffPCMBE
				case "sowt":
					d.enc = aiffPCMLE
				case "raw ":
					d.enc = aiffPCMU8
				case "fl32", "FL32":
					d.enc = aiffFloat32BE
					d.sampleBits = 32
				case "fl64", "FL64":
					d.enc = aiffFloat64BE
					d.sampleBits = 64
				default:
					return nil, errors.New("localmusic: unsupported AIFC compression " + comp)
				}
			}
			d.sampleByte = (d.sampleBits + 7) / 8
			if d.channels < 1 || d.channels > 32 || d.rate < 1 || (d.sampleByte != 1 && d.sampleByte != 2 && d.sampleByte != 3 && d.sampleByte != 4 && d.sampleByte != 8) {
				return nil, errors.New("localmusic: unsupported AIFF PCM parameters")
			}
			haveComm = true

		case "SSND":
			if csize < 8 {
				return nil, errors.New("localmusic: short AIFF SSND chunk")
			}
			var shdr [8]byte
			if _, err := f.ReadAt(shdr[:], off); err != nil {
				return nil, err
			}
			dataOffset := int64(binary.BigEndian.Uint32(shdr[0:4]))
			d.start = off + 8 + dataOffset
			ssndEnd = off + csize
			haveSSND = true
		}

		step := csizeU
		if step&1 == 1 && step < rem {
			step++
		}
		off += int64(step)
	}

	if !haveComm || !haveSSND {
		return nil, errors.New("localmusic: incomplete AIFF chunks")
	}
	if d.start < 0 || d.start > fileSize {
		return nil, errors.New("localmusic: invalid AIFF SSND offset")
	}
	availBytes := max(min(ssndEnd, fileSize)-d.start, 0)
	availFrames := availBytes / int64(d.frameBytes())
	if d.length < 0 || d.length > availFrames {
		d.length = availFrames
	}
	if _, err := f.Seek(d.start, io.SeekStart); err != nil {
		return nil, err
	}
	d.br = bufio.NewReaderSize(f, 32<<10)
	return d, nil
}

func (d *aiffDecoder) frameBytes() int { return d.channels * d.sampleByte }
func (d *aiffDecoder) SampleRate() int { return d.rate }
func (d *aiffDecoder) Channels() int   { return d.channels }
func (d *aiffDecoder) Len() int64      { return d.length }

func (d *aiffDecoder) Read(dst []float32) (int, error) {
	fb := d.frameBytes()
	n := len(dst) / d.channels
	if d.length >= 0 {
		n = int(min(int64(n), d.length-d.pos))
	}
	if n <= 0 {
		return 0, io.EOF
	}
	need := n * fb
	if cap(d.buf) < need {
		d.buf = make([]byte, need)
	}
	b := d.buf[:need]
	got, err := io.ReadFull(d.br, b)
	n = got / fb
	if n == 0 {
		if err == nil || err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return 0, err
	}

	sb := d.sampleByte
	totalSamples := n * d.channels
	for i := range totalSamples {
		s := b[i*sb : (i+1)*sb]
		var v float32
		switch d.enc {
		case aiffFloat32BE:
			v = math.Float32frombits(binary.BigEndian.Uint32(s))
		case aiffFloat64BE:
			v = float32(math.Float64frombits(binary.BigEndian.Uint64(s)))
		case aiffPCMU8:
			v = (float32(s[0]) - 128.0) / 128.0
		case aiffPCMLE:
			switch sb {
			case 1:
				v = float32(int8(s[0])) / 128.0
			case 2:
				v = float32(int16(binary.LittleEndian.Uint16(s))) / (1 << 15)
			case 3:
				v = float32(int32(uint32(s[0])<<8|uint32(s[1])<<16|uint32(s[2])<<24)>>8) / (1 << 23)
			default:
				v = float32(int32(binary.LittleEndian.Uint32(s))) / (1 << 31)
			}
		default: // Big-endian signed PCM
			switch sb {
			case 1:
				v = float32(int8(s[0])) / 128.0
			case 2:
				v = float32(int16(binary.BigEndian.Uint16(s))) / (1 << 15)
			case 3:
				v = float32(int32(uint32(s[0])<<24|uint32(s[1])<<16|uint32(s[2])<<8)>>8) / (1 << 23)
			default:
				v = float32(int32(binary.BigEndian.Uint32(s))) / (1 << 31)
			}
		}
		dst[i] = v
	}
	d.pos += int64(n)
	return totalSamples, nil
}

func (d *aiffDecoder) SetPosition(frame int64) error {
	if frame < 0 || (d.length >= 0 && frame > d.length) {
		return errors.New("localmusic: AIFF seek out of range")
	}
	if _, err := d.f.Seek(d.start+frame*int64(d.frameBytes()), io.SeekStart); err != nil {
		return err
	}
	d.br.Reset(d.f)
	d.pos = frame
	return nil
}

func (d *aiffDecoder) Close() error {
	if d.f != nil {
		err := d.f.Close()
		d.f = nil
		return err
	}
	return nil
}

// --- Pure-Go DSD (DSF & DFF) Preview Decoder ---

type dsdChannelState struct {
	h0, h1, h2 float32 // 3-stage CIC / 4-tap [1,3,3,1]/8 FIR history
	dcX, dcY   float32 // 1st-order DC blocker state
}

func (st *dsdChannelState) reset() {
	*st = dsdChannelState{}
}

func (st *dsdChannelState) step(blockMean float32) float32 {
	// 4-tap binomial/CIC-3 low-pass across consecutive decimation blocks.
	lp := (st.h0 + 3*st.h1 + 3*st.h2 + blockMean) * 0.125
	st.h0, st.h1, st.h2 = st.h1, st.h2, blockMean

	// Gentle 1st-order DC block (~7 Hz @ 88.2 kHz) + 0.5 gain (6 dB DSD headroom).
	y := lp - st.dcX + 0.9995*st.dcY
	st.dcX = lp
	st.dcY = y

	out := y * 0.5
	if out > 1 {
		return 1
	}
	if out < -1 {
		return -1
	}
	return out
}

type dsdPreviewDecoder struct {
	f              *os.File
	br             *bufio.Reader // used by interleaved DFF mode
	isDSF          bool
	srcChannels    int
	dsdRate        int
	outRate        int
	decimBits      int // 32 (DSD64), 64 (DSD128), 128 (DSD256), 256 (DSD512)
	bytesPerStep   int // decimBits / 8
	dataStart      int64
	dataBytes      int64
	blockSizePerCh int   // DSF per-channel block size (typically 4096)
	curBlockIdx    int64 // DSF loaded block group index, or -1
	inBlockOff     int   // DSF byte offset within current block
	blockBuf       []byte
	dffBuf         []byte
	totalFrames    int64
	pos            int64
	left, right    dsdChannelState
}

func chooseDSDDecimation(dsdRate int) (decimBits, outRate int, err error) {
	if dsdRate < 2822400 || dsdRate > maxDSDSampleRate {
		return 0, 0, errors.New("localmusic: invalid DSD sample rate")
	}
	switch {
	case dsdRate >= 90316800:
		decimBits = 1024
	case dsdRate >= 45158400:
		decimBits = 512
	case dsdRate >= 22579200:
		decimBits = 256
	case dsdRate >= 11289600:
		decimBits = 128
	case dsdRate >= 5644800:
		decimBits = 64
	default:
		decimBits = 32
	}
	if dsdRate%decimBits != 0 {
		return 0, 0, errors.New("localmusic: invalid DSD sample rate")
	}
	outRate = dsdRate / decimBits
	if outRate <= 0 {
		return 0, 0, errors.New("localmusic: invalid decimated DSD rate")
	}
	return decimBits, outRate, nil
}

func newDSFDecoder(f *os.File) (*dsdPreviewDecoder, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := st.Size()
	if fileSize < 28+52+12 {
		return nil, errors.New("localmusic: invalid DSF header")
	}

	var dsdHdr [28]byte
	if _, err := f.ReadAt(dsdHdr[:], 0); err != nil || string(dsdHdr[0:4]) != "DSD " {
		return nil, errors.New("localmusic: invalid DSF header")
	}
	dsdChunkU := binary.LittleEndian.Uint64(dsdHdr[4:12])
	if dsdChunkU < 28 || dsdChunkU > uint64(fileSize) {
		return nil, errors.New("localmusic: invalid DSF header")
	}
	dsdChunkSize := int64(dsdChunkU)
	if fileSize-dsdChunkSize < 52 {
		return nil, errors.New("localmusic: invalid DSF fmt chunk")
	}

	var fmtHdr [52]byte
	if _, err := f.ReadAt(fmtHdr[:], dsdChunkSize); err != nil || string(fmtHdr[0:4]) != "fmt " {
		return nil, errors.New("localmusic: invalid DSF fmt chunk")
	}
	fmtChunkU := binary.LittleEndian.Uint64(fmtHdr[4:12])
	if fmtChunkU < 52 || fmtChunkU > uint64(fileSize-dsdChunkSize) {
		return nil, errors.New("localmusic: invalid DSF fmt chunk")
	}
	fmtChunkSize := int64(fmtChunkU)

	channelsU := binary.LittleEndian.Uint32(fmtHdr[24:28])
	dsdRateU := binary.LittleEndian.Uint32(fmtHdr[28:32])
	sampleCount := binary.LittleEndian.Uint64(fmtHdr[36:44])
	blockSizeU := binary.LittleEndian.Uint32(fmtHdr[44:48])

	if channelsU < 1 || channelsU > 8 {
		return nil, errors.New("localmusic: invalid DSF channel count")
	}
	if dsdRateU < 2822400 || dsdRateU > maxDSDSampleRate {
		return nil, errors.New("localmusic: invalid DSD sample rate")
	}
	channels := int(channelsU)
	dsdRate := int(dsdRateU)
	decimBits, outRate, err := chooseDSDDecimation(dsdRate)
	if err != nil {
		return nil, err
	}
	bytesPerStep := decimBits / 8
	if blockSizeU < 1 || blockSizeU > 65536 {
		return nil, errors.New("localmusic: incompatible DSF block size")
	}
	blockSize := int(blockSizeU)
	if blockSize < bytesPerStep || blockSize%bytesPerStep != 0 {
		return nil, errors.New("localmusic: incompatible DSF block size")
	}

	dataHdrOff := dsdChunkSize + fmtChunkSize
	if fileSize-dataHdrOff < 12 {
		return nil, errors.New("localmusic: DSF missing data chunk")
	}
	var dataHdr [12]byte
	if _, err := f.ReadAt(dataHdr[:], dataHdrOff); err != nil || string(dataHdr[0:4]) != "data" {
		return nil, errors.New("localmusic: DSF missing data chunk")
	}
	dataChunkU := binary.LittleEndian.Uint64(dataHdr[4:12])
	if dataChunkU < 12 {
		return nil, errors.New("localmusic: invalid DSF data chunk")
	}
	dataPayloadBytes := dataChunkU - 12
	dataOffset := dataHdrOff + 12
	fileRem := uint64(fileSize - dataOffset)
	availU := min(dataPayloadBytes, fileRem)
	avail := int64(availU)
	groupBytes := int64(channels * blockSize)
	if avail < groupBytes {
		return nil, errors.New("localmusic: truncated DSF audio payload")
	}

	maxSamples := uint64(avail/groupBytes) * uint64(blockSize) * 8
	if sampleCount == 0 || sampleCount > maxSamples {
		sampleCount = maxSamples
	}
	totalFrames := int64(sampleCount / uint64(decimBits))
	if totalFrames <= 0 {
		return nil, errors.New("localmusic: empty DSF audio payload")
	}

	d := &dsdPreviewDecoder{
		f:              f,
		isDSF:          true,
		srcChannels:    channels,
		dsdRate:        dsdRate,
		outRate:        outRate,
		decimBits:      decimBits,
		bytesPerStep:   bytesPerStep,
		dataStart:      dataOffset,
		dataBytes:      avail,
		blockSizePerCh: blockSize,
		curBlockIdx:    -1,
		blockBuf:       make([]byte, channels*blockSize),
		totalFrames:    totalFrames,
	}
	return d, nil
}

func newDFFDecoder(f *os.File) (*dsdPreviewDecoder, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := st.Size()

	var frm [16]byte
	if _, err := f.ReadAt(frm[:], 0); err != nil || string(frm[0:4]) != "FRM8" || string(frm[12:16]) != "DSD " {
		return nil, errors.New("localmusic: invalid DSDIFF header")
	}

	var (
		meta      parsedMeta
		dataStart int64
		dataBytes int64
		compType  = "DSD "
	)
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
					parseDFFProp(b[4:], &meta)
					compType = findDFFCompression(b[4:])
				}
			}
		case "DSD ":
			dataStart = off
			dataBytes = csize
		}
		step := csizeU
		if step&1 == 1 && step < rem {
			step++
		}
		off += int64(step)
	}
	if compType != "" && compType != "DSD " {
		return nil, errors.New("localmusic: compressed DST DSDIFF is not supported for local preview")
	}
	if meta.spec.SampleRate <= 0 || meta.spec.Channels < 1 || meta.spec.Channels > 8 || dataStart <= 0 {
		return nil, errors.New("localmusic: incomplete DSDIFF audio stream")
	}
	decimBits, outRate, err := chooseDSDDecimation(meta.spec.SampleRate)
	if err != nil {
		return nil, err
	}
	bytesPerStep := decimBits / 8
	dataBytes = min(dataBytes, max(fileSize-dataStart, 0))
	totalFrames := dataBytes / int64(meta.spec.Channels*bytesPerStep)
	if totalFrames <= 0 {
		return nil, errors.New("localmusic: truncated DSDIFF audio payload")
	}
	if _, err := f.Seek(dataStart, io.SeekStart); err != nil {
		return nil, err
	}

	return &dsdPreviewDecoder{
		f:            f,
		br:           bufio.NewReaderSize(f, 64<<10),
		isDSF:        false,
		srcChannels:  meta.spec.Channels,
		dsdRate:      meta.spec.SampleRate,
		outRate:      outRate,
		decimBits:    decimBits,
		bytesPerStep: bytesPerStep,
		dataStart:    dataStart,
		dataBytes:    dataBytes,
		totalFrames:  totalFrames,
	}, nil
}

func findDFFCompression(b []byte) string {
	pos := 0
	for pos+12 <= len(b) {
		id := string(b[pos : pos+4])
		sz := binary.BigEndian.Uint64(b[pos+4 : pos+12])
		pos += 12
		rem := uint64(len(b) - pos)
		if sz > rem {
			break
		}
		if id == "CMPR" && sz >= 4 {
			return string(b[pos : pos+4])
		}
		step := sz
		if step&1 == 1 && step < rem {
			step++
		}
		pos += int(step)
	}
	return "DSD "
}

func (d *dsdPreviewDecoder) SampleRate() int { return d.outRate }
func (d *dsdPreviewDecoder) Channels() int   { return 2 }
func (d *dsdPreviewDecoder) Len() int64      { return d.totalFrames }

func (d *dsdPreviewDecoder) Read(dst []float32) (int, error) {
	framesWanted := len(dst) / 2
	if d.totalFrames >= 0 {
		framesWanted = int(min(int64(framesWanted), d.totalFrames-d.pos))
	}
	if framesWanted <= 0 {
		return 0, io.EOF
	}
	if d.isDSF {
		return d.readDSF(dst, framesWanted)
	}
	return d.readDFF(dst, framesWanted)
}

func (d *dsdPreviewDecoder) readDSF(dst []float32, framesWanted int) (int, error) {
	bs := d.bytesPerStep
	invBS := 1.0 / float32(bs)
	writtenFrames := 0

	for writtenFrames < framesWanted {
		byteIdxPerCh := d.pos * int64(bs)
		wantBlock := byteIdxPerCh / int64(d.blockSizePerCh)
		inOff := int(byteIdxPerCh % int64(d.blockSizePerCh))

		if d.curBlockIdx != wantBlock {
			groupOff := d.dataStart + wantBlock*int64(len(d.blockBuf))
			clear(d.blockBuf)
			n, err := d.f.ReadAt(d.blockBuf, groupOff)
			if n == 0 {
				if writtenFrames > 0 {
					return writtenFrames * 2, nil
				}
				if err == nil || err == io.ErrUnexpectedEOF {
					err = io.EOF
				}
				return 0, err
			}
			d.curBlockIdx = wantBlock
		}

		ch0Block := d.blockBuf[:d.blockSizePerCh]
		ch1Block := ch0Block
		if d.srcChannels >= 2 {
			ch1Block = d.blockBuf[d.blockSizePerCh : 2*d.blockSizePerCh]
		}

		for writtenFrames < framesWanted && inOff+bs <= d.blockSizePerCh {
			var sumL, sumR float32
			lSlice := ch0Block[inOff : inOff+bs]
			rSlice := ch1Block[inOff : inOff+bs]
			for k := range bs {
				sumL += dsdByteAvg[lSlice[k]]
				sumR += dsdByteAvg[rSlice[k]]
			}
			dst[writtenFrames*2] = d.left.step(sumL * invBS)
			dst[writtenFrames*2+1] = d.right.step(sumR * invBS)
			inOff += bs
			d.pos++
			writtenFrames++
		}
	}
	return writtenFrames * 2, nil
}

func (d *dsdPreviewDecoder) readDFF(dst []float32, framesWanted int) (int, error) {
	stride := d.srcChannels * d.bytesPerStep
	need := framesWanted * stride
	if cap(d.dffBuf) < need {
		d.dffBuf = make([]byte, need)
	}
	b := d.dffBuf[:need]
	got, err := io.ReadFull(d.br, b)
	frames := got / stride
	if frames == 0 {
		if err == nil || err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return 0, err
	}

	bs := d.bytesPerStep
	ch := d.srcChannels
	invBS := 1.0 / float32(bs)
	for fIdx := range frames {
		base := fIdx * stride
		var sumL, sumR float32
		if ch == 1 {
			for k := range bs {
				v := dsdByteAvg[b[base+k]]
				sumL += v
				sumR += v
			}
		} else {
			for k := range bs {
				off := base + k*ch
				sumL += dsdByteAvg[b[off]]
				sumR += dsdByteAvg[b[off+1]]
			}
		}
		dst[fIdx*2] = d.left.step(sumL * invBS)
		dst[fIdx*2+1] = d.right.step(sumR * invBS)
	}
	d.pos += int64(frames)
	return frames * 2, nil
}

func (d *dsdPreviewDecoder) SetPosition(frame int64) error {
	if frame < 0 || (d.totalFrames >= 0 && frame > d.totalFrames) {
		return errors.New("localmusic: DSD seek out of range")
	}
	d.left.reset()
	d.right.reset()
	if d.isDSF {
		d.pos = frame
		return nil
	}
	byteOff := d.dataStart + frame*int64(d.srcChannels*d.bytesPerStep)
	if _, err := d.f.Seek(byteOff, io.SeekStart); err != nil {
		return err
	}
	d.br.Reset(d.f)
	d.pos = frame
	return nil
}

func (d *dsdPreviewDecoder) Close() error {
	if d.f != nil {
		err := d.f.Close()
		d.f = nil
		return err
	}
	return nil
}

// --- Frame-Accurate CUE Clip Decoder ---

type clipDecoder struct {
	dec        audio.Decoder
	startFrame int64
	endFrame   int64 // -1 if unbounded to end of underlying decoder
	pos        int64 // relative to startFrame
}

func newClipDecoder(dec audio.Decoder, spec AudioSpec) (*clipDecoder, error) {
	sr := dec.SampleRate()
	if sr <= 0 {
		return nil, errors.New("localmusic: decoder has invalid sample rate")
	}
	total := dec.Len()
	var startFrame int64
	if spec.HasCueFrames {
		startFrame = max((spec.ClipStartFrames*int64(sr))/75, 0)
	} else {
		startFrame = max((spec.ClipStartMs*int64(sr))/1000, 0)
	}
	if total >= 0 && startFrame > total {
		startFrame = total
	}

	endFrame := int64(-1)
	if spec.HasCueFrames {
		if spec.ClipEndFrames > 0 {
			endFrame = (spec.ClipEndFrames * int64(sr)) / 75
			if total >= 0 && endFrame > total {
				endFrame = total
			}
			if endFrame < startFrame {
				endFrame = startFrame
			}
		} else if total >= 0 {
			endFrame = total
		}
	} else if spec.ClipEndMs > 0 {
		endFrame = (spec.ClipEndMs * int64(sr)) / 1000
		if total >= 0 && endFrame > total {
			endFrame = total
		}
		if endFrame < startFrame {
			endFrame = startFrame
		}
	} else if total >= 0 {
		endFrame = total
	}

	if err := dec.SetPosition(startFrame); err != nil {
		return nil, err
	}
	return &clipDecoder{
		dec:        dec,
		startFrame: startFrame,
		endFrame:   endFrame,
	}, nil
}

func (c *clipDecoder) SampleRate() int { return c.dec.SampleRate() }
func (c *clipDecoder) Channels() int   { return c.dec.Channels() }

func (c *clipDecoder) Len() int64 {
	if c.endFrame < 0 {
		return -1
	}
	return c.endFrame - c.startFrame
}

func (c *clipDecoder) Read(dst []float32) (int, error) {
	ch := c.dec.Channels()
	if ch <= 0 {
		return 0, io.EOF
	}
	framesAvail := len(dst) / ch
	if c.endFrame >= 0 {
		rem := (c.endFrame - c.startFrame) - c.pos
		if rem <= 0 {
			return 0, io.EOF
		}
		framesAvail = int(min(int64(framesAvail), rem))
	}
	if framesAvail <= 0 {
		return 0, nil
	}
	n, err := c.dec.Read(dst[:framesAvail*ch])
	c.pos += int64(n / ch)
	return n, err
}

func (c *clipDecoder) SetPosition(frame int64) error {
	length := c.Len()
	if frame < 0 || (length >= 0 && frame > length) {
		return errors.New("localmusic: clip seek out of range")
	}
	if err := c.dec.SetPosition(c.startFrame + frame); err != nil {
		return err
	}
	c.pos = frame
	return nil
}

func (c *clipDecoder) Close() error {
	if cl, ok := c.dec.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}
