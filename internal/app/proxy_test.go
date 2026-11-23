package app

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"ytb-music/internal/ytm"
)

// Choosing a proxy mode in Settings validates custom proxy addresses and
// updates the active proxy setting.
func TestSettingsProxy(t *testing.T) {
	defer func() { _ = ytm.SetProxy("") }()
	a := newTestApp()
	a.settings = defaultSettings()
	a.syncProxyState()
	tt := ui.NewTester(a.settingsView, 560, 2000)
	tt.Frame()

	if err := tt.Click("自定义"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()

	a.proxyInput = "ftp://bad"
	tt.Frame()
	if err := tt.Click("应用"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.proxyErr == "" || a.settings.Proxy != "" {
		t.Fatalf("invalid proxy accepted: err=%q proxy=%q", a.proxyErr, a.settings.Proxy)
	}

	a.proxyInput = "socks5://127.0.0.1:1080"
	tt.Frame()
	if err := tt.Click("应用"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.proxyErr != "" || a.settings.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("valid proxy not applied: err=%q proxy=%q", a.proxyErr, a.settings.Proxy)
	}

	if err := tt.Click("直连"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.settings.Proxy != "direct" {
		t.Fatalf("direct mode: got %q", a.settings.Proxy)
	}
}

// With NM_FRAMES set, saves the network proxy settings and Windows login
// dialog for visual inspection.
func TestSettingsProxyFrames(t *testing.T) {
	dir := os.Getenv("NM_FRAMES")
	if dir == "" {
		t.Skip()
	}
	save := func(name string, tt *ui.Tester) {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		png.Encode(f, tt.Image())
	}

	a := newTestApp()
	a.settings = defaultSettings()
	a.settings.Quality = "exhigh"
	a.syncProxyState()
	tt := ui.NewTester(a.settingsView, 560, 760)
	tt.SetDark(true)
	tt.Frame()
	save("settings-proxy-system.png", tt)

	_ = tt.Click("自定义")
	a.proxyInput = "http://127.0.0.1:3067"
	a.applyCustomProxy()
	tt.Advance(300 * time.Millisecond)
	tt.Frame()
	save("settings-proxy-custom.png", tt)

	la, ltt := loginApp(t)
	_ = la
	ltt.SetDark(true)
	ltt.Advance(time.Second)
	ltt.Frame()
	save("login-windows.png", ltt)
}
