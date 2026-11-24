//go:build windows

package ytm

import (
	"net/url"
	"testing"
)

// Windows proxy server lists honour protocol boundaries and wildcard bypasses.
func TestWindowsProxyParsingAndBypass(t *testing.T) {
	if got := pickWindowsProxy("http=127.0.0.1:8080", "https"); got != "" {
		t.Fatalf("HTTPS borrowed HTTP-only entry: %q", got)
	}
	if got := pickWindowsProxy("http=127.0.0.1:8080;socks=127.0.0.1:1080", "https"); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("HTTPS did not fall back to SOCKS: %q", got)
	}
	if got := pickWindowsProxy("http=127.0.0.1:8080;https=127.0.0.1:8443", "https"); got != "127.0.0.1:8443" {
		t.Fatalf("HTTPS entry mismatch: %q", got)
	}

	mustURL := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	bypass := "192.168.*;*youtube.com;<local>;http://onlyhttp.example"
	if !windowsBypassed(mustURL("https://192.168.1.50/"), bypass) {
		t.Error("192.168.* did not match")
	}
	if !windowsBypassed(mustURL("https://music.youtube.com/"), bypass) {
		t.Error("*youtube.com did not match")
	}
	if !windowsBypassed(mustURL("http://intranet/"), bypass) {
		t.Error("<local> did not match plain hostname")
	}
	if windowsBypassed(mustURL("https://onlyhttp.example/"), bypass) {
		t.Error("http:// bypass matched https:// target")
	}
}
