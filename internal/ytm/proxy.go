package ytm

import (
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
	proxyMu          sync.RWMutex
	customProxy      string // "", "direct", or a validated proxy URL
	parsedProxy      *url.URL
	webviewEnvOwned  bool // true when SetProxy manages WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS
	webviewEnvChecked bool

	proxyTransport = newProxyTransport()

	// StreamClient fetches media streams without a fixed whole-body timeout,
	// using the same proxy settings as API requests.
	StreamClient = &http.Client{Transport: proxyTransport}
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

// ParseProxy validates raw ("", "direct", "127.0.0.1:7890",
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
	// Reject malformed host:port syntax such as trailing colons or out-of-range ports.
	if strings.Contains(u.Host, ":") {
		host, portStr, splitErr := net.SplitHostPort(u.Host)
		if splitErr != nil || host == "" || portStr == "" {
			return nil, errors.New("ytm: invalid proxy host or port")
		}
		port, convErr := strconv.Atoi(portStr)
		if convErr != nil || port < 1 || port > 65535 {
			return nil, errors.New("ytm: proxy port out of range")
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Path, u.RawPath = "", ""
	return u, nil
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
// routes YouTube requests through that proxy. When the route changes, signed
// stream caches and idle sockets are invalidated so new requests bind to the
// new egress IP.
func SetProxy(raw string) error {
	s := strings.TrimSpace(raw)
	if strings.EqualFold(s, "none") {
		s = "direct"
	}
	u, err := ParseProxy(s)
	if err != nil {
		return err
	}
	proxyMu.Lock()
	if !webviewEnvChecked {
		webviewEnvChecked = true
		webviewEnvOwned = os.Getenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS") == ""
	}
	changed := customProxy != s
	customProxy = s
	parsedProxy = u
	ownWebViewEnv := webviewEnvOwned
	proxyMu.Unlock()

	if ownWebViewEnv && runtime.GOOS == "windows" {
		syncWebView2ProxyEnv(s, u)
	}
	if changed {
		InvalidateRouteCaches()
	}
	return nil
}

// InvalidateRouteCaches drops pooled connections, visitor tokens, and cached
// IP-bound stream URLs after a network route change.
func InvalidateRouteCaches() {
	proxyTransport.CloseIdleConnections()
	playerCache.clear()
	premiumCache.clear()
	resetVisitorData()
}

// syncWebView2ProxyEnv propagates unauthenticated custom/direct proxy settings
// to WebView2 before its environment is created on Windows.
func syncWebView2ProxyEnv(mode string, u *url.URL) {
	switch {
	case strings.EqualFold(mode, "direct"):
		_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--no-proxy-server")
	case u != nil && u.User == nil:
		_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--proxy-server="+u.String())
	default:
		_ = os.Unsetenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS")
	}
}

// EffectiveProxy resolves the active proxy route for YouTube Music using the
// exact request pipeline and returns its redacted URL and source.
func EffectiveProxy() (string, ProxySource) {
	target := &url.URL{Scheme: "https", Host: "music.youtube.com"}
	u, src, err := resolveProxy(target)
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
	u, _, err := resolveProxy(req.URL)
	return u, err
}

func resolveProxy(target *url.URL) (*url.URL, ProxySource, error) {
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
	return systemProxy(target)
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
