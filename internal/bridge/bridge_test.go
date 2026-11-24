package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"ytb-music/internal/player"
	"ytb-music/internal/ytm"
)

type rampPCMDecoder struct {
	rate   int
	frames int64
	pos    int64
}

func (d *rampPCMDecoder) SampleRate() int { return d.rate }
func (d *rampPCMDecoder) Channels() int   { return 2 }
func (d *rampPCMDecoder) Len() int64      { return d.frames }
func (d *rampPCMDecoder) Close() error    { return nil }

func (d *rampPCMDecoder) SetPosition(frame int64) error {
	d.pos = frame
	return nil
}

func (d *rampPCMDecoder) Read(dst []float32) (int, error) {
	if d.pos >= d.frames {
		return 0, io.EOF
	}
	// Cap each Read at 142 frames (like the tail of a 1024-sample AAC packet after 882 frames)
	// so Roon's 882-frame WAV tick loop is verified to accumulate across short reads.
	avail := int(min(int64(len(dst)/2), min(int64(142), d.frames-d.pos)))
	for i := range avail {
		idx := d.pos + int64(i)
		val := float32((idx%1000)+1) / 2000.0
		dst[i*2] = val
		dst[i*2+1] = -val
	}
	d.pos += int64(avail)
	return avail * 2, nil
}

// HQPlayerBackend materializes a local 24-bit WAV file with ID3v2.4 metadata for same-host HQPlayer, serves HTTP 206 byte ranges for remote peers without re-decoding, seeks in integer seconds, ignores premature stops, and fires OnSinkEnd at track end.
func TestHQPlayerBackendLocalFileAndRangeWAV(t *testing.T) {
	coverSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xD9})
	}))
	defer coverSrv.Close()

	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var (
		mu       sync.Mutex
		hqpConn  net.Conn
		openPCMs atomic.Int32
	)
	playEvents := make(chan struct{}, 8)
	seekPositions := make(chan string, 8)
	addedURIs := make(chan string, 8)

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		mu.Lock()
		hqpConn = conn
		mu.Unlock()
		defer conn.Close()

		xr := newXMLFrameReader(conn)
		for {
			tag, doc, rerr := xr.Next()
			if rerr != nil {
				return
			}
			_, attrs, _ := parseXMLAttrs(doc)

			switch tag {
			case "GetInfo":
				_, _ = conn.Write([]byte(xmlDecl + `<GetInfo product="HQPlayer Desktop" version="5.9.0"/>`))
			case "SetRepeat":
				_, _ = conn.Write([]byte(xmlDecl + `<SetRepeat result="OK"/>`))
			case "VolumeRange":
				_, _ = conn.Write([]byte(xmlDecl + `<VolumeRange min="-60" max="0"/>`))
			case "Status":
				_, _ = conn.Write([]byte(xmlDecl + `<Status state="0" position="0.0"/>`))
			case "PlaylistClear":
				_, _ = conn.Write([]byte(xmlDecl + `<PlaylistClear result="OK"/>`))
			case "PlaylistAdd":
				select {
				case addedURIs <- attrs["uri"]:
				default:
				}
				coalesced := xmlDecl + `<Status state="0" position="0.0"/>` + xmlDecl + `<PlaylistAdd result="OK"/>`
				_, _ = conn.Write([]byte(coalesced))
			case "Volume":
				_, _ = conn.Write([]byte(xmlDecl + `<Volume result="OK"/>`))
			case "Seek":
				posAttr := attrs["position"]
				if strings.Contains(posAttr, ".") {
					_, _ = conn.Write([]byte(xmlDecl + `<Seek result="Error">clString::ToInt(): not an integer</Seek>`))
					continue
				}
				select {
				case seekPositions <- posAttr:
				default:
				}
				_, _ = conn.Write([]byte(xmlDecl + `<Seek result="OK"/>`))
			case "Play":
				_, _ = conn.Write([]byte(xmlDecl + `<Play result="OK"/>` + xmlDecl + `<Status state="2" position="1.25"/>`))
				playEvents <- struct{}{}
			case "Pause":
				_, _ = conn.Write([]byte(xmlDecl + `<Pause result="OK"/>`))
			case "Stop":
				_, _ = conn.Write([]byte(xmlDecl + `<Stop result="OK"/>`))
			}
		}
	}()

	var clockMu sync.Mutex
	fakeNow := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	backend := NewHQPlayerBackend(srv, ln.Addr().String(), nil)
	backend.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return fakeNow
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		fakeNow = fakeNow.Add(d)
		clockMu.Unlock()
	}
	defer backend.Disconnect()

	const totalFrames = 44100 * 10
	src := player.TrackSource{
		Track: ytm.Track{
			ID:       "vid1",
			Name:     "グッドバイ",
			Artists:  []ytm.Artist{{Name: "toe"}},
			Album:    ytm.Album{Name: "New Sentimentality - EP", PicURL: coverSrv.URL + "/cover.jpg"},
			Duration: 10000,
		},
		Duration: 10 * time.Second,
		Gain:     0.64,
		OpenPCM: func() (player.PCMDecoder, error) {
			openPCMs.Add(1)
			return &rampPCMDecoder{rate: 44100, frames: totalFrames}, nil
		},
	}

	sink, err := backend.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer sink.Close()

	gotURI := <-addedURIs
	if !strings.HasPrefix(gotURI, "file:///") {
		t.Fatalf("PlaylistAdd URI = %q, want local file:/// URI", gotURI)
	}
	unescapedURIPath, uerr := url.PathUnescape(strings.TrimPrefix(gotURI, "file:///"))
	if uerr != nil {
		t.Fatalf("PathUnescape(%q): %v", gotURI, uerr)
	}
	localPath := filepath.FromSlash(unescapedURIPath)
	if filepath.Separator == '/' {
		localPath = "/" + unescapedURIPath
	}
	wavBytes, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("reading materialized WAV %q: %v", localPath, err)
	}
	if string(wavBytes[:4]) != "RIFF" || string(wavBytes[8:12]) != "WAVE" {
		t.Fatalf("invalid RIFF/WAVE header in materialized file")
	}
	if string(wavBytes[12:16]) != "JUNK" || binary.LittleEndian.Uint32(wavBytes[16:20]) != 28 {
		t.Fatalf("missing 28-byte JUNK placeholder chunk after WAVE")
	}
	if string(wavBytes[48:52]) != "fmt " || binary.LittleEndian.Uint32(wavBytes[52:56]) != 16 {
		t.Fatalf("missing fmt chunk at offset 48")
	}
	if string(wavBytes[72:76]) != "id3 " {
		t.Fatalf("expected id3 chunk before data at offset 72, got %q", string(wavBytes[72:76]))
	}
	id3BodyLen := int(binary.LittleEndian.Uint32(wavBytes[76:80]))
	id3TotalLen := 8 + id3BodyLen + (id3BodyLen % 2)
	id3Slice := wavBytes[72 : 72+id3TotalLen]
	if !bytes.Contains(id3Slice, []byte("グッドバイ")) || !bytes.Contains(id3Slice, []byte("New Sentimentality - EP")) || !bytes.Contains(id3Slice, []byte("APIC")) {
		t.Fatalf("WAV upfront id3 chunk missing UTF-8 metadata or APIC cover art")
	}
	dataHdrOff := 72 + id3TotalLen
	if string(wavBytes[dataHdrOff:dataHdrOff+4]) != "data" {
		t.Fatalf("expected data chunk at offset %d, got %q", dataHdrOff, string(wavBytes[dataHdrOff:dataHdrOff+4]))
	}
	dataBytes := binary.LittleEndian.Uint32(wavBytes[dataHdrOff+4 : dataHdrOff+8])
	if dataBytes != totalFrames*6 {
		t.Fatalf("WAV data chunk = %d bytes, want %d", dataBytes, totalFrames*6)
	}
	pcmStartOff := int64(dataHdrOff + 8)
	if int64(len(wavBytes)) != pcmStartOff+int64(dataBytes) {
		t.Fatalf("WAV total size = %d, want %d", len(wavBytes), pcmStartOff+int64(dataBytes))
	}

	// Remote HTTP WAV endpoint serves Accept-Ranges + 206 Partial Content from disk without re-opening PCMDecoder.
	wavURL := srv.HQPWAVURL(1, "")
	headResp, err := http.Head(wavURL)
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if ar := headResp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("HEAD Accept-Ranges = %q, want bytes", ar)
	}
	byteOff := pcmStartOff + 44100*6 // frame 44100
	req, err := http.NewRequest(http.MethodGet, wavURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", byteOff, byteOff+5))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var frameSample [6]byte
	if _, err := io.ReadFull(resp.Body, frameSample[:]); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range status = %d, want 206 Partial Content", resp.StatusCode)
	}
	gotLeft := int32(uint32(frameSample[0]) | uint32(frameSample[1])<<8 | uint32(frameSample[2])<<16)
	if gotLeft&0x800000 != 0 {
		gotLeft |= ^0xffffff
	}
	if gotLeft < 423620 || gotLeft > 423630 {
		t.Fatalf("frame 44100 24-bit sample = %d, want ~423625", gotLeft)
	}
	if got := openPCMs.Load(); got != 1 {
		t.Fatalf("OpenPCM called %d times across prepare + HTTP Range, want 1", got)
	}

	ended := make(chan struct{}, 2)
	sink.Activate()
	sink.OnSinkEnd(func() { ended <- struct{}{} })
	sink.Play()
	select {
	case <-playEvents:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first <Play/>")
	}
	for start := time.Now(); backend.Signal().Transport != "wav-file" && time.Since(start) < time.Second; time.Sleep(10 * time.Millisecond) {
	}
	if tr := backend.Signal().Transport; tr != "wav-file" {
		t.Fatalf("materialized WAV Signal().Transport = %q, want wav-file", tr)
	}

	// Seeking while playing sends integer seconds in <Seek position="..."/>.
	sink.Seek(3400 * time.Millisecond)
	select {
	case posAttr := <-seekPositions:
		if posAttr != "3" {
			t.Fatalf("Seek position = %q, want integer \"3\"", posAttr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for <Seek position=\"3\"/>")
	}

	// Premature stop at 1.25s of a 10s track must NOT fire OnSinkEnd.
	advanceClock(1 * time.Second)
	mu.Lock()
	conn := hqpConn
	mu.Unlock()
	_, _ = conn.Write([]byte(xmlDecl + `<Status state="2" position="1.25"/>`))
	time.Sleep(20 * time.Millisecond)
	advanceClock(600 * time.Millisecond)
	_, _ = conn.Write([]byte(xmlDecl + `<Status state="0" position="1.25"/>`))
	time.Sleep(40 * time.Millisecond)
	select {
	case <-ended:
		t.Fatal("premature mid-track stop falsely triggered OnSinkEnd")
	default:
	}

	// Resume, advance near track end (9.2s of 10s), and verify natural state=0 fires OnSinkEnd.
	sink.Play()
	<-playEvents
	advanceClock(1 * time.Second)
	_, _ = conn.Write([]byte(xmlDecl + `<Status state="2" position="9.20"/>`))
	time.Sleep(20 * time.Millisecond)
	advanceClock(600 * time.Millisecond)
	_, _ = conn.Write([]byte(xmlDecl + `<Status state="0" position="0.0"/>`))
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("HQPlayer natural track end did not trigger OnSinkEnd")
	}

	// Verify RepeatOne replay: Seek(0) while stopped followed by Play() seeks to "0", plays, and rearms OnSinkEnd.
	sink.Seek(0)
	sink.Play()
	select {
	case posAttr := <-seekPositions:
		if posAttr != "0" {
			t.Fatalf("RepeatOne Seek position = %q, want \"0\"", posAttr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RepeatOne did not send <Seek position=\"0\"/>")
	}
	select {
	case <-playEvents:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RepeatOne <Play/>")
	}
	advanceClock(1 * time.Second)
	_, _ = conn.Write([]byte(xmlDecl + `<Status state="2" position="9.40"/>`))
	time.Sleep(20 * time.Millisecond)
	advanceClock(600 * time.Millisecond)
	_, _ = conn.Write([]byte(xmlDecl + `<Status state="0" position="0.0"/>`))
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("HQPlayer RepeatOne second pass did not re-trigger OnSinkEnd")
	}

	sink.Close()
	_ = srv.Close()
	if _, serr := os.Stat(localPath); !os.IsNotExist(serr) {
		t.Fatalf("Server.Close did not clean up temp WAV %q", localPath)
	}
}

// RoonManager completes MOO/1 extension pairing, discovers HQPlayer zones, triggers My Live Radio playback, and streams 24-bit WAV with ICY StreamTitle metadata.
func TestRoonMOOExtensionAndICYStream(t *testing.T) {
	// Verify SOOD v2 reply parser round-trip.
	soodReply := append([]byte("SOOD\x02R"), buildSOODQuery(map[string]string{
		"service_id":   roonRegistrySvcID,
		"http_port":    "9330",
		"display_name": "LivingRoomCore",
	})[6:]...)
	props, ok := parseSOODReply(soodReply)
	if !ok || props["http_port"] != "9330" || props["display_name"] != "LivingRoomCore" {
		t.Fatalf("parseSOODReply failed: %+v", props)
	}

	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	var (
		savedToken  string
		playedZone  = make(chan string, 4)
		stoppedZone = make(chan string, 4)
	)

	// Spin up a mock Roon Core MOO/1 WebSocket server with realistic station -> action_list drill-down.
	mux := http.NewServeMux()
	mux.Handle("/api", websocket.Handler(func(ws *websocket.Conn) {
		inActionList := false
		for {
			var raw []byte
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			msg, ok := parseMOOFrame(raw)
			if !ok {
				continue
			}
			send := func(verb, name string, payload any) {
				var body []byte
				if payload != nil {
					body, _ = json.Marshal(payload)
				}
				var buf bytes.Buffer
				fmt.Fprintf(&buf, "MOO/1 %s %s\nRequest-Id: %s\n", verb, name, msg.RequestID)
				if len(body) > 0 {
					fmt.Fprintf(&buf, "Content-Length: %d\nContent-Type: application/json\n", len(body))
				}
				buf.WriteByte('\n')
				buf.Write(body)
				_ = websocket.Message.Send(ws, buf.Bytes())
			}

			switch msg.Name {
			case "com.roonlabs.registry:1/info":
				send("COMPLETE", "Success", map[string]any{
					"core_id":      "core-123",
					"display_name": "Studio Roon Core",
				})
			case "com.roonlabs.registry:1/register":
				send("CONTINUE", "Registered", map[string]any{
					"core_id":      "core-123",
					"display_name": "Studio Roon Core",
					"token":        "tok-abc-999",
				})
				ping := []byte("MOO/1 REQUEST com.roonlabs.ping:1/ping\nRequest-Id: 77\n\n")
				_ = websocket.Message.Send(ws, ping)
			case "com.roonlabs.transport:2/subscribe_zones":
				send("CONTINUE", "Subscribed", map[string]any{
					"zones": []RoonZone{
						{ZoneID: "z-hqp", DisplayName: "HQPlayer DSD256", State: "stopped"},
						{ZoneID: "z-desk", DisplayName: "Desktop DAC", State: "stopped"},
					},
				})
			case "com.roonlabs.transport:2/control":
				var req struct {
					ZoneID  string `json:"zone_or_output_id"`
					Control string `json:"control"`
				}
				_ = json.Unmarshal(msg.Body, &req)
				if req.Control == "stop" {
					stoppedZone <- req.ZoneID
				}
				send("COMPLETE", "Success", nil)
			case "com.roonlabs.browse:1/browse":
				var req struct {
					PopAll  bool   `json:"pop_all"`
					ItemKey string `json:"item_key"`
					ZoneID  string `json:"zone_or_output_id"`
				}
				_ = json.Unmarshal(msg.Body, &req)
				switch {
				case req.PopAll:
					inActionList = false
					send("COMPLETE", "Success", map[string]any{"action": "list"})
				case req.ItemKey == "st-1":
					inActionList = true
					send("COMPLETE", "Success", map[string]any{"action": "list"})
				case req.ItemKey == "act-play":
					inActionList = false
					playedZone <- req.ZoneID
					send("COMPLETE", "Success", map[string]any{"action": "none"})
				}
			case "com.roonlabs.browse:1/load":
				if !inActionList {
					send("COMPLETE", "Loaded", map[string]any{
						"items": []map[string]any{
							{"title": "Jazz FM", "item_key": "st-other"},
							{"title": "YTB Music Bridge", "item_key": "st-1"},
						},
					})
				} else {
					send("COMPLETE", "Loaded", map[string]any{
						"items": []map[string]any{
							{"title": "Play Now", "item_key": "act-play"},
						},
					})
				}
			}
		}
	}))
	coreSrv := httptest.NewServer(mux)
	defer coreSrv.Close()
	coreHostPort := strings.TrimPrefix(coreSrv.URL, "http://")

	pairedCh := make(chan RoonStatus, 4)
	mgr := NewRoonManager(srv, RoonCallbacks{
		GetToken: func(string) string { return "" },
		SaveToken: func(_, tok string) {
			savedToken = tok
		},
		OnChange: func(st RoonStatus) {
			if st.Paired && len(st.Zones) > 0 {
				select {
				case pairedCh <- st:
				default:
				}
			}
		},
	})
	mgr.SetDirectCoreAddr(coreHostPort)
	mgr.Start()
	defer mgr.Close()

	select {
	case st := <-pairedCh:
		hasHQP := slices.ContainsFunc(st.Zones, func(z RoonZone) bool { return z.ZoneID == "z-hqp" })
		hasDesk := slices.ContainsFunc(st.Zones, func(z RoonZone) bool { return z.ZoneID == "z-desk" })
		if savedToken != "tok-abc-999" || !hasHQP || !hasDesk {
			t.Fatalf("unexpected Roon status: token=%q zones=%+v", savedToken, st.Zones)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Roon MOO pairing and zones")
	}

	// Directly verify on an unactivated sink (isolated from runRoonClock) that a single 882-frame WAV pull accumulates across 142-frame decoder reads.
	rb := NewRoonBackend(srv, mgr, "z-hqp")
	isolatedSink, err := rb.Open(context.Background(), player.TrackSource{
		Track:    ytm.Track{ID: "iso", Name: "Isolated", Duration: 3000},
		Duration: 3 * time.Second,
		Gain:     1.0,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &rampPCMDecoder{rate: 44100, frames: 44100 * 3}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	isoRS := isolatedSink.(*roonSink)
	isoRS.stateMu.Lock()
	isoRS.playing = true
	isoRS.stateMu.Unlock()
	oneTick := isoRS.pullPCM24(882)
	isolatedSink.Close()
	if len(oneTick) != 882*6 {
		t.Fatalf("pullPCM24(882) returned %d bytes, want %d", len(oneTick), 882*6)
	}
	lastOff := 881 * 6
	lastLeft := int32(uint32(oneTick[lastOff]) | uint32(oneTick[lastOff+1])<<8 | uint32(oneTick[lastOff+2])<<16)
	if lastLeft&0x800000 != 0 {
		lastLeft |= ^0xffffff
	}
	if lastLeft < 3699370 || lastLeft > 3699382 {
		t.Fatalf("frame 881 sample = %d, want ~3699376", lastLeft)
	}

	sink, err := rb.Open(context.Background(), player.TrackSource{
		Track: ytm.Track{
			ID:       "t1",
			Name:     "夜曲",
			Artists:  []ytm.Artist{{Name: "周杰伦"}},
			Duration: 3000,
		},
		Duration: 3 * time.Second,
		Gain:     1.0,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &rampPCMDecoder{rate: 44100, frames: 44100 * 3}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	sink.Activate()
	sink.Play()

	// Verify Roon station startup actually invoked Play Now ("act-play") on z-hqp BEFORE any HTTP listener connected.
	select {
	case z := <-playedZone:
		if z != "z-hqp" {
			t.Fatalf("started zone = %q, want z-hqp", z)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureStationPlaying did not invoke Play Now on z-hqp")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.RoonWAVURL(""), nil)
	req.Header.Set("Icy-MetaData", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Switching zones while an HTTP listener is already connected must stop the previous zone and start the new one!
	rb.SetZoneID("z-desk")
	select {
	case z := <-playedZone:
		if z != "z-desk" {
			t.Fatalf("switched zone = %q, want z-desk", z)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("switching Roon zone did not start playback on z-desk")
	}
	select {
	case stopped := <-stoppedZone:
		if stopped != "z-hqp" {
			t.Fatalf("stopped zone = %q, want z-hqp", stopped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("switching Roon zone did not stop previous zone z-hqp")
	}

	if resp.Header.Get("icy-metaint") != "16384" {
		t.Fatalf("icy-metaint = %q, want 16384", resp.Header.Get("icy-metaint"))
	}
	audioBlock := make([]byte, 16384)
	if _, err := io.ReadFull(resp.Body, audioBlock); err != nil {
		t.Fatalf("reading first 16384 WAV bytes: %v", err)
	}
	if string(audioBlock[:4]) != "RIFF" || string(audioBlock[8:12]) != "WAVE" {
		t.Fatalf("missing RIFF/WAVE header in live stream")
	}
	sr := binary.LittleEndian.Uint32(audioBlock[24:28])
	bits := binary.LittleEndian.Uint16(audioBlock[34:36])
	if sr != 44100 || bits != 24 {
		t.Fatalf("WAV format = %dHz/%dbit, want 44100Hz/24bit", sr, bits)
	}

	var lenByte [1]byte
	if _, err := io.ReadFull(resp.Body, lenByte[:]); err != nil {
		t.Fatal(err)
	}
	metaLen := int(lenByte[0]) * 16
	metaBuf := make([]byte, metaLen)
	if metaLen > 0 {
		if _, err := io.ReadFull(resp.Body, metaBuf); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(string(metaBuf), "StreamTitle='周杰伦 - 夜曲';") {
		t.Fatalf("ICY metadata = %q, want StreamTitle='周杰伦 - 夜曲';", string(metaBuf))
	}
}

// Same-host HQPlayerBackend passes supported local files directly via file:/// without calling OpenPCM, deduplicates live <Status> upsampling telemetry including <metadata bitrate>, and resets signal state on disconnect.
func TestHQPlayerNativePassThroughAndSignalTelemetry(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	nativeFile := filepath.Join(t.TempDir(), "native-track.dsf")
	if err := os.WriteFile(nativeFile, []byte("DSD "), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var (
		connMu    sync.Mutex
		hqpConn   net.Conn
		openPCMs  atomic.Int32
		addedURIs = make(chan string, 4)
	)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		connMu.Lock()
		hqpConn = conn
		connMu.Unlock()
		defer conn.Close()

		xr := newXMLFrameReader(conn)
		for {
			tag, doc, rerr := xr.Next()
			if rerr != nil {
				return
			}
			_, attrs, _ := parseXMLAttrs(doc)
			switch tag {
			case "GetInfo":
				_, _ = conn.Write([]byte(xmlDecl + `<GetInfo product="HQPlayer Desktop" version="5.10.0"/>`))
			case "SetRepeat":
				_, _ = conn.Write([]byte(xmlDecl + `<SetRepeat result="OK"/>`))
			case "VolumeRange":
				_, _ = conn.Write([]byte(xmlDecl + `<VolumeRange min="-60" max="0"/>`))
			case "Status":
				_, _ = conn.Write([]byte(xmlDecl + `<Status state="0" position="0.0"/>`))
			case "PlaylistClear":
				_, _ = conn.Write([]byte(xmlDecl + `<PlaylistClear result="OK"/>`))
			case "PlaylistAdd":
				addedURIs <- attrs["uri"]
				_, _ = conn.Write([]byte(xmlDecl + `<PlaylistAdd result="OK"/>`))
			case "Volume":
				_, _ = conn.Write([]byte(xmlDecl + `<Volume result="OK"/>`))
			case "Play":
				_, _ = conn.Write([]byte(xmlDecl + `<Play result="OK"/>`))
			case "Stop":
				_, _ = conn.Write([]byte(xmlDecl + `<Stop result="OK"/>`))
			}
		}
	}()

	sigEvents := make(chan HQPSignalInfo, 8)
	backend := NewHQPlayerBackend(srv, ln.Addr().String(), nil)
	backend.SetOnSignal(func(sig HQPSignalInfo) {
		sigEvents <- sig
	})
	defer backend.Disconnect()

	sink, err := backend.Open(context.Background(), player.TrackSource{
		Track: ytm.Track{
			ID:       "localfile:dsf1",
			Name:     "Blue in Green",
			Artists:  []ytm.Artist{{Name: "Miles Davis"}},
			Album:    ytm.Album{Name: "Kind of Blue"},
			Duration: 327000,
		},
		Stream:    &ytm.Stream{Codec: "DSD64", SampleRate: 2822400, BitDepth: 1, IsDSD: true},
		Duration:  327 * time.Second,
		Gain:      1.0,
		LocalPath: nativeFile,
		NativeHQP: true,
		OpenPCM: func() (player.PCMDecoder, error) {
			openPCMs.Add(1)
			return nil, fmt.Errorf("unexpected OpenPCM call during native pass-through")
		},
	})
	if err != nil {
		t.Fatalf("Open native local file: %v", err)
	}
	defer sink.Close()

	if got := openPCMs.Load(); got != 0 {
		t.Fatalf("OpenPCM called %d times, want 0", got)
	}
	gotURI := <-addedURIs
	wantURI := LocalFileURI(nativeFile)
	if gotURI != wantURI {
		t.Fatalf("PlaylistAdd uri = %q, want %q", gotURI, wantURI)
	}

	connMu.Lock()
	conn := hqpConn
	connMu.Unlock()

	statusXML := xmlDecl + `<Status state="2" position="1.0" rate="11289600" bits="1" channels="2" sdm="1" filter="poly-sinc-gauss-long" shaper="ASDM7EC-super 512+fs" clips="0" apod="4"><metadata song="Blue in Green" bitrate="2116800"/></Status>`
	_, _ = conn.Write([]byte(statusXML))

	var gotSig HQPSignalInfo
	select {
	case gotSig = <-sigEvents:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HQPSignalInfo")
	}
	if !gotSig.Active || gotSig.State != 2 || gotSig.Mode != "SDM (DSD)" || gotSig.Rate != 11289600 || gotSig.Bits != 1 || gotSig.Channels != 2 || gotSig.Transport != "file" {
		t.Fatalf("unexpected signal core fields: %+v", gotSig)
	}
	if gotSig.Filter != "poly-sinc-gauss-long" || gotSig.Shaper != "ASDM7EC-super 512+fs" || gotSig.Apod != 4 || gotSig.SourceBitrate != 2116800 {
		t.Fatalf("unexpected signal DSP/metadata fields: %+v", gotSig)
	}
	if got := gotSig.OutputBadge(); got != "DSD256" {
		t.Fatalf("OutputBadge() = %q, want DSD256", got)
	}
	if got := gotSig.OutputDetail(); got != "SDM · DSD256 (11.29 MHz / 1-bit)" {
		t.Fatalf("OutputDetail() = %q, want %q", got, "SDM · DSD256 (11.29 MHz / 1-bit)")
	}
	if got := gotSig.SummaryChain(); got != "DSD256 · poly-sinc-gauss-long · ASDM7EC-super 512+fs" {
		t.Fatalf("SummaryChain() = %q", got)
	}

	// Verify 48k-family DSD and Hi-Res PCM badge/detail formatting as well.
	x48 := HQPSignalInfo{Active: true, State: 2, Mode: "SDM (DSD)", Rate: 12288000, Bits: 1}
	if x48.OutputBadge() != "DSD256x48" {
		t.Fatalf("48k DSD OutputBadge() = %q, want DSD256x48", x48.OutputBadge())
	}
	pcmSig := HQPSignalInfo{Active: true, State: 2, Mode: "PCM", Rate: 705600, Bits: 24}
	if pcmSig.OutputBadge() != "PCM 705.6k/24b" || pcmSig.OutputDetail() != "PCM · 705.6 kHz / 24-bit" {
		t.Fatalf("PCM signal formatting = %q / %q", pcmSig.OutputBadge(), pcmSig.OutputDetail())
	}

	// Identical DSP status with a new position tick must be deduplicated.
	dupXML := xmlDecl + `<Status state="2" position="2.0" rate="11289600" bits="1" channels="2" sdm="1" filter="poly-sinc-gauss-long" shaper="ASDM7EC-super 512+fs" clips="0" apod="4"><metadata song="Blue in Green" bitrate="2116800"/></Status>`
	_, _ = conn.Write([]byte(dupXML))
	time.Sleep(40 * time.Millisecond)
	select {
	case extra := <-sigEvents:
		t.Fatalf("duplicate status was not deduplicated: %+v", extra)
	default:
	}

	// Disconnect resets signal state to zero value.
	backend.Disconnect()
	if cur := backend.Signal(); cur != (HQPSignalInfo{}) {
		t.Fatalf("Signal() after Disconnect = %+v, want zero HQPSignalInfo", cur)
	}
	select {
	case cleared := <-sigEvents:
		if cleared != (HQPSignalInfo{}) {
			t.Fatalf("cleared callback = %+v, want zero HQPSignalInfo", cleared)
		}
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not emit reset HQPSignalInfo")
	}
}

// RoonManager.FetchLibraryGenres traverses both nested Albums/专辑 buckets and direct genre album lists using an isolated multi_session_key and deduplicates genres per album and artist.
func TestRoonFetchLibraryGenresMultiSession(t *testing.T) {
	var (
		mu              sync.Mutex
		sawBrowseSvc    bool
		missingMultiKey bool
		finalStackDepth int
	)

	mux := http.NewServeMux()
	mux.Handle("/api", websocket.Handler(func(ws *websocket.Conn) {
		stacks := map[string][]string{}
		for {
			var raw []byte
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			msg, ok := parseMOOFrame(raw)
			if !ok {
				continue
			}
			send := func(verb, name string, payload any) {
				var body []byte
				if payload != nil {
					body, _ = json.Marshal(payload)
				}
				var buf bytes.Buffer
				fmt.Fprintf(&buf, "MOO/1 %s %s\nRequest-Id: %s\n", verb, name, msg.RequestID)
				if len(body) > 0 {
					fmt.Fprintf(&buf, "Content-Length: %d\nContent-Type: application/json\n", len(body))
				}
				buf.WriteByte('\n')
				buf.Write(body)
				_ = websocket.Message.Send(ws, buf.Bytes())
			}

			switch msg.Name {
			case "com.roonlabs.registry:1/info":
				send("COMPLETE", "Success", map[string]any{
					"core_id":      "core-genre",
					"display_name": "Genre Core",
				})
			case "com.roonlabs.registry:1/register":
				var reg struct {
					Required []string `json:"required_services"`
				}
				_ = json.Unmarshal(msg.Body, &reg)
				mu.Lock()
				sawBrowseSvc = slices.Contains(reg.Required, "com.roonlabs.browse:1")
				mu.Unlock()
				send("CONTINUE", "Registered", map[string]any{
					"core_id":      "core-genre",
					"display_name": "Genre Core",
					"token":        "tok-genre",
				})
			case "com.roonlabs.transport:2/subscribe_zones":
				send("CONTINUE", "Subscribed", map[string]any{"zones": []RoonZone{}})
			case "com.roonlabs.browse:1/browse":
				var req struct {
					Hierarchy       string `json:"hierarchy"`
					MultiSessionKey string `json:"multi_session_key"`
					PopAll          bool   `json:"pop_all"`
					PopLevels       int    `json:"pop_levels"`
					ItemKey         string `json:"item_key"`
				}
				_ = json.Unmarshal(msg.Body, &req)
				if req.Hierarchy == "genres" && req.MultiSessionKey != "ytb-genre-sync" {
					mu.Lock()
					missingMultiKey = true
					mu.Unlock()
				}
				sk := req.Hierarchy + "|" + req.MultiSessionKey
				switch {
				case req.PopAll:
					stacks[sk] = []string{"root"}
				case req.PopLevels > 0:
					st := stacks[sk]
					if len(st) > req.PopLevels {
						stacks[sk] = st[:len(st)-req.PopLevels]
					} else {
						stacks[sk] = []string{"root"}
					}
				case req.ItemKey != "":
					stacks[sk] = append(stacks[sk], req.ItemKey)
				}
				mu.Lock()
				finalStackDepth = len(stacks[sk])
				mu.Unlock()
				send("COMPLETE", "Success", map[string]any{"action": "list"})

			case "com.roonlabs.browse:1/load":
				var req struct {
					Hierarchy       string `json:"hierarchy"`
					MultiSessionKey string `json:"multi_session_key"`
				}
				_ = json.Unmarshal(msg.Body, &req)
				if req.Hierarchy == "genres" && req.MultiSessionKey != "ytb-genre-sync" {
					mu.Lock()
					missingMultiKey = true
					mu.Unlock()
				}
				sk := req.Hierarchy + "|" + req.MultiSessionKey
				st := stacks[sk]
				top := "root"
				if len(st) > 0 {
					top = st[len(st)-1]
				}
				var items []map[string]any
				switch top {
				case "root":
					items = []map[string]any{
						{"title": "Jazz", "item_key": "g-jazz", "hint": "list"},
						{"title": "Cool Jazz", "item_key": "g-cool", "hint": "list"},
						{"title": "古典", "item_key": "g-classical", "hint": "list"},
					}
				case "g-jazz":
					items = []map[string]any{
						{"title": "Play Genre", "item_key": "act-play", "hint": "action"},
						{"title": "Artists", "item_key": "j-artists", "hint": "list"},
						{"title": "Albums", "item_key": "j-albums", "hint": "list"},
					}
				case "j-albums":
					items = []map[string]any{
						{"title": "Kind of Blue", "subtitle": "Miles Davis", "item_key": "alb-1"},
						{"title": "Waltz for Debby", "subtitle": "Bill Evans Trio", "item_key": "alb-2"},
					}
				case "g-cool":
					items = []map[string]any{
						{"title": "Kind of Blue", "subtitle": "Miles Davis", "item_key": "alb-1"},
					}
				case "g-classical":
					items = []map[string]any{
						{"title": "专辑", "item_key": "c-albums", "hint": "list"},
					}
				case "c-albums":
					items = []map[string]any{
						{"title": "Goldberg Variations", "subtitle": "Glenn Gould", "item_key": "alb-3"},
					}
				}
				send("COMPLETE", "Loaded", map[string]any{"items": items})
			}
		}
	}))
	coreSrv := httptest.NewServer(mux)
	defer coreSrv.Close()

	paired := make(chan struct{}, 1)
	mgr := NewRoonManager(nil, RoonCallbacks{
		OnChange: func(st RoonStatus) {
			if st.Paired {
				select {
				case paired <- struct{}{}:
				default:
				}
			}
		},
	})
	mgr.SetDirectCoreAddr(strings.TrimPrefix(coreSrv.URL, "http://"))
	mgr.Start()
	defer mgr.Close()

	select {
	case <-paired:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Roon pairing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := mgr.FetchLibraryGenres(ctx)
	if err != nil {
		t.Fatalf("FetchLibraryGenres: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawBrowseSvc {
		t.Fatal("registerPayload missing com.roonlabs.browse:1 in required_services")
	}
	if missingMultiKey {
		t.Fatal("genres browse/load call omitted multi_session_key=\"ytb-genre-sync\"")
	}
	if finalStackDepth != 1 {
		t.Fatalf("final genres browse stack depth = %d, want 1 (root)", finalStackDepth)
	}
	if len(got) != 3 {
		t.Fatalf("got %d albums (%+v), want 3", len(got), got)
	}
	if got[0].Album != "Kind of Blue" || got[0].Artist != "Miles Davis" || !slices.Equal(got[0].Genres, []string{"Jazz", "Cool Jazz"}) {
		t.Fatalf("entry[0] = %+v", got[0])
	}
	if got[1].Album != "Waltz for Debby" || got[1].Artist != "Bill Evans Trio" || !slices.Equal(got[1].Genres, []string{"Jazz"}) {
		t.Fatalf("entry[1] = %+v", got[1])
	}
	if got[2].Album != "Goldberg Variations" || got[2].Artist != "Glenn Gould" || !slices.Equal(got[2].Genres, []string{"古典"}) {
		t.Fatalf("entry[2] = %+v", got[2])
	}
}

// LocalFileURI percent-encodes reserved URI characters across Windows drive paths, POSIX root paths, and UNC network shares per RFC 8089.
func TestLocalFileURIRFC8089Encoding(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   `D:\Music\Take #2.flac`,
			want: "file:///D:/Music/Take%20%232.flac",
		},
		{
			in:   `C:\Lossless\100% Pure [DSD]\What? #1.dsf`,
			want: "file:///C:/Lossless/100%25%20Pure%20%5BDSD%5D/What%3F%20%231.dsf",
		},
		{
			in:   `/music/Take #2.flac`,
			want: "file:///music/Take%20%232.flac",
		},
		{
			in:   `/music/100% [Live]/グッドバイ #1.wav`,
			want: "file:///music/100%25%20%5BLive%5D/%E3%82%B0%E3%83%83%E3%83%89%E3%83%90%E3%82%A4%20%231.wav",
		},
		{
			in:   `\\server\share\dir\Take #2.flac`,
			want: "file://server/share/dir/Take%20%232.flac",
		},
		{
			in:   `\\192.168.1.20\Music Share\100% [Hi-Res]\Song?.flac`,
			want: "file://192.168.1.20/Music%20Share/100%25%20%5BHi-Res%5D/Song%3F.flac",
		},
	}
	for _, tc := range cases {
		if got := LocalFileURI(tc.in); got != tc.want {
			t.Errorf("LocalFileURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// PrepareHQPTrack and serveHQPCover serve local CoverBytes without fetching localcover:// URLs, while ensureHQPDir's .lock handle protects active WAV directories during temp scavenging.
func TestHQPlayerLocalCoverAndDirectoryLockfile(t *testing.T) {
	tempRoot := t.TempDir()

	srv1, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv1.tempRoot = tempRoot
	defer srv1.Close()

	// Track with local CoverBytes and localcover:// PicURL embeds APIC and serves cover.jpg without ytm.Fetch.
	pngCover := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	seq1, fileURI1, _, coverURL1, _, err := srv1.PrepareHQPTrack(context.Background(), player.TrackSource{
		Track: ytm.Track{
			ID:       "localfile:art1",
			Name:     "OddPad!", // odd byte length exercises 1-byte RIFF word-alignment padding on id3
			Artists:  []ytm.Artist{{Name: "Artist"}},
			Album:    ytm.Album{Name: "Album", PicURL: "localcover://abc123"},
			Duration: 1000,
		},
		Duration:   time.Second,
		CoverBytes: pngCover,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &rampPCMDecoder{rate: 44100, frames: 4410}, nil
		},
	}, "127.0.0.1:4321")
	if err != nil {
		t.Fatalf("PrepareHQPTrack with local CoverBytes: %v", err)
	}
	if seq1 == 0 || fileURI1 == "" || coverURL1 == "" {
		t.Fatalf("expected non-empty fileURI and coverURL, got seq=%d fileURI=%q coverURL=%q", seq1, fileURI1, coverURL1)
	}

	cResp, err := http.Get(coverURL1)
	if err != nil {
		t.Fatal(err)
	}
	gotCover, _ := io.ReadAll(cResp.Body)
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK || cResp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(gotCover, pngCover) {
		t.Fatalf("serveHQPCover status=%d type=%q match=%v", cResp.StatusCode, cResp.Header.Get("Content-Type"), bytes.Equal(gotCover, pngCover))
	}

	// Track with localcover:// PicURL and empty CoverBytes must return empty coverURL and never call ytm.Fetch.
	_, _, _, coverURL2, _, err := srv1.PrepareHQPTrack(context.Background(), player.TrackSource{
		Track: ytm.Track{
			ID:       "localfile:noart",
			Name:     "NoArt",
			Album:    ytm.Album{Name: "Album", PicURL: "localcover://missing"},
			Duration: 1000,
		},
		Duration: time.Second,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &rampPCMDecoder{rate: 44100, frames: 4410}, nil
		},
	}, "127.0.0.1:4321")
	if err != nil {
		t.Fatal(err)
	}
	if coverURL2 != "" {
		t.Fatalf("coverURL for localcover:// without CoverBytes = %q, want empty", coverURL2)
	}

	// Verify .lock protection: backdate srv1's active hqpDir and an unlocked orphan dir >6h, then trigger srv2 scavenging.
	dir1, err := srv1.ensureHQPDir()
	if err != nil {
		t.Fatal(err)
	}
	activeWAV := filepath.Join(dir1, "track-1.wav")
	if _, err := os.Stat(activeWAV); err != nil {
		t.Fatalf("expected active WAV %q: %v", activeWAV, err)
	}

	orphanDir := filepath.Join(tempRoot, "ytb-music-hqp-orphan-stale")
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphanLock, err := openExclusiveLockFile(filepath.Join(orphanDir, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = orphanLock.Close() // simulate crashed previous process releasing OS handle

	staleTime := time.Now().Add(-8 * time.Hour)
	_ = os.Chtimes(dir1, staleTime, staleTime)
	_ = os.Chtimes(orphanDir, staleTime, staleTime)

	srv2, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv2.tempRoot = tempRoot
	defer srv2.Close()
	if _, err := srv2.ensureHQPDir(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("stale unlocked orphan dir %q was not scavenged (err=%v)", orphanDir, err)
	}
	if _, err := os.Stat(activeWAV); err != nil {
		t.Fatalf("live srv1 WAV %q was deleted by srv2 scavenging: %v", activeWAV, err)
	}

	// Closing srv1 releases .lock and removes dir1.
	_ = srv1.Close()
	if _, err := os.Stat(dir1); !os.IsNotExist(err) {
		t.Fatalf("srv1.Close() did not remove %q (err=%v)", dir1, err)
	}
}

type gatedPCMDecoder struct {
	rate        int
	estFrames   int64
	fastFrames  int64
	totalFrames int64
	gate        <-chan struct{}
	midErr      error
	pos         int64
}

func (d *gatedPCMDecoder) SampleRate() int          { return d.rate }
func (d *gatedPCMDecoder) Channels() int            { return 2 }
func (d *gatedPCMDecoder) Len() int64               { return d.estFrames }
func (d *gatedPCMDecoder) SetPosition(f int64) error { d.pos = f; return nil }
func (d *gatedPCMDecoder) Close() error             { return nil }

func (d *gatedPCMDecoder) Read(dst []float32) (int, error) {
	if d.pos >= d.totalFrames {
		if d.midErr != nil {
			return 0, d.midErr
		}
		return 0, io.EOF
	}
	if d.pos >= d.fastFrames && d.gate != nil {
		<-d.gate
		if d.midErr != nil {
			return 0, d.midErr
		}
	}
	limit := d.totalFrames
	if d.pos < d.fastFrames {
		limit = min(limit, d.fastFrames)
	}
	avail := int(min(int64(len(dst)/2), limit-d.pos))
	for i := range avail {
		idx := d.pos + int64(i)
		v := float32((idx%500)+1) / 1000.0
		dst[i*2] = v
		dst[i*2+1] = -v
	}
	d.pos += int64(avail)
	return avail * 2, nil
}

// PrepareHQPTrack returns an HTTP WAV URI immediately after pre-buffering 256 KB of an active download, streams progressive HTTP 206 ranges with EOF zero-padding or mid-stream error propagation, and promotes >4 GB streams to EBU Tech 3306 RF64/ds64.
func TestHQPlayerProgressiveDownloadAndRF64Promotion(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var (
		connMu    sync.Mutex
		hqpConn   net.Conn
		addedURIs = make(chan string, 4)
	)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		connMu.Lock()
		hqpConn = conn
		connMu.Unlock()
		defer conn.Close()

		xr := newXMLFrameReader(conn)
		for {
			tag, doc, rerr := xr.Next()
			if rerr != nil {
				return
			}
			_, attrs, _ := parseXMLAttrs(doc)
			switch tag {
			case "GetInfo":
				_, _ = conn.Write([]byte(xmlDecl + `<GetInfo product="HQPlayer Desktop" version="5.10.0"/>`))
			case "SetRepeat":
				_, _ = conn.Write([]byte(xmlDecl + `<SetRepeat result="OK"/>`))
			case "VolumeRange":
				_, _ = conn.Write([]byte(xmlDecl + `<VolumeRange min="-60" max="0"/>`))
			case "Status":
				_, _ = conn.Write([]byte(xmlDecl + `<Status state="0" position="0.0"/>`))
			case "PlaylistClear":
				_, _ = conn.Write([]byte(xmlDecl + `<PlaylistClear result="OK"/>`))
			case "PlaylistAdd":
				addedURIs <- attrs["uri"]
				_, _ = conn.Write([]byte(xmlDecl + `<PlaylistAdd result="OK"/>`))
			case "Volume":
				_, _ = conn.Write([]byte(xmlDecl + `<Volume result="OK"/>`))
			case "Stop":
				_, _ = conn.Write([]byte(xmlDecl + `<Stop result="OK"/>`))
			}
		}
	}()

	sigEvents := make(chan HQPSignalInfo, 8)
	backend := NewHQPlayerBackend(srv, ln.Addr().String(), nil)
	backend.SetOnSignal(func(sig HQPSignalInfo) {
		sigEvents <- sig
	})
	defer backend.Disconnect()

	// 1) Active download: estFrames = 44100*4 (176,400 frames = 1,058,400 PCM bytes),
	// yields 50,000 frames (~300 KB > 256 KB pre-buffer) immediately and blocks on releaseGate,
	// then finishes 200 frames short of estFrames so zero-padding to estimatedDataBytes is exercised.
	releaseGate := make(chan struct{})
	const estFrames = 44100 * 4
	sink, err := backend.Open(context.Background(), player.TrackSource{
		Track: ytm.Track{
			ID:       "prog1",
			Name:     "Progressive Track",
			Artists:  []ytm.Artist{{Name: "Artist"}},
			Album:    ytm.Album{Name: "Album"},
			Duration: 4000,
		},
		Duration:    4 * time.Second,
		Gain:        1.0,
		Downloading: true,
		CoverBytes:  []byte{0xFF, 0xD8, 0xFF, 0xD9},
		OpenPCM: func() (player.PCMDecoder, error) {
			return &gatedPCMDecoder{
				rate:        44100,
				estFrames:   estFrames,
				fastFrames:  50000,
				totalFrames: estFrames - 200,
				gate:        releaseGate,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("progressive Open failed while download still in progress: %v", err)
	}
	defer sink.Close()

	gotURI := <-addedURIs
	if !strings.HasPrefix(gotURI, "http://") || !strings.HasSuffix(gotURI, ".wav") {
		t.Fatalf("progressive PlaylistAdd uri = %q, want http://...wav", gotURI)
	}

	connMu.Lock()
	conn := hqpConn
	connMu.Unlock()
	_, _ = conn.Write([]byte(xmlDecl + `<Status state="2" position="0.5" rate="705600" bits="24" channels="2" filter="poly-sinc-ext2"/>`))
	select {
	case sig := <-sigEvents:
		if sig.Transport != "wav-http" {
			t.Fatalf("progressive Signal().Transport = %q, want wav-http", sig.Transport)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for progressive HQPSignalInfo")
	}

	// Initial byte range (0..511) returns 206 immediately with upfront RIFF + JUNK + fmt + id3 before data while releaseGate is still closed!
	reqHeadRange, _ := http.NewRequest(http.MethodGet, gotURI, nil)
	reqHeadRange.Header.Set("Range", "bytes=0-511")
	resp0, err := http.DefaultClient.Do(reqHeadRange)
	if err != nil {
		t.Fatal(err)
	}
	headBytes, err := io.ReadAll(resp0.Body)
	resp0.Body.Close()
	if err != nil || resp0.StatusCode != http.StatusPartialContent || len(headBytes) != 512 {
		t.Fatalf("initial Range status=%d len=%d err=%v", resp0.StatusCode, len(headBytes), err)
	}
	if string(headBytes[:4]) != "RIFF" || string(headBytes[12:16]) != "JUNK" || string(headBytes[48:52]) != "fmt " || string(headBytes[72:76]) != "id3 " {
		t.Fatalf("unexpected progressive WAV header layout")
	}

	// Parse total size from Content-Range: bytes 0-511/<total>
	cr := resp0.Header.Get("Content-Range")
	_, totalStr, ok := strings.Cut(cr, "/")
	totalWAVSize, _ := strconv.ParseInt(totalStr, 10, 64)
	if !ok || totalWAVSize <= estFrames*6 {
		t.Fatalf("invalid Content-Range %q", cr)
	}

	// Request final 600 bytes concurrently; it waits on sync.Cond until releaseGate closes, then reads exact zero-padded tail!
	type tailResult struct {
		status int
		data   []byte
		err    error
	}
	tailCh := make(chan tailResult, 1)
	go func() {
		reqTail, _ := http.NewRequest(http.MethodGet, gotURI, nil)
		reqTail.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", totalWAVSize-600, totalWAVSize-1))
		r, doErr := http.DefaultClient.Do(reqTail)
		if doErr != nil {
			tailCh <- tailResult{err: doErr}
			return
		}
		defer r.Body.Close()
		b, rErr := io.ReadAll(r.Body)
		tailCh <- tailResult{status: r.StatusCode, data: b, err: rErr}
	}()

	close(releaseGate)
	select {
	case tr := <-tailCh:
		if tr.err != nil || tr.status != http.StatusPartialContent || len(tr.data) != 600 {
			t.Fatalf("tail Range status=%d len=%d err=%v", tr.status, len(tr.data), tr.err)
		}
		// Final 600 bytes lie within the 200*6 = 1200 zero-padded EOF tail.
		for i, b := range tr.data {
			if b != 0 {
				t.Fatalf("expected zero-padded EOF byte at tail[%d], got %d", i, b)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for progressive tail Range response")
	}

	// 2) Mid-stream decoder error before EOF fails reads past written rather than zero-padding.
	errGate := make(chan struct{})
	_, _, errWAVURL, _, _, err := srv.PrepareHQPTrack(context.Background(), player.TrackSource{
		Track:       ytm.Track{ID: "prog_err", Name: "Broken Stream", Duration: 4000},
		Duration:    4 * time.Second,
		Downloading: true,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &gatedPCMDecoder{
				rate:        44100,
				estFrames:   estFrames,
				fastFrames:  50000,
				totalFrames: 60000,
				gate:        errGate,
				midErr:      errors.New("upstream stream reset"),
			}, nil
		},
	}, "127.0.0.1:4321")
	if err != nil {
		t.Fatal(err)
	}
	close(errGate)
	for start := time.Now(); time.Since(start) < time.Second; time.Sleep(5 * time.Millisecond) {
		if e := srv.lookupHQP(2); e != nil && e.prog != nil {
			if failed, _, _ := e.prog.failureState(); failed {
				break
			}
		}
	}
	reqPast, _ := http.NewRequest(http.MethodGet, errWAVURL, nil)
	reqPast.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", estFrames*6-120, estFrames*6-1))
	respPast, err := http.DefaultClient.Do(reqPast)
	if err != nil {
		t.Fatal(err)
	}
	respPast.Body.Close()
	if respPast.StatusCode == http.StatusPartialContent || respPast.StatusCode == http.StatusOK {
		t.Fatalf("Range past mid-stream error returned %d, want error status", respPast.StatusCode)
	}

	// 3) >4 GB stream promotes "RIFF"+"JUNK" to EBU Tech 3306 "RF64"+"ds64" without silent truncation.
	holdRF64 := make(chan struct{})
	defer close(holdRF64)
	const rf64Frames int64 = 720_000_000 // 720M * 6 = 4,320,000,000 bytes > math.MaxUint32
	_, _, rf64URL, _, _, err := srv.PrepareHQPTrack(context.Background(), player.TrackSource{
		Track:       ytm.Track{ID: "rf64_mix", Name: "Long Mix", Duration: 7500000},
		Duration:    7500 * time.Second,
		Downloading: true,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &gatedPCMDecoder{
				rate:        96000,
				estFrames:   rf64Frames,
				fastFrames:  50000,
				totalFrames: rf64Frames,
				gate:        holdRF64,
			}, nil
		},
	}, "127.0.0.1:4321")
	if err != nil {
		t.Fatalf("PrepareHQPTrack >4GB RF64: %v", err)
	}
	reqRF64, _ := http.NewRequest(http.MethodGet, rf64URL, nil)
	reqRF64.Header.Set("Range", "bytes=0-127")
	respRF64, err := http.DefaultClient.Do(reqRF64)
	if err != nil {
		t.Fatal(err)
	}
	rf64Hdr, _ := io.ReadAll(respRF64.Body)
	respRF64.Body.Close()
	if string(rf64Hdr[0:4]) != "RF64" || binary.LittleEndian.Uint32(rf64Hdr[4:8]) != 0xFFFFFFFF || string(rf64Hdr[8:12]) != "WAVE" {
		t.Fatalf("expected RF64/WAVE magic with 0xFFFFFFFF size32")
	}
	if string(rf64Hdr[12:16]) != "ds64" || binary.LittleEndian.Uint32(rf64Hdr[16:20]) != 28 {
		t.Fatalf("expected 28-byte ds64 chunk at offset 12, got %q", string(rf64Hdr[12:16]))
	}
	gotDataSize64 := binary.LittleEndian.Uint64(rf64Hdr[28:36])
	gotSamples64 := binary.LittleEndian.Uint64(rf64Hdr[36:44])
	if gotDataSize64 != uint64(rf64Frames*6) || gotSamples64 != uint64(rf64Frames) {
		t.Fatalf("ds64 dataSize=%d samples=%d, want %d and %d", gotDataSize64, gotSamples64, rf64Frames*6, rf64Frames)
	}

	// Also verify materialized in-place promotion when riffPayload > math.MaxUint32 even if pcmBytes <= math.MaxUint32.
	id3 := buildWAVID3Chunk("Boundary", "Artist", "Album", nil)
	boundaryPCM := int64(math.MaxUint32 - 12)
	patchedPath := filepath.Join(t.TempDir(), "boundary.wav")
	pf, err := os.OpenFile(patchedPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pf.Write(encodeWAVContainerHeader(48000, 2, 24, id3, 0))
	if err := finalizeHQPWAVHeader(pf, 48000, id3, boundaryPCM); err != nil {
		pf.Close()
		t.Fatal(err)
	}
	pf.Close()
	patchedHdr, err := os.ReadFile(patchedPath)
	if err != nil {
		t.Fatal(err)
	}
	dataOff := 72 + len(id3)
	if string(patchedHdr[0:4]) != "RF64" || string(patchedHdr[12:16]) != "ds64" || binary.LittleEndian.Uint32(patchedHdr[dataOff+4:dataOff+8]) != 0xFFFFFFFF {
		t.Fatalf("boundary riffPayload > MaxUint32 did not promote to RF64/ds64")
	}
}

type memRawStream struct {
	*bytes.Reader
	size   int64
	closed *atomic.Int32
}

func (m *memRawStream) Size() (int64, error) { return m.size, nil }
func (m *memRawStream) Close() error {
	if m.closed != nil {
		m.closed.Add(1)
	}
	return nil
}

type closeTrackPCMDecoder struct {
	rampPCMDecoder
	closed *atomic.Int32
}

func (d *closeTrackPCMDecoder) Close() error {
	if d.closed != nil {
		d.closed.Add(1)
	}
	return nil
}

// RoonBackend in AAC mode or with AAC-only listeners rejects non-AAC tracks while reusing validated ADTS readers for AAC-in-MP4, runRoonClock advances non-ADTS sinks cleanly if an AAC listener connects mid-track, and FetchLibraryGenres waits for cold-start pairing.
func TestRoonAACRejectsIncompatibleTracksAndWaitPaired(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	rb := NewRoonBackend(srv, nil, "")
	if rb.StreamFormat() != "wav" {
		t.Fatalf("default StreamFormat() = %q, want wav", rb.StreamFormat())
	}
	rb.SetStreamFormat("aac")
	if rb.StreamFormat() != "aac" {
		t.Fatalf("StreamFormat() = %q, want aac", rb.StreamFormat())
	}

	// 1) Local FLAC (OpenRaw == nil) in AAC mode closes dec and returns clear guidance error.
	var flacDecClosed atomic.Int32
	_, err = rb.Open(context.Background(), player.TrackSource{
		Track:    ytm.Track{ID: "localfile:flac1", Name: "Lossless", Duration: 2000},
		Duration: 2 * time.Second,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &closeTrackPCMDecoder{rampPCMDecoder: rampPCMDecoder{rate: 44100, frames: 88200}, closed: &flacDecClosed}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "当前曲目不支持 Roon AAC 直通") || flacDecClosed.Load() != 1 {
		t.Fatalf("FLAC on Roon AAC: err=%v decClosed=%d", err, flacDecClosed.Load())
	}

	// 2) Invalid/non-AAC OpenRaw (e.g. ALAC or MP3 bytes) closes both raw and dec and returns clear error.
	var badDecClosed, badRawClosed atomic.Int32
	badBytes := []byte("fLaC\x00\x00\x00\x22not-mp4-aac")
	_, err = rb.Open(context.Background(), player.TrackSource{
		Track:    ytm.Track{ID: "localfile:alac1", Name: "ALAC", Duration: 2000},
		Duration: 2 * time.Second,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &closeTrackPCMDecoder{rampPCMDecoder: rampPCMDecoder{rate: 44100, frames: 88200}, closed: &badDecClosed}, nil
		},
		OpenRaw: func() (player.RawStream, error) {
			return &memRawStream{Reader: bytes.NewReader(badBytes), size: int64(len(badBytes)), closed: &badRawClosed}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "当前曲目不支持 Roon AAC 直通") || badDecClosed.Load() != 1 || badRawClosed.Load() != 1 {
		t.Fatalf("non-AAC raw on Roon AAC: err=%v decClosed=%d rawClosed=%d", err, badDecClosed.Load(), badRawClosed.Load())
	}

	// 3) Valid AAC-in-MP4 succeeds and reuses the single validated RawStream + ADTS reader without opening twice.
	m4aBytes, err := os.ReadFile("../player/testdata/seek.m4a")
	if err != nil {
		t.Fatal(err)
	}
	var openRawCalls atomic.Int32
	validSink, err := rb.Open(context.Background(), player.TrackSource{
		Track:    ytm.Track{ID: "aac_ok", Name: "AAC Song", Duration: 3000},
		Duration: 3 * time.Second,
		Gain:     1.0,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &rampPCMDecoder{rate: 44100, frames: 44100 * 3}, nil
		},
		OpenRaw: func() (player.RawStream, error) {
			openRawCalls.Add(1)
			return &memRawStream{Reader: bytes.NewReader(m4aBytes), size: int64(len(m4aBytes))}, nil
		},
	})
	if err != nil {
		t.Fatalf("valid AAC-in-MP4 on Roon AAC failed: %v", err)
	}
	rsValid := validSink.(*roonSink)
	rsValid.stateMu.Lock()
	rsValid.playing = true
	rsValid.stateMu.Unlock()
	pkt := rsValid.pullADTS(true)
	validSink.Close()
	if len(pkt) < 7 || pkt[0] != 0xFF || (pkt[1]&0xF0) != 0xF0 || openRawCalls.Load() != 1 {
		t.Fatalf("pullADTS len=%d openRawCalls=%d, want valid ADTS packet and 1 OpenRaw call", len(pkt), openRawCalls.Load())
	}

	// 4) With StreamFormat("wav"), if a FLAC sink is already playing and an AAC-only listener connects,
	// HasOnlyAACListeners() becomes true (rejecting subsequent non-AAC Opens) while runRoonClock advances/ends the active sink cleanly instead of freezing at 0:00!
	rb.SetStreamFormat("wav")
	shortSink, err := rb.Open(context.Background(), player.TrackSource{
		Track:    ytm.Track{ID: "localfile:short", Name: "Short FLAC", Duration: 60},
		Duration: 60 * time.Millisecond,
		Gain:     1.0,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &rampPCMDecoder{rate: 44100, frames: 882 * 2}, nil // 40ms (2 clock ticks)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	endedCh := make(chan struct{}, 1)
	shortSink.OnSinkEnd(func() { endedCh <- struct{}{} })
	shortSink.Activate()

	aacCtx, aacCancel := context.WithCancel(context.Background())
	aacReq, _ := http.NewRequestWithContext(aacCtx, http.MethodGet, srv.RoonAACURL(""), nil)
	aacResp, err := http.DefaultClient.Do(aacReq)
	if err != nil {
		aacCancel()
		t.Fatal(err)
	}
	defer func() {
		aacCancel()
		aacResp.Body.Close()
	}()
	for start := time.Now(); !srv.HasOnlyAACListeners() && time.Since(start) < time.Second; time.Sleep(5 * time.Millisecond) {
	}
	if !srv.HasOnlyAACListeners() {
		t.Fatal("HasOnlyAACListeners() = false with active /roon/live.aac subscriber")
	}

	// Start playing short FLAC while ONLY the AAC listener is connected: playhead must advance and fire OnSinkEnd!
	shortSink.Play()
	select {
	case <-endedCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("non-ADTS sink froze at %v with AAC-only listener instead of ending cleanly", shortSink.Position())
	}
	shortSink.Close()

	// Opening another non-AAC track while HasOnlyAACListeners() is true must also fail upfront.
	if _, err := rb.Open(context.Background(), player.TrackSource{
		Track:    ytm.Track{ID: "localfile:flac2", Name: "Another FLAC", Duration: 1000},
		Duration: time.Second,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &rampPCMDecoder{rate: 44100, frames: 44100}, nil
		},
	}); err == nil || !strings.Contains(err.Error(), "当前曲目不支持 Roon AAC 直通") {
		t.Fatalf("expected AAC-only listener rejection, got %v", err)
	}

	// 5) Cold-start FetchLibraryGenres waits via WaitPaired(ctx) for delayed discovery + MOO pairing.
	mux := http.NewServeMux()
	mux.Handle("/api", websocket.Handler(func(ws *websocket.Conn) {
		for {
			var raw []byte
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			msg, ok := parseMOOFrame(raw)
			if !ok {
				continue
			}
			send := func(verb, name string, payload any) {
				var body []byte
				if payload != nil {
					body, _ = json.Marshal(payload)
				}
				var buf bytes.Buffer
				fmt.Fprintf(&buf, "MOO/1 %s %s\nRequest-Id: %s\n", verb, name, msg.RequestID)
				if len(body) > 0 {
					fmt.Fprintf(&buf, "Content-Length: %d\nContent-Type: application/json\n", len(body))
				}
				buf.WriteByte('\n')
				buf.Write(body)
				_ = websocket.Message.Send(ws, buf.Bytes())
			}
			switch msg.Name {
			case "com.roonlabs.registry:1/info":
				send("COMPLETE", "Success", map[string]any{"core_id": "cold-core", "display_name": "Cold Core"})
			case "com.roonlabs.registry:1/register":
				send("CONTINUE", "Registered", map[string]any{"core_id": "cold-core", "display_name": "Cold Core", "token": "t1"})
			case "com.roonlabs.transport:2/subscribe_zones":
				send("CONTINUE", "Subscribed", map[string]any{"zones": []RoonZone{}})
			case "com.roonlabs.browse:1/browse":
				send("COMPLETE", "Success", map[string]any{"action": "list"})
			case "com.roonlabs.browse:1/load":
				var req struct {
					Offset int `json:"offset"`
				}
				_ = json.Unmarshal(msg.Body, &req)
				send("COMPLETE", "Loaded", map[string]any{"items": []map[string]any{}})
			}
		}
	}))
	coreSrv := httptest.NewServer(mux)
	defer coreSrv.Close()

	origSOOD := DiscoverSOODFunc
	DiscoverSOODFunc = func(time.Duration) (string, string, error) {
		return "", "", errors.New("not yet")
	}
	defer func() { DiscoverSOODFunc = origSOOD }()

	mgr := NewRoonManager(nil, RoonCallbacks{})
	defer mgr.Close()

	// Verify canceled ctx fails WaitPaired immediately.
	canceledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := mgr.WaitPaired(canceledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitPaired(canceled) = %v, want context.Canceled", err)
	}

	// Launch FetchLibraryGenres BEFORE mgr.Start() / Core connection, then connect Core asynchronously.
	genreErrCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, gerr := mgr.FetchLibraryGenres(ctx)
		genreErrCh <- gerr
	}()

	time.AfterFunc(40*time.Millisecond, func() {
		mgr.SetDirectCoreAddr(strings.TrimPrefix(coreSrv.URL, "http://"))
		mgr.Start()
	})

	select {
	case gerr := <-genreErrCh:
		if gerr != nil {
			t.Fatalf("cold-start FetchLibraryGenres failed instead of waiting for pairing: %v", gerr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cold-start FetchLibraryGenres")
	}
}
