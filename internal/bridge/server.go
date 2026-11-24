package bridge

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ytb-music/internal/player"
	"ytb-music/internal/ytm"
)

const (
	DefaultListenAddr   = "127.0.0.1:17890"
	DefaultHQPlayerAddr = "127.0.0.1:4321"

	roonTargetRate = 44100
	icyMetaInt     = 16384
)

type hqpTrackEntry struct {
	seq          uint64
	src          player.TrackSource
	eofDelivered atomic.Bool
	coverOnce    sync.Once
	coverData    []byte
}

type streamChunk struct {
	data  []byte
	title string
}

type roonSubscriber struct {
	aac bool
	ch  chan streamChunk
}

// Server serves decoded 24-bit PCM WAV (with HTTP Range seeking) and cover art
// to HQPlayer, plus continuous 44.1kHz 24-bit WAV and ADTS-AAC Live Radio
// streams (with optional ICY track metadata) to Roon Core.
type Server struct {
	mu         sync.RWMutex
	listenAddr string
	lanIP      string
	ln         net.Listener
	httpSrv    *http.Server
	secret     string

	hqpSeq    atomic.Uint64
	hqpTracks map[uint64]*hqpTrackEntry

	hubMu       sync.Mutex
	subs        map[*roonSubscriber]struct{}
	activeSink  *roonSink
	stopClock   chan struct{}
	listeners   atomic.Int32
	onListeners func(int)
}

func NewServer(listenAddr string) (*Server, error) {
	if strings.TrimSpace(listenAddr) == "" {
		listenAddr = DefaultListenAddr
	}
	var tok [16]byte
	_, _ = rand.Read(tok[:])
	s := &Server{
		secret:    hex.EncodeToString(tok[:]),
		hqpTracks: make(map[uint64]*hqpTrackEntry),
		subs:      make(map[*roonSubscriber]struct{}),
		stopClock: make(chan struct{}),
	}
	if err := s.Rebind(listenAddr); err != nil {
		return nil, err
	}
	go s.runRoonClock()
	return s, nil
}

func isWildcardIP(host string) bool {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func hostsOverlap(h1, h2 string) bool {
	if isWildcardIP(h1) || isWildcardIP(h2) || strings.EqualFold(h1, h2) {
		return true
	}
	ip1, ip2 := net.ParseIP(h1), net.ParseIP(h2)
	if ip1 != nil && ip2 != nil {
		return ip1.Equal(ip2)
	}
	return false
}

// Rebind restarts the HTTP listener on a new host:port if changed, handling
// overlapping same-port loopback <-> wildcard transitions and preserving active
// streams when a non-overlapping rebind fails.
func (s *Server) Rebind(listenAddr string) error {
	listenAddr = strings.TrimSpace(listenAddr)
	if listenAddr == "" {
		listenAddr = DefaultListenAddr
	}
	newHost, newPort, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("无效的桥接监听地址 %q: %w", listenAddr, err)
	}
	resAddr, err := net.ResolveTCPAddr("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("无效的桥接监听地址 %q: %w", listenAddr, err)
	}
	resolvedHost := newHost
	if resAddr.IP != nil {
		resolvedHost = resAddr.IP.String()
	}

	s.mu.Lock()
	if s.ln != nil && (s.listenAddr == listenAddr || s.listenAddr == resAddr.String()) {
		s.mu.Unlock()
		return nil
	}
	oldAddr := s.listenAddr
	oldSrv := s.httpSrv
	oldLn := s.ln
	oldHost, oldPort, _ := net.SplitHostPort(oldAddr)
	overlapSamePort := oldLn != nil && newPort != "0" && oldPort == strconv.Itoa(resAddr.Port) && hostsOverlap(oldHost, resolvedHost)
	if overlapSamePort {
		s.httpSrv = nil
		s.ln = nil
	}
	s.mu.Unlock()

	if overlapSamePort {
		if oldLn != nil {
			_ = oldLn.Close()
		}
		if oldSrv != nil {
			_ = oldSrv.Close()
		}
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		if overlapSamePort && oldAddr != "" {
			if restoredLn, rerr := net.Listen("tcp", oldAddr); rerr == nil {
				s.installListener(restoredLn)
			}
		}
		return err
	}

	if !overlapSamePort {
		if oldLn != nil {
			_ = oldLn.Close()
		}
		if oldSrv != nil {
			go func() { _ = oldSrv.Close() }()
		}
	}
	s.installListener(ln)
	return nil
}

func (s *Server) installListener(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/hqp/", s.handleHQP)
	mux.HandleFunc("/roon/live.wav", s.handleRoonWAV)
	mux.HandleFunc("/roon/live.aac", s.handleRoonAAC)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	lanIP := outboundIPFor("")
	s.mu.Lock()
	s.listenAddr = ln.Addr().String()
	s.lanIP = lanIP
	s.ln = ln
	s.httpSrv = srv
	s.mu.Unlock()
	go func() {
		_ = srv.Serve(ln)
	}()
}

// ReleasePort synchronously closes the listening TCP socket so the port can be
// rebound immediately, while active HTTP handlers finish shutting down in Close.
func (s *Server) ReleasePort() {
	select {
	case <-s.stopClock:
	default:
		close(s.stopClock)
	}
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

func (s *Server) Close() error {
	s.ReleasePort()
	s.mu.Lock()
	srv := s.httpSrv
	s.httpSrv = nil
	s.mu.Unlock()
	if srv != nil {
		return srv.Close()
	}
	return nil
}

func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listenAddr
}

func (s *Server) ActiveListeners() int {
	return int(s.listeners.Load())
}

func (s *Server) SetOnListeners(fn func(int)) {
	s.hubMu.Lock()
	s.onListeners = fn
	s.hubMu.Unlock()
}

// BaseURL returns the HTTP origin reachable by peerAddr (e.g. HQPlayer's host:port).
func (s *Server) BaseURL(peerAddr string) string {
	s.mu.RLock()
	addr := s.listenAddr
	cachedLAN := s.lanIP
	s.mu.RUnlock()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + DefaultListenAddr
	}
	ip := net.ParseIP(host)
	if host == "" || host == "0.0.0.0" || host == "::" || (ip != nil && ip.IsUnspecified()) {
		resolved := cachedLAN
		if peerAddr != "" || resolved == "" {
			resolved = outboundIPFor(peerAddr)
		}
		return "http://" + net.JoinHostPort(resolved, port)
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (s *Server) RoonWAVURL(peerAddr string) string {
	return s.BaseURL(peerAddr) + "/roon/live.wav"
}

func (s *Server) RoonAACURL(peerAddr string) string {
	return s.BaseURL(peerAddr) + "/roon/live.aac"
}

func outboundIPFor(peerAddr string) string {
	if peerAddr != "" {
		phost, _, err := net.SplitHostPort(peerAddr)
		if err != nil {
			phost = peerAddr
		}
		if pip := net.ParseIP(phost); pip != nil && pip.IsLoopback() {
			return "127.0.0.1"
		}
		if c, err := net.DialTimeout("udp", net.JoinHostPort(phost, "9003"), 500*time.Millisecond); err == nil {
			defer c.Close()
			if ua, ok := c.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsUnspecified() {
				return ua.IP.String()
			}
		}
	}
	// When bound to 0.0.0.0 without a discovered Core, determine the primary LAN IPv4 address.
	if c, err := net.DialTimeout("udp4", "192.0.2.1:80", 200*time.Millisecond); err == nil {
		defer c.Close()
		if ua, ok := c.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsLoopback() && !ua.IP.IsUnspecified() {
			return ua.IP.String()
		}
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, aerr := iface.Addrs()
			if aerr != nil {
				continue
			}
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP != nil {
					if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
						return ip4.String()
					}
				}
			}
		}
	}
	return "127.0.0.1"
}

// RegisterHQPTrack registers src under a unique monotonic sequence URL so even
// RepeatOne track reloads present a fresh URI to HQPlayer.
func (s *Server) RegisterHQPTrack(src player.TrackSource, peerAddr string) (seq uint64, wavURL, coverURL string) {
	seq = s.hqpSeq.Add(1)
	entry := &hqpTrackEntry{seq: seq, src: src}
	s.mu.Lock()
	s.hqpTracks[seq] = entry
	for k := range s.hqpTracks {
		if seq > 3 && k < seq-2 {
			delete(s.hqpTracks, k)
		}
	}
	secret := s.secret
	s.mu.Unlock()

	base := s.BaseURL(peerAddr)
	wavURL = fmt.Sprintf("%s/hqp/%s/%d.wav", base, secret, seq)
	if src.Track.Album.PicURL != "" {
		coverURL = fmt.Sprintf("%s/hqp/%s/%d/cover.jpg", base, secret, seq)
	}
	return seq, wavURL, coverURL
}

func (s *Server) handleHQP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Expected paths:
	//   /hqp/<secret>/<seq>.wav
	//   /hqp/<secret>/<seq>/cover.jpg
	rest := strings.TrimPrefix(r.URL.Path, "/hqp/")
	parts := strings.Split(rest, "/")
	s.mu.RLock()
	secret := s.secret
	s.mu.RUnlock()
	if len(parts) < 2 || parts[0] != secret {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 3 && parts[2] == "cover.jpg" {
		seq, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.serveHQPCover(w, r, seq)
		return
	}
	if len(parts) == 2 && strings.HasSuffix(parts[1], ".wav") {
		seqStr := strings.TrimSuffix(parts[1], ".wav")
		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.serveHQPWAV(w, r, seq)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) lookupHQP(seq uint64) *hqpTrackEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hqpTracks[seq]
}

// HQPTrackEOFDelivered reports whether the WAV HTTP handler for seq has
// finished streaming the final PCM frame of the track.
func (s *Server) HQPTrackEOFDelivered(seq uint64) bool {
	entry := s.lookupHQP(seq)
	return entry != nil && entry.eofDelivered.Load()
}

// ResetHQPTrackEOF clears the EOF flag before a seek or RepeatOne stream reload.
func (s *Server) ResetHQPTrackEOF(seq uint64) {
	if entry := s.lookupHQP(seq); entry != nil {
		entry.eofDelivered.Store(false)
	}
}

func (s *Server) serveHQPCover(w http.ResponseWriter, r *http.Request, seq uint64) {
	entry := s.lookupHQP(seq)
	if entry == nil || entry.src.Track.Album.PicURL == "" {
		http.NotFound(w, r)
		return
	}
	entry.coverOnce.Do(func() {
		if data, err := ytm.Fetch(ytm.Sized(entry.src.Track.Album.PicURL, 600)); err == nil {
			entry.coverData = data
		}
	})
	if len(entry.coverData) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(entry.coverData)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(entry.coverData)
}

func (s *Server) serveHQPWAV(w http.ResponseWriter, r *http.Request, seq uint64) {
	entry := s.lookupHQP(seq)
	if entry == nil || entry.src.OpenPCM == nil {
		http.NotFound(w, r)
		return
	}
	dec, err := entry.src.OpenPCM()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer dec.Close()

	sr := dec.SampleRate()
	if sr <= 0 {
		sr = 44100
	}
	frames := dec.Len()
	if frames <= 0 {
		frames = int64(entry.src.Duration.Seconds() * float64(sr))
	}
	if frames <= 0 {
		frames = int64(sr * 300)
	}

	var startFrame int64
	if rawMs := r.URL.Query().Get("start_ms"); rawMs != "" {
		if ms, perr := strconv.ParseInt(rawMs, 10, 64); perr == nil && ms > 0 {
			startFrame = min(frames-1, max(0, ms*int64(sr)/1000))
		}
	}

	const (
		channels      = 2
		bytesPerSamp  = 3 // 24-bit signed LE PCM
		blockAlign    = channels * bytesPerSamp
		wavHeaderSize = 44
	)
	remFrames := max(int64(1), frames-startFrame)
	dataBytes := remFrames * blockAlign
	totalSize := wavHeaderSize + dataBytes
	hdr := buildWAVHeader(sr, channels, 24, dataBytes)

	// Serve as a continuous single-connection 200 OK WAV stream without
	// advertising Accept-Ranges. Advertising byte ranges causes HQPlayer 5's
	// clStreamReaderHTTP::RequestOffset() to issue thousands of short-lived
	// Range connections per second and exhaust Windows's ephemeral TCP ports.
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Content-Length", strconv.FormatInt(totalSize, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	written, werr := w.Write(hdr[:])
	if werr != nil || written < wavHeaderSize {
		return
	}
	remaining := dataBytes

	if startFrame > 0 {
		if err := dec.SetPosition(startFrame); err != nil {
			return
		}
	}

	const chunkFrames = 8192
	floatBuf := make([]float32, chunkFrames*channels)
	pcmBuf := make([]byte, chunkFrames*blockAlign)

	for remaining > 0 && r.Context().Err() == nil {
		nFloats, derr := dec.Read(floatBuf)
		nFrames := nFloats / channels
		if nFrames > 0 {
			nBytes := encodePCM24LE(pcmBuf, floatBuf[:nFrames*channels], 1.0)
			slice := pcmBuf[:nBytes]
			if int64(len(slice)) > remaining {
				slice = slice[:remaining]
			}
			if len(slice) > 0 {
				nw, werr := w.Write(slice)
				remaining -= int64(nw)
				if werr != nil {
					return
				}
			}
		}
		if derr != nil {
			// Pad remaining promised Content-Length with digital silence if MP4
			// actual sample count was slightly shorter than header metadata.
			if errors.Is(derr, io.EOF) && remaining > 0 {
				zero := make([]byte, min(remaining, int64(len(pcmBuf))))
				for remaining > 0 {
					step := min(remaining, int64(len(zero)))
					nw, werr := w.Write(zero[:step])
					remaining -= int64(nw)
					if werr != nil || nw == 0 {
						return
					}
				}
			}
			if errors.Is(derr, io.EOF) && remaining == 0 {
				entry.eofDelivered.Store(true)
			}
			return
		}
	}
	if remaining == 0 {
		entry.eofDelivered.Store(true)
	}
}

func buildWAVHeader(sampleRate, channels, bitsPerSample int, dataBytes int64) [44]byte {
	var h [44]byte
	copy(h[0:4], "RIFF")
	var riffLen uint32 = 0xFFFFFFFF
	var dataLen uint32 = 0xFFFFFFFF
	if dataBytes >= 0 && dataBytes+36 <= math.MaxUint32 {
		riffLen = uint32(dataBytes + 36)
		dataLen = uint32(dataBytes)
	}
	binary.LittleEndian.PutUint32(h[4:8], riffLen)
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:28], uint32(sampleRate))
	blockAlign := channels * (bitsPerSample / 8)
	binary.LittleEndian.PutUint32(h[28:32], uint32(sampleRate*blockAlign))
	binary.LittleEndian.PutUint16(h[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[34:36], uint16(bitsPerSample))
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], dataLen)
	return h
}

func encodePCM24LE(dst []byte, samples []float32, gain float64) int {
	g := float32(gain)
	j := 0
	for _, s := range samples {
		v := float64(s * g)
		if v > 1.0 {
			v = 1.0
		} else if v < -1.0 {
			v = -1.0
		}
		iv := int32(math.Round(v * 8388607.0))
		dst[j] = byte(iv)
		dst[j+1] = byte(iv >> 8)
		dst[j+2] = byte(iv >> 16)
		j += 3
	}
	return j
}

// --- Roon Live Radio Continuous Broadcaster (WAV & ADTS-AAC + ICY) ---

func (s *Server) addSubscriber(aac bool) (*roonSubscriber, func()) {
	sub := &roonSubscriber{aac: aac, ch: make(chan streamChunk, 64)}
	s.hubMu.Lock()
	s.subs[sub] = struct{}{}
	cb := s.onListeners
	s.hubMu.Unlock()
	cnt := int(s.listeners.Add(1))
	if cb != nil {
		go cb(cnt)
	}
	return sub, func() {
		s.hubMu.Lock()
		if _, ok := s.subs[sub]; !ok {
			s.hubMu.Unlock()
			return
		}
		delete(s.subs, sub)
		cb := s.onListeners
		s.hubMu.Unlock()
		cnt := int(s.listeners.Add(-1))
		if cb != nil {
			go cb(cnt)
		}
	}
}

func (s *Server) setActiveRoonSink(rs *roonSink) {
	s.hubMu.Lock()
	s.activeSink = rs
	s.hubMu.Unlock()
}

func (s *Server) clearActiveRoonSink(rs *roonSink) {
	s.hubMu.Lock()
	if s.activeSink == rs {
		s.activeSink = nil
	}
	s.hubMu.Unlock()
}

func (s *Server) currentStreamTitle() string {
	s.hubMu.Lock()
	rs := s.activeSink
	s.hubMu.Unlock()
	if rs == nil {
		return "YTB Music"
	}
	return rs.streamTitle()
}

func (s *Server) runRoonClock() {
	const (
		tickDur       = 20 * time.Millisecond
		wavFramesTick = roonTargetRate / 50 // 882 frames per 20ms
		wavBytesTick  = wavFramesTick * 6   // 5292 bytes of 24-bit stereo
	)
	silencePCM := make([]byte, wavBytesTick)
	ticker := time.NewTicker(tickDur)
	defer ticker.Stop()

	var aacAccumNs int64
	lastTick := time.Now()

	for {
		select {
		case <-s.stopClock:
			return
		case now := <-ticker.C:
			dt := now.Sub(lastTick)
			if dt <= 0 || dt > 200*time.Millisecond {
				dt = tickDur
			}
			lastTick = now
			aacAccumNs += dt.Nanoseconds()

			s.hubMu.Lock()
			rs := s.activeSink
			hasWAV, hasAAC := false, false
			for sub := range s.subs {
				if sub.aac {
					hasAAC = true
				} else {
					hasWAV = true
				}
			}
			s.hubMu.Unlock()

			title := "YTB Music"
			var wavPayload []byte
			if rs != nil {
				title = rs.streamTitle()
			}

			// Pull or advance PCM clock when WAV listeners exist or sink is playing without AAC-only mode.
			if rs != nil && (hasWAV || !hasAAC) {
				wavPayload = rs.pullPCM24(wavFramesTick)
			}
			if len(wavPayload) == 0 {
				wavPayload = silencePCM
			}

			var aacFrames [][]byte
			aacRate := roonTargetRate
			silentFrame := silentADTSFrame44kStereo
			if rs != nil {
				if sr, sf := rs.aacFormat(); sr > 0 {
					aacRate = sr
					silentFrame = sf
				}
			}
			frameDurNs := int64(1024*1e9) / int64(aacRate)
			for aacAccumNs >= frameDurNs {
				aacAccumNs -= frameDurNs
				if rs != nil && hasAAC {
					if pkt := rs.pullADTS(!hasWAV); len(pkt) > 0 {
						aacFrames = append(aacFrames, pkt)
						if sr, sf := rs.aacFormat(); sr > 0 && sr != aacRate {
							aacRate = sr
							silentFrame = sf
							frameDurNs = int64(1024*1e9) / int64(aacRate)
						}
						continue
					}
					if sr, sf := rs.aacFormat(); sr > 0 {
						aacRate = sr
						silentFrame = sf
						frameDurNs = int64(1024*1e9) / int64(aacRate)
					}
				}
				if hasAAC {
					aacFrames = append(aacFrames, silentFrame)
				}
			}

			if !hasWAV && !hasAAC {
				continue
			}

			s.hubMu.Lock()
			for sub := range s.subs {
				if !sub.aac {
					select {
					case sub.ch <- streamChunk{data: wavPayload, title: title}:
					default:
					}
				} else {
					for _, pkt := range aacFrames {
						select {
						case sub.ch <- streamChunk{data: pkt, title: title}:
						default:
						}
					}
				}
			}
			s.hubMu.Unlock()
		}
	}
}

func (s *Server) handleRoonWAV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	wantICY := strings.TrimSpace(r.Header.Get("Icy-MetaData")) == "1"
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("icy-name", "YTB Music")
	if wantICY {
		w.Header().Set("icy-metaint", strconv.Itoa(icyMetaInt))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	out := io.Writer(w)
	var icy *icyStreamWriter
	if wantICY {
		icy = newICYStreamWriter(w, icyMetaInt, s.currentStreamTitle())
		out = icy
	}

	// Open-ended 44.1kHz 24-bit stereo WAV header.
	hdr := buildWAVHeader(roonTargetRate, 2, 24, -1)
	if _, err := out.Write(hdr[:]); err != nil {
		return
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}

	sub, remove := s.addSubscriber(false)
	defer remove()

	flusher, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		case chunk := <-sub.ch:
			if icy != nil {
				icy.SetTitle(chunk.title)
			}
			if _, err := out.Write(chunk.data); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) handleRoonAAC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	wantICY := strings.TrimSpace(r.Header.Get("Icy-MetaData")) == "1"
	w.Header().Set("Content-Type", "audio/aacp")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("icy-name", "YTB Music")
	if wantICY {
		w.Header().Set("icy-metaint", strconv.Itoa(icyMetaInt))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	out := io.Writer(w)
	var icy *icyStreamWriter
	if wantICY {
		icy = newICYStreamWriter(w, icyMetaInt, s.currentStreamTitle())
		out = icy
	}

	sub, remove := s.addSubscriber(true)
	defer remove()

	flusher, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		case chunk := <-sub.ch:
			if icy != nil {
				icy.SetTitle(chunk.title)
			}
			if _, err := out.Write(chunk.data); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

type icyStreamWriter struct {
	w         io.Writer
	metaInt   int
	untilMeta int
	curTitle  string
	sentTitle string
}

func newICYStreamWriter(w io.Writer, metaInt int, initTitle string) *icyStreamWriter {
	return &icyStreamWriter{
		w:         w,
		metaInt:   metaInt,
		untilMeta: metaInt,
		curTitle:  initTitle,
	}
}

func (iw *icyStreamWriter) SetTitle(title string) {
	if title != "" {
		iw.curTitle = title
	}
}

func (iw *icyStreamWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		step := min(len(p), iw.untilMeta)
		n, err := iw.w.Write(p[:step])
		total += n
		iw.untilMeta -= n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if iw.untilMeta == 0 {
			if err := iw.writeMetaBlock(); err != nil {
				return total, err
			}
			iw.untilMeta = iw.metaInt
		}
	}
	return total, nil
}

func (iw *icyStreamWriter) writeMetaBlock() error {
	if iw.curTitle == iw.sentTitle {
		_, err := iw.w.Write([]byte{0})
		return err
	}
	clean := strings.ReplaceAll(iw.curTitle, "'", " ")
	meta := fmt.Sprintf("StreamTitle='%s';", clean)
	if len(meta) > 4000 {
		meta = meta[:4000]
	}
	blocks := (len(meta) + 15) / 16
	buf := make([]byte, 1+blocks*16)
	buf[0] = byte(blocks)
	copy(buf[1:], meta)
	if _, err := iw.w.Write(buf); err != nil {
		return err
	}
	iw.sentTitle = iw.curTitle
	return nil
}

// Ensure context import is referenced cleanly.
var _ = context.Background
