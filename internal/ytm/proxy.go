package ytm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http/httpproxy"
)

// ProxySource describes where the active route for YouTube requests came from.
type ProxySource string

const (
	ProxySourceNone   ProxySource = ""
	ProxySourceCustom ProxySource = "custom"
	ProxySourceDirect ProxySource = "direct"
	ProxySourceEnv    ProxySource = "env"
	ProxySourceSystem ProxySource = "system"
	ProxySourcePAC    ProxySource = "pac"
)

var (
	proxyMu     sync.RWMutex
	customProxy string // "", "direct", or a normalized proxy URL
	parsedProxy *url.URL

	proxyTransport = newProxyTransport()

	// StreamClient fetches media streams without a fixed whole-body timeout,
	// using the same proxy settings as API requests.
	StreamClient = &http.Client{Transport: proxyTransport}

	// systemProxyFn resolves the OS system proxy; tests replace it.
	systemProxyFn = systemProxy
)

func newProxyTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t := base.Clone()
		t.Proxy = proxyForRequest
		return t
	}
	return &http.Transport{Proxy: proxyForRequest}
}

// Transport returns the shared HTTP transport that honours SetProxy,
// environment variables, and the OS system proxy.
func Transport() http.RoundTripper {
	return proxyTransport
}

// ParseProxy validates raw ("", "direct", "127.0.0.1:7890", "[::1]",
// "http://...", "socks5://...") and returns the normalized URL, or nil
// for automatic / direct modes.
func ParseProxy(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.EqualFold(s, "direct") || strings.EqualFold(s, "none") {
		return nil, nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.New("ytm: invalid proxy address")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("ytm: unsupported proxy scheme " + u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.New("ytm: proxy missing hostname")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("ytm: proxy URL must not contain path, query, or fragment")
	}
	if err := validateHostPort(u.Host); err != nil {
		return nil, err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawPath = "", ""
	return u, nil
}

func validateHostPort(hostport string) error {
	if hostport == "" {
		return errors.New("ytm: empty proxy host")
	}
	rest := hostport
	if strings.HasPrefix(hostport, "[") {
		closeIdx := strings.IndexByte(hostport, ']')
		if closeIdx <= 1 {
			return errors.New("ytm: invalid IPv6 proxy host")
		}
		rest = hostport[closeIdx+1:]
		if rest == "" {
			return nil // bracketed IPv6 literal without explicit port
		}
	} else if !strings.Contains(hostport, ":") {
		return nil // hostname or IPv4 without explicit port
	}
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil || host == "" || portStr == "" {
		return errors.New("ytm: invalid proxy host or port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("ytm: proxy port out of range")
	}
	return nil
}

// NormalizeProxy returns the canonical stored representation of raw ("",
// "direct", or a normalized proxy URL).
func NormalizeProxy(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if strings.EqualFold(s, "direct") || strings.EqualFold(s, "none") {
		return "direct", nil
	}
	u, err := ParseProxy(s)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// RedactedProxy formats raw with any embedded password masked for display.
func RedactedProxy(raw string) string {
	u, err := ParseProxy(raw)
	if err != nil || u == nil {
		return strings.TrimSpace(raw)
	}
	return u.Redacted()
}

// SetProxy sets the proxy override: "" uses environment and OS system proxy,
// "direct" forces direct connections, and any valid HTTP/SOCKS5 address
// routes YouTube requests through that proxy. When the normalized route
// changes, signed stream caches and idle sockets are invalidated.
func SetProxy(raw string) error {
	norm, err := NormalizeProxy(raw)
	if err != nil {
		return err
	}
	var u *url.URL
	if norm != "" && norm != "direct" {
		u, _ = ParseProxy(norm)
	}
	proxyMu.Lock()
	changed := customProxy != norm
	customProxy = norm
	parsedProxy = u
	proxyMu.Unlock()

	if changed {
		InvalidateRouteCaches()
	}
	return nil
}

// InitWebViewProxy configures WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS once at
// startup on Windows before any WebView2 environment is created, unless the
// caller already set custom browser arguments.
func InitWebViewProxy(raw string) {
	if runtime.GOOS != "windows" || os.Getenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS") != "" {
		return
	}
	s := strings.TrimSpace(raw)
	if strings.EqualFold(s, "direct") || strings.EqualFold(s, "none") {
		_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--no-proxy-server")
		return
	}
	u, err := ParseProxy(s)
	if err != nil || u == nil || u.User != nil {
		return
	}
	wv := *u
	if wv.Scheme == "socks5h" {
		wv.Scheme = "socks5"
	}
	_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--proxy-server="+wv.String())
}

// InvalidateRouteCaches drops pooled connections, visitor tokens, and cached
// IP-bound stream URLs after a network route change.
func InvalidateRouteCaches() {
	proxyTransport.CloseIdleConnections()
	clearStreamCaches()
	resetVisitorData()
}

// InvalidateTrackStream drops cached stream URLs for video id alone without
// disturbing other tracks' caches or pooled connections.
func InvalidateTrackStream(id string) {
	if id == "" {
		return
	}
	invalidateStreamSuffix("/" + id)
}

var musicOriginURL = &url.URL{Scheme: "https", Host: "music.youtube.com", Path: "/"}

// RouteSignature identifies the current route for YouTube media requests so
// active range downloads can tell whether the route changed mid-song.
func RouteSignature() string {
	return routeCacheKeyFor(playerAPI)
}

// routeCacheKeyFor identifies the active route for rawURL so cached signed
// stream URLs are never reused across a proxy or exit-IP switch, even when
// host-specific bypasses or OS proxy settings change externally.
func routeCacheKeyFor(rawURL string) string {
	target, err := url.Parse(rawURL)
	if err != nil || target == nil {
		target = musicOriginURL
	}
	u, src, err := resolveProxy(context.Background(), target)
	if err != nil || u == nil {
		return string(src) + ":direct"
	}
	return string(src) + ":" + u.String()
}

// EffectiveProxy resolves the active proxy route for YouTube Music using the
// exact request pipeline and returns its redacted URL and source. Callers on UI
// threads should invoke this from a background goroutine because PAC resolution
// may perform WinHTTP lookups.
func EffectiveProxy() (string, ProxySource) {
	u, src, err := resolveProxy(context.Background(), musicOriginURL)
	if err != nil || u == nil {
		return "", src
	}
	return u.Redacted(), src
}

// SystemProxy returns the redacted proxy URL currently active in automatic mode.
func SystemProxy() string {
	u, _ := EffectiveProxy()
	return u
}

func proxyForRequest(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	u, _, err := resolveProxy(req.Context(), req.URL)
	return u, err
}

func resolveProxy(ctx context.Context, target *url.URL) (*url.URL, ProxySource, error) {
	if target == nil || isLoopbackHost(target.Hostname()) {
		return nil, ProxySourceNone, nil
	}
	proxyMu.RLock()
	mode := customProxy
	override := parsedProxy
	proxyMu.RUnlock()

	if strings.EqualFold(mode, "direct") {
		return nil, ProxySourceDirect, nil
	}
	if override != nil {
		cp := *override
		return &cp, ProxySourceCustom, nil
	}
	u, excluded, err := envProxy(target)
	if err != nil {
		return nil, ProxySourceEnv, err
	}
	if excluded {
		// NO_PROXY explicitly excluded target: do not fall through to OS proxy.
		return nil, ProxySourceNone, nil
	}
	if u != nil {
		return u, ProxySourceEnv, nil
	}
	return systemProxyFn(ctx, target)
}

func envProxy(target *url.URL) (proxy *url.URL, excluded bool, err error) {
	cfg := httpproxy.FromEnvironment()
	if cfg.NoProxy != "" {
		probe := (&httpproxy.Config{
			HTTPProxy:  "http://127.0.0.1:1",
			HTTPSProxy: "http://127.0.0.1:1",
			NoProxy:    cfg.NoProxy,
		}).ProxyFunc()
		if u, probeErr := probe(target); probeErr == nil && u == nil {
			return nil, true, nil
		}
	}
	if cfg.HTTPProxy == "" && cfg.HTTPSProxy == "" {
		return nil, false, nil
	}
	u, err := cfg.ProxyFunc()(target)
	return u, false, err
}

func isLoopbackHost(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}
