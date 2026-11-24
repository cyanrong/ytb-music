package ytm

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"testing"
)

// Proxy validation rejects malformed endpoints, accepts IPv6 literals with or
// without explicit ports, honours NO_PROXY ahead of OS fallbacks, and keys
// stream caches by the effective route of the target endpoint.
func TestProxyForRequest(t *testing.T) {
	realSystem := systemProxyFn
	defer func() {
		systemProxyFn = realSystem
		_ = SetProxy("")
		_ = os.Unsetenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS")
	}()

	for _, bad := range []string{
		"ftp://127.0.0.1:21",
		"http://:7890",
		":7890",
		"http://[::1",
		"http://[::1]:0",
		"http://127.0.0.1:0",
		"http://127.0.0.1:70000",
		"http://127.0.0.1:abc",
		"http://127.0.0.1:7890/extra",
		"http://127.0.0.1:7890?x=1",
	} {
		if _, err := ParseProxy(bad); err == nil {
			t.Errorf("ParseProxy(%q) succeeded, want error", bad)
		}
	}

	for _, good := range [][2]string{
		{"127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"http://[::1]", "http://[::1]"},
		{"socks5h://[2001:db8::1]:1080", "socks5h://[2001:db8::1]:1080"},
	} {
		u, err := ParseProxy(good[0])
		if err != nil || u == nil || u.String() != good[1] {
			t.Fatalf("ParseProxy(%q) = %v, %v; want %q", good[0], u, err, good[1])
		}
	}

	if got := RedactedProxy("http://user:secret123@127.0.0.1:7890"); got != "http://user:xxxxx@127.0.0.1:7890" {
		t.Fatalf("RedactedProxy leaked secret: %q", got)
	}

	// Changing proxy clears cached signed stream URLs, while reapplying an
	// equivalent normalized address is a no-op.
	_, _ = playerCache.get("route|en/US/vid1", func() (*player, error) {
		return &player{status: "OK", stream: &Stream{URL: "https://old-ip"}}, nil
	})
	if err := SetProxy("127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	playerCache.mu.Lock()
	left := len(playerCache.entries)
	playerCache.mu.Unlock()
	if left != 0 {
		t.Fatalf("playerCache kept %d entries across proxy change", left)
	}

	_, _ = playerCache.get("route|en/US/vid2", func() (*player, error) {
		return &player{status: "OK", stream: &Stream{URL: "https://same-ip"}}, nil
	})
	if err := SetProxy("http://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	playerCache.mu.Lock()
	kept := len(playerCache.entries)
	playerCache.mu.Unlock()
	if kept != 1 {
		t.Fatalf("reapplying normalized proxy flushed cache: kept=%d", kept)
	}
	InvalidateTrackStream("vid2")
	playerCache.mu.Lock()
	afterTrack := len(playerCache.entries)
	playerCache.mu.Unlock()
	if afterTrack != 0 {
		t.Fatalf("InvalidateTrackStream left %d entries", afterTrack)
	}

	extReq, _ := http.NewRequest("GET", "https://music.youtube.com/", nil)
	got, err := proxyForRequest(extReq)
	if err != nil || got == nil || got.String() != "http://127.0.0.1:1080" {
		t.Fatalf("override proxy: %v, %v", got, err)
	}

	localReq, _ := http.NewRequest("GET", "http://127.0.0.1:8080/test", nil)
	if got, err := proxyForRequest(localReq); err != nil || got != nil {
		t.Fatalf("loopback went through proxy: %v, %v", got, err)
	}

	if err := SetProxy("direct"); err != nil {
		t.Fatal(err)
	}
	if got, err := proxyForRequest(extReq); err != nil || got != nil {
		t.Fatalf("direct mode used proxy: %v, %v", got, err)
	}

	// Deterministic OS fallback: clear both upper- and lowercase env vars so
	// Linux/macOS runners never inherit external proxies.
	osProxyURL, _ := url.Parse("http://10.0.0.2:8888")
	systemProxyFn = func(context.Context, *url.URL) (*url.URL, ProxySource, error) {
		cp := *osProxyURL
		return &cp, ProxySourceSystem, nil
	}
	if err := SetProxy(""); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	if got, err := proxyForRequest(extReq); err != nil || got == nil || got.String() != "http://10.0.0.2:8888" {
		t.Fatalf("expected OS fallback proxy, got %v (%v)", got, err)
	}
	keyBefore := routeCacheKeyFor(playerAPI)

	t.Setenv("NO_PROXY", "music.youtube.com")
	if got, err := proxyForRequest(extReq); err != nil || got != nil {
		t.Fatalf("NO_PROXY fell through to OS proxy: %v (%v)", got, err)
	}

	t.Setenv("NO_PROXY", "")
	osProxyURL, _ = url.Parse("http://10.0.0.3:9999")
	if keyAfter := routeCacheKeyFor(playerAPI); keyAfter == keyBefore {
		t.Fatalf("routeCacheKeyFor did not change after external OS proxy switch: %q", keyAfter)
	}
}
