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

// Choosing a proxy mode in Settings validates addresses, binds masked
// credentials to their original endpoint, applies the active transport route,
// clears failed covers, and retries an errored home page without hitting the
// network in offline tests.
func TestSettingsProxy(t *testing.T) {
	var applied []string
	homeRetries := 0
	realSet, realReload := setProxy, reloadHome
	setProxy = func(raw string) error {
		if err := realSet(raw); err != nil {
			return err
		}
		applied = append(applied, raw)
		return nil
	}
	reloadHome = func(a *app) {
		homeRetries++
		a.homeLoading, a.homeErr = true, ""
	}
	defer func() {
		setProxy = realSet
		reloadHome = realReload
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
	a.proxyInput = "https://alice:s3cr3tPass@127.0.0.1:1080"
	tt.Frame()
	if err := tt.Click("应用"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.proxyErr != "" || a.settings.Proxy != "https://alice:s3cr3tPass@127.0.0.1:1080" {
		t.Fatalf("valid proxy not saved: err=%q proxy=%q", a.proxyErr, a.settings.Proxy)
	}
	if strings.Contains(a.proxyInput, "s3cr3tPass") || tt.HasText("s3cr3tPass") {
		t.Fatalf("plaintext password remained visible: input=%q", a.proxyInput)
	}
	activeURL, activeSrc := ytm.EffectiveProxy()
	if activeSrc != ytm.ProxySourceCustom || activeURL != "https://alice:xxxxx@127.0.0.1:1080" {
		t.Fatalf("effective route mismatch: %q (%s)", activeURL, activeSrc)
	}
	if _, stillFailed := a.art.art["https://img/fail"]; stillFailed {
		t.Error("failed cover entry was not cleared after route change")
	}
	if len(a.video.urls) != 0 {
		t.Error("cached video URLs were not cleared after route change")
	}

	// Editing the endpoint or downgrading the scheme while keeping the masked
	// password must NOT forward the saved password to the new endpoint.
	a.proxyInput = "http://alice:xxxxx@127.0.0.1:1080"
	tt.Frame()
	if err := tt.Click("应用"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.proxyErr == "" || a.settings.Proxy != "https://alice:s3cr3tPass@127.0.0.1:1080" {
		t.Fatalf("masked password leaked to downgraded endpoint: err=%q proxy=%q", a.proxyErr, a.settings.Proxy)
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
	if homeRetries != 1 {
		t.Fatalf("switching to Direct retried home %d times, want 1", homeRetries)
	}

	// Switching back to Custom preserves the original password for the same endpoint.
	if err := tt.Click("自定义"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	a.proxyInput = "https://alice:xxxxx@127.0.0.1:1080"
	tt.Frame()
	if err := tt.Click("应用"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.proxyErr != "" || a.settings.Proxy != "https://alice:s3cr3tPass@127.0.0.1:1080" {
		t.Fatalf("round-trip back to Custom lost password: err=%q proxy=%q", a.proxyErr, a.settings.Proxy)
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
	a.autoProxyDesc = formatAutoProxyDesc(ytm.EffectiveProxy())
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
