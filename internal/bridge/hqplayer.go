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

	cfgMu   sync.RWMutex
	addr    string
	onState func(connected bool, detail string)

	cmdMu sync.Mutex
	conn  *hqpConn

	sinkMu sync.Mutex
	epoch  atomic.Uint64
	active *hqpSink
}

func NewHQPlayerBackend(srv *Server, addr string, onState func(bool, string)) *HQPlayerBackend {
	return &HQPlayerBackend{
		srv:     srv,
		addr:    normalizeHostPort(addr, DefaultHQPlayerAddr),
		onState: onState,
	}
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
		b.Disconnect()
	}
}

func (b *HQPlayerBackend) Disconnect() {
	b.cmdMu.Lock()
	c := b.conn
	b.conn = nil
	b.cmdMu.Unlock()
	if c != nil {
		c.closed.Store(true)
		_ = c.conn.Close()
	}
	b.emitState(false, "未连接")
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
	if b.conn != nil && !b.conn.closed.Load() {
		return b.conn, nil
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
	_, _ = b.sendLocked(ctx, hc, "Status", `<Status subscribe="1"/>`, 3*time.Second)

	b.conn = hc
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

		// Deliver solicited command response if a waiter matches.
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

		// Always route <Status .../> frames to active sink position/EOF tracking.
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
			return xmlReply{}, rep.err
		}
		if strings.EqualFold(rep.attrs["result"], "Error") || rep.tag == "Error" {
			errText := rep.attrs["value"]
			if errText == "" {
				errText = rep.attrs["error"]
			}
			// HQPlayer returns GetAlbumGain error on <Volume> when playlist is empty.
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

// execWithReconnect runs one command, reconnecting once if an idle TCP socket dropped.
func (b *HQPlayerBackend) execWithReconnect(ctx context.Context, expectTag, xmlBody string, timeout time.Duration) (xmlReply, error) {
	b.cmdMu.Lock()
	defer b.cmdMu.Unlock()
	hc, err := b.ensureConnLocked(ctx)
	if err != nil {
		return xmlReply{}, err
	}
	rep, err := b.sendLocked(ctx, hc, expectTag, xmlBody, timeout)
	if err != nil && ctx.Err() == nil && hc.closed.Load() {
		b.conn = nil
		hc, err = b.ensureConnLocked(ctx)
		if err != nil {
			return xmlReply{}, err
		}
		rep, err = b.sendLocked(ctx, hc, expectTag, xmlBody, timeout)
	}
	return rep, err
}

func (b *HQPlayerBackend) Open(ctx context.Context, src player.TrackSource) (player.OutputSink, error) {
	epoch := b.epoch.Add(1)
	addr := b.Addr()
	_, wavURL, coverURL := b.srv.RegisterHQPTrack(src, addr)

	if _, err := b.execWithReconnect(ctx, "PlaylistClear", `<PlaylistClear/>`, 5*time.Second); err != nil {
		return nil, err
	}

	secs := max(1, int(src.Duration.Round(time.Second).Seconds()))
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

	addCmd := fmt.Sprintf(
		`<PlaylistAdd uri="%s" queued="0" clear="0" start="0" freewheel="1">%s</PlaylistAdd>`,
		xmlEscapeAttr(wavURL),
		meta.String(),
	)
	if _, err := b.execWithReconnect(ctx, "PlaylistAdd", addCmd, 25*time.Second); err != nil {
		return nil, fmt.Errorf("HQPlayer 加载音轨失败: %w", err)
	}

	sink := &hqpSink{
		backend:  b,
		epoch:    epoch,
		duration: src.Duration,
		posAt:    time.Now(),
		volCh:    make(chan float64, 1),
		stopCh:   make(chan struct{}),
	}
	b.sinkMu.Lock()
	b.active = sink
	b.sinkMu.Unlock()

	go sink.volWorker()
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
	duration time.Duration

	mu              sync.Mutex
	closed          bool
	playing         bool
	hqState         int
	sawPlaying      bool
	playRequestedAt time.Time
	pos             time.Duration
	posAt           time.Time
	pendingSeek     time.Duration
	hasPendingSeek  bool
	onEnd           func()
	endedOnce       bool

	volCh  chan float64
	stopCh chan struct{}
}

func (s *hqpSink) isCurrent() bool {
	return s.backend.epoch.Load() == s.epoch
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
		p += time.Since(s.posAt)
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

func (s *hqpSink) Play() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.playing = true
	s.playRequestedAt = time.Now()
	s.posAt = time.Now()
	seek := s.pendingSeek
	doSeek := s.hasPendingSeek
	s.hasPendingSeek = false
	s.mu.Unlock()

	go func() {
		if !s.isCurrent() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_, err := s.backend.execWithReconnect(ctx, "Play", `<Play/>`, 5*time.Second)
		if err == nil && doSeek && seek > 0 && s.isCurrent() {
			cmd := fmt.Sprintf(`<Seek position="%.2f"/>`, seek.Seconds())
			_, _ = s.backend.execWithReconnect(ctx, "Seek", cmd, 5*time.Second)
		}
	}()
}

func (s *hqpSink) Pause() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.playing && s.hqState == 2 && !s.posAt.IsZero() {
		s.pos += time.Since(s.posAt)
	}
	s.playing = false
	s.hqState = 1
	s.mu.Unlock()

	go func() {
		if !s.isCurrent() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_, _ = s.backend.execWithReconnect(ctx, "Pause", `<Pause/>`, 3*time.Second)
	}()
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
	s.posAt = time.Now()
	if !s.playing {
		s.pendingSeek = t
		s.hasPendingSeek = true
		s.mu.Unlock()
		return
	}
	s.hasPendingSeek = false
	s.mu.Unlock()

	go func() {
		if !s.isCurrent() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := fmt.Sprintf(`<Seek position="%.2f"/>`, t.Seconds())
		_, _ = s.backend.execWithReconnect(ctx, "Seek", cmd, 4*time.Second)
	}()
}

func (s *hqpSink) SetSinkVolume(v float64) {
	select {
	case s.volCh <- v:
	default:
		select {
		case <-s.volCh:
		default:
		}
		select {
		case s.volCh <- v:
		default:
		}
	}
}

func (s *hqpSink) volWorker() {
	for {
		select {
		case <-s.stopCh:
			return
		case g := <-s.volCh:
			if !s.isCurrent() {
				return
			}
			s.sendVolume(g)
		}
	}
}

func (s *hqpSink) sendVolume(gain float64) {
	s.backend.cmdMu.Lock()
	defer s.backend.cmdMu.Unlock()
	hc := s.backend.conn
	if hc == nil || hc.closed.Load() || !s.isCurrent() {
		return
	}
	db := hc.volMinDB
	if gain > 0.0001 {
		db = 20.0 * math.Log10(gain)
		db = max(hc.volMinDB, min(hc.volMaxDB, db))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(`<Volume value="%.2f"/>`, db)
	_, _ = s.backend.sendLocked(ctx, hc, "Volume", cmd, 2*time.Second)
}

func (s *hqpSink) updateFromHQP(state int, posSec float64) {
	var fireEnd func()
	s.mu.Lock()
	if !s.closed && s.isCurrent() {
		s.hqState = state
		if state == 2 {
			s.sawPlaying = true
		}
		if !s.hasPendingSeek && (state == 2 || state == 1) {
			s.pos = time.Duration(posSec * float64(time.Second))
			s.posAt = time.Now()
		}
		if state == 0 && s.sawPlaying && s.playing && !s.endedOnce && time.Since(s.playRequestedAt) > 500*time.Millisecond {
			s.endedOnce = true
			s.playing = false
			s.pos = s.duration
			fireEnd = s.onEnd
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
	close(s.stopCh)
	s.mu.Unlock()

	s.backend.sinkMu.Lock()
	if s.backend.active == s {
		s.backend.active = nil
	}
	s.backend.sinkMu.Unlock()

	// Only send <Stop/> if no newer track superseded this sink.
	if wasPlaying && s.isCurrent() {
		go func() {
			if !s.isCurrent() {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = s.backend.execWithReconnect(ctx, "Stop", `<Stop/>`, 2*time.Second)
		}()
	}
}
