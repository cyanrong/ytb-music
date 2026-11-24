package player

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/audio"

	"ytb-music/internal/lyrics"
	"ytb-music/internal/ytm"
)

type RepeatMode int

const (
	RepeatOff RepeatMode = iota
	RepeatAll
	RepeatOne
)

// Player plays a queue of tracks. Its methods run on the main thread;
// loading happens on goroutines, whose results post brings back there.
type Player struct {
	post func(func())
	// Quality returns the stream level to load, as the settings say.
	Quality func() string
	// Normalize reports whether to even out the loudness of tracks, as
	// the settings say.
	Normalize func() bool
	// Cache keeps the songs played, if set.
	Cache *Cache
	// Local opens tracks and lyrics from the listener's local music library, if set.
	Local LocalProvider
	// Changed, if set, hears that what plays, where it is or whether it
	// plays changed.
	Changed func()

	queue   []ytm.Track
	order   []int // the queue's indices in play order: shuffled or not
	at      int   // where in order the current track is, -1 for none
	shuffle bool
	repeat  RepeatMode
	volume  float64

	// A radio goes on with the tracks its feed gives, as the queue nears
	// its end, for as long as nothing else is played.
	radio    string
	feed     func(got func([]ytm.Track))
	radioGen int  // counts radios, to drop what an old one gives
	feeding  bool // the feed is asked
	awaiting bool // at the queue's end, for what the feed gives

	track      *ytm.Track
	backend    OutputBackend
	out        OutputSink
	stream     stream
	info       *ytm.Stream // what the current track streams
	fade       fader
	gain       float64 // the current track's normalizing gain
	loading    bool
	playing    bool // what the listener wants: false while paused
	err        string
	gen        int // counts loads, to drop the results of stale ones
	loadCancel context.CancelFunc

	lyrics       []lyrics.Line
	plainLyrics  []string // lyrics without times, when no timed ones are found
	lyricsLoaded bool

	// pending holds a seek asked for while the track loads.
	pending time.Duration
	// A seek playing fades out, then lands, then fades in: meanwhile the
	// position is where it goes.
	seeking bool
	seekTo  time.Duration
	seekGen int
}

func New(post func(func())) *Player {
	p := &Player{post: post, at: -1, volume: 0.8, gain: 1}
	// Music, mixed far enough ahead to play on while macOS keeps the app
	// waiting, as it does while its appearance changes.
	audio.SetLatency(audio.LatencyPlayback)
	// The media keys, and the controls of Control Center.
	mygo.NowPlaying.OnCommand(func(cmd mygo.MediaCommand, pos time.Duration) {
		switch cmd {
		case mygo.MediaPlay, mygo.MediaPause:
			if (cmd == mygo.MediaPlay) != p.playing {
				p.TogglePlay()
			}
		case mygo.MediaNext:
			p.Next()
		case mygo.MediaPrevious:
			p.Prev()
		case mygo.MediaSeek:
			p.Seek(pos)
		}
		p.post(func() {})
	})
	return p
}

// sync shows the system, and Changed, where playback is.
func (p *Player) sync() {
	if p.Changed != nil {
		defer p.Changed()
	}
	if p.track == nil {
		mygo.NowPlaying.Clear()
		return
	}
	state := mygo.PlaybackPaused
	if p.playing {
		state = mygo.PlaybackPlaying
	}
	mygo.NowPlaying.SetPlayback(state, p.Position())
}

// announce shows the system the track, and its cover once loaded.
func (p *Player) announce(t *ytm.Track, artwork []byte) {
	state := mygo.PlaybackPaused
	if p.playing {
		state = mygo.PlaybackPlaying
	}
	mygo.NowPlaying.Set(mygo.NowPlayingInfo{
		Title:    t.Name,
		Artist:   t.ArtistNames(),
		Album:    t.Album.Name,
		Artwork:  artwork,
		Duration: t.Length(),
		Position: p.Position(),
		State:    state,
		Commands: mygo.MediaPlay | mygo.MediaPause | mygo.MediaNext | mygo.MediaPrevious | mygo.MediaSeek,
	})
}

func (p *Player) ended() {
	if p.repeat == RepeatOne {
		p.out.Seek(0)
		p.out.Play()
		return
	}
	if p.at+1 >= len(p.order) && p.feed != nil {
		p.awaitFeed()
		return
	}
	if p.at+1 >= len(p.order) && p.repeat == RepeatOff {
		p.playing = false
		p.sync()
		return
	}
	p.Next()
}

// PlayQueue replaces the queue with tracks and plays the one at start.
func (p *Player) PlayQueue(tracks []ytm.Track, start int) {
	if len(tracks) == 0 {
		return
	}
	p.endRadio()
	start = min(max(start, 0), len(tracks)-1)
	p.queue = append([]ytm.Track(nil), tracks...)
	p.order = make([]int, len(p.queue))
	for i := range p.order {
		p.order[i] = i
	}
	if p.shuffle {
		p.shuffleOrder(start)
		p.load(0)
		return
	}
	p.load(start)
}

// PlayRadio plays tracks in their order, then goes on with those feed
// gives as the queue nears its end, until something else is played. The
// radio's name says which it is. feed calls got on the main thread.
func (p *Player) PlayRadio(name string, tracks []ytm.Track, feed func(got func([]ytm.Track))) {
	if len(tracks) == 0 {
		return
	}
	shuffle := p.shuffle
	p.shuffle = false
	p.PlayQueue(tracks, 0)
	p.shuffle = shuffle
	p.AttachRadio(name, feed)
}

// RadioFrom makes the track playing the first of a radio's queue, which
// goes on with tracks, then with those feed gives: without starting it
// over.
func (p *Player) RadioFrom(name string, tracks []ytm.Track, feed func(got func([]ytm.Track))) {
	if p.track == nil {
		return
	}
	p.queue, p.order, p.at = []ytm.Track{*p.track}, []int{0}, 0
	p.appendTracks(tracks)
	p.AttachRadio(name, feed)
}

// AttachRadio makes the queue the radio name's, which feed goes on with,
// as after restoring a session that played it.
func (p *Player) AttachRadio(name string, feed func(got func([]ytm.Track))) {
	p.endRadio()
	p.radio, p.feed = name, feed
	p.refill()
}

// EndRadio stops the radio playing: the song playing plays on, but the
// songs the radio queued after it go, and it adds no more.
func (p *Player) EndRadio() {
	if p.radio == "" && p.feed == nil {
		return
	}
	awaiting := p.awaiting
	p.endRadio()
	if p.at >= 0 && p.at+1 < len(p.order) {
		kept := make([]ytm.Track, p.at+1)
		for i, k := range p.order[:p.at+1] {
			kept[i] = p.queue[k]
		}
		p.queue, p.order = kept, make([]int, len(kept))
		for i := range p.order {
			p.order[i] = i
		}
	}
	if awaiting {
		// At its end, waiting for more: nothing plays on.
		p.playing = false
		p.sync()
	}
}

func (p *Player) endRadio() {
	p.radioGen++
	p.radio, p.feed, p.feeding, p.awaiting = "", nil, false, false
}

// Radio returns the name of the radio playing, or "".
func (p *Player) Radio() string { return p.radio }

// refill asks the radio for more once the queue is near its end.
func (p *Player) refill() {
	if p.feed == nil || p.feeding || p.at < len(p.order)-2 {
		return
	}
	p.feeding = true
	gen := p.radioGen
	p.feed(func(more []ytm.Track) {
		if gen != p.radioGen {
			return
		}
		p.feeding = false
		p.appendTracks(more)
		switch {
		case !p.awaiting:
		case p.at+1 < len(p.order):
			p.awaiting = false
			p.load(p.at + 1)
		default:
			// The radio has nothing more: it stops at its end.
			p.awaiting, p.playing = false, false
			p.err = "电台暂时没有更多歌曲"
			p.sync()
		}
	})
}

// awaitFeed waits at the end of a radio's queue for what its feed gives.
func (p *Player) awaitFeed() {
	p.awaiting = true
	p.refill()
}

// appendTracks adds tracks to the end of the queue, but those in it.
func (p *Player) appendTracks(tracks []ytm.Track) {
	in := make(map[string]bool, len(p.queue))
	for _, t := range p.queue {
		in[t.ID] = true
	}
	for _, t := range tracks {
		if !in[t.ID] {
			in[t.ID] = true
			p.order = append(p.order, len(p.queue))
			p.queue = append(p.queue, t)
		}
	}
}

// Extend adds more to the end of the queue, if it is from, as the rest of
// a playlist comes after its first page was played: shuffled, they go in
// among the songs still to come. It reports whether it added them.
func (p *Player) Extend(from, more []ytm.Track) bool {
	if p.radio != "" || len(more) == 0 || len(p.queue) != len(from) {
		return false
	}
	for i := range from {
		if p.queue[i].ID != from[i].ID {
			return false
		}
	}
	for _, t := range more {
		p.queue = append(p.queue, t)
		if p.shuffle && p.at >= 0 {
			j := p.at + 1 + rand.IntN(len(p.order)-p.at)
			p.order = slices.Insert(p.order, j, len(p.queue)-1)
		} else {
			p.order = append(p.order, len(p.queue)-1)
		}
	}
	return true
}

// shuffleOrder shuffles the order, with first first.
func (p *Player) shuffleOrder(first int) {
	rand.Shuffle(len(p.order), func(i, j int) { p.order[i], p.order[j] = p.order[j], p.order[i] })
	for i, v := range p.order {
		if v == first {
			p.order[0], p.order[i] = p.order[i], p.order[0]
		}
	}
}

func (p *Player) ToggleShuffle() {
	p.shuffle = !p.shuffle
	if len(p.order) == 0 {
		return
	}
	cur := p.order[max(p.at, 0)]
	if p.shuffle {
		p.shuffleOrder(cur)
		p.at = 0
	} else {
		for i := range p.order {
			p.order[i] = i
		}
		p.at = cur
	}
}

func (p *Player) CycleRepeat() { p.repeat = (p.repeat + 1) % 3 }

// Current returns the track playing or loading, or nil.
func (p *Player) Current() *ytm.Track { return p.track }

// Queue returns the tracks queued, in the order they were given.
func (p *Player) Queue() []ytm.Track { return p.queue }

// Playing reports what the listener wants: false while paused.
func (p *Player) Playing() bool { return p.playing }

// Loading reports whether the current track is still loading.
func (p *Player) Loading() bool { return p.loading || p.awaiting }

// Ready reports whether the current track has loaded, to play.
func (p *Player) Ready() bool { return p.out != nil }

// Err returns why the current track did not play, or "".
func (p *Player) Err() string { return p.err }

// Stream returns what the current track streams, once loaded, or nil.
func (p *Player) Stream() *ytm.Stream { return p.info }

func (p *Player) Shuffle() bool      { return p.shuffle }
func (p *Player) Repeat() RepeatMode { return p.repeat }

// Lyrics returns the current track's lyrics, and whether they have loaded.
func (p *Player) Lyrics() ([]lyrics.Line, bool) { return p.lyrics, p.lyricsLoaded }

// PlainLyrics returns the current track's lyrics without times, when it
// has no timed ones.
func (p *Player) PlainLyrics() []string { return p.plainLyrics }

// Downloaded returns the share of the current track downloaded so far, or
// false before it has started to.
func (p *Player) Downloaded() (float64, bool) {
	if p.stream == nil {
		return 0, false
	}
	dl, _ := p.stream.source()
	if dl == nil {
		return 1, true
	}
	return dl.Progress(), true
}

// Audio is the current track's sound decoded apart from what plays, as
// OpenAudio opens it: interleaved stereo at its rate, read as it
// downloads.
type Audio interface {
	SampleRate() int
	Channels() int
	Read(dst []float32) (int, error)
	// SetPosition goes to a frame, of SampleRate a second.
	SetPosition(frame int64) error
	Close() error
}

// OpenAudio opens the current track's sound again, for decoding apart
// from what plays, as for transcribing an episode. It reads the same
// download, waiting for what has not come in, and fails once the track
// changes. It works on any goroutine once opened; close it when done.
func (p *Player) OpenAudio() (Audio, error) {
	if p.stream == nil || p.track == nil {
		return nil, errors.New("player: nothing loaded")
	}
	if p.Local != nil && p.Local.IsLocalTrack(p.track.ID) {
		opened, err := p.Local.OpenLocalTrack(p.track.ID)
		if err != nil {
			return nil, err
		}
		if opened == nil {
			return nil, errors.New("player: empty local audio")
		}
		if opened.OpenPCM != nil {
			if opened.Decoder != nil {
				if c, ok := opened.Decoder.(io.Closer); ok {
					_ = c.Close()
				}
			}
			dec, derr := opened.OpenPCM()
			if derr != nil {
				return nil, derr
			}
			if dec == nil {
				return nil, errors.New("player: empty local audio")
			}
			return newLocalPCMStream(dec, nil), nil
		}
		if opened.Decoder == nil {
			return nil, errors.New("player: empty local audio")
		}
		return newLocalPCMStream(opened.Decoder, nil), nil
	}
	dl, _ := p.stream.source()
	if dl == nil {
		return nil, errors.New("player: nothing loaded")
	}
	return openShared(dl, p.track.Duration)
}

// PlayOrder returns the queue's indices in play order, and where the
// current track is among them, -1 for none.
func (p *Player) PlayOrder() ([]int, int) { return p.order, p.at }

func (p *Player) Next() {
	if len(p.order) == 0 {
		return
	}
	at := p.at + 1
	if at >= len(p.order) {
		if p.feed != nil {
			p.stop()
			p.awaitFeed()
			return
		}
		at = 0
	}
	p.load(at)
}

// Prev plays the track before this one, however far this one has played:
// once a few seconds in, going back to its start only kept the listener
// pressing twice.
func (p *Player) Prev() {
	if len(p.order) == 0 {
		return
	}
	at := p.at - 1
	if at < 0 {
		at = len(p.order) - 1
	}
	p.load(at)
}

// Jump plays the queue's track i.
func (p *Player) Jump(i int) {
	for k, v := range p.order {
		if v == i {
			p.load(k)
			return
		}
	}
}

// Backend returns the active external audio backend, or nil for built-in output.
func (p *Player) Backend() OutputBackend { return p.backend }

// SetBackend switches audio output between the built-in OS mixer (nil) and an
// external audiophile chain, seamlessly reopening the current track at its
// playback position when one is loaded.
func (p *Player) SetBackend(b OutputBackend) {
	if p.backend == b {
		return
	}
	hasTrack := p.track != nil && p.at >= 0 && p.at < len(p.order)
	wasPlaying := p.playing
	pos := p.Position()
	at := p.at
	p.backend = b
	if hasTrack {
		p.loadSkipping(at, 0, wasPlaying, pos)
	}
}

// TogglePlay pauses, fading out, or plays, fading in.
func (p *Player) TogglePlay() {
	if p.out == nil {
		switch {
		case p.loading:
			// It plays once loaded, or not.
			p.playing = !p.playing
			p.sync()
		case len(p.order) > 0:
			p.loadSkipping(max(p.at, 0), 0, true, p.pending)
		}
		return
	}
	out := p.out
	if out.DirectTransport() {
		if p.playing {
			p.playing = false
			out.Pause()
		} else {
			p.playing = true
			if out.Duration() > 0 && out.Position() >= out.Duration()-time.Second {
				out.Seek(0)
			}
			out.Play()
		}
		p.sync()
		return
	}
	if p.playing {
		p.playing = false
		p.fade.fade(0, fadeOut, func() {
			p.post(func() {
				if p.out == out && !p.playing {
					out.Pause()
					p.sync()
				}
			})
		})
		p.sync()
		return
	}
	p.playing = true
	if !p.seeking && !out.Playing() {
		if out.Position() >= out.Duration()-time.Second {
			out.Seek(0)
		}
		out.Play()
	}
	p.fade.fade(1, fadeIn, nil)
	p.sync()
}

// seekGap is how long a seek waits for what plays to fade out, which the
// audio player does over 5 ms, as it pauses: within the next buffer or
// two it sends the system.
const seekGap = 30 * time.Millisecond

// Seek moves to t. Playing, it pauses and plays again around the jump,
// which the audio player fades over a few milliseconds, rather than
// cutting from one sound to the other with a click.
func (p *Player) Seek(t time.Duration) {
	if p.out == nil {
		p.pending = t
		return
	}
	out := p.out
	if out.DirectTransport() {
		out.Seek(t)
		p.sync()
		return
	}
	// Paused, it jumps at once; but a seek on its way paused it only for
	// the jump, and this one takes its place, playing on from there.
	if !out.Playing() && !p.seeking {
		out.Seek(t)
		p.sync()
		return
	}
	p.seekGen++
	gen := p.seekGen
	p.seeking, p.seekTo = true, t
	out.Pause()
	time.AfterFunc(seekGap, func() {
		p.post(func() {
			if gen != p.seekGen || p.out != out {
				return
			}
			p.seeking = false
			out.Seek(t)
			if p.playing || !p.fade.silent() {
				out.Play()
			}
			p.sync()
		})
	})
	p.sync()
}

func (p *Player) SetVolume(v float64) {
	p.volume = v
	p.applyGain()
}

func (p *Player) effectiveGain(infoGain float64) float64 {
	g := p.volume * p.volume
	if p.Normalize != nil && p.Normalize() {
		g *= infoGain
	}
	return g
}

// applyGain sets the volume of what plays: the listener's, which the ear
// hears in powers, and the track's gain, if normalizing.
func (p *Player) applyGain() {
	g := p.effectiveGain(p.gain)
	if p.out != nil && p.out.DirectTransport() {
		p.out.SetSinkVolume(g)
		return
	}
	p.fade.setGain(g)
}

// normPreamp lifts normalized tracks, which NetEase brings to the quiet
// loudness of ReplayGain, nearer to how loud music plays elsewhere.
const normPreamp = 4

// normGain is the gain that brings a track as loud as others, as NetEase
// measured it, but never so loud that its peaks would clip.
func normGain(st *ytm.Stream) float64 {
	if st == nil || st.Gain == 0 && st.Peak == 0 {
		return 1
	}
	g := math.Pow(10, (st.Gain+normPreamp)/20)
	if st.Peak > 0 {
		g = min(g, 1/st.Peak)
	}
	return g
}

func (p *Player) Position() time.Duration {
	switch {
	case p.out == nil:
		return p.pending
	case p.seeking:
		return p.seekTo
	}
	return p.out.Position()
}

func (p *Player) Duration() time.Duration {
	if p.track == nil {
		return 0
	}
	return p.track.Length()
}

// Levels returns the spectrum of what plays now.
func (p *Player) Levels() ([Bands]float32, bool) {
	if p.stream == nil || p.out == nil || !p.playing {
		return [Bands]float32{}, false
	}
	_, spec := p.stream.source()
	if spec == nil {
		return [Bands]float32{}, false
	}
	frame := int64(p.Position().Seconds() * float64(p.stream.SampleRate()))
	return spec.at(frame)
}

func (p *Player) stop() {
	if p.loadCancel != nil {
		p.loadCancel()
		p.loadCancel = nil
	}
	out, st := p.out, p.stream
	if out != nil {
		if out.DirectTransport() {
			out.Close()
			if st != nil {
				_ = st.Close()
			}
		} else {
			// Paused first, it fades out over a few milliseconds, rather than
			// stopping with a click.
			out.Pause()
			time.AfterFunc(100*time.Millisecond, func() {
				out.Close()
				if st != nil {
					_ = st.Close()
				}
			})
		}
	} else if st != nil {
		_ = st.Close()
	}
	p.out, p.stream, p.info = nil, nil, nil
	p.seeking = false
	p.fade.attachSink(nil, 1)
}

// Stop releases any active audio sink and download when the app exits.
func (p *Player) Stop() {
	p.stop()
}

// load plays the track at order[at], skipping those that cannot play.
func (p *Player) load(at int) {
	p.loadSkipping(at, 0, true, 0)
}

// loadSkipping loads the track at order[at], to play or not, from from,
// skipping those that cannot play: skipped of them so far.
func (p *Player) loadSkipping(at, skipped int, play bool, from time.Duration) {
	p.stop()
	p.gen++
	gen := p.gen
	ctx, cancel := context.WithCancel(context.Background())
	p.loadCancel = cancel
	backend := p.backend
	p.at = at
	t := p.queue[p.order[at]]
	p.track = &t
	p.loading, p.playing, p.err = true, play, ""
	p.awaiting = false
	p.lyrics, p.plainLyrics, p.lyricsLoaded = nil, nil, false
	p.pending = from
	p.announce(&t, nil)
	p.refill()

	localProv := p.Local
	isLocal := localProv != nil && localProv.IsLocalTrack(t.ID)

	if !isLocal && t.Album.PicURL != "" {
		go func() {
			cover, err := ytm.Fetch(ytm.Sized(t.Album.PicURL, 600))
			if err == nil {
				p.post(func() {
					if gen == p.gen {
						p.announce(&t, cover)
					}
				})
			}
		}()
	}

	go func() {
		var lines []lyrics.Line
		var plain []string
		if isLocal {
			lrc, trans, rawPlain := localProv.LocalLyrics(t.ID)
			plain = rawPlain
			if lrc != "" {
				lines = lyrics.Build(lrc, trans, "")
				if len(lines) == 0 && len(plain) == 0 {
					plain = lyrics.Plain(lrc)
				}
			}
		} else {
			lrc, trans, yrc, err := ytm.Lyrics(&t)
			if err == nil {
				lines = lyrics.Build(lrc, trans, yrc)
				if len(lines) == 0 {
					plain = lyrics.Plain(lrc)
				}
			}
		}
		p.post(func() {
			if gen == p.gen {
				p.lyrics, p.plainLyrics = lines, plain
				p.lyricsLoaded = true
			}
		})
	}()
	initGain := p.effectiveGain(1)
	go func() {
		var (
			info       *ytm.Stream
			s          stream
			err        error
			extOut     OutputSink
			backendErr error
		)
		if isLocal {
			var opened *LocalOpenedTrack
			opened, err = localProv.OpenLocalTrack(t.ID)
			if err == nil && opened == nil {
				err = errors.New("player: empty local audio")
			}
			if err == nil {
				var (
					decMu    sync.Mutex
					firstDec = opened.Decoder
					specMu   sync.Mutex
					spec     *spectrum
				)
				closeUnusedFirst := func() {
					decMu.Lock()
					d := firstDec
					firstDec = nil
					decMu.Unlock()
					if d != nil {
						if c, ok := d.(io.Closer); ok {
							_ = c.Close()
						}
					}
				}
				defer closeUnusedFirst()

				if len(opened.Cover) > 0 {
					cover := opened.Cover
					p.post(func() {
						if gen == p.gen {
							p.announce(&t, cover)
						}
					})
				}

				if opened.Stream != nil {
					cp := *opened.Stream
					info = &cp
				} else {
					info = &ytm.Stream{}
				}
				if firstDec != nil && info.SampleRate <= 0 && firstDec.SampleRate() > 0 {
					info.SampleRate = firstDec.SampleRate()
				}

				getOrInitSpec := func(sr int, frames int64) *spectrum {
					specMu.Lock()
					defer specMu.Unlock()
					if spec == nil {
						spec = newSpectrum(sr, frames)
					}
					return spec
				}
				currentSpec := func() *spectrum {
					specMu.Lock()
					defer specMu.Unlock()
					return spec
				}

				openPCM := func() (*localPCMStream, error) {
					var base audio.Decoder
					if opened.OpenPCM != nil {
						var oerr error
						base, oerr = opened.OpenPCM()
						if oerr != nil {
							return nil, oerr
						}
						if base == nil {
							return nil, errors.New("player: empty local audio")
						}
					} else {
						decMu.Lock()
						if firstDec != nil {
							base = firstDec
							firstDec = nil
						}
						decMu.Unlock()
						if base == nil {
							op, oerr := localProv.OpenLocalTrack(t.ID)
							if oerr != nil {
								return nil, oerr
							}
							if op == nil {
								return nil, errors.New("player: empty local audio")
							}
							if op.OpenPCM != nil {
								if op.Decoder != nil {
									if c, ok := op.Decoder.(io.Closer); ok {
										_ = c.Close()
									}
								}
								var perr error
								base, perr = op.OpenPCM()
								if perr != nil {
									return nil, perr
								}
							} else {
								base = op.Decoder
							}
							if base == nil {
								return nil, errors.New("player: empty local audio")
							}
						}
					}
					sr := base.SampleRate()
					if sr <= 0 {
						if info.SampleRate > 0 {
							sr = info.SampleRate
						} else {
							sr = 44100
						}
					}
					frames := max(base.Len(), int64(0))
					if frames == 0 && t.Duration > 0 {
						frames = t.Duration * int64(sr) / 1000
					}
					return newLocalPCMStream(base, getOrInitSpec(sr, frames)), nil
				}

				if backend != nil {
					if ctx.Err() != nil {
						backendErr = ctx.Err()
					} else {
						dur := t.Length()
						if firstDec != nil && firstDec.Len() > 0 && firstDec.SampleRate() > 0 {
							dur = time.Duration(float64(firstDec.Len()) / float64(firstDec.SampleRate()) * float64(time.Second))
						}
						src := TrackSource{
							Track:      t,
							Stream:     info,
							Duration:   dur,
							Gain:       initGain * normGain(info),
							LocalPath:  opened.LocalPath,
							NativeHQP:  opened.NativeHQP,
							CoverBytes: opened.Cover,
							OpenPCM:    func() (PCMDecoder, error) { return openPCM() },
							OpenRaw:    opened.OpenRaw,
						}
						extOut, backendErr = backend.Open(ctx, src)
						if backendErr == nil {
							if ls, lerr := openPCM(); lerr == nil {
								s = ls
								if info.SampleRate <= 0 {
									info.SampleRate = ls.SampleRate()
								}
							} else {
								s = newInertLocalStream(info.SampleRate, t.Duration, currentSpec())
								if info.SampleRate <= 0 {
									info.SampleRate = s.SampleRate()
								}
							}
							if ctx.Err() != nil {
								_ = s.Close()
							}
						}
					}
				} else {
					ls, lerr := openPCM()
					if lerr != nil {
						err = lerr
					} else {
						s = ls
						if info.SampleRate <= 0 {
							info.SampleRate = ls.SampleRate()
						}
					}
				}
			}
		} else {
			level := "standard"
			if p.Quality != nil {
				level = p.Quality()
			}
			info, s, err = p.open(t, level)
			openBackend := func() {
				if ctx.Err() != nil {
					s.Close()
					return
				}
				dl, spec := s.source()
				dur := t.Length()
				if frames := s.Len(); frames > 0 && s.SampleRate() > 0 {
					dur = time.Duration(float64(frames) / float64(s.SampleRate()) * float64(time.Second))
				}
				src := TrackSource{
					Track:       t,
					Stream:      info,
					Duration:    dur,
					Gain:        initGain * normGain(info),
					Downloading: dl != nil && !dl.completed(),
					OpenPCM:     func() (PCMDecoder, error) { return openSharedWithSpec(dl, spec) },
					OpenRaw:     func() (RawStream, error) { return openSharedRaw(dl) },
				}
				stopLoad := context.AfterFunc(ctx, func() { _ = s.Close() })
				extOut, backendErr = backend.Open(ctx, src)
				stopLoad()
			}
			if err == nil && backend != nil {
				openBackend()
				if backendErr != nil && ctx.Err() == nil && info != nil && info.Level == "exhigh" && isForbiddenStreamErr(backendErr) {
					ytm.MarkExhighForbidden()
					_ = s.Close()
					extOut, backendErr = nil, nil
					if info, s, err = p.open(t, "standard"); err == nil {
						openBackend()
					}
				}
			}
		}
		p.post(func() {
			if gen != p.gen {
				if extOut != nil {
					extOut.Close()
				}
				if s != nil {
					s.Close()
				}
				return
			}
			p.loading = false
			if backendErr != nil {
				if s != nil {
					s.Close()
				}
				if !errors.Is(backendErr, context.Canceled) {
					p.err = backendErr.Error()
				}
				p.playing = false
				p.sync()
				return
			}
			if err != nil {
				switch {
				case errors.Is(err, ytm.ErrBotCheck):
					if ytm.SignedIn() {
						p.err = "当前代理节点触发 YouTube 风控，请更换节点或重新登录"
					} else {
						p.err = "当前节点触发验证，请先登录 YouTube 账号"
					}
				case errors.Is(err, ytm.ErrUnavailable):
					p.err = "这首歌暂时无法播放"
				default:
					p.err = err.Error()
				}
				const maxConsecutiveSkips = 3
				// Skip individual unplayable tracks, but stop on a bot check or
				// after a few consecutive failures instead of draining the queue.
				if !errors.Is(err, ytm.ErrBotCheck) && skipped+1 < len(p.order) && skipped+1 < maxConsecutiveSkips {
					next := at + 1
					if next >= len(p.order) {
						next = 0
					}
					p.loadSkipping(next, skipped+1, p.playing, 0)
				} else {
					p.playing = false
					p.sync()
				}
				return
			}
			p.stream, p.info = s, info
			if p.track != nil && p.track.Duration <= 0 && s != nil && s.Len() > 0 && s.SampleRate() > 0 {
				p.track.Duration = s.Len() * 1000 / int64(s.SampleRate())
			}
			p.gain = normGain(info)
			if extOut != nil {
				extOut.Activate()
				p.out = extOut
			} else {
				p.out = localSink{Player: audio.NewPlayer(s)}
			}
			if !p.out.DirectTransport() {
				// From its middle, as a session picked up again, it fades in.
				lvl := 1.0
				if p.pending > 0 {
					lvl = 0
				}
				p.fade.attachSink(p.out, lvl)
			}
			p.applyGain()
			p.out.OnSinkEnd(func() {
				p.post(func() {
					if gen == p.gen {
						p.ended()
					}
				})
			})
			if p.pending > 0 {
				p.out.Seek(p.pending)
				p.pending = 0
			}
			if p.playing {
				p.out.Play()
				if !p.out.DirectTransport() {
					p.fade.fade(1, fadeIn, nil)
				}
			}
			p.sync()
		})
	}()
}

func isForbiddenStreamErr(err error) bool {
	if err == nil {
		return false
	}
	var se httpStatusError
	if errors.As(err, &se) && se.code == http.StatusForbidden {
		return true
	}
	return strings.Contains(err.Error(), "403")
}

// open opens track t's audio at level: kept in the cache, or else
// streamed into it, and kept once it has all come in.
func (p *Player) open(t ytm.Track, level string) (*ytm.Stream, stream, error) {
	dl, info, ok := p.Cache.lookup(t.ID, level)
	if !ok {
		var err error
		info, err = songURL(t.ID, level)
		if err != nil {
			return nil, nil, err
		}
		dl = p.Cache.download(t.ID, info)
	}
	var err error
	var s stream
	// WEB_REMIX 256 kbps streams without a browser PO token often serve the
	// initial 256 KB header range and reject chunk 2 with 403 Forbidden; probe
	// past firstRange so we fall back seamlessly to standard before playback.
	if !ok && info.Level == "exhigh" && info.Size > firstRange {
		err = dl.probeRange(firstRange + 1)
	}
	if err == nil {
		s, err = openStream(dl, t.Duration)
	}
	if err != nil && !ok {
		if info.Level == "exhigh" && isForbiddenStreamErr(err) {
			ytm.MarkExhighForbidden()
		}
		dl.Close()
		ytm.InvalidateTrackStream(t.ID)
		fallback := "standard"
		if info.Level == "standard" {
			if !ytm.SignedIn() {
				return nil, nil, err
			}
			fallback = "standard_fallback"
		}
		if fallback == "standard" {
			if cdl, cinfo, cok := p.Cache.lookup(t.ID, "standard"); cok {
				if cs, cerr := openStream(cdl, t.Duration); cerr == nil {
					return cinfo, cs, nil
				}
				cdl.Close()
			}
		}
		if info, err = songURL(t.ID, fallback); err != nil {
			return nil, nil, err
		}
		dl = p.Cache.download(t.ID, info)
		s, err = openStream(dl, t.Duration)
	}
	if err != nil {
		dl.Close()
		return nil, nil, err
	}
	return info, s, nil
}

// songURL finds where a song streams; tests replace it.
var songURL = ytm.SongURL

// Swap plays t in place of the track playing, from where that one is, as
// the music video of a song, whose sound plays instead.
func (p *Player) Swap(t ytm.Track) {
	if p.track == nil || p.at < 0 {
		return
	}
	from := p.Position()
	p.queue[p.order[p.at]] = t
	p.loadSkipping(p.at, 0, p.playing, from)
}

// Session is where playback was left, to pick up again.
type Session struct {
	Queue    []ytm.Track `json:"queue"`
	Order    []int       `json:"order"`
	At       int         `json:"at"`
	Shuffle  bool        `json:"shuffle"`
	Repeat   RepeatMode  `json:"repeat"`
	Position int64       `json:"position"` // milliseconds
	Radio    string      `json:"radio,omitempty"`
}

// Session returns where playback is, or false with nothing queued.
func (p *Player) Session() (Session, bool) {
	if p.track == nil || p.at < 0 {
		return Session{}, false
	}
	return Session{
		Queue:    p.queue,
		Order:    p.order,
		At:       p.at,
		Shuffle:  p.shuffle,
		Repeat:   p.repeat,
		Position: p.Position().Milliseconds(),
		Radio:    p.radio,
	}, true
}

// Restore picks up a session where it was left, paused, and loads its
// track to play at once when asked.
func (p *Player) Restore(s Session) {
	n := len(s.Queue)
	if n == 0 || len(s.Order) != n || s.At < 0 || s.At >= n {
		return
	}
	seen := make([]bool, n)
	for _, i := range s.Order {
		if i < 0 || i >= n || seen[i] {
			return
		}
		seen[i] = true
	}
	p.queue, p.order = s.Queue, s.Order
	p.shuffle, p.repeat = s.Shuffle, s.Repeat%3
	p.loadSkipping(s.At, 0, false, time.Duration(s.Position)*time.Millisecond)
}
