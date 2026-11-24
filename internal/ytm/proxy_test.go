package ytm

import (
	"net/http"
	"net/url"
	"os"
	"testing"
)

// Proxy validation rejects malformed endpoints, accepts IPv6 literals with or
// without explicit ports, honours NO_PROXY ahead of OS fallbacks, and keys
// stream caches by the effective route.
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

	// Changing proxy clears cached signed stream URLs.
	_, _ = playerCache.get("en/US/vid1", func() (*player, error) {
		return &player{status: "OK", stream: &Stream{URL: "https://old-ip"}}, nil
	})
	if err := SetProxy("socks5://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	playerCache.mu.Lock()
	left := len(playerCache.entries)
	playerCache.mu.Unlock()
	if left != 0 {
		t.Fatalf("playerCache kept %d entries across proxy change", left)
	}

	extReq, _ := http.NewRequest("GET", "https://music.youtube.com/", nil)
	got, err := proxyForRequest(extReq)
	if err != nil || got == nil || got.String() != "socks5://127.0.0.1:1080" {
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

	// Deterministic OS fallback: without NO_PROXY it routes through the OS
	// proxy; with NO_PROXY=music.youtube.com it connects directly; and when the
	// OS proxy changes externally, routeCacheKey changes automatically.
	osProxyURL, _ := url.Parse("http://10.0.0.2:8888")
	systemProxyFn = func(*url.URL) (*url.URL, ProxySource, error) {
		cp := *osProxyURL
		return &cp, ProxySourceSystem, nil
	}
	if err := SetProxy(""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")
	if got, err := proxyForRequest(extReq); err != nil || got == nil || got.String() != "http://10.0.0.2:8888" {
		t.Fatalf("expected OS fallback proxy, got %v (%v)", got, err)
	}
	keyBefore := routeCacheKey()

	t.Setenv("NO_PROXY", "music.youtube.com")
	if got, err := proxyForRequest(extReq); err != nil || got != nil {
		t.Fatalf("NO_PROXY fell through to OS proxy: %v (%v)", got, err)
	}

	t.Setenv("NO_PROXY", "")
	osProxyURL, _ = url.Parse("http://10.0.0.3:9999")
	if keyAfter := routeCacheKey(); keyAfter == keyBefore {
		t.Fatalf("routeCacheKey did not change after external OS proxy switch: %q", keyAfter)
	}
}
