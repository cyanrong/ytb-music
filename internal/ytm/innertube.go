package ytm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Language and Region are what YouTube Music answers in and for: the
// language of its words ("en", "zh-CN") and the country whose catalogue
// and charts it shows ("US"). Set them before the first request.
var (
	Language = "en"
	Region   = "US"
)

const (
	musicAPI     = "https://music.youtube.com/youtubei/v1/"
	playerAPI    = "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"
	remixName    = "WEB_REMIX"
	remixID      = "67"
	remixVer     = "1.20260707.12.00"
	visionName   = "VISIONOS"
	visionID     = "101"
	visionVer    = "1.02"
	visionUA     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15"
	tvName       = "TVHTML5"
	tvID         = "7"
	tvSamsungVer = "5.20260707"
	tvSamsungUA  = "Mozilla/5.0 (SMART-TV; Linux; Tizen 2.4.0) AppleWebKit/538.1 (KHTML, like Gecko) Version/2.4.0 TV Safari/538.1"
	webName      = "WEB"
	webID        = "1"
	webVer       = "2.20260708.00.00"
	webSafariUA  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/15.5 Safari/605.1.15,gzip(gfe)"
)

var httpClient = &http.Client{Timeout: 20 * time.Second, Transport: proxyTransport}

// visitor is the visitorData that anonymous requests carry, as a browser
// that has been to YouTube before: fetched once, shared, and fetched again
// when it gets old or YouTube starts refusing it. Network fetches run
// outside mu and check gen so resetVisitorData never blocks on slow I/O or
// accepts tokens from an obsolete route.
type visitorFetch struct {
	done chan struct{}
	data string
	gen  uint64
}

var visitor struct {
	mu         sync.Mutex
	data       string
	at         time.Time
	refreshing bool
	inflight   *visitorFetch
	gen        uint64
}

var visitorRe = regexp.MustCompile(`"(Cg[A-Za-z0-9_%-]{10,})"`)

func visitorData() string {
	for range 2 {
		visitor.mu.Lock()
		if visitor.data != "" {
			// Old, it serves on while another is fetched: no request waits
			// for one but the first.
			if time.Since(visitor.at) >= 6*time.Hour && !visitor.refreshing {
				visitor.refreshing = true
				gen := visitor.gen
				go refreshVisitorData(gen)
			}
			d := visitor.data
			visitor.mu.Unlock()
			return d
		}
		f := visitor.inflight
		leader := false
		if f == nil || f.gen != visitor.gen {
			f = &visitorFetch{done: make(chan struct{}), gen: visitor.gen}
			visitor.inflight = f
			leader = true
		}
		visitor.mu.Unlock()

		if leader {
			d, err := fetchVisitorData()
			visitor.mu.Lock()
			if visitor.gen == f.gen {
				if err == nil && d != "" {
					f.data = d
					if visitor.data == "" {
						visitor.data, visitor.at = d, time.Now()
					}
				}
				if visitor.inflight == f {
					visitor.inflight = nil
				}
			}
			visitor.mu.Unlock()
			close(f.done)
		} else {
			<-f.done
		}

		visitor.mu.Lock()
		sameGen := visitor.gen == f.gen
		cur := visitor.data
		visitor.mu.Unlock()
		if sameGen {
			if cur != "" {
				return cur
			}
			return f.data
		}
	}
	return ""
}

func refreshVisitorData(gen uint64) {
	d, err := fetchVisitorData()
	visitor.mu.Lock()
	if visitor.gen == gen {
		if err == nil && d != "" {
			visitor.data, visitor.at = d, time.Now()
		}
		visitor.refreshing = false
	}
	visitor.mu.Unlock()
}

// VisitorData returns the visitorData requests carry and when it was
// fetched, for the app to keep for its next run, or "" before the first
// request.
func VisitorData() (string, time.Time) {
	visitor.mu.Lock()
	defer visitor.mu.Unlock()
	return visitor.data, visitor.at
}

// SetVisitorData has requests carry data, the visitorData an earlier run
// kept, found at, rather than fetch one before the first request, which
// costs a round trip; once old, another is fetched meanwhile.
func SetVisitorData(data string, at time.Time) {
	if data == "" {
		return
	}
	visitor.mu.Lock()
	if visitor.data == "" {
		visitor.data, visitor.at = data, at
	}
	visitor.mu.Unlock()
}

// dropVisitorData forgets data, if it is still the visitorData in use, so
// the next request fetches another.
func dropVisitorData(data string) {
	visitor.mu.Lock()
	if visitor.data == data {
		visitor.data = ""
		visitor.inflight = nil
		visitor.gen++
		visitor.refreshing = false
	}
	visitor.mu.Unlock()
}

// resetVisitorData forgets the visitorData in use.
func resetVisitorData() {
	visitor.mu.Lock()
	visitor.data = ""
	visitor.inflight = nil
	visitor.gen++
	visitor.refreshing = false
	visitor.mu.Unlock()
}

func fetchVisitorData() (string, error) {
	req, err := http.NewRequest("GET", "https://www.youtube.com/sw.js_data", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ytm: visitor data: %s", resp.Status)
	}
	m := visitorRe.FindSubmatch(body)
	if m == nil {
		return "", errors.New("ytm: no visitor data")
	}
	return string(m[1]), nil
}

// call posts body to a WEB_REMIX endpoint of music.youtube.com, as its
// web player does, and returns the JSON it answers.
func call(endpoint string, body map[string]any) (any, error) {
	return post(musicAPI+endpoint+"?prettyPrint=false", body, func(vd string) (map[string]any, http.Header) {
		client := map[string]any{"clientName": remixName, "clientVersion": remixVer, "hl": Language, "gl": Region}
		if vd != "" {
			client["visitorData"] = vd
		}
		h := http.Header{}
		h.Set("Origin", "https://music.youtube.com")
		h.Set("Referer", "https://music.youtube.com/")
		h.Set("X-YouTube-Client-Name", remixID)
		h.Set("X-YouTube-Client-Version", remixVer)
		signRequest(h, "https://music.youtube.com")
		return client, h
	})
}

// callPlayer asks the visionOS client for a video's streams: its formats
// come as plain URLs that serve whole when YouTube allows anonymous access.
func callPlayer(id string) (any, error) {
	return callPlayerOn(httpClient, id)
}

func callPlayerOn(hc *http.Client, id string) (any, error) {
	body := map[string]any{"videoId": id, "contentCheckOk": true, "racyCheckOk": true}
	return postOn(hc, playerAPI, body, func(vd string) (map[string]any, http.Header) {
		client := map[string]any{
			"clientName": visionName, "clientVersion": visionVer, "hl": Language,
			"deviceMake": "Apple", "deviceModel": "RealityDevice17,1",
			"userAgent": visionUA,
			"osName": "visionOS", "osVersion": "26.5.23O471",
		}
		if vd != "" {
			client["visitorData"] = vd
		}
		h := http.Header{}
		h.Set("User-Agent", visionUA)
		h.Set("X-YouTube-Client-Name", visionID)
		h.Set("X-YouTube-Client-Version", visionVer)
		return client, h
	})
}

// callTVPlayer asks the Samsung Smart TV client (TVHTML5) for a video's
// streams. Unlike WEB_REMIX, TVHTML5 supports account cookies without
// requiring GVS PO tokens on googlevideo.com, so signed-in non-Premium and
// Premium accounts both receive playable 128 kbps AAC streams.
func callTVPlayer(id string, sts int) (any, error) {
	return callTVPlayerOn(httpClient, id, sts)
}

func callTVPlayerOn(hc *http.Client, id string, sts int) (any, error) {
	cpc := map[string]any{"html5Preference": "HTML5_PREF_WANTS"}
	if sts > 0 {
		cpc["signatureTimestamp"] = sts
	}
	body := map[string]any{
		"videoId": id, "contentCheckOk": true, "racyCheckOk": true,
		"playbackContext": map[string]any{"contentPlaybackContext": cpc},
	}
	signedIn := SignedIn()
	return postWithVisitorOn(hc, playerAPI, body, !signedIn, func(vd string) (map[string]any, http.Header) {
		client := map[string]any{
			"clientName": tvName, "clientVersion": tvSamsungVer, "hl": Language,
			"deviceMake": "Samsung", "deviceModel": "UKS9800",
			"userAgent": tvSamsungUA,
			"osName": "Tizen", "osVersion": "2.4.0",
			"timeZone": "UTC", "utcOffsetMinutes": 0,
		}
		if vd != "" {
			client["visitorData"] = vd
		}
		h := http.Header{}
		h.Set("User-Agent", tvSamsungUA)
		h.Set("Origin", "https://www.youtube.com")
		h.Set("Referer", "https://www.youtube.com/")
		h.Set("X-YouTube-Client-Name", tvID)
		h.Set("X-YouTube-Client-Version", tvSamsungVer)
		signRequest(h, "https://www.youtube.com")
		return client, h
	})
}

// callWebRemixPlayer asks YouTube Music's WEB_REMIX player endpoint for a
// track's streams (itag 141 for Premium, itag 140 for standard, and HLS).
func callWebRemixPlayer(id string, sts int) (any, error) {
	return callWebRemixPlayerOn(httpClient, id, sts)
}

func callWebRemixPlayerOn(hc *http.Client, id string, sts int) (any, error) {
	cpc := map[string]any{"html5Preference": "HTML5_PREF_WANTS"}
	if sts > 0 {
		cpc["signatureTimestamp"] = sts
	}
	body := map[string]any{
		"videoId": id, "contentCheckOk": true, "racyCheckOk": true,
		"playbackContext": map[string]any{
			"contentPlaybackContext": cpc,
			"adPlaybackContext":      map[string]any{"pyv": true},
		},
	}
	signedIn := SignedIn()
	return postWithVisitorOn(hc, musicAPI+"player?prettyPrint=false", body, !signedIn, func(vd string) (map[string]any, http.Header) {
		client := map[string]any{"clientName": remixName, "clientVersion": remixVer, "hl": Language}
		if vd != "" {
			client["visitorData"] = vd
		}
		h := http.Header{}
		h.Set("Origin", "https://music.youtube.com")
		h.Set("Referer", "https://music.youtube.com/")
		h.Set("X-YouTube-Client-Name", remixID)
		h.Set("X-YouTube-Client-Version", remixVer)
		signRequest(h, "https://music.youtube.com")
		return client, h
	})
}

// callSafariPlayer asks the Safari WEB client for HLS video manifests when
// signed in and anonymous VISIONOS is blocked.
func callSafariPlayer(id string, sts int) (any, error) {
	return callSafariPlayerOn(httpClient, id, sts)
}

func callSafariPlayerOn(hc *http.Client, id string, sts int) (any, error) {
	cpc := map[string]any{"html5Preference": "HTML5_PREF_WANTS"}
	if sts > 0 {
		cpc["signatureTimestamp"] = sts
	}
	body := map[string]any{
		"videoId": id, "contentCheckOk": true, "racyCheckOk": true,
		"playbackContext": map[string]any{"contentPlaybackContext": cpc},
	}
	signedIn := SignedIn()
	return postWithVisitorOn(hc, playerAPI, body, !signedIn, func(vd string) (map[string]any, http.Header) {
		client := map[string]any{
			"clientName": webName, "clientVersion": webVer, "hl": Language,
			"userAgent": webSafariUA,
		}
		if vd != "" {
			client["visitorData"] = vd
		}
		h := http.Header{}
		h.Set("User-Agent", webSafariUA)
		h.Set("Origin", "https://www.youtube.com")
		h.Set("Referer", "https://www.youtube.com/")
		h.Set("X-YouTube-Client-Name", webID)
		h.Set("X-YouTube-Client-Version", webVer)
		signRequest(h, "https://www.youtube.com")
		return client, h
	})
}

// post sends body with the context client makes, and tries once more with
// fresh visitorData if YouTube refuses the one it had.
func post(url string, body map[string]any, client func(vd string) (map[string]any, http.Header)) (any, error) {
	return postOn(httpClient, url, body, client)
}

func postOn(hc *http.Client, url string, body map[string]any, client func(vd string) (map[string]any, http.Header)) (any, error) {
	return postWithVisitorOn(hc, url, body, true, client)
}

func postWithVisitorOn(hc *http.Client, url string, body map[string]any, useVisitor bool, client func(vd string) (map[string]any, http.Header)) (any, error) {
	var err error
	tries := 1
	if useVisitor {
		tries = 2
	}
	for try := 0; try < tries; try++ {
		vd := ""
		if useVisitor {
			vd = visitorData()
		}
		c, h := client(vd)
		payload := make(map[string]any, len(body)+1)
		for k, v := range body {
			payload[k] = v
		}
		payload["context"] = map[string]any{"client": c}
		var v any
		var status int
		v, status, err = postJSONOn(hc, url, payload, h, vd)
		if err == nil {
			return v, nil
		}
		if !useVisitor || (status != 401 && status != 403) {
			break
		}
		dropVisitorData(vd)
	}
	return nil, err
}

func postJSONOn(hc *http.Client, url string, payload any, h http.Header, vd string) (any, int, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", UserAgent)
	}
	if vd != "" {
		req.Header.Set("X-Goog-Visitor-Id", vd)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	var v any
	jerr := json.Unmarshal(data, &v)
	if resp.StatusCode != 200 {
		if msg := str(v, "error", "message"); msg != "" {
			return nil, resp.StatusCode, fmt.Errorf("ytm: %s: %s", resp.Status, msg)
		}
		return nil, resp.StatusCode, fmt.Errorf("ytm: %s", resp.Status)
	}
	if jerr != nil {
		return nil, resp.StatusCode, fmt.Errorf("ytm: %w", jerr)
	}
	if ds := str(v, "responseContext", "mainAppWebResponseContext", "datasyncId"); ds != "" {
		noteDataSyncID(ds)
	}
	return v, resp.StatusCode, nil
}

// get fetches url with the app's User-Agent and returns its body, which
// must come with status 200.
func get(url string, ua string) ([]byte, int, error) {
	return getOn(httpClient, url, ua)
}

func getOn(hc *http.Client, url string, ua string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode, fmt.Errorf("ytm: GET %s: %s", url, resp.Status)
	}
	return body, resp.StatusCode, nil
}

// ttlCache keeps values for a while, up to max of them, sharing one fetch
// among the callers that ask for the same key at once.
type ttlCache[V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]*cacheEntry[V]
}

type cacheEntry[V any] struct {
	mu  sync.Mutex
	at  time.Time
	ok  bool
	val V
}

func newCache[V any](ttl time.Duration, max int) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, max: max, entries: map[string]*cacheEntry[V]{}}
}

// get returns the value of key, fetching it if it is missing or old. A
// failed fetch isn't kept.
func (c *ttlCache[V]) get(key string, fetch func() (V, error)) (V, error) {
	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		if len(c.entries) >= c.max {
			c.evict()
		}
		e = &cacheEntry[V]{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ok && time.Since(e.at) < c.ttl {
		return e.val, nil
	}
	v, err := fetch()
	if err != nil {
		var zero V
		return zero, err
	}
	e.val, e.at, e.ok = v, time.Now(), true
	return v, nil
}

// evict drops the oldest entry, or an unfilled one; c.mu is held.
func (c *ttlCache[V]) evict() {
	var oldest string
	var at time.Time
	for k, e := range c.entries {
		if !e.mu.TryLock() {
			continue
		}
		t, ok := e.at, e.ok
		e.mu.Unlock()
		if !ok {
			t = time.Time{}
		}
		if oldest == "" || t.Before(at) {
			oldest, at = k, t
		}
	}
	if oldest != "" {
		delete(c.entries, oldest)
	}
}

// clear drops every value kept.
func (c *ttlCache[V]) clear() {
	c.mu.Lock()
	c.entries = map[string]*cacheEntry[V]{}
	c.mu.Unlock()
}

// deleteSuffix drops cached entries whose key ends with suffix.
func (c *ttlCache[V]) deleteSuffix(suffix string) {
	c.mu.Lock()
	for k := range c.entries {
		if strings.HasSuffix(k, suffix) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

// clearCaches drops what was kept of the catalogue: signed in or out, the
// home page, the charts and the artists' pages are another listener's.
func clearCaches() {
	homeCache.clear()
	homeFirstCache.clear()
	previewCache.clear()
	chartCache.clear()
	artistCache.clear()
	discoCache.clear()
	clearStreamCaches()
	forgetPremium()
	searches.mu.Lock()
	searches.m = nil
	searches.mu.Unlock()
}
