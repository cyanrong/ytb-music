package app

import (
	"context"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"ytb-music/internal/bridge"
	"ytb-music/internal/ytm"
)

// Switching output modes from the player bar popover attaches the selected external backend and cleans up on return to local output.
func TestPlayerBarOutputPopoverSwitchesBackends(t *testing.T) {
	realNewSrv := newBridgeServer
	newBridgeServer = func(_ string) (*bridge.Server, error) {
		return bridge.NewServer("127.0.0.1:0")
	}
	defer func() { newBridgeServer = realNewSrv }()

	a := editTestApp()
	defer a.closeAudioChain()

	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()

	if err := tt.Click("输出设备"); err != nil {
		t.Fatal(err)
	}
	tt.Advance(300 * time.Millisecond)
	tt.Frame()

	if err := tt.Click("HQPlayer 直推"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.settings.OutputMode != outputHQPlayer || a.pl.Backend() == nil || a.pl.Backend().Name() != "hqplayer" {
		t.Fatalf("mode=%q backend=%v, want hqplayer", a.settings.OutputMode, a.pl.Backend())
	}

	if err := tt.Click("Roon 联动桥接"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.settings.OutputMode != outputRoon || a.pl.Backend() == nil || a.pl.Backend().Name() != "roon" {
		t.Fatalf("mode=%q backend=%v, want roon", a.settings.OutputMode, a.pl.Backend())
	}
	boundAddr := a.audio.srv.Addr()
	if boundAddr == "" {
		t.Fatal("bridge server did not report a bound address")
	}

	if err := tt.Click("本机输出"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.settings.OutputMode != outputLocal || a.pl.Backend() != nil {
		t.Fatalf("mode=%q backend=%v, want local nil backend", a.settings.OutputMode, a.pl.Backend())
	}
	// Verify the bridge listening port was synchronously released when returning to local output.
	ln, err := net.Listen("tcp", boundAddr)
	if err != nil {
		t.Fatalf("bridge port %s remained occupied after switching to local output: %v", boundAddr, err)
	}
	_ = ln.Close()
}

// Probing HQPlayer and copying the Roon Live Radio stream URL in Settings update status and clipboard off the UI thread.
func TestSettingsAudioChainProbeAndCopyURL(t *testing.T) {
	realNewSrv, realProbe, realCopy := newBridgeServer, probeHQPlayer, copyText
	newBridgeServer = func(_ string) (*bridge.Server, error) {
		return bridge.NewServer("127.0.0.1:0")
	}
	probeHQPlayer = func(_ context.Context, addr string) (string, error) {
		return "HQPlayer Desktop 5.9 (" + addr + ")", nil
	}
	var copied string
	copyText = func(s string) error { copied = s; return nil }
	defer func() {
		newBridgeServer = realNewSrv
		probeHQPlayer = realProbe
		copyText = realCopy
	}()

	posts := make(chan func(), 16)
	a := newTestApp()
	a.settings = defaultSettings()
	a.postHook = func(fn func()) { posts <- fn }
	defer a.closeAudioChain()

	tt := ui.NewTester(a.settingsView, 580, 2200)
	tt.Frame()

	if err := tt.Click("HQPlayer"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()

	a.audio.hqpInput = "192.168.1.88:4321"
	tt.Frame()
	if err := tt.Click("保存"); err != nil {
		t.Fatal(err)
	}
	drainPosts(t, posts, func() bool { return !a.audio.hqpTesting && a.audio.hqpConnected })
	tt.Frame()

	if a.settings.HQPlayerAddr != "192.168.1.88:4321" || !strings.Contains(a.audio.hqpStatus, "192.168.1.88:4321") {
		t.Fatalf("HQPlayer addr=%q status=%q", a.settings.HQPlayerAddr, a.audio.hqpStatus)
	}

	if err := tt.Click("Roon"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("复制地址"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !strings.HasSuffix(copied, "/roon/live.wav") {
		t.Fatalf("copied = %q, want /roon/live.wav URL", copied)
	}
}

// With NM_FRAMES set, renders the output popover and Settings audio chain views to PNGs.
func TestAudioOutputFrames(t *testing.T) {
	dir := os.Getenv("NM_FRAMES")
	if dir == "" {
		t.Skip()
	}
	realNewSrv := newBridgeServer
	newBridgeServer = func(_ string) (*bridge.Server, error) {
		return bridge.NewServer("127.0.0.1:0")
	}
	defer func() { newBridgeServer = realNewSrv }()

	a := editTestApp()
	defer a.closeAudioChain()
	a.setOutputMode(outputRoon)
	a.audio.roonStatus = bridge.RoonStatus{
		Discovered: true,
		Paired:     true,
		CoreName:   "Desktop Roon Core",
		Zones: []bridge.RoonZone{
			{ZoneID: "z1", DisplayName: "HQPlayer ASIO DSD256", State: "playing"},
			{ZoneID: "z2", DisplayName: "桌面监听音箱", State: "stopped"},
		},
		Detail: "已连接 Roon Core：Desktop Roon Core",
	}
	a.settings.RoonZoneID = "z1"
	a.settings.RoonZoneName = "HQPlayer ASIO DSD256"
	a.audio.open = true

	tt := ui.NewTester(a.view, 1200, 800)
	tt.SetDark(true)
	tt.Advance(400 * time.Millisecond)
	tt.Frame()
	f1, err := os.Create(filepath.Join(dir, "output-popover-dark.png"))
	if err != nil {
		t.Fatal(err)
	}
	_ = png.Encode(f1, tt.Image())
	f1.Close()

	st := ui.NewTester(a.settingsView, 580, 1050)
	st.SetDark(true)
	st.Advance(400 * time.Millisecond)
	st.Frame()
	f2, err := os.Create(filepath.Join(dir, "settings-audio-roon-dark.png"))
	if err != nil {
		t.Fatal(err)
	}
	_ = png.Encode(f2, st.Image())
	f2.Close()

	a.setOutputMode(outputHQPlayer)
	a.audio.hqpConnected = true
	a.audio.hqpStatus = "已连接 HQPlayer Desktop 5.9.0"
	st.Frame()
	st.Advance(400 * time.Millisecond)
	st.Frame()
	f3, err := os.Create(filepath.Join(dir, "settings-audio-hqp-dark.png"))
	if err != nil {
		t.Fatal(err)
	}
	_ = png.Encode(f3, st.Image())
	f3.Close()

	a.pl.Cue([]ytm.Track{{
		ID:       "sig-shot",
		Name:     "Hotel California (Live)",
		Artists:  []ytm.Artist{{Name: "Eagles"}},
		Duration: 432000,
	}})
	a.pl.SetLyrics(nil)
	a.pl.SetStream(&ytm.Stream{
		Codec:      "FLAC",
		SampleRate: 96000,
		BitDepth:   24,
		Bitrate:    2845,
	})
	a.audio.hqpSignal = bridge.HQPSignalInfo{
		Active:     true,
		State:      1,
		Mode:       "SDM (DSD)",
		Rate:       11289600,
		Bits:       1,
		Channels:   2,
		Filter:     "poly-sinc-gauss-long",
		Shaper:     "ASDM7EC-super 512+fs",
		Clips:      2,
		SourceRate: 96000,
		SourceBits: 24,
		Transport:  "file",
	}
	a.audio.open = true
	tt.Advance(400 * time.Millisecond)
	tt.Frame()
	f4, err := os.Create(filepath.Join(dir, "signal-path-hqp-dark.png"))
	if err != nil {
		t.Fatal(err)
	}
	_ = png.Encode(f4, tt.Image())
	f4.Close()

	a.audio.open = false
	a.nowPlaying = true
	tt.Frame()
	tt.Advance(time.Second)
	tt.Frame()
	f5, err := os.Create(filepath.Join(dir, "nowplaying-signal-hqp-dark.png"))
	if err != nil {
		t.Fatal(err)
	}
	_ = png.Encode(f5, tt.Image())
	f5.Close()
}

// Clicking the player bar signal pill opens the Signal Path popover with live HQPlayer upsampling telemetry without requesting idle frames.
func TestSignalPillOpensSignalPathPopover(t *testing.T) {
	realNewSrv := newBridgeServer
	newBridgeServer = func(_ string) (*bridge.Server, error) {
		return bridge.NewServer("127.0.0.1:0")
	}
	defer func() { newBridgeServer = realNewSrv }()

	a := editTestApp()
	defer a.closeAudioChain()

	a.setOutputMode(outputHQPlayer)
	stream := &ytm.Stream{
		Codec:      "AAC",
		Level:      "exhigh",
		SampleRate: 44100,
		Bitrate:    256,
	}
	a.pl.Cue([]ytm.Track{{
		ID:       "sig1",
		Name:     "Hotel California",
		Artists:  []ytm.Artist{{Name: "Eagles"}},
		Duration: 200000,
	}})
	a.pl.SetLyrics(nil)
	a.pl.SetStream(stream)
	a.audio.hqpSignal = bridge.HQPSignalInfo{
		Active:        true,
		State:         1,
		Mode:          "SDM (DSD)",
		Rate:          11289600,
		Bits:          1,
		Channels:      2,
		Filter:        "poly-sinc-gauss-long",
		Shaper:        "ASDM7EC-super 512+fs",
		Clips:         3,
		SourceRate:    44100,
		SourceBits:    24,
		SourceBitrate: 2116800,
	}

	label, gold, tip := a.signalBarLabel()
	if label != "AAC 256kbps · 44.1kHz → DSD256" || !gold {
		t.Fatalf("signalBarLabel() = (%q, %v), want (\"AAC 256kbps · 44.1kHz → DSD256\", true)", label, gold)
	}
	if tip != "DSD256 · poly-sinc-gauss-long · ASDM7EC-super 512+fs" {
		t.Fatalf("tooltip = %q", tip)
	}

	tt := ui.NewTester(a.view, 1200, 800)
	tt.SetDark(true)
	tt.Advance(time.Second)
	tt.Frame()
	tt.Advance(time.Second)
	if tt.FramePending() {
		t.Fatal("player bar with signal pill asks for frames while idle")
	}

	if !tt.HasText("AAC 256kbps · 44.1kHz → DSD256") {
		t.Fatalf("missing player bar signal pill in %v", tt.Texts())
	}

	if err := tt.Click("AAC 256kbps · 44.1kHz → DSD256"); err != nil {
		t.Fatal(err)
	}
	tt.Advance(300 * time.Millisecond)
	tt.Frame()
	if !a.audio.open {
		t.Fatal("clicking signal pill did not open output popover")
	}

	// Clicking the pill again closes the popover cleanly.
	if err := tt.Click("AAC 256kbps · 44.1kHz → DSD256"); err != nil {
		t.Fatal(err)
	}
	tt.Advance(300 * time.Millisecond)
	tt.Frame()
	if a.audio.open {
		t.Fatal("second click on signal pill did not close output popover")
	}

	// Now Playing displays the condensed HQPlayer upsampling badge, and clears hqpSignal when returning to local output.
	a.nowPlaying = true
	tt.Frame()
	tt.Advance(time.Second)
	tt.Frame()
	if !tt.HasText("AAC 256k → DSD256 · gauss-long · ASDM7EC-super") {
		t.Fatalf("Now Playing missing HQPlayer upsampling badge in %v", tt.Texts())
	}

	a.setOutputMode(outputLocal)
	if a.audio.hqpSignal.Active {
		t.Fatal("switching away from HQPlayer did not clear hqpSignal")
	}
	a.pl.SetStream(stream)
	tt.Frame()
	if !tt.HasText("AAC 256kbps · 44.1kHz") {
		t.Fatalf("Now Playing missing source stream badge after local fallback in %v", tt.Texts())
	}
}

// Signal labels and Signal Path nodes adapt across Roon streaming, native lossless HQPlayer pass-through, and narrow window widths.
func TestSignalBarLabelModesAndPassThroughNode(t *testing.T) {
	realNewSrv := newBridgeServer
	newBridgeServer = func(_ string) (*bridge.Server, error) {
		return bridge.NewServer("127.0.0.1:0")
	}
	defer func() { newBridgeServer = realNewSrv }()

	a := editTestApp()
	defer a.closeAudioChain()

	flac := &ytm.Stream{
		Codec:      "FLAC",
		SampleRate: 96000,
		BitDepth:   24,
		Bitrate:    2845,
	}
	a.setOutputMode(outputRoon)
	a.audio.roonStatus.Listeners = 1
	a.pl.Cue([]ytm.Track{{ID: "flac1", Name: "Time", Artists: []ytm.Artist{{Name: "Pink Floyd"}}, Duration: 413000}})
	a.pl.SetLyrics(nil)
	a.pl.SetStream(flac)

	if got, gold, _ := a.signalBarLabel(); got != "FLAC 96kHz/24b · 2845kbps → Roon 24/44.1" || !gold {
		t.Fatalf("Roon WAV signalBarLabel = (%q, %v)", got, gold)
	}
	a.settings.RoonStreamFormat = "aac"
	if got, gold, _ := a.signalBarLabel(); got != "FLAC 96kHz/24b · 2845kbps → Roon AAC" || gold {
		t.Fatalf("Roon AAC signalBarLabel = (%q, %v)", got, gold)
	}

	a.setOutputMode(outputHQPlayer)
	a.pl.SetStream(flac)
	a.audio.hqpSignal = bridge.HQPSignalInfo{
		Active:     true,
		State:      1,
		Mode:       "SDM (DSD)",
		Rate:       22579200,
		Bits:       1,
		Filter:     "poly-sinc-gauss-xla",
		Shaper:     "ASDM7EC-super 512+fs",
		SourceRate: 96000,
		SourceBits: 24,
		Transport:  "file",
	}

	// Narrow window (< 960 DIPs) uses compact SpecBadge() on the player bar pill.
	narrow := ui.NewTester(a.view, 900, 620)
	narrow.Frame()
	if !narrow.HasText("FLAC 24/96 → DSD512") {
		t.Fatalf("narrow window missing compact signal pill in %v", narrow.Texts())
	}
	if err := narrow.Click("FLAC 24/96 → DSD512"); err != nil {
		t.Fatal(err)
	}
	narrow.Advance(300 * time.Millisecond)
	narrow.Frame()
	if !narrow.HasText("原生文件直通 (file:///)") {
		t.Fatalf("Signal Path popover missing native file pass-through node in %v", narrow.Texts())
	}

	// A DSD source converted to HTTP WAV for a remote HQPlayer reports WAV HTTP rather than native file pass-through.
	a.pl.SetStream(&ytm.Stream{Codec: "DSD128", SampleRate: 5644800, BitDepth: 1, IsDSD: true})
	a.audio.hqpSignal.Transport = "wav-http"
	narrow.Frame()
	if narrow.HasText("原生文件直通 (file:///)") || !narrow.HasText("24-bit PCM WAV (HTTP)") {
		t.Fatalf("remote DSD HTTP stream reported wrong transport in %v", narrow.Texts())
	}
}
