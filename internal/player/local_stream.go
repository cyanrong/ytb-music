package player

import (
	"io"
	"sync"
	"sync/atomic"

	"github.com/egoist/mygo/audio"
)

// localPCMStream wraps an arbitrary local audio.Decoder as interleaved stereo
// float32 PCM while feeding the player's 24-band FFT spectrum analyzer.
type localPCMStream struct {
	dec       audio.Decoder
	srcCh     int
	rate      int
	pos       int64
	spec      *spectrum
	writer    *specWriter
	scratch   []float32
	rem       []float32
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func newLocalPCMStream(dec audio.Decoder, spec *spectrum) *localPCMStream {
	ch := dec.Channels()
	if ch <= 0 {
		ch = 2
	}
	sr := dec.SampleRate()
	if sr <= 0 {
		sr = 44100
	}
	return &localPCMStream{
		dec:    dec,
		srcCh:  ch,
		rate:   sr,
		spec:   spec,
		writer: newSpecWriter(spec),
	}
}

func (s *localPCMStream) SampleRate() int { return s.rate }
func (s *localPCMStream) Channels() int   { return 2 }
func (s *localPCMStream) Len() int64      { return s.dec.Len() }

func (s *localPCMStream) Read(dst []float32) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	wantFrames := len(dst) / 2
	if wantFrames == 0 {
		return 0, nil
	}
	switch {
	case s.srcCh == 2:
		n, err := s.dec.Read(dst[:wantFrames*2])
		if s.closed.Load() {
			return 0, io.ErrClosedPipe
		}
		frames := n / 2
		out := frames * 2
		if out > 0 {
			s.writer.feed(s.pos, dst[:out])
			s.pos += int64(frames)
		}
		return out, err

	case s.srcCh == 1:
		n, err := s.dec.Read(dst[:wantFrames:wantFrames])
		if s.closed.Load() {
			return 0, io.ErrClosedPipe
		}
		for i := n - 1; i >= 0; i-- {
			v := dst[i]
			dst[i*2] = v
			dst[i*2+1] = v
		}
		out := n * 2
		if out > 0 {
			s.writer.feed(s.pos, dst[:out])
			s.pos += int64(n)
		}
		return out, err

	default:
		needSamples := wantFrames * s.srcCh
		if cap(s.scratch) < needSamples {
			s.scratch = make([]float32, needSamples)
		}
		buf := s.scratch[:needSamples]
		copied := copy(buf, s.rem)
		s.rem = s.rem[:0]
		var err error
		for copied < s.srcCh && err == nil {
			var n int
			n, err = s.dec.Read(buf[copied:])
			if s.closed.Load() {
				return 0, io.ErrClosedPipe
			}
			copied += n
			if n == 0 && err == nil {
				break
			}
		}
		frames := copied / s.srcCh
		if remCount := copied % s.srcCh; remCount > 0 {
			s.rem = append(s.rem[:0], buf[frames*s.srcCh:copied]...)
		}
		for i := range frames {
			base := i * s.srcCh
			dst[i*2] = buf[base]
			dst[i*2+1] = buf[base+1]
		}
		out := frames * 2
		if out > 0 {
			s.writer.feed(s.pos, dst[:out])
			s.pos += int64(frames)
		}
		return out, err
	}
}

func (s *localPCMStream) SetPosition(frame int64) error {
	if s.closed.Load() {
		return io.ErrClosedPipe
	}
	if err := s.dec.SetPosition(frame); err != nil {
		return err
	}
	s.pos = frame
	s.rem = s.rem[:0]
	return nil
}

func (s *localPCMStream) source() (*download, *spectrum) {
	return nil, s.spec
}

func (s *localPCMStream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if c, ok := s.dec.(io.Closer); ok {
			s.closeErr = c.Close()
		}
	})
	return s.closeErr
}

// inertLocalStream stands in for p.stream when an external backend (such as
// native file:/// HQPlayer pass-through on WavPack .wv) opens a local track
// that has no pure-Go preview PCM decoder.
type inertLocalStream struct {
	rate   int
	frames int64
	spec   *spectrum
}

func newInertLocalStream(sampleRate int, durationMs int64, spec *spectrum) *inertLocalStream {
	if sampleRate <= 0 {
		sampleRate = 44100
	}
	var frames int64
	if durationMs > 0 {
		frames = durationMs * int64(sampleRate) / 1000
	}
	return &inertLocalStream{
		rate:   sampleRate,
		frames: frames,
		spec:   spec,
	}
}

func (s *inertLocalStream) SampleRate() int                 { return s.rate }
func (s *inertLocalStream) Channels() int                   { return 2 }
func (s *inertLocalStream) Len() int64                      { return s.frames }
func (s *inertLocalStream) Read(_ []float32) (int, error)   { return 0, io.EOF }
func (s *inertLocalStream) SetPosition(_ int64) error       { return nil }
func (s *inertLocalStream) source() (*download, *spectrum)  { return nil, s.spec }
func (s *inertLocalStream) Close() error                    { return nil }
