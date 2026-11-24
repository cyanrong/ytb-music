package bridge

import (
	"bufio"
	"bytes"
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
	"net/url"
	"os"
	"path/filepath"
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
	wavPath   string
	modTime   time.Time
	prog      *progressiveWAV
	coverOnce sync.Once
	coverData []byte
}

type progressiveWAV struct {
	mu        sync.Mutex
	cond      sync.Cond
	totalSize int64
	written   int64
	done      bool
	err       error
	cancel    context.CancelFunc
	doneCh    chan struct{}
}

func newProgressiveWAV(totalSize, written int64, cancel context.CancelFunc) *progressiveWAV {
	p := &progressiveWAV{
		totalSize: totalSize,
		written:   written,
		cancel:    cancel,
		doneCh:    make(chan struct{}),
	}
	p.cond.L = &p.mu
	return p
}

func (p *progressiveWAV) advance(written int64) {
	p.mu.Lock()
	p.written = written
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *progressiveWAV) finish(err error) {
	p.mu.Lock()
	p.done = true
	if p.err == nil && err != nil {
		p.err = err
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *progressiveWAV) failureState() (failed bool, written int64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done && p.err != nil {
		return true, p.written, p.err
	}
	return false, p.written, nil
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
	tempRoot  string
	hqpDir    string
	hqpLock   *os.File
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
	hqpDir := s.hqpDir
	s.hqpDir = ""
	hqpLock := s.hqpLock
	s.hqpLock = nil
	var progs []*progressiveWAV
	for _, e := range s.hqpTracks {
		if e != nil && e.prog != nil {
			e.prog.cancel()
			progs = append(progs, e.prog)
		}
	}
	clear(s.hqpTracks)
	s.mu.Unlock()

	var err error
	if srv != nil {
		err = srv.Close()
	}
	for _, p := range progs {
		<-p.doneCh
	}
	if hqpLock != nil {
		_ = hqpLock.Close()
	}
	if hqpDir != "" {
		_ = os.RemoveAll(hqpDir)
	}
	return err
}

func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listenAddr
}

func (s *Server) ActiveListeners() int {
	return int(s.listeners.Load())
}

// HasOnlyAACListeners reports whether Roon Live Radio currently has at least
// one connected ADTS-AAC listener and zero WAV listeners.
func (s *Server) HasOnlyAACListeners() bool {
	if s == nil {
		return false
	}
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	hasWAV, hasAAC := false, false
	for sub := range s.subs {
		if sub.aac {
			hasAAC = true
		} else {
			hasWAV = true
		}
	}
	return hasAAC && !hasWAV
}

func (s *Server) activeRoonSinkHasAAC(rs *roonSink) bool {
	if s == nil || rs == nil {
		return false
	}
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.activeSink != rs {
		return false
	}
	for sub := range s.subs {
		if sub.aac {
			return true
		}
	}
	return false
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

func scavengeHQPDirs(root string, now time.Time) {
	if root == "" {
		root = os.TempDir()
	}
	matches, err := filepath.Glob(filepath.Join(root, "ytb-music-hqp-*"))
	if err != nil {
		return
	}
	for _, old := range matches {
		info, serr := os.Stat(old)
		if serr != nil || !info.IsDir() || now.Sub(info.ModTime()) <= 6*time.Hour {
			continue
		}
		if removeLockFile(filepath.Join(old, ".lock")) == nil {
			_ = os.RemoveAll(old)
		}
	}
}

func (s *Server) ensureHQPDir() (string, error) {
	s.mu.Lock()
	if s.hqpDir != "" {
		dir := s.hqpDir
		s.mu.Unlock()
		return dir, nil
	}
	root := s.tempRoot
	s.mu.Unlock()

	if root == "" {
		root = os.TempDir()
	}
	scavengeHQPDirs(root, time.Now())

	dir, err := os.MkdirTemp(root, "ytb-music-hqp-*")
	if err != nil {
		return "", err
	}
	lf, err := openExclusiveLockFile(filepath.Join(dir, ".lock"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}

	s.mu.Lock()
	if s.hqpDir != "" {
		existing := s.hqpDir
		s.mu.Unlock()
		_ = lf.Close()
		_ = os.RemoveAll(dir)
		return existing, nil
	}
	s.hqpDir = dir
	s.hqpLock = lf
	s.mu.Unlock()
	return dir, nil
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func escapeFileSegments(slashPath string) string {
	if slashPath == "" {
		return ""
	}
	parts := strings.Split(slashPath, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

func localFileURI(rawPath string) string {
	if rawPath == "" {
		return "file:///"
	}
	p := rawPath
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`) || strings.HasPrefix(p, `\\.\UNC\`) ||
		strings.HasPrefix(p, `//?/UNC/`) || strings.HasPrefix(p, `//./UNC/`):
		p = `\\` + p[8:]
	case strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) ||
		strings.HasPrefix(p, `//?/`) || strings.HasPrefix(p, `//./`):
		p = p[4:]
	}

	isUNC := len(p) >= 2 && (p[0] == '\\' || p[0] == '/') && (p[1] == '\\' || p[1] == '/') && (len(p) == 2 || (p[2] != '\\' && p[2] != '/'))
	if isUNC {
		s := strings.ReplaceAll(p, `\`, `/`)
		rest := strings.TrimPrefix(s, "//")
		host, rem, ok := strings.Cut(rest, "/")
		if !ok || rem == "" {
			return "file://" + host + "/"
		}
		return "file://" + host + "/" + escapeFileSegments(rem)
	}

	s := strings.ReplaceAll(p, `\`, `/`)
	hasDrive := len(s) >= 2 && isASCIIAlpha(s[0]) && s[1] == ':'
	isPOSIXAbs := strings.HasPrefix(p, "/")

	if !hasDrive && !isPOSIXAbs {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
			s = strings.ReplaceAll(p, `\`, `/`)
			hasDrive = len(s) >= 2 && isASCIIAlpha(s[0]) && s[1] == ':'
			isPOSIXAbs = strings.HasPrefix(p, "/")
		}
	}

	if hasDrive {
		drive := s[:2]
		rem := strings.TrimPrefix(s[2:], "/")
		if rem == "" {
			return "file:///" + drive + "/"
		}
		return "file:///" + drive + "/" + escapeFileSegments(rem)
	}

	rem := strings.TrimPrefix(p, "/")
	if rem == "" {
		return "file:///"
	}
	return "file:///" + escapeFileSegments(rem)
}

// LocalFileURI formats an absolute local filesystem path as an RFC 8089 file URI for HQPlayer.
func LocalFileURI(absPath string) string {
	return localFileURI(absPath)
}

func isHTTPURL(u string) bool {
	u = strings.TrimSpace(u)
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// RegisterHQPCover registers artwork for a native file:/// HQPlayer track so
// HQPlayer can fetch local or HTTP cover art without materializing a WAV file.
func (s *Server) RegisterHQPCover(src player.TrackSource, peerAddr string) (uint64, string) {
	if s == nil {
		return 0, ""
	}
	var coverData []byte
	if len(src.CoverBytes) > 0 {
		coverData = append([]byte(nil), src.CoverBytes...)
	}
	if len(coverData) == 0 && !isHTTPURL(src.Track.Album.PicURL) {
		return 0, ""
	}
	seq := s.hqpSeq.Add(1)
	entry := &hqpTrackEntry{
		seq:       seq,
		src:       src,
		modTime:   time.Now(),
		coverData: coverData,
	}
	if len(coverData) > 0 {
		entry.coverOnce.Do(func() {})
	}
	secret := s.registerHQPEntry(seq, entry)
	return seq, fmt.Sprintf("%s/hqp/%s/%d/cover.jpg", s.BaseURL(peerAddr), secret, seq)
}

func (s *Server) registerHQPEntry(seq uint64, entry *hqpTrackEntry) string {
	var evicted []*hqpTrackEntry
	s.mu.Lock()
	s.hqpTracks[seq] = entry
	for k, old := range s.hqpTracks {
		if seq > 3 && k < seq-2 {
			evicted = append(evicted, old)
			delete(s.hqpTracks, k)
		}
	}
	secret := s.secret
	s.mu.Unlock()

	if len(evicted) > 0 {
		go func(items []*hqpTrackEntry) {
			for _, old := range items {
				if old == nil {
					continue
				}
				if old.prog != nil {
					old.prog.cancel()
					<-old.prog.doneCh
				}
				if old.wavPath != "" {
					_ = os.Remove(old.wavPath)
				}
			}
		}(evicted)
	}
	return secret
}

func finalizeHQPWAVHeader(w io.WriterAt, sampleRate int, id3Chunk []byte, pcmBytes int64) error {
	hdr := encodeWAVContainerHeader(sampleRate, 2, 24, id3Chunk, pcmBytes)
	_, err := w.WriteAt(hdr, 0)
	return err
}

// PrepareHQPTrack prepares a 24-bit stereo WAV/RF64 file with UTF-8 ID3v2.4
// tags and artwork placed before the "data" chunk. Cached and local tracks are
// fully materialized so same-host HQPlayer opens fileURI directly via native
// file I/O, while active network downloads pre-buffer ~256 KB and stream
// progressively over HTTP 206 byte ranges without waiting for full download.
func (s *Server) PrepareHQPTrack(ctx context.Context, src player.TrackSource, peerAddr string) (seq uint64, fileURI, wavURL, coverURL string, actualDur time.Duration, err error) {
	if src.OpenPCM == nil {
		return 0, "", "", "", 0, errors.New("bridge: missing PCM decoder")
	}
	hqpDir, err := s.ensureHQPDir()
	if err != nil {
		return 0, "", "", "", 0, err
	}

	var coverData []byte
	var coverCh chan []byte
	if len(src.CoverBytes) > 0 {
		coverData = append([]byte(nil), src.CoverBytes...)
	} else if picURL := strings.TrimSpace(src.Track.Album.PicURL); isHTTPURL(picURL) {
		coverCh = make(chan []byte, 1)
		go func() {
			data, ferr := ytm.Fetch(ytm.Sized(picURL, 600))
			if ferr == nil && len(data) > 0 {
				coverCh <- data
				return
			}
			coverCh <- nil
		}()
	}

	dec, err := src.OpenPCM()
	if err != nil {
		return 0, "", "", "", 0, err
	}
	var closeDecOnce sync.Once
	closeDec := func() {
		closeDecOnce.Do(func() {
			_ = dec.Close()
		})
	}
	stopPrepCancel := func() bool { return false }
	if src.Downloading {
		stopPrepCancel = context.AfterFunc(ctx, closeDec)
	}

	sr := dec.SampleRate()
	if sr <= 0 {
		sr = 44100
	}

	const (
		channels     = 2
		bytesPerSamp = 3 // 24-bit signed LE PCM
		blockAlign   = channels * bytesPerSamp
		chunkFrames  = 16384
		prebufTarget = 43692 * blockAlign // ~256 KB, exact multiple of blockAlign
	)

	// Pre-buffer up to ~256 KB of 24-bit PCM before deciding between full file
	// materialization and progressive HTTP Range streaming.
	preBuf := make([]byte, 0, prebufTarget)
	floatBuf := make([]float32, chunkFrames*channels)
	pcmBuf := make([]byte, chunkFrames*blockAlign)
	hitEOF := false
	for len(preBuf) < prebufTarget {
		if cerr := ctx.Err(); cerr != nil {
			stopPrepCancel()
			closeDec()
			return 0, "", "", "", 0, cerr
		}
		remFrames := (prebufTarget - len(preBuf)) / blockAlign
		readFrames := min(chunkFrames, max(1, remFrames))
		nFloats, derr := dec.Read(floatBuf[:readFrames*channels])
		if cerr := ctx.Err(); cerr != nil {
			stopPrepCancel()
			closeDec()
			return 0, "", "", "", 0, cerr
		}
		nFrames := nFloats / channels
		if nFrames > 0 {
			nBytes := encodePCM24LE(pcmBuf, floatBuf[:nFrames*channels], 1.0)
			preBuf = append(preBuf, pcmBuf[:nBytes]...)
		}
		if derr != nil {
			if errors.Is(derr, io.EOF) {
				hitEOF = true
				break
			}
			stopPrepCancel()
			closeDec()
			if cerr := ctx.Err(); cerr != nil {
				return 0, "", "", "", 0, cerr
			}
			return 0, "", "", "", 0, derr
		}
		if nFloats == 0 {
			hitEOF = true
			break
		}
	}

	var estimatedFrames int64
	if frames := dec.Len(); frames > 0 {
		estimatedFrames = frames
	} else if src.Duration > 0 {
		estimatedFrames = int64(math.Round(src.Duration.Seconds() * float64(sr)))
	}
	estimatedDataBytes := estimatedFrames * blockAlign
	prebufferedBytes := int64(len(preBuf))
	progressive := src.Downloading && !hitEOF && estimatedFrames > 0 && estimatedDataBytes > prebufferedBytes
	if coverCh != nil {
		coverWait := 1500 * time.Millisecond
		if progressive {
			coverWait = 120 * time.Millisecond
		}
		timer := time.NewTimer(coverWait)
		select {
		case coverData = <-coverCh:
		case <-ctx.Done():
			timer.Stop()
			stopPrepCancel()
			closeDec()
			return 0, "", "", "", 0, ctx.Err()
		case <-timer.C:
		}
		timer.Stop()
	}

	id3Chunk := buildWAVID3Chunk(src.Track.Name, src.Track.ArtistNames(), src.Track.Album.Name, coverData)

	seq = s.hqpSeq.Add(1)
	wavPath := filepath.Join(hqpDir, fmt.Sprintf("track-%d.wav", seq))
	f, err := os.OpenFile(wavPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		stopPrepCancel()
		closeDec()
		return 0, "", "", "", 0, err
	}
	cleanupSyncErr := func(cerr error) (uint64, string, string, string, time.Duration, error) {
		stopPrepCancel()
		closeDec()
		_ = f.Close()
		_ = os.Remove(wavPath)
		return 0, "", "", "", 0, cerr
	}

	if !progressive {
		bw := bufio.NewWriterSize(f, 256<<10)
		placeholderHdr := encodeWAVContainerHeader(sr, channels, 24, id3Chunk, 0)
		if _, err := bw.Write(placeholderHdr); err != nil {
			return cleanupSyncErr(err)
		}
		var pcmBytes int64
		if len(preBuf) > 0 {
			if _, err := bw.Write(preBuf); err != nil {
				return cleanupSyncErr(err)
			}
			pcmBytes += int64(len(preBuf))
		}
		for !hitEOF {
			if cerr := ctx.Err(); cerr != nil {
				return cleanupSyncErr(cerr)
			}
			nFloats, derr := dec.Read(floatBuf)
			if cerr := ctx.Err(); cerr != nil {
				return cleanupSyncErr(cerr)
			}
			nFrames := nFloats / channels
			if nFrames > 0 {
				nBytes := encodePCM24LE(pcmBuf, floatBuf[:nFrames*channels], 1.0)
				if _, werr := bw.Write(pcmBuf[:nBytes]); werr != nil {
					return cleanupSyncErr(werr)
				}
				pcmBytes += int64(nBytes)
			}
			if derr != nil {
				if errors.Is(derr, io.EOF) {
					break
				}
				if cerr := ctx.Err(); cerr != nil {
					return cleanupSyncErr(cerr)
				}
				return cleanupSyncErr(derr)
			}
			if nFloats == 0 {
				break
			}
		}
		stopPrepCancel()
		closeDec()

		if pcmBytes == 0 {
			var silentFrame [blockAlign]byte
			if _, werr := bw.Write(silentFrame[:]); werr != nil {
				_ = f.Close()
				_ = os.Remove(wavPath)
				return 0, "", "", "", 0, werr
			}
			pcmBytes = blockAlign
		}
		if err := bw.Flush(); err != nil {
			_ = f.Close()
			_ = os.Remove(wavPath)
			return 0, "", "", "", 0, err
		}
		if err := finalizeHQPWAVHeader(f, sr, id3Chunk, pcmBytes); err != nil {
			_ = f.Close()
			_ = os.Remove(wavPath)
			return 0, "", "", "", 0, err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(wavPath)
			return 0, "", "", "", 0, err
		}

		entry := &hqpTrackEntry{
			seq:       seq,
			src:       src,
			wavPath:   wavPath,
			modTime:   time.Now(),
			coverData: coverData,
		}
		if len(coverData) > 0 {
			entry.coverOnce.Do(func() {})
		}
		secret := s.registerHQPEntry(seq, entry)

		actualFrames := pcmBytes / blockAlign
		actualDur = time.Duration(float64(actualFrames) / float64(sr) * float64(time.Second))
		if actualDur <= 0 {
			actualDur = src.Duration
		}
		base := s.BaseURL(peerAddr)
		fileURI = localFileURI(wavPath)
		wavURL = fmt.Sprintf("%s/hqp/%s/%d.wav", base, secret, seq)
		if len(coverData) > 0 || isHTTPURL(src.Track.Album.PicURL) {
			coverURL = fmt.Sprintf("%s/hqp/%s/%d/cover.jpg", base, secret, seq)
		}
		return seq, fileURI, wavURL, coverURL, actualDur, nil
	}

	// Progressive streaming path: write upfront RIFF/RF64 header for estimatedDataBytes
	// and continue decoding in a background goroutine while serving HTTP 206 byte ranges.
	hdr := encodeWAVContainerHeader(sr, channels, 24, id3Chunk, estimatedDataBytes)
	if _, err := f.Write(hdr); err != nil {
		return cleanupSyncErr(err)
	}
	if len(preBuf) > 0 {
		if _, err := f.Write(preBuf); err != nil {
			return cleanupSyncErr(err)
		}
	}
	stopPrepCancel()

	written := int64(len(hdr)) + int64(len(preBuf))
	totalSize := int64(len(hdr)) + estimatedDataBytes
	bgCtx, bgCancel := context.WithCancel(context.Background())
	prog := newProgressiveWAV(totalSize, written, bgCancel)

	entry := &hqpTrackEntry{
		seq:       seq,
		src:       src,
		wavPath:   wavPath,
		modTime:   time.Now(),
		prog:      prog,
		coverData: coverData,
	}
	if len(coverData) > 0 {
		entry.coverOnce.Do(func() {})
	}
	secret := s.registerHQPEntry(seq, entry)

	go func() {
		stopCancel := context.AfterFunc(bgCtx, closeDec)
		defer close(prog.doneCh)
		defer f.Close()
		defer closeDec()
		defer stopCancel()

		bgFloats := make([]float32, chunkFrames*channels)
		bgPCM := make([]byte, chunkFrames*blockAlign)
		cur := written
		for cur < totalSize {
			if cerr := bgCtx.Err(); cerr != nil {
				prog.finish(cerr)
				return
			}
			nFloats, derr := dec.Read(bgFloats)
			if cerr := bgCtx.Err(); cerr != nil {
				prog.finish(cerr)
				return
			}
			nFrames := nFloats / channels
			if nFrames > 0 {
				nBytes := encodePCM24LE(bgPCM, bgFloats[:nFrames*channels], 1.0)
				if rem := totalSize - cur; int64(nBytes) > rem {
					nBytes = int(rem)
				}
				if nBytes > 0 {
					if _, werr := f.Write(bgPCM[:nBytes]); werr != nil {
						prog.finish(werr)
						return
					}
					cur += int64(nBytes)
					prog.advance(cur)
				}
			}
			if derr != nil {
				if errors.Is(derr, io.EOF) {
					break
				}
				prog.finish(derr)
				return
			}
			if nFloats == 0 {
				break
			}
		}

		// Bound EOF zero-padding strictly to minor codec/priming rounding discrepancies (<= 2s).
		if cur < totalSize {
			if cerr := bgCtx.Err(); cerr != nil {
				prog.finish(cerr)
				return
			}
			writtenPCM := cur - int64(len(hdr))
			shortfall := estimatedDataBytes - writtenPCM
			maxPad := int64(sr) * blockAlign * 2
			if shortfall > maxPad {
				prog.finish(io.ErrUnexpectedEOF)
				return
			}
			zeroBuf := make([]byte, 64<<10)
			for cur < totalSize {
				if cerr := bgCtx.Err(); cerr != nil {
					prog.finish(cerr)
					return
				}
				step := int(min(int64(len(zeroBuf)), totalSize-cur))
				if _, werr := f.Write(zeroBuf[:step]); werr != nil {
					prog.finish(werr)
					return
				}
				cur += int64(step)
				prog.advance(cur)
			}
		}
		prog.finish(nil)
	}()

	actualDur = time.Duration(float64(estimatedFrames) / float64(sr) * float64(time.Second))
	if actualDur <= 0 {
		actualDur = src.Duration
	}
	base := s.BaseURL(peerAddr)
	wavURL = fmt.Sprintf("%s/hqp/%s/%d.wav", base, secret, seq)
	if len(coverData) > 0 || isHTTPURL(src.Track.Album.PicURL) {
		coverURL = fmt.Sprintf("%s/hqp/%s/%d/cover.jpg", base, secret, seq)
	}
	return seq, "", wavURL, coverURL, actualDur, nil
}

func detectCoverMIME(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 4 && string(b[:4]) == "GIF8":
		return "image/gif"
	default:
		return "image/jpeg"
	}
}

func id3Syncsafe(n int) [4]byte {
	return [4]byte{
		byte((n >> 21) & 0x7F),
		byte((n >> 14) & 0x7F),
		byte((n >> 7) & 0x7F),
		byte(n & 0x7F),
	}
}

func appendID3v24Frame(buf *bytes.Buffer, id string, payload []byte) {
	if len(payload) == 0 {
		return
	}
	buf.WriteString(id)
	sz := id3Syncsafe(len(payload))
	buf.Write(sz[:])
	buf.Write([]byte{0, 0})
	buf.Write(payload)
}

func appendID3v24Text(buf *bytes.Buffer, id, val string) {
	val = strings.TrimSpace(val)
	if val == "" {
		return
	}
	var p bytes.Buffer
	p.WriteByte(3) // UTF-8 without BOM
	p.WriteString(val)
	appendID3v24Frame(buf, id, p.Bytes())
}

func buildWAVID3Chunk(title, artist, album string, coverData []byte) []byte {
	var frames bytes.Buffer
	appendID3v24Text(&frames, "TIT2", title)
	appendID3v24Text(&frames, "TPE1", artist)
	appendID3v24Text(&frames, "TALB", album)
	if len(coverData) > 0 {
		mime := detectCoverMIME(coverData)
		var apic bytes.Buffer
		apic.WriteByte(0) // ISO-8859-1 description
		apic.WriteString(mime)
		apic.WriteByte(0)
		apic.WriteByte(3) // Front cover
		apic.WriteByte(0) // empty description + NUL
		apic.Write(coverData)
		appendID3v24Frame(&frames, "APIC", apic.Bytes())
	}
	if frames.Len() == 0 {
		return nil
	}

	var id3 bytes.Buffer
	id3.WriteString("ID3")
	id3.Write([]byte{4, 0, 0}) // ID3v2.4.0
	sz := id3Syncsafe(frames.Len())
	id3.Write(sz[:])
	id3.Write(frames.Bytes())

	body := id3.Bytes()
	var chunk bytes.Buffer
	chunk.WriteString("id3 ")
	var lenBytes [4]byte
	binary.LittleEndian.PutUint32(lenBytes[:], uint32(len(body)))
	chunk.Write(lenBytes[:])
	chunk.Write(body)
	if len(body)%2 == 1 {
		chunk.WriteByte(0)
	}
	return chunk.Bytes()
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

// HQPWAVURL returns the HTTP WAV URL registered for seq, or "".
func (s *Server) HQPWAVURL(seq uint64, peerAddr string) string {
	if s.lookupHQP(seq) == nil {
		return ""
	}
	s.mu.RLock()
	secret := s.secret
	s.mu.RUnlock()
	return fmt.Sprintf("%s/hqp/%s/%d.wav", s.BaseURL(peerAddr), secret, seq)
}

func (s *Server) serveHQPCover(w http.ResponseWriter, r *http.Request, seq uint64) {
	entry := s.lookupHQP(seq)
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	entry.coverOnce.Do(func() {
		if len(entry.coverData) == 0 && len(entry.src.CoverBytes) > 0 {
			entry.coverData = append([]byte(nil), entry.src.CoverBytes...)
		}
		if len(entry.coverData) == 0 && isHTTPURL(entry.src.Track.Album.PicURL) {
			if data, err := ytm.Fetch(ytm.Sized(entry.src.Track.Album.PicURL, 600)); err == nil {
				entry.coverData = data
			}
		}
	})
	if len(entry.coverData) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", detectCoverMIME(entry.coverData))
	w.Header().Set("Content-Length", strconv.Itoa(len(entry.coverData)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(entry.coverData)
}

type progressiveWAVReader struct {
	f    *os.File
	prog *progressiveWAV
	ctx  context.Context
	pos  int64
}

func (r *progressiveWAVReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.prog.totalSize + offset
	default:
		return 0, errors.New("bridge: invalid seek whence")
	}
	if next < 0 {
		return 0, errors.New("bridge: negative seek position")
	}
	r.pos = next
	return next, nil
}

func (r *progressiveWAVReader) Read(p []byte) (int, error) {
	if r.pos >= r.prog.totalSize {
		return 0, io.EOF
	}
	if rem := r.prog.totalSize - r.pos; int64(len(p)) > rem {
		p = p[:rem]
	}
	if len(p) == 0 {
		return 0, nil
	}

	stopWake := context.AfterFunc(r.ctx, func() {
		r.prog.mu.Lock()
		r.prog.cond.Broadcast()
		r.prog.mu.Unlock()
	})
	defer stopWake()

	r.prog.mu.Lock()
	for r.prog.written <= r.pos && !r.prog.done && r.ctx.Err() == nil {
		r.prog.cond.Wait()
	}
	avail := r.prog.written - r.pos
	done := r.prog.done
	pErr := r.prog.err
	r.prog.mu.Unlock()

	if cerr := r.ctx.Err(); cerr != nil {
		return 0, cerr
	}
	if avail <= 0 {
		if pErr != nil {
			return 0, pErr
		}
		if done {
			return 0, io.EOF
		}
		return 0, io.ErrUnexpectedEOF
	}

	toRead := int(min(int64(len(p)), avail))
	n, err := r.f.ReadAt(p[:toRead], r.pos)
	r.pos += int64(n)
	if n > 0 && errors.Is(err, io.EOF) && r.pos < r.prog.totalSize {
		err = nil
	}
	return n, err
}

func parseFirstByteRangeStart(rangeHdr string, totalSize int64) (int64, bool) {
	s := strings.TrimSpace(rangeHdr)
	if !strings.HasPrefix(s, "bytes=") {
		return 0, false
	}
	spec := strings.TrimPrefix(s, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	startStr, endStr, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return 0, false
	}
	startStr, endStr = strings.TrimSpace(startStr), strings.TrimSpace(endStr)
	if startStr == "" {
		// Suffix range bytes=-N
		suf, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || suf <= 0 {
			return 0, false
		}
		if suf > totalSize {
			return 0, true
		}
		return totalSize - suf, true
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, false
	}
	return start, true
}

func (s *Server) serveHQPWAV(w http.ResponseWriter, r *http.Request, seq uint64) {
	entry := s.lookupHQP(seq)
	if entry == nil || entry.wavPath == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(entry.wavPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	if entry.prog != nil {
		if failed, written, pErr := entry.prog.failureState(); failed {
			if rng := r.Header.Get("Range"); rng != "" {
				if start, ok := parseFirstByteRangeStart(rng, entry.prog.totalSize); ok && start >= written {
					http.Error(w, pErr.Error(), http.StatusBadGateway)
					return
				}
			} else if written <= 0 {
				http.Error(w, pErr.Error(), http.StatusBadGateway)
				return
			}
		}
		w.Header().Set("Content-Type", "audio/wav")
		rs := &progressiveWAVReader{f: f, prog: entry.prog, ctx: r.Context()}
		http.ServeContent(w, r, fmt.Sprintf("%d.wav", seq), entry.modTime, rs)
		return
	}

	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeContent(w, r, fmt.Sprintf("%d.wav", seq), info.ModTime(), f)
}

// encodeWAVContainerHeader builds a RIFF/WAVE header with a 28-byte "JUNK"
// placeholder chunk or promotes it in-place to an EBU Tech 3306 RF64/WAVE
// header with a 28-byte "ds64" chunk whenever pcmBytes or riffPayload exceeds
// math.MaxUint32. Placing "fmt " and the optional 2-byte-aligned "id3 " chunk
// before "data" lets HQPlayer read tags and artwork from the start of the stream.
func encodeWAVContainerHeader(sampleRate, channels, bitsPerSample int, id3Chunk []byte, pcmBytes int64) []byte {
	blockAlign := channels * (bitsPerSample / 8)
	if blockAlign <= 0 {
		blockAlign = 6
	}
	hdrLen := 80 + len(id3Chunk)
	riffPayload := int64(hdrLen-8) + pcmBytes
	isRF64 := pcmBytes > math.MaxUint32 || riffPayload > math.MaxUint32

	h := make([]byte, hdrLen)
	if isRF64 {
		copy(h[0:4], "RF64")
		binary.LittleEndian.PutUint32(h[4:8], 0xFFFFFFFF)
		copy(h[8:12], "WAVE")
		copy(h[12:16], "ds64")
		binary.LittleEndian.PutUint32(h[16:20], 28)
		binary.LittleEndian.PutUint64(h[20:28], uint64(riffPayload))
		binary.LittleEndian.PutUint64(h[28:36], uint64(pcmBytes))
		binary.LittleEndian.PutUint64(h[36:44], uint64(pcmBytes/int64(blockAlign)))
		binary.LittleEndian.PutUint32(h[44:48], 0)
	} else {
		copy(h[0:4], "RIFF")
		if pcmBytes >= 0 {
			binary.LittleEndian.PutUint32(h[4:8], uint32(riffPayload))
		} else {
			binary.LittleEndian.PutUint32(h[4:8], 0xFFFFFFFF)
		}
		copy(h[8:12], "WAVE")
		copy(h[12:16], "JUNK")
		binary.LittleEndian.PutUint32(h[16:20], 28)
	}

	copy(h[48:52], "fmt ")
	binary.LittleEndian.PutUint32(h[52:56], 16)
	binary.LittleEndian.PutUint16(h[56:58], 1) // PCM
	binary.LittleEndian.PutUint16(h[58:60], uint16(channels))
	binary.LittleEndian.PutUint32(h[60:64], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[64:68], uint32(sampleRate*blockAlign))
	binary.LittleEndian.PutUint16(h[68:70], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[70:72], uint16(bitsPerSample))

	if len(id3Chunk) > 0 {
		copy(h[72:72+len(id3Chunk)], id3Chunk)
	}
	dataOff := 72 + len(id3Chunk)
	copy(h[dataOff:dataOff+4], "data")
	if isRF64 || pcmBytes < 0 {
		binary.LittleEndian.PutUint32(h[dataOff+4:dataOff+8], 0xFFFFFFFF)
	} else {
		binary.LittleEndian.PutUint32(h[dataOff+4:dataOff+8], uint32(pcmBytes))
	}
	return h
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

			canADTS := rs != nil && hasAAC && rs.hasADTS()

			// Pull or advance PCM clock when WAV listeners exist, when no AAC listener is
			// driving the clock, or when an AAC-only sink cannot produce ADTS packets.
			if rs != nil && (hasWAV || !hasAAC || !canADTS) {
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
				if canADTS {
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
