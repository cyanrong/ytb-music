//go:build windows

package ytm

import (
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

var winProxyCache struct {
	mu      sync.Mutex
	checked time.Time
	enabled bool
	server  string
	bypass  string
}

func systemProxy(target *url.URL) (*url.URL, error) {
	enabled, server, bypass := readWindowsProxy()
	if !enabled || server == "" {
		return nil, nil
	}
	if target != nil && windowsBypassed(target.Hostname(), bypass) {
		return nil, nil
	}
	scheme := "https"
	if target != nil && target.Scheme != "" {
		scheme = strings.ToLower(target.Scheme)
	}
	picked := pickWindowsProxy(server, scheme)
	if picked == "" {
		return nil, nil
	}
	return ParseProxy(picked)
}

func readWindowsProxy() (bool, string, string) {
	winProxyCache.mu.Lock()
	defer winProxyCache.mu.Unlock()
	if time.Since(winProxyCache.checked) < 5*time.Second {
		return winProxyCache.enabled, winProxyCache.server, winProxyCache.bypass
	}
	winProxyCache.checked = time.Now()
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		winProxyCache.enabled, winProxyCache.server, winProxyCache.bypass = false, "", ""
		return false, "", ""
	}
	defer k.Close()
	en, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || en == 0 {
		winProxyCache.enabled, winProxyCache.server, winProxyCache.bypass = false, "", ""
		return false, "", ""
	}
	srv, _, _ := k.GetStringValue("ProxyServer")
	byp, _, _ := k.GetStringValue("ProxyOverride")
	winProxyCache.enabled = strings.TrimSpace(srv) != ""
	winProxyCache.server = strings.TrimSpace(srv)
	winProxyCache.bypass = strings.TrimSpace(byp)
	return winProxyCache.enabled, winProxyCache.server, winProxyCache.bypass
}

// pickWindowsProxy selects the proxy entry for scheme from a Windows
// ProxyServer string, which may be a single host:port or a semicolon-separated
// list such as "http=127.0.0.1:7890;https=127.0.0.1:7890;socks=127.0.0.1:1080".
func pickWindowsProxy(server, scheme string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return ""
	}
	if !strings.Contains(server, "=") {
		return server
	}
	entries := map[string]string{}
	for _, part := range strings.Split(server, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(v) != "" {
			entries[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if v := entries[scheme]; v != "" {
		return v
	}
	if v := entries["https"]; v != "" {
		return v
	}
	if v := entries["http"]; v != "" {
		return v
	}
	if v := entries["socks"]; v != "" {
		if !strings.Contains(v, "://") {
			return "socks5://" + v
		}
		return v
	}
	return ""
}

func windowsBypassed(host, bypass string) bool {
	if host == "" || bypass == "" {
		return false
	}
	host = strings.ToLower(host)
	for _, pat := range strings.Split(bypass, ";") {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == "" {
			continue
		}
		if pat == "<local>" && !strings.Contains(host, ".") {
			return true
		}
		if pat == host {
			return true
		}
		if strings.HasPrefix(pat, "*.") && strings.HasSuffix(host, pat[1:]) {
			return true
		}
	}
	return false
}
