package ytm

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

var (
	proxyMu     sync.RWMutex
	customProxy string // "", "direct", or a proxy URL
	parsedProxy *url.URL

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
	if err != nil || u.Host == "" {
		return nil, errors.New("ytm: invalid proxy address")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("ytm: unsupported proxy scheme " + u.Scheme)
	}
	return u, nil
}

// SetProxy sets the proxy override: "" uses environment and OS system proxy,
// "direct" forces direct connections, and any valid HTTP/SOCKS5 address
// routes YouTube requests through that proxy.
func SetProxy(raw string) error {
	s := strings.TrimSpace(raw)
	u, err := ParseProxy(s)
	if err != nil {
		return err
	}
	proxyMu.Lock()
	changed := customProxy != s
	customProxy = s
	parsedProxy = u
	proxyMu.Unlock()
	if changed {
		proxyTransport.CloseIdleConnections()
	}
	return nil
}

// SystemProxy returns the OS proxy address currently detected for HTTPS
// traffic, if any, for display in the settings UI.
func SystemProxy() string {
	u, _ := systemProxy(&url.URL{Scheme: "https", Host: "music.youtube.com"})
	if u == nil {
		return ""
	}
	return u.String()
}

func proxyForRequest(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil || isLoopbackHost(req.URL.Hostname()) {
		return nil, nil
	}
	proxyMu.RLock()
	mode := customProxy
	override := parsedProxy
	proxyMu.RUnlock()

	if strings.EqualFold(mode, "direct") || strings.EqualFold(mode, "none") {
		return nil, nil
	}
	if override != nil {
		return override, nil
	}
	if u, err := http.ProxyFromEnvironment(req); u != nil || err != nil {
		return u, err
	}
	return systemProxy(req.URL)
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
