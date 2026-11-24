package bridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
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

// HQPSignalInfo reports live DSP upsampling and modulator telemetry parsed from
// HQPlayer's subscribed <Status> XML frames.
type HQPSignalInfo struct {
	Active        bool
	State         int
	Mode          string // e.g. "SDM (DSD)", "PCM"
	Rate          int    // e.g. 11289600, 22579200, 705600
	Bits          int    // e.g. 1, 20, 24, 32
	Channels      int
	Filter        string // e.g. "poly-sinc-gauss-long"
	Shaper        string // e.g. "ASDM7EC-super 512+fs"
	Clips         int    // limiter/clip counter
	Apod          int    // apodizing counter
	SourceRate    int
	SourceBits    int
	SourceBitrate int // bps from <metadata bitrate="..."/>
	SourceSDM     bool
}

func formatHQPKHz(hz int) string {
	v := strconv.FormatFloat(float64(hz)/1000.0, 'f', 1, 64)
	return strings.TrimSuffix(v, ".0")
}

func hqpDSDName(rate int) string {
	switch rate {
	case 2822400:
		return "DSD64"
	case 5644800:
		return "DSD128"
	case 11289600:
		return "DSD256"
	case 22579200:
		return "DSD512"
	case 45158400:
		return "DSD1024"
	case 90316800:
		return "DSD2048"
	case 3072000:
		return "DSD64x48"
	case 6144000:
		return "DSD128x48"
	case 12288000:
		return "DSD256x48"
	case 24576000:
		return "DSD512x48"
	case 49152000:
		return "DSD1024x48"
	case 98304000:
		return "DSD2048x48"
	}
	if rate >= 2822400 && rate%44100 == 0 {
		return fmt.Sprintf("DSD%d", rate/44100)
	}
	if rate >= 3072000 && rate%48000 == 0 {
		return fmt.Sprintf("DSD%dx48", rate/48000)
	}
	return ""
}

func (s HQPSignalInfo) isSDM() bool {
	m := strings.ToUpper(s.Mode)
	return strings.Contains(m, "SDM") || strings.Contains(m, "DSD") || s.Bits == 1 || s.Rate >= 2822400
}

// OutputBadge returns a concise output format label such as "DSD256", "DSD256x48", or "PCM 705.6k/24b".
func (s HQPSignalInfo) OutputBadge() string {
	if !s.Active || s.Rate <= 0 {
		return ""
	}
	if s.isSDM() {
		if name := hqpDSDName(s.Rate); name != "" {
			return name
		}
		return fmt.Sprintf("SDM %.2fMHz", float64(s.Rate)/1e6)
	}
	if s.Bits > 0 {
		return fmt.Sprintf("PCM %sk/%db", formatHQPKHz(s.Rate), s.Bits)
	}
	return fmt.Sprintf("PCM %sk", formatHQPKHz(s.Rate))
}

// OutputDetail returns a detailed output signal description such as "SDM · DSD256 (11.29 MHz / 1-bit)".
func (s HQPSignalInfo) OutputDetail() string {
	if !s.Active || s.Rate <= 0 {
		return ""
	}
	if s.isSDM() {
		bits := s.Bits
		if bits <= 0 {
			bits = 1
		}
		if name := hqpDSDName(s.Rate); name != "" {
			return fmt.Sprintf("SDM · %s (%.2f MHz / %d-bit)", name, float64(s.Rate)/1e6, bits)
		}
		return fmt.Sprintf("SDM · %.2f MHz / %d-bit", float64(s.Rate)/1e6, bits)
	}
	if s.Bits > 0 {
		return fmt.Sprintf("PCM · %s kHz / %d-bit", formatHQPKHz(s.Rate), s.Bits)
	}
	return fmt.Sprintf("PCM · %s kHz", formatHQPKHz(s.Rate))
}

// SummaryChain summarizes the active HQPlayer output rate, digital filter, and modulator/shaper.
func (s HQPSignalInfo) SummaryChain() string {
	badge := s.OutputBadge()
	if badge == "" {
		return ""
	}
	parts := []string{badge}
	if f := strings.TrimSpace(s.Filter); f != "" {
		parts = append(parts, f)
	}
	if sh := strings.TrimSpace(s.Shaper); sh != "" {
		parts = append(parts, sh)
	}
	return strings.Join(parts, " · ")
}

// HQPlayerBackend delivers native-rate 24-bit WAV (via local file:/// URI on
// the same host, or HTTP 206 byte-range WAV across LAN) to Signalyst HQPlayer
// Desktop / Embedded via its TCP 4321 XML control protocol.
type HQPlayerBackend struct {
	srv       *Server
	now       func() time.Time
	ForceHTTP bool

	cfgMu   sync.RWMutex
	addr    string
	onState func(connected bool, detail string)

	sigMu    sync.RWMutex
	signal   HQPSignalInfo
	onSignal func(HQPSignalInfo)

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
	b.updateSignal(HQPSignalInfo{})
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
		b.updateSignal(HQPSignalInfo{})
		b.emitState(false, "未连接")
	}()
}

// SetOnSignal registers a callback notified whenever HQPlayer's live DSP signal telemetry changes.
func (b *HQPlayerBackend) SetOnSignal(fn func(HQPSignalInfo)) {
	b.sigMu.Lock()
	b.onSignal = fn
	b.sigMu.Unlock()
}

// Signal returns the latest HQPlayer DSP signal telemetry snapshot.
func (b *HQPlayerBackend) Signal() HQPSignalInfo {
	b.sigMu.RLock()
	defer b.sigMu.RUnlock()
	return b.signal
}

func (b *HQPlayerBackend) updateSignal(next HQPSignalInfo) {
	b.sigMu.Lock()
	if b.signal == next {
		b.sigMu.Unlock()
		return
	}
	b.signal = next
	fn := b.onSignal
	b.sigMu.Unlock()
	if fn != nil {
		fn(next)
	}
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
			swapped := b.connPtr.CompareAndSwap(hc, nil)
			hc.waitMu.Lock()
			ch := hc.waitCh
			hc.waitCh = nil
			hc.waitTag = ""
			hc.waitMu.Unlock()
			if ch != nil {
				ch <- xmlReply{err: err}
			}
			if swapped || b.connPtr.Load() == nil {
				b.updateSignal(HQPSignalInfo{})
				b.emitState(false, "连接已断开")
			}
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
			if errText == "" {
				errText = rep.attrs["_text"]
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

func isLocalHQPPeer(peerAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(peerAddr))
	if err != nil {
		host = strings.TrimSpace(peerAddr)
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP != nil && ipn.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func buildHQPPlaylistAdd(uri string, src player.TrackSource, coverURL string, dur time.Duration) string {
	if dur <= 0 {
		dur = src.Duration
	}
	secs := max(1, int(dur.Round(time.Second).Seconds()))
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

func (b *HQPlayerBackend) newActiveSink(epoch, seq uint64, src player.TrackSource, targetURI, wavURL, coverURL string, actualDur time.Duration) *hqpSink {
	sinkCtx, sinkCancel := context.WithCancel(context.Background())
	sink := &hqpSink{
		backend:  b,
		epoch:    epoch,
		seq:      seq,
		src:      src,
		uri:      targetURI,
		wavURL:   wavURL,
		coverURL: coverURL,
		duration: actualDur,
		posAt:    b.nowTime(),
		ctx:      sinkCtx,
		cancel:   sinkCancel,
		wakeCh:   make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
	}
	go sink.transportWorker()
	sink.SetSinkVolume(src.Gain)
	return sink
}

func (b *HQPlayerBackend) Open(ctx context.Context, src player.TrackSource) (player.OutputSink, error) {
	epoch := b.epoch.Add(1)
	addr := b.Addr()

	// Bit-perfect native file pass-through for same-host HQPlayer on supported local files.
	if !b.ForceHTTP && isLocalHQPPeer(addr) && src.NativeHQP && src.LocalPath != "" {
		if st, serr := os.Stat(src.LocalPath); serr == nil && !st.IsDir() {
			if _, err := b.execForEpoch(ctx, epoch, "PlaylistClear", `<PlaylistClear/>`, 5*time.Second); err != nil {
				return nil, err
			}
			targetURI := LocalFileURI(src.LocalPath)
			actualDur := src.Duration
			addCmd := buildHQPPlaylistAdd(targetURI, src, "", actualDur)
			if _, err := b.execForEpoch(ctx, epoch, "PlaylistAdd", addCmd, 25*time.Second); err != nil {
				return nil, fmt.Errorf("HQPlayer 加载音轨失败: %w", err)
			}
			return b.newActiveSink(epoch, 0, src, targetURI, "", "", actualDur), nil
		} else if src.OpenPCM == nil {
			return nil, serr
		}
	}

	// Verify HQPlayer TCP 4321 control connection upfront before decoding PCM.
	if _, err := b.execForEpoch(ctx, epoch, "PlaylistClear", `<PlaylistClear/>`, 5*time.Second); err != nil {
		return nil, err
	}

	seq, fileURI, wavURL, coverURL, actualDur, err := b.srv.PrepareHQPTrack(ctx, src, addr)
	if err != nil {
		return nil, err
	}
	if actualDur <= 0 {
		actualDur = src.Duration
	}

	targetURI := wavURL
	if !b.ForceHTTP && isLocalHQPPeer(addr) && fileURI != "" {
		targetURI = fileURI
	}

	addCmd := buildHQPPlaylistAdd(targetURI, src, coverURL, actualDur)
	if _, err := b.execForEpoch(ctx, epoch, "PlaylistAdd", addCmd, 25*time.Second); err != nil {
		return nil, fmt.Errorf("HQPlayer 加载音轨失败: %w", err)
	}

	return b.newActiveSink(epoch, seq, src, targetURI, wavURL, coverURL, actualDur), nil
}

func firstNonEmptyAttr(attrs map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(attrs[k]); v != "" {
			return v
		}
	}
	return ""
}

func parseHQPBoolAttr(v string) bool {
	v = strings.TrimSpace(v)
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func parseHQPSignal(attrs map[string]string, state int, prev HQPSignalInfo) HQPSignalInfo {
	active := state == 1 || state == 2
	if !active {
		return HQPSignalInfo{Active: false, State: state}
	}

	_, hasRate := attrs["rate"]
	if !hasRate && prev.Active {
		sig := prev
		sig.Active = true
		sig.State = state
		if v := firstNonEmptyAttr(attrs, "clips", "clip"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				sig.Clips = n
			}
		}
		if v := firstNonEmptyAttr(attrs, "apod", "apodizing"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				sig.Apod = n
			}
		}
		if v := firstNonEmptyAttr(attrs, "filter", "active_filter"); v != "" {
			sig.Filter = v
		}
		if v := firstNonEmptyAttr(attrs, "shaper", "modulator", "dither"); v != "" {
			sig.Shaper = v
		}
		if v := firstNonEmptyAttr(attrs, "meta_bitrate", "bitrate", "src_bitrate"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				sig.SourceBitrate = n
			}
		}
		return sig
	}

	rate, _ := strconv.Atoi(strings.TrimSpace(attrs["rate"]))
	bits, _ := strconv.Atoi(strings.TrimSpace(attrs["bits"]))
	channels, _ := strconv.Atoi(strings.TrimSpace(attrs["channels"]))
	clips, _ := strconv.Atoi(firstNonEmptyAttr(attrs, "clips", "clip"))
	apod, _ := strconv.Atoi(firstNonEmptyAttr(attrs, "apod", "apodizing"))
	filter := firstNonEmptyAttr(attrs, "filter", "active_filter")
	shaper := firstNonEmptyAttr(attrs, "shaper", "modulator", "dither")

	srcRate, _ := strconv.Atoi(firstNonEmptyAttr(attrs, "src_rate", "source_rate", "orig_rate", "meta_samplerate", "meta_sample_rate", "meta_rate"))
	srcBits, _ := strconv.Atoi(firstNonEmptyAttr(attrs, "src_bits", "source_bits", "orig_bits", "meta_bits", "meta_bitdepth", "meta_bit_depth"))
	srcBitrate, _ := strconv.Atoi(firstNonEmptyAttr(attrs, "meta_bitrate", "bitrate", "src_bitrate"))
	srcSDM := parseHQPBoolAttr(firstNonEmptyAttr(attrs, "src_sdm", "source_sdm", "orig_sdm", "meta_sdm")) || srcBits == 1 || srcRate >= 2822400

	if prev.Active && prev.Rate == rate {
		if srcBitrate == 0 {
			srcBitrate = prev.SourceBitrate
		}
		if srcRate == 0 {
			srcRate = prev.SourceRate
		}
		if srcBits == 0 {
			srcBits = prev.SourceBits
		}
		if !srcSDM && prev.SourceSDM && firstNonEmptyAttr(attrs, "src_sdm", "source_sdm", "orig_sdm", "meta_sdm") == "" {
			srcSDM = prev.SourceSDM
		}
	}

	isSDM := parseHQPBoolAttr(attrs["sdm"]) || bits == 1 || rate >= 2822400
	mode := strings.TrimSpace(attrs["mode"])
	switch {
	case strings.EqualFold(mode, "sdm") || strings.EqualFold(mode, "dsd") || strings.EqualFold(mode, "sdm (dsd)"):
		mode = "SDM (DSD)"
	case strings.EqualFold(mode, "pcm"):
		mode = "PCM"
	case mode == "":
		if isSDM {
			mode = "SDM (DSD)"
		} else if rate > 0 {
			mode = "PCM"
		}
	}
	if isSDM && bits == 0 {
		bits = 1
	}

	return HQPSignalInfo{
		Active:        true,
		State:         state,
		Mode:          mode,
		Rate:          rate,
		Bits:          bits,
		Channels:      channels,
		Filter:        filter,
		Shaper:        shaper,
		Clips:         clips,
		Apod:          apod,
		SourceRate:    srcRate,
		SourceBits:    srcBits,
		SourceBitrate: srcBitrate,
		SourceSDM:     srcSDM,
	}
}

func (b *HQPlayerBackend) onStatusFrame(attrs map[string]string) {
	state, err := strconv.Atoi(attrs["state"])
	if err != nil {
		return
	}
	posSec, _ := strconv.ParseFloat(attrs["position"], 64)

	b.sigMu.RLock()
	prevSig := b.signal
	b.sigMu.RUnlock()
	b.updateSignal(parseHQPSignal(attrs, state, prevSig))

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
	uri      string
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
	seekDoneAt      time.Time
	lastSeekTarget  time.Duration
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
			wasStopped := s.hqState == 0
			if doSeek && doPlay && !wantPlay {
				s.pendingSeek = seekTo
				s.hasPendingSeek = true
				doSeek = false
			}
			if doSeek {
				now := s.backend.nowTime()
				s.seekInFlight = true
				s.lastSeekTarget = seekTo
				s.seekDoneAt = now
				s.pos = seekTo
				s.posAt = now
			}
			s.mu.Unlock()

			if !doVol && !doPlay && !doSeek {
				break
			}
			if doVol {
				s.sendVolume(vol)
			}
			if doSeek {
				seekSec := max(0, int(seekTo.Round(time.Second).Seconds()))
				seekCmd := fmt.Sprintf(`<Seek position="%d"/>`, seekSec)
				ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
				if doPlay && wantPlay {
					doPlay = false
					if wasStopped && seekSec > 0 {
						// Start track 1 from stopped state first, then seek into it.
						_, _ = s.backend.execForEpoch(ctx, s.epoch, "Play", `<Play/>`, 4*time.Second)
						_, _ = s.backend.execForEpoch(ctx, s.epoch, "Seek", seekCmd, 4*time.Second)
						s.finishSeek(seekTo)
					} else {
						_, _ = s.backend.execForEpoch(ctx, s.epoch, "Seek", seekCmd, 4*time.Second)
						s.finishSeek(seekTo)
						_, _ = s.backend.execForEpoch(ctx, s.epoch, "Play", `<Play/>`, 4*time.Second)
					}
				} else {
					_, _ = s.backend.execForEpoch(ctx, s.epoch, "Seek", seekCmd, 4*time.Second)
					s.finishSeek(seekTo)
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

func (s *hqpSink) finishSeek(seekTo time.Duration) {
	now := s.backend.nowTime()
	s.mu.Lock()
	s.seekInFlight = false
	s.seekDoneAt = now
	s.lastSeekTarget = seekTo
	s.pos = seekTo
	s.posAt = now
	s.mu.Unlock()
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
	s.mu.Lock()
	if !s.closed && s.isCurrent() {
		s.hqState = state
		if state == 2 {
			s.sawPlaying = true
		}
		if !s.hasPendingSeek && !s.seekDirty && !s.seekInFlight && (state == 2 || state == 1) {
			repPos := time.Duration(posSec * float64(time.Second))
			stalePreSeek := !s.seekDoneAt.IsZero() && now.Sub(s.seekDoneAt) < 800*time.Millisecond && absDuration(repPos-s.lastSeekTarget) > 3*time.Second
			if !stalePreSeek {
				s.pos = repPos
				s.posAt = now
			}
		}
		if state == 0 && s.sawPlaying && s.playing && !s.endedOnce && !s.hasPendingSeek && !s.seekDirty && !s.seekInFlight && now.Sub(s.playRequestedAt) > 500*time.Millisecond {
			estPos := s.pos
			if !s.posAt.IsZero() {
				estPos += min(max(time.Duration(0), now.Sub(s.posAt)), 2500*time.Millisecond)
			}
			nearEnd := s.duration <= 0 || s.duration <= 5*time.Second || estPos >= s.duration-5*time.Second
			if nearEnd {
				s.endedOnce = true
				s.playing = false
				s.pos = s.duration
				fireEnd = s.onEnd
			} else {
				// Stopped prematurely (e.g. manual Stop in HQPlayer): stop playback
				// in place without falsely skipping to the next track.
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
