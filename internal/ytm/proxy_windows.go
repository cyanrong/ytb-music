//go:build windows

package ytm

import (
	"net"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

const (
	winhttpAccessTypeNoProxy    = 1
	winhttpAccessTypeNamedProxy = 3
	winhttpAutoProxyAutoDetect  = 0x00000001
	winhttpAutoProxyConfigURL   = 0x00000002
	winhttpAutoDetectTypeDHCP   = 0x00000001
	winhttpAutoDetectTypeDNSA   = 0x00000002

	maxPACCacheEntries = 64
)

type winhttpIEProxyConfig struct {
	AutoDetect    int32
	AutoConfigURL *uint16
	Proxy         *uint16
	ProxyBypass   *uint16
}

type winhttpAutoProxyOptions struct {
	Flags                 uint32
	AutoDetectFlags       uint32
	AutoConfigURL         *uint16
	Reserved1             uintptr
	Reserved2             uint32
	AutoLogonIfChallenged int32
}

type winhttpProxyInfo struct {
	AccessType  uint32
	Proxy       *uint16
	ProxyBypass *uint16
}

var (
	modwinhttp                           = windows.NewLazySystemDLL("winhttp.dll")
	modkernel32                          = windows.NewLazySystemDLL("kernel32.dll")
	procWinHttpGetIEProxyConfigForCurUsr = modwinhttp.NewProc("WinHttpGetIEProxyConfigForCurrentUser")
	procWinHttpOpen                      = modwinhttp.NewProc("WinHttpOpen")
	procWinHttpGetProxyForUrl            = modwinhttp.NewProc("WinHttpGetProxyForUrl")
	procWinHttpCloseHandle               = modwinhttp.NewProc("WinHttpCloseHandle")
	procGlobalFree                       = modkernel32.NewProc("GlobalFree")
)

type winProxyConfig struct {
	manualEnabled bool
	server        string
	bypass        string
	autoDetect    bool
	autoConfigURL string
}

var winProxyCache struct {
	mu      sync.Mutex
	checked time.Time
	cfg     winProxyConfig
	pac     map[string]pacEntry
}

type pacEntry struct {
	at  time.Time
	url *url.URL // nil means PAC explicitly returned DIRECT
}

func systemProxy(target *url.URL) (*url.URL, ProxySource, error) {
	cfg := readWindowsProxy()
	scheme := "https"
	if target != nil && target.Scheme != "" {
		scheme = strings.ToLower(target.Scheme)
	}
	// Windows evaluates automatic proxy configuration (WPAD / PAC) first,
	// falling back to manual settings only when auto-proxy fails rather than
	// when a PAC script explicitly chooses DIRECT.
	if (cfg.autoDetect || cfg.autoConfigURL != "") && target != nil {
		if u, decided := resolveWindowsPAC(target, cfg.autoDetect, cfg.autoConfigURL); decided {
			if u == nil {
				return nil, ProxySourceNone, nil
			}
			return u, ProxySourcePAC, nil
		}
	}
	if cfg.manualEnabled && cfg.server != "" {
		if target != nil && windowsBypassed(target, cfg.bypass) {
			return nil, ProxySourceNone, nil
		}
		if picked := pickWindowsProxy(cfg.server, scheme); picked != "" {
			u, err := ParseProxy(picked)
			return u, ProxySourceSystem, err
		}
	}
	return nil, ProxySourceNone, nil
}

func readWindowsProxy() winProxyConfig {
	winProxyCache.mu.Lock()
	defer winProxyCache.mu.Unlock()
	if time.Since(winProxyCache.checked) < 5*time.Second {
		return winProxyCache.cfg
	}
	winProxyCache.checked = time.Now()
	var cfg winProxyConfig

	var ie winhttpIEProxyConfig
	r1, _, _ := procWinHttpGetIEProxyConfigForCurUsr.Call(uintptr(unsafe.Pointer(&ie)))
	runtime.KeepAlive(&ie)
	if r1 != 0 {
		cfg.autoDetect = ie.AutoDetect != 0
		if ie.AutoConfigURL != nil {
			cfg.autoConfigURL = strings.TrimSpace(windows.UTF16PtrToString(ie.AutoConfigURL))
			globalFree(ie.AutoConfigURL)
		}
		if ie.Proxy != nil {
			cfg.server = strings.TrimSpace(windows.UTF16PtrToString(ie.Proxy))
			cfg.manualEnabled = cfg.server != ""
			globalFree(ie.Proxy)
		}
		if ie.ProxyBypass != nil {
			cfg.bypass = strings.TrimSpace(windows.UTF16PtrToString(ie.ProxyBypass))
			globalFree(ie.ProxyBypass)
		}
		winProxyCache.cfg = cfg
		return cfg
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		winProxyCache.cfg = cfg
		return cfg
	}
	defer k.Close()
	if ac, _, err := k.GetStringValue("AutoConfigURL"); err == nil {
		cfg.autoConfigURL = strings.TrimSpace(ac)
	}
	if en, _, err := k.GetIntegerValue("ProxyEnable"); err == nil && en != 0 {
		srv, _, _ := k.GetStringValue("ProxyServer")
		byp, _, _ := k.GetStringValue("ProxyOverride")
		cfg.server = strings.TrimSpace(srv)
		cfg.bypass = strings.TrimSpace(byp)
		cfg.manualEnabled = cfg.server != ""
	}
	winProxyCache.cfg = cfg
	return cfg
}

func resolveWindowsPAC(target *url.URL, autoDetect bool, autoConfigURL string) (*url.URL, bool) {
	sanitized := url.URL{
		Scheme: strings.ToLower(target.Scheme),
		Host:   strings.ToLower(target.Host),
		Path:   target.Path,
	}
	if sanitized.Path == "" {
		sanitized.Path = "/"
	}
	pacInput := sanitized.String()
	key := pacInput + "|" + autoConfigURL
	if autoDetect {
		key += "|wpad"
	}

	winProxyCache.mu.Lock()
	if winProxyCache.pac == nil {
		winProxyCache.pac = map[string]pacEntry{}
	}
	if e, ok := winProxyCache.pac[key]; ok && time.Since(e.at) < 30*time.Second {
		winProxyCache.mu.Unlock()
		if e.url == nil {
			return nil, true
		}
		cp := *e.url
		return &cp, true
	}
	winProxyCache.mu.Unlock()

	ua, _ := windows.UTF16PtrFromString("ytb-music")
	session, _, _ := procWinHttpOpen.Call(uintptr(unsafe.Pointer(ua)), 1, 0, 0, 0)
	runtime.KeepAlive(ua)
	if session == 0 {
		return nil, false
	}
	defer procWinHttpCloseHandle.Call(session)

	var opts winhttpAutoProxyOptions
	var acPtr *uint16
	if autoDetect {
		opts.Flags |= winhttpAutoProxyAutoDetect
		opts.AutoDetectFlags = winhttpAutoDetectTypeDHCP | winhttpAutoDetectTypeDNSA
	}
	if autoConfigURL != "" {
		if p, err := windows.UTF16PtrFromString(autoConfigURL); err == nil {
			acPtr = p
			opts.Flags |= winhttpAutoProxyConfigURL
			opts.AutoConfigURL = acPtr
		}
	}
	// Never automatically send Windows logon credentials to untrusted WPAD/PAC servers.
	opts.AutoLogonIfChallenged = 0

	u16, err := windows.UTF16PtrFromString(pacInput)
	if err != nil {
		return nil, false
	}
	var info winhttpProxyInfo
	ok, _, _ := procWinHttpGetProxyForUrl.Call(session, uintptr(unsafe.Pointer(u16)), uintptr(unsafe.Pointer(&opts)), uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(u16)
	runtime.KeepAlive(acPtr)
	runtime.KeepAlive(&opts)
	if info.ProxyBypass != nil {
		globalFree(info.ProxyBypass)
	}
	if ok == 0 {
		if info.Proxy != nil {
			globalFree(info.Proxy)
		}
		return nil, false
	}

	var resolved *url.URL
	decided := false
	switch info.AccessType {
	case winhttpAccessTypeNoProxy:
		decided = true
	case winhttpAccessTypeNamedProxy:
		if info.Proxy != nil {
			rawList := windows.UTF16PtrToString(info.Proxy)
			resolved, decided = parseWinHTTPProxyList(rawList, target.Scheme)
		}
	}
	if info.Proxy != nil {
		globalFree(info.Proxy)
	}
	if !decided {
		return nil, false
	}

	winProxyCache.mu.Lock()
	if len(winProxyCache.pac) >= maxPACCacheEntries {
		var oldestKey string
		var oldestAt time.Time
		for k, v := range winProxyCache.pac {
			if oldestKey == "" || v.at.Before(oldestAt) {
				oldestKey, oldestAt = k, v.at
			}
		}
		if oldestKey != "" {
			delete(winProxyCache.pac, oldestKey)
		}
	}
	winProxyCache.pac[key] = pacEntry{at: time.Now(), url: resolved}
	winProxyCache.mu.Unlock()
	if resolved == nil {
		return nil, true
	}
	cp := *resolved
	return &cp, true
}

func parseWinHTTPProxyList(list, scheme string) (*url.URL, bool) {
	for _, item := range strings.Split(list, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "=") {
			if picked := pickWindowsProxy(item, strings.ToLower(scheme)); picked != "" {
				if u, err := ParseProxy(picked); err == nil && u != nil {
					return u, true
				}
			}
			continue
		}
		fields := strings.Fields(item)
		if len(fields) == 2 {
			switch strings.ToUpper(fields[0]) {
			case "PROXY", "HTTP":
				item = "http://" + fields[1]
			case "HTTPS":
				item = "https://" + fields[1]
			case "SOCKS", "SOCKS5":
				item = "socks5://" + fields[1]
			case "DIRECT":
				return nil, true
			}
		} else if strings.EqualFold(item, "DIRECT") {
			return nil, true
		}
		if u, err := ParseProxy(item); err == nil && u != nil {
			return u, true
		}
	}
	return nil, false
}

func globalFree(p *uint16) {
	if p != nil {
		procGlobalFree.Call(uintptr(unsafe.Pointer(p)))
	}
}

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
	scheme = strings.ToLower(scheme)
	if v := entries[scheme]; v != "" {
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

func windowsBypassed(target *url.URL, bypass string) bool {
	if target == nil || bypass == "" {
		return false
	}
	host := strings.ToLower(target.Hostname())
	if host == "" {
		return false
	}
	scheme := strings.ToLower(target.Scheme)
	port := target.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	hostPort := host
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}

	for _, pat := range strings.Split(bypass, ";") {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == "" {
			continue
		}
		if pat == "<local>" {
			if !strings.Contains(host, ".") && net.ParseIP(host) == nil {
				return true
			}
			continue
		}
		if idx := strings.Index(pat, "://"); idx >= 0 {
			patScheme := pat[:idx]
			if scheme != "" && patScheme != scheme {
				continue
			}
			pat = pat[idx+3:]
		}
		candidate := host
		if strings.Contains(pat, ":") && !strings.HasPrefix(pat, "[") {
			candidate = hostPort
		} else if strings.HasPrefix(pat, "[") && strings.Contains(pat, "]:") {
			candidate = hostPort
		}
		if wildcardMatch(pat, candidate) {
			return true
		}
	}
	return false
}

func wildcardMatch(pattern, s string) bool {
	pIdx, sIdx := 0, 0
	starIdx, matchIdx := -1, 0
	for sIdx < len(s) {
		if pIdx < len(pattern) && (pattern[pIdx] == '?' || pattern[pIdx] == s[sIdx]) {
			pIdx++
			sIdx++
		} else if pIdx < len(pattern) && pattern[pIdx] == '*' {
			starIdx = pIdx
			matchIdx = sIdx
			pIdx++
		} else if starIdx != -1 {
			pIdx = starIdx + 1
			matchIdx++
			sIdx = matchIdx
		} else {
			return false
		}
	}
	for pIdx < len(pattern) && pattern[pIdx] == '*' {
		pIdx++
	}
	return pIdx == len(pattern)
}
