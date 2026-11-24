package player

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/audio/mp4"

	"ytb-music/internal/ytm"
)

// PCMDecoder decodes stereo float32 PCM frames from a shared track download
// while feeding the player's FFT spectrum analyzer for visualizers.
type PCMDecoder interface {
	audio.Decoder
	Close() error
}

// RawStream exposes random-access reads over the underlying M4A/MP4 container
// as it downloads or from disk cache.
type RawStream interface {
	io.ReaderAt
	io.ReadSeeker
	Size() (int64, error)
	Close() error
}

// TrackSource bundles the current track's metadata and stream openers for an
// external OutputBackend (such as HQPlayer or Roon).
type TrackSource struct {
	Track    ytm.Track
	Stream   *ytm.Stream
	Duration time.Duration
	Gain     float64
	OpenPCM  func() (PCMDecoder, error)
	OpenRaw  func() (RawStream, error)
}

// OutputSink controls playback of a single loaded track on either the built-in
// OS audio mixer or an external audiophile transport.
type OutputSink interface {
	Play()
	Pause()
	Playing() bool
	Seek(t time.Duration)
	Position() time.Duration
	Duration() time.Duration
	SetSinkVolume(v float64)
	OnSinkEnd(fn func())
	// DirectTransport reports whether the sink talks to an external hardware/DSP
	// engine over IPC/TCP (skipping 8ms software volume ramps and pause-seeks).
	DirectTransport() bool
	Close()
}

// OutputBackend opens tracks on an external audio chain. A nil backend on
// Player uses the built-in OS audio output.
type OutputBackend interface {
	Name() string
	Open(ctx context.Context, src TrackSource) (OutputSink, error)
}

// localSink adapts MyGo's built-in *audio.Player to OutputSink.
type localSink struct {
	*audio.Player
}

func (s localSink) SetSinkVolume(v float64) {
	if s.Player != nil {
		s.Player.SetVolume(v)
	}
}

func (s localSink) OnSinkEnd(fn func()) {
	if s.Player != nil {
		s.Player.OnEnd(fn)
	}
}

func (localSink) DirectTransport() bool { return false }

func openSharedWithSpec(dl *download, spec *spectrum) (PCMDecoder, error) {
	if !dl.share() {
		return nil, errors.New("player: the song is gone")
	}
	dec, err := mp4.NewDecoder(&downloadFile{d: dl})
	if err != nil {
		dl.release()
		return nil, err
	}
	return &m4aStream{
		dl:   dl,
		dec:  dec,
		spec: spec,
		drop: dl.release,
	}, nil
}

type sharedRawStream struct {
	downloadFile
	once sync.Once
	drop func()
}

func openSharedRaw(dl *download) (RawStream, error) {
	if !dl.share() {
		return nil, errors.New("player: the song is gone")
	}
	return &sharedRawStream{
		downloadFile: downloadFile{d: dl},
		drop:         dl.release,
	}, nil
}

func (r *sharedRawStream) Size() (int64, error) {
	return r.downloadFile.size()
}

func (r *sharedRawStream) Close() error {
	r.once.Do(r.drop)
	return nil
}
