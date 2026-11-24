package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	localPath := filepath.FromSlash(strings.TrimPrefix(gotURI, "file:///"))
	if filepath.Separator == '/' {
		localPath = "/" + strings.TrimPrefix(gotURI, "file:///")
	}
	wavBytes, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("reading materialized WAV %q: %v", localPath, err)
	}
	if string(wavBytes[:4]) != "RIFF" || string(wavBytes[8:12]) != "WAVE" {
		t.Fatalf("invalid RIFF/WAVE header in materialized file")
	}
	dataBytes := binary.LittleEndian.Uint32(wavBytes[40:44])
	if dataBytes != totalFrames*6 {
		t.Fatalf("WAV data chunk = %d bytes, want %d", dataBytes, totalFrames*6)
	}
	tail := wavBytes[44+dataBytes:]
	if !bytes.Contains(tail, []byte("id3 ")) || !bytes.Contains(tail, []byte("グッドバイ")) || !bytes.Contains(tail, []byte("New Sentimentality - EP")) || !bytes.Contains(tail, []byte("APIC")) {
		t.Fatalf("WAV id3 chunk missing UTF-8 metadata or APIC cover art")
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
	byteOff := int64(44 + 44100*6) // frame 44100
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
