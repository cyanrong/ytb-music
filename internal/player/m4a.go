package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo/audio"
	_ "github.com/egoist/mygo/audio/aac" // AAC in MP4: YouTube's itag 140
	"github.com/egoist/mygo/audio/mp4"

	"ytb-music/internal/ytm"
)

// m4aStream decodes a song of AAC in MP4, as YouTube serves it, from a
// download as it comes in, and measures the spectrum of what it decodes,
// for the visualizers.
type m4aStream struct {
	dl      *download
	file    *downloadFile
	dec     audio.Decoder
	pos     int64 // the frame decoded next
	spec    *spectrum
	writer  *specWriter
	drop    func() // lets the download go, as the stream closes
	closeMu sync.Once
}

func openM4AStream(dl *download) (*m4aStream, error) {
	df := &downloadFile{d: dl}
	dec, err := mp4.NewDecoder(df)
	if err != nil {
		return nil, err
	}
	spec := newSpectrum(dec.SampleRate(), max(dec.Len(), 0))
	s := &m4aStream{dl: dl, file: df, dec: dec, spec: spec, writer: newSpecWriter(spec), drop: dl.Close}
	return s, nil
}

func (s *m4aStream) SampleRate() int { return s.dec.SampleRate() }
func (s *m4aStream) Channels() int   { return 2 }
func (s *m4aStream) Len() int64      { return s.dec.Len() }

func (s *m4aStream) Read(dst []float32) (int, error) {
	n, err := s.dec.Read(dst)
	if n > 0 {
		s.writer.feed(s.pos, dst[:n])
		s.pos += int64(n / 2)
	}
	return n, err
}

func (s *m4aStream) SetPosition(frame int64) error {
	if err := s.dec.SetPosition(frame); err != nil {
		return err
	}
	s.pos = frame
	return nil
}

func (s *m4aStream) source() (*download, *spectrum) { return s.dl, s.spec }

func (s *m4aStream) Close() error {
	s.closeMu.Do(func() {
		s.file.closeReader()
		if s.drop != nil {
			s.drop()
		}
	})
	return nil
}

// A downloadFile reads a download as a file, at any offset, waiting for
// the bytes asked of it: the MP4 decoder reads the header, then each
// fragment as it plays.
type downloadFile struct {
	d      *download
	pos    int64
	closed bool // guarded by d.mu
}

func (f *downloadFile) closeReader() {
	if f == nil || f.d == nil {
		return
	}
	d := f.d
	d.mu.Lock()
	f.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
}

func (f *downloadFile) size() (int64, error) {
	if f.d.size > 0 {
		return f.d.size, nil
	}
	d := f.d
	d.mu.Lock()
	for !d.done && !f.closed {
		d.cond.Wait()
	}
	n, err, closed := d.n, d.err, f.closed
	d.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	return n, err
}

var onDownloadWaitHook func()

func (f *downloadFile) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("player: negative offset")
	}
	d := f.d
	want := off + int64(len(b))
	if d.size > 0 {
		want = min(want, d.size)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.n < want && !d.done && !f.closed {
		if h := onDownloadWaitHook; h != nil {
			h()
		}
		d.cond.Wait()
	}
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := d.readAt(b, off)
	if n < len(b) {
		if err == nil {
			err = d.err
		}
		if err == nil {
			err = io.EOF
		}
	}
	return n, err
}

func (f *downloadFile) Read(b []byte) (int, error) {
	n, err := f.ReadAt(b, f.pos)
	f.pos += int64(n)
	if n > 0 && err == io.EOF {
		err = nil
	}
	return n, err
}

func (f *downloadFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.pos
	case io.SeekEnd:
		size, err := f.size()
		if err != nil {
			return 0, err
		}
		offset += size
	}
	if offset < 0 {
		return 0, errors.New("player: negative position")
	}
	f.pos = offset
	return offset, nil
}

// YouTube's servers send a song a range at a time: asked for all of it at
// once, they send it slowly, or refuse. The first range is small, so that
// playback starts with it.
const (
	firstRange = 256 << 10
	rangeSize  = 2 << 20
)

// ranged reports whether url is YouTube's, which serves ranges.
func ranged(url string) bool { return strings.Contains(url, ".googlevideo.com/") }

type httpStatusError struct {
	code   int
	status string
}

func (e httpStatusError) Error() string { return e.status }

func needsStreamRefresh(err error, routeSig *string) bool {
	var se httpStatusError
	if errors.As(err, &se) && (se.code == http.StatusForbidden || se.code == http.StatusUnauthorized) {
		*routeSig = ytm.RouteSignature()
		return true
	}
	if cur := ytm.RouteSignature(); cur != *routeSig {
		*routeSig = cur
		return true
	}
	return false
}

// fetchRanges fetches the whole song, of d.size bytes, a range at a time,
// trying a range again where it broke off, twice, and refreshing the signed
// stream URL only when an authorization error or route change invalidates it.
func (d *download) fetchRanges(ctx context.Context, url string) error {
	routeSig := ytm.RouteSignature()
	for off := int64(0); off < d.size; {
		size := int64(rangeSize)
		if off == 0 {
			size = firstRange
		}
		end := min(off+size, d.size) - 1
		var err error
		refreshed := false
		for range 3 {
			var got int64
			got, err = d.fetchRange(ctx, url, off, end)
			off += got
			if err == nil || ctx.Err() != nil {
				break
			}
			var se httpStatusError
			if errors.As(err, &se) && se.code == http.StatusForbidden && d.inStartupProbe() && ytm.RouteSignature() == routeSig {
				break
			}
			if !refreshed && d.refresh != nil && needsStreamRefresh(err, &routeSig) {
				refreshed = true
				if fresh, rerr := d.refresh(); rerr == nil && fresh != "" {
					url = fresh
				}
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// fetchRange fetches the bytes from to to of the song, and returns how many
// came.
func (d *download) fetchRange(ctx context.Context, url string, from, to int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s&range=%d-%d", url, from, to), nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	req.Header.Set("User-Agent", d.userAgent())
	res, err := ytm.StreamClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return 0, httpStatusError{code: res.StatusCode, status: res.Status}
	}
	var got int64
	want := to - from + 1
	buf := make([]byte, 64<<10)
	for got < want {
		n, err := res.Body.Read(buf[:min(int64(len(buf)), want-got)])
		if n > 0 {
			d.write(buf[:n])
			got += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return got, err
		}
	}
	if got < want {
		return got, io.ErrUnexpectedEOF
	}
	noteSpeed(got, time.Since(start))
	return got, nil
}

// speed is how fast songs came in lately, in bits a second, from ranges
// of them big enough to tell: what the network gives a stream from
// YouTube's servers, the wait for each range counted, as a video's
// segments wait too.
var speed struct {
	mu  sync.Mutex
	bps float64
}

// speedRange is the least of a range that tells the speed: smaller ones
// are mostly the wait for them.
const speedRange = 512 << 10

// noteSpeed takes a range of n bytes that came in d.
func noteSpeed(n int64, d time.Duration) {
	if n < speedRange || d <= 0 {
		return
	}
	v := float64(n) * 8 / d.Seconds()
	speed.mu.Lock()
	defer speed.mu.Unlock()
	if speed.bps == 0 {
		speed.bps = v
	} else {
		speed.bps = (speed.bps + v) / 2 // the latest weighs as much as all before
	}
}

// Throughput returns how fast songs came in lately, in bits a second, or 0
// before any came in.
func Throughput() float64 {
	speed.mu.Lock()
	defer speed.mu.Unlock()
	return speed.bps
}
