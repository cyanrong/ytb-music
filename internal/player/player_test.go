package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/audio"

	"ytb-music/internal/ytm"
)

func TestPlayEmptyQueue(t *testing.T) {
	p := &Player{at: -1}
	p.PlayQueue(nil, 0) // an empty playlist's play button
	if p.Current() != nil {
		t.Error("playing from an empty queue")
	}
}

// silence is a long song of nothing.
type silence struct{}

func (silence) SampleRate() int               { return 48000 }
func (silence) Channels() int                 { return 2 }
func (silence) Len() int64                    { return 48000 * 600 }
func (silence) SetPosition(int64) error       { return nil }
func (silence) Read(b []float32) (int, error) { clear(b); return len(b), nil }

// posted collects what the player posts to the main thread, for the test
// to run there.
func posted() (chan func(), *Player) {
	ch := make(chan func(), 16)
	p := New(func(fn func()) { ch <- fn })
	p.Cue([]ytm.Track{{ID: "1", Name: "一", Duration: 600000}})
	out := audio.NewPlayer(silence{})
	out.SetVolume(0)
	out.Play()
	p.Attach(out)
	p.fade.attach(out, 1)
	return ch, p
}

func next(t *testing.T, ch chan func()) {
	t.Helper()
	select {
	case fn := <-ch:
		fn()
	case <-time.After(2 * time.Second):
		t.Fatal("nothing posted")
	}
}

// Pausing fades out before the music stops; playing again fades it in.
func TestPauseFades(t *testing.T) {
	ch, p := posted()
	defer p.out.Close()
	p.TogglePlay()
	if p.Playing() || !p.out.Playing() {
		t.Fatal("paused at once, or not at all")
	}
	next(t, ch)
	if p.out.Playing() {
		t.Fatal("still playing after fading out")
	}
	if !p.fade.silent() {
		t.Error("faded out to something")
	}
	p.TogglePlay()
	if !p.Playing() || !p.out.Playing() {
		t.Fatal("did not play again")
	}
	time.Sleep(fadeIn + 50*time.Millisecond)
	p.fade.mu.Lock()
	level := p.fade.level(time.Now())
	p.fade.mu.Unlock()
	if level != 1 {
		t.Errorf("faded in to %v", level)
	}
}

// A seek playing pauses, which fades out over a few milliseconds, and
// plays again once there, its position the one asked for meanwhile.
func TestSeekWithoutClick(t *testing.T) {
	ch, p := posted()
	defer p.out.Close()
	p.Seek(90 * time.Second)
	if p.out.Playing() {
		t.Error("jumped while playing")
	}
	if p.Position() != 90*time.Second {
		t.Errorf("position %v while seeking", p.Position())
	}
	next(t, ch)
	if !p.out.Playing() || p.out.Position() < 90*time.Second {
		t.Errorf("after the seek: playing %v at %v", p.out.Playing(), p.out.Position())
	}
}

// Seeks one after the other, as the progress bar asks when let go, play
// on from the last.
func TestSeekTwicePlaysOn(t *testing.T) {
	ch, p := posted()
	defer p.out.Close()
	p.Seek(90 * time.Second)
	p.Seek(95 * time.Second)
	next(t, ch)
	for len(ch) > 0 {
		next(t, ch)
	}
	time.Sleep(seekGap * 2)
	for len(ch) > 0 {
		next(t, ch)
	}
	if !p.out.Playing() || p.out.Position() < 95*time.Second {
		t.Errorf("after two seeks: playing %v at %v", p.out.Playing(), p.out.Position())
	}
}

// A radio asks for more as its queue nears its end, and adds what is not
// in it already.
func TestRadioRefills(t *testing.T) {
	p := &Player{at: -1}
	p.Cue([]ytm.Track{{ID: "1"}, {ID: "2"}})
	asked := 0
	p.AttachRadio("fm:DEFAULT", func(got func([]ytm.Track)) {
		asked++
		got([]ytm.Track{{ID: "2"}, {ID: "3"}})
	})
	if asked != 1 || len(p.Queue()) != 3 || p.Queue()[2].ID != "3" {
		t.Fatalf("asked %d, queue %v", asked, p.Queue())
	}
	if p.Radio() != "fm:DEFAULT" {
		t.Errorf("radio %q", p.Radio())
	}
	// Playing something else ends the radio.
	p.endRadio()
	if p.Radio() != "" || p.feed != nil {
		t.Error("radio still on")
	}
}

func TestNormGain(t *testing.T) {
	for _, c := range []struct {
		st   *ytm.Stream
		want float64
	}{
		{nil, 1},
		{&ytm.Stream{}, 1},
		{&ytm.Stream{Gain: -6.6, Peak: 1}, math.Pow(10, -2.6/20)},
		// Quiet, it is lifted, but not past its peak.
		{&ytm.Stream{Gain: 3, Peak: 0.8}, 1.25},
		{&ytm.Stream{Gain: -4, Peak: 0}, 1},
	} {
		if g := normGain(c.st); math.Abs(g-c.want) > 1e-9 {
			t.Errorf("%+v: %v, want %v", c.st, g, c.want)
		}
	}
}

// The cache gives back what it kept, and drops what was played longest ago once over its limit.
func TestCache(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir, 400)
	store := func(id string, st *ytm.Stream, n int) {
		f := c.part(st)
		f.Write(make([]byte, n))
		f.Close()
		c.keep(id, st, f.Name(), int64(n))
	}
	store("1", &ytm.Stream{Level: "standard", Type: "m4a", Gain: -3}, 100)
	dl, st, ok := c.lookup("1", "standard")
	if !ok || dl.size != 100 || st.Type != "m4a" || st.Gain != -3 {
		t.Fatalf("lookup: %v %+v", ok, st)
	}
	dl.Close()
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "1-standard.m4a"), old, old)
	store("2", &ytm.Stream{Level: "standard", Type: "m4a"}, 100)
	store("3", &ytm.Stream{Level: "standard", Type: "m4a"}, 100)
	store("4", &ytm.Stream{Level: "standard", Type: "m4a"}, 100)
	store("5", &ytm.Stream{Level: "standard", Type: "m4a"}, 100)
	if _, _, ok := c.lookup("1", "standard"); ok {
		t.Error("kept the song played longest ago")
	}
	if dl, _, ok := c.lookup("5", "standard"); !ok {
		t.Error("dropped the song just kept")
	} else {
		dl.Close()
	}
	if n := c.Size(); n > 400 {
		t.Errorf("takes %d of 400", n)
	}
	c.Clear()
	if n := c.Size(); n != 0 {
		t.Errorf("cleared to %d", n)
	}
}

// Leaving a radio keeps the song playing and those before it, drops those
// it queued after, and asks it for no more.
func TestEndRadio(t *testing.T) {
	p := &Player{at: -1}
	p.Cue([]ytm.Track{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}})
	p.at, p.track = 1, &p.queue[1]
	asked := 0
	p.AttachRadio("heart", func(got func([]ytm.Track)) { asked++ })
	p.EndRadio()
	if p.Radio() != "" || p.Current().ID != "2" {
		t.Fatalf("radio %q, current %v", p.Radio(), p.Current())
	}
	if q := p.Queue(); len(q) != 2 || q[0].ID != "1" || q[1].ID != "2" {
		t.Fatalf("queue %v", q)
	}
	p.refill()
	if asked != 0 {
		t.Error("asked the radio after leaving it")
	}
}

// The rest of a playlist goes after the songs played from its first page,
// and among those still to come when shuffled, never before the one
// playing; a queue that has changed since takes none of it.
func TestExtend(t *testing.T) {
	tracks := func(ids ...string) []ytm.Track {
		out := make([]ytm.Track, len(ids))
		for i, id := range ids {
			out[i] = ytm.Track{ID: id}
		}
		return out
	}
	first, more := tracks("a", "b", "c"), tracks("d", "e")

	p := &Player{queue: first, order: []int{0, 1, 2}, at: 1}
	if !p.Extend(first, more) || len(p.queue) != 5 || p.order[3] != 3 || p.order[4] != 4 {
		t.Errorf("in order: queue %v, order %v", p.queue, p.order)
	}

	for range 20 {
		p := &Player{queue: first, order: []int{2, 0, 1}, at: 1, shuffle: true}
		p.Extend(first, more)
		if len(p.order) != 5 || p.order[0] != 2 || p.order[1] != 0 {
			t.Fatalf("shuffled: the songs played or playing moved: order %v", p.order)
		}
	}

	p = &Player{queue: tracks("x", "y", "z"), order: []int{0, 1, 2}, at: 0}
	if p.Extend(first, more) || len(p.queue) != 3 {
		t.Errorf("another queue took the rest: %v", p.queue)
	}
}

// Wrapped ErrUnavailable errors show the localized message and cap auto-skips
// at 3 tracks rather than draining the whole queue, while ErrBotCheck stops
// immediately on the first track.
func TestSkipGuardAndErrorText(t *testing.T) {
	was := songURL
	defer func() { songURL = was }()
	wasLyrics := fetchLyrics
	fetchLyrics = func(*ytm.Track) (string, string, string, error) { return "", "", "", nil }
	defer func() { fetchLyrics = wasLyrics }()

	queue := make([]ytm.Track, 8)
	for i := range queue {
		queue[i] = ytm.Track{ID: fmt.Sprintf("t%d", i)}
	}

	var tried []string
	songURL = func(id, level string) (*ytm.Stream, error) {
		tried = append(tried, id)
		return nil, fmt.Errorf("%w: uploader blocked country", ytm.ErrUnavailable)
	}
	ch := make(chan func(), 64)
	p := New(func(fn func()) { ch <- fn })
	p.PlayQueue(queue, 0)
	for start := time.Now(); p.Playing() && time.Since(start) < 2*time.Second; {
		select {
		case fn := <-ch:
			fn()
		case <-time.After(20 * time.Millisecond):
		}
	}
	for len(ch) > 0 {
		(<-ch)()
	}
	if len(tried) != 3 {
		t.Fatalf("unavailable queue tried %d tracks (%v), want 3", len(tried), tried)
	}
	if p.err != "这首歌暂时无法播放" {
		t.Fatalf("err = %q, want localized unavailable message", p.err)
	}

	tried = nil
	songURL = func(id, level string) (*ytm.Stream, error) {
		tried = append(tried, id)
		return nil, fmt.Errorf("%w: %w: Sign in to confirm you’re not a bot", ytm.ErrUnavailable, ytm.ErrBotCheck)
	}
	p.PlayQueue(queue, 0)
	for start := time.Now(); p.Playing() && time.Since(start) < 2*time.Second; {
		select {
		case fn := <-ch:
			fn()
		case <-time.After(20 * time.Millisecond):
		}
	}
	for len(ch) > 0 {
		(<-ch)()
	}
	if len(tried) != 1 {
		t.Fatalf("bot check tried %d tracks (%v), want 1", len(tried), tried)
	}
	if strings.Contains(p.err, "available") || p.err == "" {
		t.Fatalf("bot check err = %q, want localized bot-check prompt", p.err)
	}
}

type fakeLocalDecoder struct {
	rate   int
	ch     int
	frames int64
	pos    int64
	closed *atomic.Int32
}

func (d *fakeLocalDecoder) SampleRate() int { return d.rate }
func (d *fakeLocalDecoder) Channels() int   { return d.ch }
func (d *fakeLocalDecoder) Len() int64      { return d.frames }
func (d *fakeLocalDecoder) SetPosition(frame int64) error {
	d.pos = frame
	return nil
}
func (d *fakeLocalDecoder) Read(dst []float32) (int, error) {
	if d.pos >= d.frames {
		return 0, io.EOF
	}
	wantFrames := min(int64(len(dst)/d.ch), d.frames-d.pos)
	for i := range int(wantFrames) {
		frameIdx := d.pos + int64(i)
		sig := float32(0.5 * math.Sin(2*math.Pi*440*float64(frameIdx)/float64(d.rate)))
		for c := range d.ch {
			dst[i*d.ch+c] = sig * float32(c+1)
		}
	}
	d.pos += wantFrames
	return int(wantFrames) * d.ch, nil
}
func (d *fakeLocalDecoder) Close() error {
	if d.closed != nil {
		d.closed.Add(1)
	}
	return nil
}

type stubLocalProvider struct {
	closed atomic.Int32
}

func (s *stubLocalProvider) IsLocalTrack(id string) bool {
	return strings.HasPrefix(id, "localfile:")
}

func (s *stubLocalProvider) OpenLocalTrack(id string) (*LocalOpenedTrack, error) {
	return &LocalOpenedTrack{
		Decoder:   &fakeLocalDecoder{rate: 96000, ch: 1, frames: 96000 * 2, closed: &s.closed},
		Stream:    &ytm.Stream{Codec: "FLAC", BitDepth: 24, Bitrate: 2845},
		LocalPath: "/music/test.flac",
		NativeHQP: true,
		Cover:     []byte{0xFF, 0xD8, 0xFF, 0xD9},
	}, nil
}

func (s *stubLocalProvider) LocalLyrics(id string) (lrc, trans string, plain []string) {
	return "[00:01.00]First line\n[00:03.00]Second line", "[00:01.00]第一行", nil
}

type stubSink struct {
	playing bool
	pos     time.Duration
	dur     time.Duration
}

func (s *stubSink) Activate()                {}
func (s *stubSink) Play()                    { s.playing = true }
func (s *stubSink) Pause()                   { s.playing = false }
func (s *stubSink) Playing() bool            { return s.playing }
func (s *stubSink) Seek(t time.Duration)     { s.pos = t }
func (s *stubSink) Position() time.Duration  { return s.pos }
func (s *stubSink) Duration() time.Duration  { return s.dur }
func (s *stubSink) SetSinkVolume(v float64)  {}
func (s *stubSink) OnSinkEnd(fn func())      {}
func (s *stubSink) DirectTransport() bool    { return true }
func (s *stubSink) Close()                   { s.playing = false }

type stubBackend struct {
	gotSrc TrackSource
}

func (b *stubBackend) Name() string { return "stub" }
func (b *stubBackend) Open(_ context.Context, src TrackSource) (OutputSink, error) {
	b.gotSrc = src
	dec, err := src.OpenPCM()
	if err != nil {
		return nil, err
	}
	buf := make([]float32, fftSize*2*2)
	_, _ = dec.Read(buf)
	_ = dec.Close()
	return &stubSink{dur: src.Duration}, nil
}

// Playing a local track through LocalProvider loads timed lyrics, reports 100% downloaded, normalizes mono/multichannel PCM to stereo with FFT spectrum analysis, and opens independent stereo audio via OpenAudio.
func TestLocalProviderPlaybackLyricsAndSpectrum(t *testing.T) {
	// Verify multichannel (>2ch) down-selection to front L/R as well.
	var multiClosed atomic.Int32
	multi := newLocalPCMStream(&fakeLocalDecoder{rate: 48000, ch: 6, frames: 64, closed: &multiClosed}, nil)
	if multi.Channels() != 2 {
		t.Fatalf("Channels() = %d, want 2", multi.Channels())
	}
	outBuf := make([]float32, 16)
	n, err := multi.Read(outBuf)
	if err != nil || n != 16 || math.Abs(float64(outBuf[3]-2*outBuf[2])) > 1e-6 {
		t.Fatalf("6ch -> 2ch Read got n=%d err=%v samples=%v", n, err, outBuf[:4])
	}
	_ = multi.Close()
	if multiClosed.Load() != 1 {
		t.Fatalf("multi Close count = %d, want 1", multiClosed.Load())
	}

	ch := make(chan func(), 32)
	p := New(func(fn func()) { ch <- fn })
	prov := &stubLocalProvider{}
	be := &stubBackend{}
	p.Local = prov
	p.SetBackend(be)

	p.PlayQueue([]ytm.Track{{
		ID:       "localfile:1",
		Name:     " Waltz ",
		Duration: 2000,
	}}, 0)

	deadline := time.Now().Add(2 * time.Second)
	for (!p.Ready() || !valueTrue(func() bool { _, ok := p.Lyrics(); return ok })) && time.Now().Before(deadline) {
		select {
		case fn := <-ch:
			fn()
		case <-time.After(10 * time.Millisecond):
		}
	}
	for len(ch) > 0 {
		(<-ch)()
	}

	if !p.Ready() || !p.Playing() {
		t.Fatalf("local track not ready/playing: ready=%v playing=%v err=%q", p.Ready(), p.Playing(), p.Err())
	}
	if prog, ok := p.Downloaded(); !ok || prog != 1 {
		t.Fatalf("Downloaded() = (%v, %v), want (1, true)", prog, ok)
	}
	if st := p.Stream(); st == nil || st.SampleRate != 96000 || st.SpecBadge() != "FLAC 24/96" {
		t.Fatalf("Stream() = %+v", st)
	}
	if be.gotSrc.LocalPath != "/music/test.flac" || !be.gotSrc.NativeHQP {
		t.Fatalf("TrackSource = %+v", be.gotSrc)
	}
	lines, loaded := p.Lyrics()
	if !loaded || len(lines) != 2 || lines[0].Translation != "第一行" {
		t.Fatalf("Lyrics() = (%+v, %v)", lines, loaded)
	}
	bands, ok := p.Levels()
	var energy float32
	for _, b := range bands {
		energy += b
	}
	if !ok || energy <= 0 {
		t.Fatalf("Levels() = (%v, %v), want active spectrum energy", bands, ok)
	}

	aud, err := p.OpenAudio()
	if err != nil {
		t.Fatalf("OpenAudio: %v", err)
	}
	if aud.Channels() != 2 || aud.SampleRate() != 96000 {
		t.Fatalf("OpenAudio format = %dHz/%dch, want 96000Hz/2ch", aud.SampleRate(), aud.Channels())
	}
	monoCheck := make([]float32, 8)
	an, aerr := aud.Read(monoCheck)
	if aerr != nil || an != 8 || monoCheck[2] != monoCheck[3] {
		t.Fatalf("mono -> stereo OpenAudio Read got n=%d err=%v samples=%v", an, aerr, monoCheck)
	}
	_ = aud.Close()
	p.Stop()
	if prov.closed.Load() < 3 {
		t.Fatalf("decoder Close count = %d, want >= 3 (backend + OpenAudio + player stream)", prov.closed.Load())
	}
}

func valueTrue(fn func() bool) bool { return fn() }

type passthroughStubBackend struct {
	gotSrc TrackSource
}

func (b *passthroughStubBackend) Name() string { return "hqp-passthrough" }
func (b *passthroughStubBackend) Open(_ context.Context, src TrackSource) (OutputSink, error) {
	b.gotSrc = src
	return &stubSink{dur: src.Duration}, nil
}

type noDecoderLocalProvider struct {
	cover []byte
}

func (p *noDecoderLocalProvider) IsLocalTrack(id string) bool {
	return strings.HasPrefix(id, "localfile:")
}

func (p *noDecoderLocalProvider) OpenLocalTrack(id string) (*LocalOpenedTrack, error) {
	return &LocalOpenedTrack{
		OpenPCM: func() (PCMDecoder, error) {
			return nil, errors.New("unsupported codec: WV")
		},
		Stream:    &ytm.Stream{Codec: "WV", SampleRate: 96000, BitDepth: 24, Bitrate: 2500},
		LocalPath: "/music/album.wv",
		NativeHQP: true,
		Cover:     p.cover,
	}, nil
}

func (p *noDecoderLocalProvider) LocalLyrics(string) (string, string, []string) {
	return "", "", nil
}

// A local track without a pure-Go PCM decoder plays through a native external backend via inert stream metadata, fails cleanly on local output, and online loads report Downloading state accurately.
func TestLocalNativeBackendWithoutPCMDecoderAndDownloadingFlag(t *testing.T) {
	ch := make(chan func(), 32)
	p := New(func(fn func()) { ch <- fn })
	coverBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	p.Local = &noDecoderLocalProvider{cover: coverBytes}
	be := &passthroughStubBackend{}
	p.SetBackend(be)

	p.PlayQueue([]ytm.Track{{
		ID:       "localfile:wv1",
		Name:     "WavPack Native",
		Duration: 180000,
	}}, 0)

	deadline := time.Now().Add(2 * time.Second)
	for p.Loading() && time.Now().Before(deadline) {
		select {
		case fn := <-ch:
			fn()
		case <-time.After(10 * time.Millisecond):
		}
	}
	for len(ch) > 0 {
		(<-ch)()
	}

	if !p.Ready() || !p.Playing() || p.Err() != "" {
		t.Fatalf("native backend without preview decoder: ready=%v playing=%v err=%q", p.Ready(), p.Playing(), p.Err())
	}
	if !be.gotSrc.NativeHQP || be.gotSrc.LocalPath != "/music/album.wv" || be.gotSrc.Downloading || string(be.gotSrc.CoverBytes) != string(coverBytes) {
		t.Fatalf("TrackSource = %+v", be.gotSrc)
	}
	if st := p.Stream(); st == nil || st.SampleRate != 96000 {
		t.Fatalf("Stream() = %+v, want 96000Hz", st)
	}
	if prog, ok := p.Downloaded(); !ok || prog != 1 {
		t.Fatalf("Downloaded() = (%v, %v), want (1, true)", prog, ok)
	}

	// Switching to built-in OS output (backend == nil) requires OpenPCM and surfaces the decoder error.
	p.SetBackend(nil)
	deadline = time.Now().Add(2 * time.Second)
	for p.Loading() && time.Now().Before(deadline) {
		select {
		case fn := <-ch:
			fn()
		case <-time.After(10 * time.Millisecond):
		}
	}
	for len(ch) > 0 {
		(<-ch)()
	}
	if p.Ready() || p.Playing() || p.Err() != "unsupported codec: WV" {
		t.Fatalf("local output without decoder: ready=%v playing=%v err=%q", p.Ready(), p.Playing(), p.Err())
	}

	// Verify online TrackSource.Downloading is true while an uncached song is still streaming in, and false once cached.
	m4aBytes, err := os.ReadFile("testdata/seek.m4a")
	if err != nil {
		t.Fatal(err)
	}
	holdDownload := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() {
		releaseOnce.Do(func() { close(holdDownload) })
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow.m4a" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(m4aBytes)+4096))
			_, _ = w.Write(m4aBytes)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			select {
			case <-holdDownload:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write(m4aBytes)
	}))
	defer srv.Close()
	defer releaseHold()

	was := songURL
	defer func() { songURL = was }()
	wasLyrics := fetchLyrics
	var lyricsCalls atomic.Int32
	fetchLyrics = func(*ytm.Track) (string, string, string, error) {
		lyricsCalls.Add(1)
		return "[00:01.00]Stubbed", "", "", nil
	}
	defer func() { fetchLyrics = wasLyrics }()
	songURL = func(id, level string) (*ytm.Stream, error) {
		if id == "online_slow" {
			return &ytm.Stream{URL: srv.URL + "/slow.m4a", Size: int64(len(m4aBytes) + 4096), Type: "m4a", Level: "standard"}, nil
		}
		return &ytm.Stream{URL: srv.URL + "/fast.m4a", Size: int64(len(m4aBytes)), Type: "m4a", Level: "standard"}, nil
	}

	p.Cache = NewCache(t.TempDir(), 1<<30)
	p.SetBackend(be)
	p.PlayQueue([]ytm.Track{{ID: "online_slow", Name: "Streaming", Duration: 5000}}, 0)
	deadline = time.Now().Add(2 * time.Second)
	for (p.Loading() || !p.lyricsLoaded) && time.Now().Before(deadline) {
		select {
		case fn := <-ch:
			fn()
		case <-time.After(10 * time.Millisecond):
		}
	}
	for len(ch) > 0 {
		(<-ch)()
	}
	if !be.gotSrc.Downloading {
		t.Fatal("active uncached online download reported TrackSource.Downloading = false, want true")
	}
	var slowDL *download
	if p.stream != nil {
		slowDL, _ = p.stream.source()
	}

	// Verify closing a shared OpenPCM stream wakes an in-flight blocked Read immediately without closing slowDL.
	sharedDec, err := be.gotSrc.OpenPCM()
	if err != nil {
		t.Fatalf("OpenPCM on slow stream: %v", err)
	}
	waitEntered := make(chan struct{})
	var waitOnce sync.Once
	prevWaitHook := onDownloadWaitHook
	onDownloadWaitHook = func() { waitOnce.Do(func() { close(waitEntered) }) }
	defer func() { onDownloadWaitHook = prevWaitHook }()

	readDone := make(chan error, 1)
	go func() {
		buf := make([]float32, 8192)
		for {
			_, rerr := sharedDec.Read(buf)
			if rerr != nil {
				readDone <- rerr
				return
			}
		}
	}()
	select {
	case <-waitEntered:
	case <-time.After(time.Second):
		_ = sharedDec.Close()
		t.Fatal("shared PCMDecoder.Read never entered download wait")
	}
	_ = sharedDec.Close()
	select {
	case rerr := <-readDone:
		if rerr == nil {
			t.Fatal("closed shared PCMDecoder returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("shared PCMDecoder.Close did not unblock in-flight Read")
	}

	// Pre-populate cache for online_cached and verify Downloading is false.
	dl := p.Cache.download("online_cached", &ytm.Stream{URL: srv.URL + "/fast.m4a", Size: int64(len(m4aBytes)), Type: "m4a", Level: "standard"})
	dl.waitFor(int64(len(m4aBytes)))
	for start := time.Now(); !dl.completed() && time.Since(start) < time.Second; time.Sleep(5 * time.Millisecond) {
	}
	dl.Close()
	waitDownloadReleased(dl)

	p.PlayQueue([]ytm.Track{{ID: "online_cached", Name: "Cached", Duration: 5000}}, 0)
	deadline = time.Now().Add(2 * time.Second)
	for (p.Loading() || !p.lyricsLoaded) && time.Now().Before(deadline) {
		select {
		case fn := <-ch:
			fn()
		case <-time.After(10 * time.Millisecond):
		}
	}
	for len(ch) > 0 {
		(<-ch)()
	}
	if be.gotSrc.Downloading {
		t.Fatal("cached online track reported TrackSource.Downloading = true, want false")
	}
	if lyricsCalls.Load() < 2 {
		t.Fatalf("fetchLyrics stub called %d times, want >= 2", lyricsCalls.Load())
	}
	var curDL *download
	if p.stream != nil {
		curDL, _ = p.stream.source()
	}
	p.Stop()
	releaseHold()
	waitDownloadReleased(slowDL)
	waitDownloadReleased(curDL)
}
