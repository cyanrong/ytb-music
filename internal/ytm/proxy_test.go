package ytm

import (
	"net/http"
	"runtime"
	"testing"
)

// Proxy validation rejects malformed endpoints, honours NO_PROXY before OS
// fallbacks, and clears IP-bound stream caches when routes change.
func TestProxyForRequest(t *testing.T) {
	defer func() { _ = SetProxy("") }()

	for _, bad := range []string{
		"ftp://127.0.0.1:21",
		"http://:7890",
		":7890",
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

	u, err := ParseProxy("127.0.0.1:7890")
	if err != nil || u == nil || u.String() != "http://127.0.0.1:7890" {
		t.Fatalf("ParseProxy bare host: %v, %v", u, err)
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

	// In automatic mode, NO_PROXY must prevent falling back to the OS proxy.
	if err := SetProxy(""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "music.youtube.com")
	if got, err := proxyForRequest(extReq); err != nil || got != nil {
		t.Fatalf("NO_PROXY bypassed to %v (%v)", got, err)
	}

	t.Setenv("NO_PROXY", "")
	if runtime.GOOS == "windows" {
		active, src := EffectiveProxy()
		t.Logf("detected Windows effective proxy: %q (source=%s)", active, src)
	}
}
