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
	seq       uint64
	src       player.TrackSource
	coverOnce sync.Once
	coverData []byte
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

// Rebind restarts the HTTP listener on a new host:port if changed.
func (s *Server) Rebind(listenAddr string) error {
	listenAddr = strings.TrimSpace(listenAddr)
	if listenAddr == "" {
		listenAddr = DefaultListenAddr
	}
	if _, _, err := net.SplitHostPort(listenAddr); err != nil {
		return fmt.Errorf("无效的桥接监听地址 %q: %w", listenAddr, err)
	}

	s.mu.Lock()
	if s.ln != nil && s.listenAddr == listenAddr {
		s.mu.Unlock()
		return nil
	}
	oldSrv := s.httpSrv
	oldLn := s.ln
	s.mu.Unlock()

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
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

	s.mu.Lock()
	s.listenAddr = ln.Addr().String()
	s.ln = ln
	s.httpSrv = srv
	s.mu.Unlock()

	if oldSrv != nil {
		_ = oldSrv.Close()
	} else if oldLn != nil {
		_ = oldLn.Close()
	}

	go func() {
		_ = srv.Serve(ln)
	}()
	return nil
}

func (s *Server) Close() error {
	select {
	case <-s.stopClock:
	default:
		close(s.stopClock)
	}
	s.mu.Lock()
	srv := s.httpSrv
	s.httpSrv = nil
	s.ln = nil
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
	addr := s.Addr()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + DefaultListenAddr
	}
	ip := net.ParseIP(host)
	if host == "" || host == "0.0.0.0" || host == "::" || (ip != nil && ip.IsUnspecified()) {
		resolved := outboundIPFor(peerAddr)
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

	const (
		channels      = 2
		bytesPerSamp  = 3 // 24-bit signed LE PCM
		blockAlign    = channels * bytesPerSamp
		wavHeaderSize = 44
	)
	dataBytes := frames * blockAlign
	totalSize := wavHeaderSize + dataBytes
	hdr := buildWAVHeader(sr, channels, 24, dataBytes)

	start, end := int64(0), totalSize-1
	partial := false
	if rng := r.Header.Get("Range"); rng != "" {
		rs, re, ok := parseHTTPRange(rng, totalSize)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalSize))
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, partial = rs, re, true
	}

	contentLen := end - start + 1
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(contentLen, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}

	remaining := contentLen
	pos := start
	if pos < wavHeaderSize {
		n := min(remaining, wavHeaderSize-pos)
		written, werr := w.Write(hdr[pos : pos+n])
		if werr != nil {
			return
		}
		pos += int64(written)
		remaining -= int64(written)
	}
	if remaining <= 0 {
		return
	}

	pcmByteOffset := pos - wavHeaderSize
	startFrame := pcmByteOffset / blockAlign
	skipFrameBytes := int(pcmByteOffset % blockAlign)
	if startFrame > 0 {
		if err := dec.SetPosition(startFrame); err != nil {
			return
		}
	}

	const chunkFrames = 4096
	floatBuf := make([]float32, chunkFrames*channels)
	pcmBuf := make([]byte, chunkFrames*blockAlign)

	for remaining > 0 && r.Context().Err() == nil {
		nFloats, derr := dec.Read(floatBuf)
		nFrames := nFloats / channels
		if nFrames > 0 {
			nBytes := encodePCM24LE(pcmBuf, floatBuf[:nFrames*channels], 1.0)
			slice := pcmBuf[:nBytes]
			if skipFrameBytes > 0 {
				if skipFrameBytes >= len(slice) {
					skipFrameBytes -= len(slice)
					slice = nil
				} else {
					slice = slice[skipFrameBytes:]
					skipFrameBytes = 0
				}
			}
			if int64(len(slice)) > remaining {
				slice = slice[:remaining]
			}
			if len(slice) > 0 {
				written, werr := w.Write(slice)
				remaining -= int64(written)
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
					written, werr := w.Write(zero[:step])
					remaining -= int64(written)
					if werr != nil || written == 0 {
						return
					}
				}
			}
			return
		}
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

func parseHTTPRange(hdr string, size int64) (start, end int64, ok bool) {
	if size <= 0 || !strings.HasPrefix(hdr, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(hdr, "bytes="))
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = strings.TrimSpace(spec[:i])
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" {
		suffix, err := strconv.ParseInt(b, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true
	}
	s, err := strconv.ParseInt(a, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false
	}
	if b == "" {
		return s, size - 1, true
	}
	e, err := strconv.ParseInt(b, 10, 64)
	if err != nil || e < s {
		return 0, 0, false
	}
	if e >= size {
		e = size - 1
	}
	return s, e, true
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
		tickDur        = 20 * time.Millisecond
		wavFramesTick  = roonTargetRate / 50 // 882 frames per 20ms
		wavBytesTick   = wavFramesTick * 6   // 5292 bytes of 24-bit stereo
		aacFrameDurNs  = int64(1024*1e9) / roonTargetRate
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
			for aacAccumNs >= aacFrameDurNs {
				aacAccumNs -= aacFrameDurNs
				if rs != nil && hasAAC {
					if pkt := rs.pullADTS(!hasWAV); len(pkt) > 0 {
						aacFrames = append(aacFrames, pkt)
						continue
					}
				}
				if hasAAC {
					aacFrames = append(aacFrames, silentADTSFrame44kStereo)
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
