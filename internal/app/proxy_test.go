package app

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"ytb-music/internal/ytm"
)

// Choosing a proxy mode in Settings validates addresses, redacts credentials,
// applies the active transport route, clears failed covers, and retries an
// errored home page across modes.
func TestSettingsProxy(t *testing.T) {
	var applied []string
	realSet := setProxy
	setProxy = func(raw string) error {
		if err := realSet(raw); err != nil {
			return err
		}
		applied = append(applied, raw)
		return nil
	}
	defer func() {
		setProxy = realSet
		_ = ytm.SetProxy("")
	}()

	a := newTestApp()
	a.settings = defaultSettings()
	a.syncProxyState()
	tt := ui.NewTester(a.settingsView, 560, 2000)
	tt.Frame()

	if err := tt.Click("自定义"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()

	a.proxyInput = "http://:7890"
	tt.Frame()
	if err := tt.Click("应用"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.proxyErr == "" || a.settings.Proxy != "" || len(applied) != 0 {
		t.Fatalf("invalid proxy accepted: err=%q proxy=%q applied=%v", a.proxyErr, a.settings.Proxy, applied)
	}

	// Failed artwork and cached video URLs clear when a new proxy route applies,
	// and credentials are redacted in the UI state.
	a.art.art["https://img/fail"] = &artwork{failed: true}
	a.video.urls = map[string]videoURL{"v1": {url: "https://signed", at: time.Now()}}
	a.proxyInput = "http://alice:s3cr3tPass@127.0.0.1:1080"
	tt.Frame()
	if err := tt.Click("应用"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.proxyErr != "" || a.settings.Proxy != "http://alice:s3cr3tPass@127.0.0.1:1080" {
		t.Fatalf("valid proxy not saved: err=%q proxy=%q", a.proxyErr, a.settings.Proxy)
	}
	if strings.Contains(a.proxyInput, "s3cr3tPass") || tt.HasText("s3cr3tPass") {
		t.Fatalf("plaintext password remained visible: input=%q", a.proxyInput)
	}
	activeURL, activeSrc := ytm.EffectiveProxy()
	if activeSrc != ytm.ProxySourceCustom || activeURL != "http://alice:xxxxx@127.0.0.1:1080" {
		t.Fatalf("effective route mismatch: %q (%s)", activeURL, activeSrc)
	}
	if _, stillFailed := a.art.art["https://img/fail"]; stillFailed {
		t.Error("failed cover entry was not cleared after route change")
	}
	if len(a.video.urls) != 0 {
		t.Error("cached video URLs were not cleared after route change")
	}

	// Switching to Direct updates the active transport and retries an errored home page.
	a.homeErr = "network error"
	if err := tt.Click("直连"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	_, activeSrc = ytm.EffectiveProxy()
	if a.settings.Proxy != "direct" || activeSrc != ytm.ProxySourceDirect {
		t.Fatalf("direct mode not active: proxy=%q src=%s", a.settings.Proxy, activeSrc)
	}
	if !a.homeLoading {
		t.Error("switching to Direct did not retry an errored home load")
	}
}

// With NM_FRAMES set, saves the network proxy settings and Windows login
// dialog for visual inspection.
func TestSettingsProxyFrames(t *testing.T) {
	dir := os.Getenv("NM_FRAMES")
	if dir == "" {
		t.Skip()
	}
	defer func() { _ = ytm.SetProxy("") }()
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
