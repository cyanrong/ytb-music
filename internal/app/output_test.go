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
}
