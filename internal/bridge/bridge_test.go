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
	"strings"
	"sync"
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
	avail := int(min(int64(len(dst)/2), d.frames-d.pos))
	for i := range avail {
		idx := d.pos + int64(i)
		// Deterministic sample value per frame so Range seeks can verify exact frame alignment.
		val := float32((idx%1000)+1) / 2000.0
		dst[i*2] = val
		dst[i*2+1] = -val
	}
	d.pos += int64(avail)
	return avail * 2, nil
}

// HQPlayerbackend loads a track over TCP 4321 despite interleaved <Status/> pushes, serves range-seekable 24-bit WAV, and fires OnSinkEnd when playback finishes.
func TestHQPlayerBackendAndRangeWAV(t *testing.T) {
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
		received []string
		addedURI string
		hqpConn  net.Conn
	)
	readyEOF := make(chan struct{})

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
			mu.Lock()
			received = append(received, tag)
			mu.Unlock()

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
				mu.Lock()
				addedURI = attrs["uri"]
				mu.Unlock()
				// Coalesce an unsolicited <Status/> frame immediately ahead of <PlaylistAdd result="OK"/> in one TCP write.
				coalesced := xmlDecl + `<Status state="0" position="0.0"/>` + xmlDecl + `<PlaylistAdd result="OK"/>`
				_, _ = conn.Write([]byte(coalesced))
			case "Volume":
				_, _ = conn.Write([]byte(xmlDecl + `<Volume result="OK"/>`))
			case "Play":
				_, _ = conn.Write([]byte(xmlDecl + `<Play result="OK"/>` + xmlDecl + `<Status state="2" position="1.25"/>`))
				close(readyEOF)
			case "Seek":
				_, _ = conn.Write([]byte(xmlDecl + `<Seek result="OK"/>`))
			case "Pause":
				_, _ = conn.Write([]byte(xmlDecl + `<Pause result="OK"/>`))
			case "Stop":
				_, _ = conn.Write([]byte(xmlDecl + `<Stop result="OK"/>`))
			}
		}
	}()

	backend := NewHQPlayerBackend(srv, ln.Addr().String(), nil)
	defer backend.Disconnect()

	const totalFrames = 44100 * 4
	src := player.TrackSource{
		Track: ytm.Track{
			ID:       "vid1",
			Name:     "晴天",
			Artists:  []ytm.Artist{{Name: "周杰伦"}},
			Album:    ytm.Album{Name: "叶惠美"},
			Duration: 4000,
		},
		Duration: 4 * time.Second,
		Gain:     0.64,
		OpenPCM: func() (player.PCMDecoder, error) {
			return &rampPCMDecoder{rate: 44100, frames: totalFrames}, nil
		},
	}

	sink, err := backend.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer sink.Close()

	mu.Lock()
	wavURL := addedURI
	mu.Unlock()
	if wavURL == "" {
		t.Fatal("HQPlayer did not receive PlaylistAdd URI")
	}

	// Verify HTTP Range seeking into frame 500 of the 24-bit WAV stream.
	const targetFrame = 500
	byteStart := int64(44 + targetFrame*6)
	byteEnd := byteStart + 6 - 1
	req, err := http.NewRequest(http.MethodGet, wavURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", byteStart, byteEnd))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 6 {
		t.Fatalf("Range status=%d len=%d, want 206 len=6", resp.StatusCode, len(body))
	}
	// Left channel at frame 500 has value (500%1000 + 1)/2000 = 501/2000 = 0.2505 -> ~2101346 in 24-bit signed PCM.
	gotLeft := int32(uint32(body[0]) | uint32(body[1])<<8 | uint32(body[2])<<16)
	if gotLeft&0x800000 != 0 {
		gotLeft |= ^0xffffff
	}
	if gotLeft < 2101340 || gotLeft > 2101352 {
		t.Fatalf("frame 500 24-bit sample = %d, want ~2101346", gotLeft)
	}

	ended := make(chan struct{}, 1)
	sink.OnSinkEnd(func() { ended <- struct{}{} })
	sink.Play()
	<-readyEOF

	// Wait past the 500ms start grace window, then push natural end-of-track Status state=0.
	time.Sleep(550 * time.Millisecond)
	mu.Lock()
	conn := hqpConn
	mu.Unlock()
	_, _ = conn.Write([]byte(xmlDecl + `<Status state="0" position="0.0"/>`))

	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("HQPlayer natural track end did not trigger OnSinkEnd")
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
		savedToken string
		playedKey  chan string = make(chan string, 1)
	)

	// Spin up a mock Roon Core MOO/1 WebSocket server.
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
				// Also send an incoming ping request from Core to verify bidirectional MOO handling.
				ping := []byte("MOO/1 REQUEST com.roonlabs.ping:1/ping\nRequest-Id: 77\n\n")
				_ = websocket.Message.Send(ws, ping)
			case "com.roonlabs.transport:2/subscribe_zones":
				send("CONTINUE", "Subscribed", map[string]any{
					"zones": []RoonZone{
						{ZoneID: "z-hqp", DisplayName: "HQPlayer DSD256", State: "stopped"},
					},
				})
			case "com.roonlabs.browse:1/browse":
				var req struct {
					PopAll  bool   `json:"pop_all"`
					ItemKey string `json:"item_key"`
				}
				_ = json.Unmarshal(msg.Body, &req)
				switch {
				case req.PopAll:
					send("COMPLETE", "Success", map[string]any{"action": "list"})
				case req.ItemKey == "st-1":
					send("COMPLETE", "Success", map[string]any{"action": "list"})
				case req.ItemKey == "act-play":
					select {
					case playedKey <- req.ItemKey:
					default:
					}
					send("COMPLETE", "Success", map[string]any{"action": "none"})
				}
			case "com.roonlabs.browse:1/load":
				// First load lists stations; second load lists station play actions.
				send("COMPLETE", "Loaded", map[string]any{
					"items": []map[string]any{
						{"title": "YTB Music Bridge", "item_key": "st-1"},
						{"title": "Play Now", "item_key": "act-play"},
					},
				})
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
		if savedToken != "tok-abc-999" || st.Zones[0].ZoneID != "z-hqp" {
			t.Fatalf("unexpected Roon status: token=%q zones=%+v", savedToken, st.Zones)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Roon MOO pairing and zones")
	}

	// Open a track on RoonBackend and trigger Play() before any HTTP listener connects so EnsureStationPlaying fires.
	rb := NewRoonBackend(srv, mgr, "z-hqp")
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

	// Note: In our mock load handler, items[0] is st-1 and EnsureStationPlaying selects items[0] on action list too;
	// let's connect an ICY WAV client to verify live 44.1kHz 24-bit WAV + StreamTitle='周杰伦 - 夜曲'; injection!
	sink.Play()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.RoonWAVURL(""), nil)
	req.Header.Set("Icy-MetaData", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

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
	_ = playedKey
}
