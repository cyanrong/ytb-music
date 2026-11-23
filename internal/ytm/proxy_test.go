package ytm

import (
	"net/http"
	"runtime"
	"testing"
)

// Proxy overrides route external requests through the configured address,
// leave loopback traffic direct, and bypass OS settings in direct mode.
func TestProxyForRequest(t *testing.T) {
	defer func() { _ = SetProxy("") }()

	if _, err := ParseProxy("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("accepted unsupported scheme")
	}
	u, err := ParseProxy("127.0.0.1:7890")
	if err != nil || u == nil || u.String() != "http://127.0.0.1:7890" {
		t.Fatalf("ParseProxy bare host: %v, %v", u, err)
	}

	if err := SetProxy("socks5://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
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

	if runtime.GOOS == "windows" {
		t.Logf("detected Windows system proxy: %q", SystemProxy())
	}
}
