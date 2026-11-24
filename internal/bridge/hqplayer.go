package bridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ytb-music/internal/player"
)

const xmlDecl = `<?xml version="1.0" encoding="UTF-8"?>`

type xmlReply struct {
	tag   string
	attrs map[string]string
	err   error
}

// ProbeHQPlayer connects to HQPlayer's TCP control port (default 4321), sends
// <GetInfo/>, and returns the reported product/version string.
func ProbeHQPlayer(ctx context.Context, addr string) (string, error) {
	addr = normalizeHostPort(addr, DefaultHQPlayerAddr)
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("无法连接 HQPlayer (%s): %w", addr, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(4 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte(xmlDecl + `<GetInfo/>`)); err != nil {
		return "", err
	}
	xr := newXMLFrameReader(conn)
	for range 4 {
		tag, doc, err := xr.Next()
		if err != nil {
			return "", err
		}
		_, attrs, perr := parseXMLAttrs(doc)
		if perr != nil {
			return "", perr
		}
		if tag == "GetInfo" || attrs["product"] != "" {
			prod := strings.TrimSpace(attrs["product"])
			ver := strings.TrimSpace(attrs["version"])
			if prod == "" {
				prod = "HQPlayer"
			}
			if ver != "" {
				return prod + " " + ver, nil
			}
			return prod, nil
		}
	}
	return "HQPlayer", nil
}

func normalizeHostPort(raw, fallback string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	if !strings.Contains(s, ":") {
		_, defPort, _ := net.SplitHostPort(fallback)
		return net.JoinHostPort(s, defPort)
	}
	return s
}

type hqpConn struct {
	conn     net.Conn
	closed   atomic.Bool
	waitMu   sync.Mutex
	waitTag  string
	waitCh   chan xmlReply
	volMinDB float64
	volMaxDB float64
	product  string
}

// HQPlayerBackend streams native-rate 24-bit WAV over HTTP to Signalyst
// HQPlayer Desktop / Embedded via its TCP 4321 XML control protocol.
type HQPlayerBackend struct {
	srv *Server
	now func() time.Time

	cfgMu   sync.RWMutex
	addr    string
	onState func(connected bool, detail string)

	retired atomic.Bool
	cmdMu   sync.Mutex
	connPtr atomic.Pointer[hqpConn]

	sinkMu sync.Mutex
	epoch  atomic.Uint64
	active *hqpSink
}

func NewHQPlayerBackend(srv *Server, addr string, onState func(bool, string)) *HQPlayerBackend {
	return &HQPlayerBackend{
		srv:     srv,
		now:     time.Now,
		addr:    normalizeHostPort(addr, DefaultHQPlayerAddr),
		onState: onState,
	}
}

func (b *HQPlayerBackend) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *HQPlayerBackend) Name() string { return "hqplayer" }

func (b *HQPlayerBackend) Addr() string {
	b.cfgMu.RLock()
	defer b.cfgMu.RUnlock()
	return b.addr
}

func (b *HQPlayerBackend) SetAddr(addr string) {
	norm := normalizeHostPort(addr, DefaultHQPlayerAddr)
	b.cfgMu.Lock()
	changed := b.addr != norm
	b.addr = norm
	b.cfgMu.Unlock()
	if changed {
		b.DisconnectAsync(true)
	}
}

// Shutdown permanently retires this backend instance so no queued sink command
// can dial a new connection, optionally sending <Stop/> on the existing socket.
func (b *HQPlayerBackend) Shutdown(sendStop bool) {
	b.retired.Store(true)
	b.epoch.Add(1)
	b.DisconnectAsync(sendStop)
}

// Disconnect closes any active TCP control socket immediately without waiting
// on cmdMu so callers never block behind an in-flight <PlaylistAdd>.
func (b *HQPlayerBackend) Disconnect() {
	hc := b.connPtr.Swap(nil)
	if hc != nil {
		hc.closed.Store(true)
		_ = hc.conn.Close()
	}
	b.emitState(false, "未连接")
}

// DisconnectAsync stops playback if requested on the existing socket and tears it down off the UI thread.
func (b *HQPlayerBackend) DisconnectAsync(sendStop bool) {
	hc := b.connPtr.Swap(nil)
	if hc == nil && !b.retired.Load() {
		return
	}
	go func() {
		b.cmdMu.Lock()
		if late := b.connPtr.Swap(nil); late != nil {
			if hc != nil {
				hc.closed.Store(true)
				_ = hc.conn.Close()
			}
			hc = late
		}
		if hc != nil {
			if sendStop && !hc.closed.Load() {
				_ = hc.conn.SetWriteDeadline(time.Now().Add(800 * time.Millisecond))
				_, _ = hc.conn.Write([]byte(xmlDecl + `<Stop/>`))
			}
			hc.closed.Store(true)
			_ = hc.conn.Close()
		}
		b.cmdMu.Unlock()
		b.emitState(false, "未连接")
	}()
}

func (b *HQPlayerBackend) emitState(ok bool, detail string) {
	b.cfgMu.RLock()
	fn := b.onState
	b.cfgMu.RUnlock()
	if fn != nil {
		fn(ok, detail)
	}
}

// ensureConnLocked establishes and initializes the persistent TCP 4321 session.
// Caller must hold b.cmdMu.
func (b *HQPlayerBackend) ensureConnLocked(ctx context.Context) (*hqpConn, error) {
	if b.retired.Load() {
		return nil, errors.New("hqplayer: backend retired")
	}
	if cur := b.connPtr.Load(); cur != nil && !cur.closed.Load() {
		return cur, nil
	}
	addr := b.Addr()
	d := net.Dialer{Timeout: 3 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		b.emitState(false, "连接失败："+addr)
		return nil, fmt.Errorf("无法连接 HQPlayer (%s)，请确认 HQPlayer 已启动并开启网络控制", addr)
	}
	hc := &hqpConn{
		conn:     nc,
		volMinDB: -60.0,
		volMaxDB: 0.0,
		product:  "HQPlayer",
	}
	go b.readLoop(hc)

	if rep, err := b.sendLocked(ctx, hc, "GetInfo", `<GetInfo/>`, 3*time.Second); err == nil {
		if p := strings.TrimSpace(rep.attrs["product"]); p != "" {
			if v := strings.TrimSpace(rep.attrs["version"]); v != "" {
				hc.product = p + " " + v
			} else {
				hc.product = p
			}
		}
	}
	_, _ = b.sendLocked(ctx, hc, "SetRepeat", `<SetRepeat value="0"/>`, 3*time.Second)
	if rep, err := b.sendLocked(ctx, hc, "VolumeRange", `<VolumeRange/>`, 3*time.Second); err == nil {
		if mn, perr := strconv.ParseFloat(rep.attrs["min"], 64); perr == nil && mn < 0 {
			hc.volMinDB = mn
		}
		if mx, perr := strconv.ParseFloat(rep.attrs["max"], 64); perr == nil {
			hc.volMaxDB = min(0.0, mx)
		}
	}
	if _, err := b.sendLocked(ctx, hc, "Status", `<Status subscribe="1"/>`, 3*time.Second); err != nil {
		hc.closed.Store(true)
		_ = hc.conn.Close()
		return nil, err
	}
	if b.retired.Load() {
		hc.closed.Store(true)
		_ = hc.conn.Close()
		return nil, errors.New("hqplayer: backend retired")
	}

	b.connPtr.Store(hc)
	b.emitState(true, "已连接 "+hc.product)
	return hc, nil
}

func (b *HQPlayerBackend) readLoop(hc *hqpConn) {
	xr := newXMLFrameReader(hc.conn)
	for {
		tag, doc, err := xr.Next()
		if err != nil {
			hc.closed.Store(true)
			_ = hc.conn.Close()
			b.connPtr.CompareAndSwap(hc, nil)
			hc.waitMu.Lock()
			ch := hc.waitCh
			hc.waitCh = nil
			hc.waitTag = ""
			hc.waitMu.Unlock()
			if ch != nil {
				ch <- xmlReply{err: err}
			}
			b.emitState(false, "连接已断开")
			return
		}
		parsedTag, attrs, perr := parseXMLAttrs(doc)
		if perr != nil {
			continue
		}
		if parsedTag != "" {
			tag = parsedTag
		}

		hc.waitMu.Lock()
		ch := hc.waitCh
		wTag := hc.waitTag
		matched := ch != nil && (tag == wTag || tag == "Reset" || tag == "Error")
		if matched {
			hc.waitCh = nil
			hc.waitTag = ""
		}
		hc.waitMu.Unlock()
		if matched {
			ch <- xmlReply{tag: tag, attrs: attrs}
		}

		if tag == "Status" {
			b.onStatusFrame(attrs)
		}
	}
}

// sendLocked writes a single XML command and waits for its matching root reply.
// Caller must hold b.cmdMu.
func (b *HQPlayerBackend) sendLocked(ctx context.Context, hc *hqpConn, expectTag, xmlBody string, timeout time.Duration) (xmlReply, error) {
	if hc == nil || hc.closed.Load() {
		return xmlReply{}, errors.New("hqplayer: connection closed")
	}
	ch := make(chan xmlReply, 1)
	hc.waitMu.Lock()
	hc.waitTag = expectTag
	hc.waitCh = ch
	hc.waitMu.Unlock()

	defer func() {
		hc.waitMu.Lock()
		if hc.waitCh == ch {
			hc.waitCh = nil
			hc.waitTag = ""
		}
		hc.waitMu.Unlock()
	}()

	stopCancel := context.AfterFunc(ctx, func() {
		// Unblock any stalled socket write/read immediately upon context cancellation.
		hc.closed.Store(true)
		_ = hc.conn.Close()
	})
	defer stopCancel()

	_ = hc.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := hc.conn.Write([]byte(xmlDecl + xmlBody)); err != nil {
		hc.closed.Store(true)
		_ = hc.conn.Close()
		return xmlReply{}, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return xmlReply{}, ctx.Err()
	case <-timer.C:
		return xmlReply{}, fmt.Errorf("HQPlayer 指令 <%s> 响应超时", expectTag)
	case rep := <-ch:
		if rep.err != nil {
			if ctx.Err() != nil {
				return xmlReply{}, ctx.Err()
			}
			return xmlReply{}, rep.err
		}
		if strings.EqualFold(rep.attrs["result"], "Error") || rep.tag == "Error" {
			errText := rep.attrs["value"]
			if errText == "" {
				errText = rep.attrs["error"]
			}
			if expectTag == "Volume" && strings.Contains(errText, "GetAlbumGain") {
				return rep, nil
			}
			if errText == "" {
				errText = "HQPlayer 返回错误"
			}
			return rep, errors.New(errText)
		}
		return rep, nil
	}
}

func (b *HQPlayerBackend) execForEpoch(ctx context.Context, epoch uint64, expectTag, xmlBody string, timeout time.Duration) (xmlReply, error) {
	b.cmdMu.Lock()
	defer b.cmdMu.Unlock()
	if ctx.Err() != nil {
		return xmlReply{}, ctx.Err()
	}
	if b.retired.Load() || (epoch != 0 && b.epoch.Load() != epoch) {
		return xmlReply{}, errors.New("hqplayer: stale sink command")
	}
	hc, err := b.ensureConnLocked(ctx)
	if err != nil {
		return xmlReply{}, err
	}
	rep, err := b.sendLocked(ctx, hc, expectTag, xmlBody, timeout)
	if err != nil && ctx.Err() == nil && !b.retired.Load() && (epoch == 0 || b.epoch.Load() == epoch) && hc.closed.Load() {
		b.connPtr.CompareAndSwap(hc, nil)
		hc, err = b.ensureConnLocked(ctx)
		if err != nil {
			return xmlReply{}, err
		}
		rep, err = b.sendLocked(ctx, hc, expectTag, xmlBody, timeout)
	}
	return rep, err
}

// sendStopExisting sends <Stop/> only if an open connection already exists for epoch, never dialing anew.
func (b *HQPlayerBackend) sendStopExisting(epoch uint64) {
	b.cmdMu.Lock()
	defer b.cmdMu.Unlock()
	if b.retired.Load() || b.epoch.Load() != epoch {
		return
	}
	hc := b.connPtr.Load()
	if hc == nil || hc.closed.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = b.sendLocked(ctx, hc, "Stop", `<Stop/>`, 1500*time.Millisecond)
}

func buildHQPPlaylistAdd(wavURL string, src player.TrackSource, coverURL string, startMs int64, seekGen uint64) string {
	uri := wavURL
	if startMs > 0 || seekGen > 0 {
		uri = fmt.Sprintf("%s?start_ms=%d&s=%d", wavURL, max(int64(0), startMs), seekGen)
	}
	remDur := src.Duration - time.Duration(max(int64(0), startMs))*time.Millisecond
	secs := max(1, int(remDur.Round(time.Second).Seconds()))
	var meta strings.Builder
	meta.WriteString(`<metadata`)
	fmt.Fprintf(&meta, ` song="%s"`, xmlEscapeAttr(src.Track.Name))
	if ar := src.Track.ArtistNames(); ar != "" {
		fmt.Fprintf(&meta, ` artist="%s"`, xmlEscapeAttr(ar))
	}
	if al := src.Track.Album.Name; al != "" {
		fmt.Fprintf(&meta, ` album="%s"`, xmlEscapeAttr(al))
	}
	if coverURL != "" {
		fmt.Fprintf(&meta, ` cover="%s"`, xmlEscapeAttr(coverURL))
	}
	fmt.Fprintf(&meta, ` length="%d"`, secs)
	if src.Stream != nil && src.Stream.Gain != 0 {
		fmt.Fprintf(&meta, ` album_gain="%.2f"`, src.Stream.Gain)
	}
	meta.WriteString(`/>`)

	return fmt.Sprintf(
		`<PlaylistAdd uri="%s" queued="0" clear="0" start="0" freewheel="0">%s</PlaylistAdd>`,
		xmlEscapeAttr(uri),
		meta.String(),
	)
}

func (b *HQPlayerBackend) Open(ctx context.Context, src player.TrackSource) (player.OutputSink, error) {
	epoch := b.epoch.Add(1)
	addr := b.Addr()
	seq, wavURL, coverURL := b.srv.RegisterHQPTrack(src, addr)

	if _, err := b.execForEpoch(ctx, epoch, "PlaylistClear", `<PlaylistClear/>`, 5*time.Second); err != nil {
		return nil, err
	}

	addCmd := buildHQPPlaylistAdd(wavURL, src, coverURL, 0, 0)
	if _, err := b.execForEpoch(ctx, epoch, "PlaylistAdd", addCmd, 25*time.Second); err != nil {
		return nil, fmt.Errorf("HQPlayer 加载音轨失败: %w", err)
	}

	sinkCtx, sinkCancel := context.WithCancel(context.Background())
	sink := &hqpSink{
		backend:  b,
		epoch:    epoch,
		seq:      seq,
		src:      src,
		wavURL:   wavURL,
		coverURL: coverURL,
		duration: src.Duration,
		posAt:    b.nowTime(),
		ctx:      sinkCtx,
		cancel:   sinkCancel,
		wakeCh:   make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
	}
	go sink.transportWorker()
	sink.SetSinkVolume(src.Gain)
	return sink, nil
}

func (b *HQPlayerBackend) onStatusFrame(attrs map[string]string) {
	state, err := strconv.Atoi(attrs["state"])
	if err != nil {
		return
	}
	posSec, _ := strconv.ParseFloat(attrs["position"], 64)

	b.sinkMu.Lock()
	sink := b.active
	b.sinkMu.Unlock()
	if sink != nil {
		sink.updateFromHQP(state, posSec)
	}
}

type hqpSink struct {
	backend  *HQPlayerBackend
	epoch    uint64
	seq      uint64
	src      player.TrackSource
	wavURL   string
	coverURL string
	duration time.Duration
	ctx      context.Context
	cancel   context.CancelFunc

	mu              sync.Mutex
	closed          bool
	playing         bool
	hqState         int
	sawPlaying      bool
	playRequestedAt time.Time
	baseOffset      time.Duration
	seekSeq         uint64
	pos             time.Duration
	posAt           time.Time
	pendingSeek     time.Duration
	hasPendingSeek  bool
	onEnd           func()
	endedOnce       bool

	// Coalesced ordered desired commands processed by transportWorker.
	playDirty    bool
	wantPlay     bool
	seekDirty    bool
	seekInFlight bool
	seekTarget   time.Duration
	volDirty     bool
	volTarget    float64

	wakeCh chan struct{}
	stopCh chan struct{}
}

func (s *hqpSink) isCurrent() bool {
	return !s.backend.retired.Load() && s.backend.epoch.Load() == s.epoch
}

func (s *hqpSink) Activate() {
	s.backend.sinkMu.Lock()
	s.backend.active = s
	s.backend.sinkMu.Unlock()
}

func (s *hqpSink) DirectTransport() bool { return true }

func (s *hqpSink) Duration() time.Duration { return s.duration }

func (s *hqpSink) Playing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.playing && !s.closed
}

func (s *hqpSink) Position() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasPendingSeek {
		return s.pendingSeek
	}
	p := s.pos
	if s.playing && s.hqState == 2 && !s.posAt.IsZero() {
		p += s.backend.nowTime().Sub(s.posAt)
	}
	if s.duration > 0 && p > s.duration {
		p = s.duration
	}
	if p < 0 {
		p = 0
	}
	return p
}

func (s *hqpSink) OnSinkEnd(fn func()) {
	s.mu.Lock()
	s.onEnd = fn
	s.mu.Unlock()
}

func (s *hqpSink) wakeWorker() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *hqpSink) Play() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	now := s.backend.nowTime()
	s.playing = true
	s.endedOnce = false
	s.sawPlaying = false
	s.playRequestedAt = now
	s.posAt = now
	s.playDirty = true
	s.wantPlay = true
	if s.hasPendingSeek {
		s.seekDirty = true
		s.seekTarget = s.pendingSeek
		s.hasPendingSeek = false
	}
	s.mu.Unlock()
	s.wakeWorker()
}

func (s *hqpSink) Pause() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.playing && s.hqState == 2 && !s.posAt.IsZero() {
		s.pos += s.backend.nowTime().Sub(s.posAt)
	}
	if s.seekDirty {
		// Preserve queued seek for next Play() when Pause supersedes dispatch.
		s.pendingSeek = s.seekTarget
		s.hasPendingSeek = true
		s.seekDirty = false
	}
	s.playing = false
	s.hqState = 1
	s.playDirty = true
	s.wantPlay = false
	s.mu.Unlock()
	s.wakeWorker()
}

func (s *hqpSink) Seek(t time.Duration) {
	if t < 0 {
		t = 0
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.pos = t
	s.posAt = s.backend.nowTime()
	s.endedOnce = false
	if !s.playing {
		s.sawPlaying = false
		s.pendingSeek = t
		s.hasPendingSeek = true
		s.seekDirty = false
		s.mu.Unlock()
		return
	}
	// Preserve sawPlaying while already playing so seeking near track end still catches state=0 EOF.
	s.hasPendingSeek = false
	s.seekDirty = true
	s.seekTarget = t
	s.mu.Unlock()
	s.wakeWorker()
}

func (s *hqpSink) SetSinkVolume(v float64) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.volDirty = true
	s.volTarget = v
	s.mu.Unlock()
	s.wakeWorker()
}

// transportWorker executes volume, play/pause, and seek commands strictly in
// causal order while coalescing rapid scrubs or play/pause toggles.
func (s *hqpSink) transportWorker() {
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.wakeCh:
		}
		for {
			s.mu.Lock()
			if s.closed || !s.isCurrent() {
				s.mu.Unlock()
				return
			}
			doVol, vol := s.volDirty, s.volTarget
			s.volDirty = false
			doPlay, wantPlay := s.playDirty, s.wantPlay
			s.playDirty = false
			doSeek, seekTo := s.seekDirty, s.seekTarget
			s.seekDirty = false
			currentlyPlaying := s.playing
			if doSeek && doPlay && !wantPlay {
				s.pendingSeek = seekTo
				s.hasPendingSeek = true
				doSeek = false
			}
			var seekGen uint64
			if doSeek {
				s.seekSeq++
				seekGen = s.seekSeq
				s.seekInFlight = true
				s.sawPlaying = false
				s.baseOffset = seekTo
				s.pos = seekTo
				s.posAt = s.backend.nowTime()
			}
			s.mu.Unlock()

			if !doVol && !doPlay && !doSeek {
				break
			}
			if doVol {
				s.sendVolume(vol)
			}
			if doSeek {
				s.backend.srv.ResetHQPTrackEOF(s.seq)
				ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
				_, _ = s.backend.execForEpoch(ctx, s.epoch, "PlaylistClear", `<PlaylistClear/>`, 4*time.Second)
				addCmd := buildHQPPlaylistAdd(s.wavURL, s.src, s.coverURL, seekTo.Milliseconds(), seekGen)
				_, _ = s.backend.execForEpoch(ctx, s.epoch, "PlaylistAdd", addCmd, 10*time.Second)
				s.mu.Lock()
				s.seekInFlight = false
				s.playRequestedAt = s.backend.nowTime()
				s.posAt = s.playRequestedAt
				s.mu.Unlock()
				shouldPlay := currentlyPlaying
				if doPlay {
					shouldPlay = wantPlay
					doPlay = false
				}
				if shouldPlay {
					_, _ = s.backend.execForEpoch(ctx, s.epoch, "Play", `<Play/>`, 4*time.Second)
				}
				cancel()
			}
			if doPlay {
				ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
				if wantPlay {
					_, _ = s.backend.execForEpoch(ctx, s.epoch, "Play", `<Play/>`, 4*time.Second)
				} else {
					_, _ = s.backend.execForEpoch(ctx, s.epoch, "Pause", `<Pause/>`, 3*time.Second)
				}
				cancel()
			}
		}
	}
}

func (s *hqpSink) sendVolume(gain float64) {
	s.backend.cmdMu.Lock()
	defer s.backend.cmdMu.Unlock()
	if s.ctx.Err() != nil || !s.isCurrent() {
		return
	}
	hc := s.backend.connPtr.Load()
	if hc == nil || hc.closed.Load() {
		return
	}
	db := hc.volMinDB
	if gain > 0.0001 {
		db = 20.0 * math.Log10(gain)
		db = max(hc.volMinDB, min(hc.volMaxDB, db))
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(`<Volume value="%.2f"/>`, db)
	_, _ = s.backend.sendLocked(ctx, hc, "Volume", cmd, 2*time.Second)
}

func (s *hqpSink) updateFromHQP(state int, posSec float64) {
	var fireEnd func()
	now := s.backend.nowTime()
	eofDelivered := s.backend.srv.HQPTrackEOFDelivered(s.seq)
	s.mu.Lock()
	if !s.closed && s.isCurrent() {
		s.hqState = state
		if state == 2 && !s.seekInFlight {
			s.sawPlaying = true
		}
		if !s.hasPendingSeek && !s.seekDirty && !s.seekInFlight && (state == 2 || state == 1) {
			s.pos = s.baseOffset + time.Duration(posSec*float64(time.Second))
			s.posAt = now
		}
		if state == 0 && s.sawPlaying && s.playing && !s.endedOnce && !s.hasPendingSeek && !s.seekDirty && !s.seekInFlight && now.Sub(s.playRequestedAt) > 500*time.Millisecond {
			estPos := s.pos
			if !s.posAt.IsZero() {
				estPos += now.Sub(s.posAt)
			}
			nearEnd := s.duration <= 0 || s.duration <= 5*time.Second || estPos >= s.duration-4*time.Second
			if eofDelivered || nearEnd {
				s.endedOnce = true
				s.playing = false
				s.pos = s.duration
				fireEnd = s.onEnd
			} else {
				// Stopped prematurely (e.g. stream interrupt or manual Stop in HQPlayer):
				// stop playback in place without falsely skipping to the next track.
				s.playing = false
				s.sawPlaying = false
			}
		}
	}
	s.mu.Unlock()
	if fireEnd != nil {
		fireEnd()
	}
}

func (s *hqpSink) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	wasPlaying := s.playing
	s.playing = false
	s.cancel()
	close(s.stopCh)
	s.mu.Unlock()

	s.backend.sinkMu.Lock()
	if s.backend.active == s {
		s.backend.active = nil
	}
	s.backend.sinkMu.Unlock()

	if wasPlaying && s.isCurrent() {
		epoch := s.epoch
		go s.backend.sendStopExisting(epoch)
	}
}
