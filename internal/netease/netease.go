package netease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"ytb-music/internal/ytm"
)

var (
	ErrInvalidInput = errors.New("netease: 未识别到有效的网易云歌单链接或 ID")
	ErrNeedMusicU   = errors.New("netease: 该歌单需要登录权限，请展开高级选项填入网易云 MUSIC_U")
	ErrNotFound     = errors.New("netease: 未找到该网易云歌单")
)

const (
	defaultAPIBase = "https://music.163.com"
	neteaseUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	songBatchSize  = 400
)

// Playlist is a NetEase Cloud Music playlist with its full ordered track list.
type Playlist struct {
	ID       string
	Name     string
	CoverURL string
	Tracks   []SourceTrack
}

// SourceTrack is a track from NetEase Cloud Music to be matched on YouTube Music.
type SourceTrack struct {
	ID         int64
	Name       string
	Alias      string
	Artists    []string
	Album      string
	CoverURL   string
	DurationMs int64
}

func (t SourceTrack) DisplayArtist() string {
	return strings.Join(t.Artists, " / ")
}

var (
	urlExtractRe  = regexp.MustCompile(`https?://[^\s"'<>）)】]+`)
	digitsOnlyRe  = regexp.MustCompile(`^\d{4,20}$`)
	playlistPathRe = regexp.MustCompile(`/playlist/(\d{4,20})(?:[/?#]|$)`)
	musicUValidRe = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)
)

// APIBase and HTTPClient allow offline unit tests to point at httptest servers.
var (
	APIBase    = defaultAPIBase
	HTTPClient = newNetEaseClient()
)

func newNetEaseClient() *http.Client {
	// NetEase is a domestic CN service: try direct connections first so an
	// overseas YouTube proxy does not block or slow down 163.com, falling back
	// to ytm.Transport() only when direct dial fails.
	direct := &http.Transport{
		Proxy:                 nil,
		ResponseHeaderTimeout: 12 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := direct.RoundTrip(req)
			if err == nil {
				return resp, nil
			}
			if req.Context().Err() != nil {
				return nil, err
			}
			return ytm.Transport().RoundTrip(req)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// SanitizeMusicU extracts and validates a MUSIC_U token from either a bare
// token value or a full Cookie header string, rejecting header-injection chars.
func SanitizeMusicU(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "Cookie:"), "cookie:"))
	if strings.Contains(s, "=") {
		for _, part := range strings.Split(s, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "MUSIC_U") {
				s = strings.TrimSpace(v)
				break
			}
		}
	}
	if s == "" || !musicUValidRe.MatchString(s) {
		return "", errors.New("netease: MUSIC_U 格式不正确")
	}
	return s, nil
}

// ParsePlaylistInput extracts a numeric playlist ID from a bare ID, a desktop
// or mobile URL, or share text. When the share text holds a 163cn.tv short
// link, shortURL is returned for redirect resolution.
func ParsePlaylistInput(raw string) (id string, shortURL string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", ErrInvalidInput
	}
	if digitsOnlyRe.MatchString(s) {
		return s, "", nil
	}
	candidate := s
	if m := urlExtractRe.FindString(s); m != "" {
		candidate = strings.TrimRight(m, ".,;!?，。；！？")
	}
	u, err := url.Parse(candidate)
	if err != nil || u.Host == "" {
		return "", "", ErrInvalidInput
	}
	host := strings.ToLower(u.Hostname())
	if host == "163cn.tv" || strings.HasSuffix(host, ".163cn.tv") {
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", "", ErrInvalidInput
		}
		return "", u.String(), nil
	}
	if !isNetEaseMusicHost(host) {
		return "", "", ErrInvalidInput
	}
	if pid := extractIDFromURL(u); pid != "" {
		return pid, "", nil
	}
	return "", "", ErrInvalidInput
}

func isNetEaseMusicHost(host string) bool {
	host = strings.ToLower(host)
	return host == "music.163.com" || strings.HasSuffix(host, ".music.163.com")
}

func extractIDFromURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	if id := u.Query().Get("id"); digitsOnlyRe.MatchString(id) {
		return id
	}
	if m := playlistPathRe.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	if u.Fragment != "" {
		frag := u.Fragment
		if _, q, ok := strings.Cut(frag, "?"); ok {
			if vals, err := url.ParseQuery(q); err == nil {
				if id := vals.Get("id"); digitsOnlyRe.MatchString(id) {
					return id
				}
			}
		}
		if m := playlistPathRe.FindStringSubmatch(frag); m != nil {
			return m[1]
		}
	}
	return ""
}

func resolveShortLink(ctx context.Context, shortURL string) (string, error) {
	noRedirect := &http.Client{
		Timeout:   10 * time.Second,
		Transport: HTTPClient.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	cur := shortURL
	for range 5 {
		u, err := url.Parse(cur)
		if err != nil {
			return "", ErrInvalidInput
		}
		host := strings.ToLower(u.Hostname())
		if host != "163cn.tv" && !strings.HasSuffix(host, ".163cn.tv") && !isNetEaseMusicHost(host) {
			return "", ErrInvalidInput
		}
		if isNetEaseMusicHost(host) {
			if pid := extractIDFromURL(u); pid != "" {
				return pid, nil
			}
		}
		req, err := http.NewRequestWithContext(ctx, "GET", cur, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", neteaseUA)
		resp, err := noRedirect.Do(req)
		if err != nil {
			return "", err
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			break
		}
		nextURL, err := u.Parse(loc)
		if err != nil {
			return "", ErrInvalidInput
		}
		cur = nextURL.String()
	}
	if u, err := url.Parse(cur); err == nil && isNetEaseMusicHost(u.Hostname()) {
		if pid := extractIDFromURL(u); pid != "" {
			return pid, nil
		}
	}
	return "", ErrInvalidInput
}

// FetchPlaylist resolves rawInput and fetches the complete ordered track list
// of the NetEase playlist, batching /api/v3/song/detail for large playlists.
func FetchPlaylist(ctx context.Context, rawInput, rawMusicU string) (*Playlist, error) {
	musicU, err := SanitizeMusicU(rawMusicU)
	if err != nil {
		return nil, err
	}
	id, shortURL, err := ParsePlaylistInput(rawInput)
	if err != nil {
		return nil, err
	}
	if id == "" && shortURL != "" {
		id, err = resolveShortLink(ctx, shortURL)
		if err != nil {
			return nil, err
		}
	}

	form := url.Values{"id": {id}, "n": {"100000"}, "s": {"0"}}
	var detail struct {
		Code     int    `json:"code"`
		Message  string `json:"message"`
		Playlist *struct {
			ID          int64     `json:"id"`
			Name        string    `json:"name"`
			CoverImgURL string    `json:"coverImgUrl"`
			Tracks      []rawSong `json:"tracks"`
			TrackIDs    []struct {
				ID int64 `json:"id"`
			} `json:"trackIds"`
		} `json:"playlist"`
	}
	if err := postFormJSON(ctx, APIBase+"/api/v6/playlist/detail", form, musicU, &detail); err != nil {
		return nil, err
	}
	switch detail.Code {
	case 200:
	case 401, 403, 20001:
		return nil, ErrNeedMusicU
	case 404:
		return nil, ErrNotFound
	default:
		if strings.Contains(detail.Message, "权限") || strings.Contains(detail.Message, "登录") {
			return nil, ErrNeedMusicU
		}
		return nil, fmt.Errorf("netease: 接口返回状态 %d", detail.Code)
	}
	if detail.Playlist == nil {
		return nil, ErrNotFound
	}

	byID := make(map[int64]SourceTrack, len(detail.Playlist.Tracks))
	for _, s := range detail.Playlist.Tracks {
		if st := s.toSourceTrack(); st.ID != 0 {
			byID[st.ID] = st
		}
	}

	order := make([]int64, 0, max(len(detail.Playlist.TrackIDs), len(detail.Playlist.Tracks)))
	for _, tid := range detail.Playlist.TrackIDs {
		if tid.ID != 0 {
			order = append(order, tid.ID)
		}
	}
	if len(order) == 0 {
		for _, s := range detail.Playlist.Tracks {
			if s.ID != 0 {
				order = append(order, s.ID)
			}
		}
	}

	var missing []int64
	seenMissing := map[int64]bool{}
	for _, tid := range order {
		if _, ok := byID[tid]; !ok && !seenMissing[tid] {
			seenMissing[tid] = true
			missing = append(missing, tid)
		}
	}

	for start := 0; start < len(missing); start += songBatchSize {
		end := min(start+songBatchSize, len(missing))
		batch, err := fetchSongBatch(ctx, missing[start:end], musicU)
		if err != nil {
			return nil, err
		}
		for _, st := range batch {
			byID[st.ID] = st
		}
	}

	tracks := make([]SourceTrack, 0, len(order))
	for _, tid := range order {
		if st, ok := byID[tid]; ok && st.Name != "" {
			tracks = append(tracks, st)
		}
	}
	if len(tracks) == 0 && len(order) > 0 && musicU == "" {
		return nil, ErrNeedMusicU
	}

	name := strings.TrimSpace(detail.Playlist.Name)
	if name == "" {
		name = "网易云歌单 " + id
	}
	return &Playlist{
		ID:       id,
		Name:     name,
		CoverURL: detail.Playlist.CoverImgURL,
		Tracks:   tracks,
	}, nil
}

type rawSong struct {
	ID   int64    `json:"id"`
	Name string   `json:"name"`
	Alia []string `json:"alia"`
	Tns  []string `json:"tns"`
	Dt   int64    `json:"dt"`
	Duration int64 `json:"duration"`
	Ar   []struct {
		Name string `json:"name"`
	} `json:"ar"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Al struct {
		Name   string `json:"name"`
		PicURL string `json:"picUrl"`
	} `json:"al"`
	Album struct {
		Name   string `json:"name"`
		PicURL string `json:"picUrl"`
	} `json:"album"`
}

func (s rawSong) toSourceTrack() SourceTrack {
	st := SourceTrack{
		ID:         s.ID,
		Name:       strings.TrimSpace(s.Name),
		DurationMs: s.Dt,
	}
	if st.DurationMs == 0 {
		st.DurationMs = s.Duration
	}
	if len(s.Tns) > 0 {
		st.Alias = strings.TrimSpace(s.Tns[0])
	} else if len(s.Alia) > 0 {
		st.Alias = strings.TrimSpace(s.Alia[0])
	}
	for _, a := range s.Ar {
		if n := strings.TrimSpace(a.Name); n != "" {
			st.Artists = append(st.Artists, n)
		}
	}
	if len(st.Artists) == 0 {
		for _, a := range s.Artists {
			if n := strings.TrimSpace(a.Name); n != "" {
				st.Artists = append(st.Artists, n)
			}
		}
	}
	st.Album = strings.TrimSpace(s.Al.Name)
	st.CoverURL = s.Al.PicURL
	if st.Album == "" {
		st.Album = strings.TrimSpace(s.Album.Name)
	}
	if st.CoverURL == "" {
		st.CoverURL = s.Album.PicURL
	}
	return st
}

func fetchSongBatch(ctx context.Context, ids []int64, musicU string) ([]SourceTrack, error) {
	type cItem struct {
		ID int64 `json:"id"`
	}
	items := make([]cItem, len(ids))
	for i, id := range ids {
		items[i] = cItem{ID: id}
	}
	cJSON, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	form := url.Values{"c": {string(cJSON)}}
	var resp struct {
		Code  int       `json:"code"`
		Songs []rawSong `json:"songs"`
	}
	if err := postFormJSON(ctx, APIBase+"/api/v3/song/detail", form, musicU, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("netease: 曲目详情返回状态 %d", resp.Code)
	}
	out := make([]SourceTrack, 0, len(resp.Songs))
	for _, s := range resp.Songs {
		if st := s.toSourceTrack(); st.ID != 0 {
			out = append(out, st)
		}
	}
	return out, nil
}

func postFormJSON(ctx context.Context, endpoint string, form url.Values, musicU string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://music.163.com/")
	req.Header.Set("Origin", "https://music.163.com")
	req.Header.Set("User-Agent", neteaseUA)
	cookie := "os=pc; appver=3.0.18.203152"
	if musicU != "" {
		cookie += "; MUSIC_U=" + musicU
	}
	req.Header.Set("Cookie", cookie)

	res, err := HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("netease: HTTP %d", res.StatusCode)
	}
	return json.Unmarshal(body, dst)
}
