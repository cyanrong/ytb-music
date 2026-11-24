package netease

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"ytb-music/internal/ytm"
)

// Playlist share links, hash URLs, mobile URLs, and bare IDs extract the
// expected NetEase playlist ID while rejecting unrelated hosts.
func TestParsePlaylistInput(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		short  bool
		ok     bool
	}{
		{"24381616", "24381616", false, true},
		{"https://music.163.com/#/playlist?id=24381616", "24381616", false, true},
		{"https://y.music.163.com/m/playlist?id=88991122&userid=1", "88991122", false, true},
		{"分享创建的歌单「爵士精选」: http://163cn.tv/AbCdEf (来自@网易云音乐)", "", true, true},
		{"https://music.163.com/song?id=24381616", "", false, false},
		{"https://music.163.com/#/album?id=24381616", "", false, false},
		{"https://evil.example.com/playlist?id=24381616", "", false, false},
		{"not-a-playlist", "", false, false},
	}
	for _, tc := range cases {
		id, shortURL, err := ParsePlaylistInput(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ParsePlaylistInput(%q) err=%v", tc.in, err)
			continue
		}
		if !tc.ok && err == nil {
			t.Errorf("ParsePlaylistInput(%q) succeeded (%q, %q), want error", tc.in, id, shortURL)
			continue
		}
		if id != tc.wantID || (shortURL != "") != tc.short {
			t.Errorf("ParsePlaylistInput(%q) = (%q, %q)", tc.in, id, shortURL)
		}
	}
}

// FetchPlaylist expands truncated track lists by batching /api/v3/song/detail
// across all trackIds in order.
func TestFetchPlaylistBatchesTrackIDs(t *testing.T) {
	total := 450
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v6/playlist/detail":
			tids := make([]map[string]int64, total)
			for i := range total {
				tids[i] = map[string]int64{"id": int64(1000 + i)}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"playlist": map[string]any{
					"id":          9999,
					"name":        "发烧试音集",
					"coverImgUrl": "https://p1.music.126.net/cover.jpg",
					"tracks": []map[string]any{
						{"id": 1000, "name": "Track 0", "dt": 210000, "ar": []map[string]string{{"name": "Artist A"}}},
					},
					"trackIds": tids,
				},
			})
		case "/api/v3/song/detail":
			_ = r.ParseForm()
			var reqIDs []struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal([]byte(r.FormValue("c")), &reqIDs)
			songs := make([]map[string]any, len(reqIDs))
			for i, it := range reqIDs {
				songs[i] = map[string]any{
					"id":   it.ID,
					"name": fmt.Sprintf("Track %d", it.ID-1000),
					"dt":   200000,
					"ar":   []map[string]string{{"name": "Artist B"}},
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "songs": songs})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldBase, oldClient := APIBase, HTTPClient
	APIBase = srv.URL
	HTTPClient = srv.Client()
	defer func() {
		APIBase = oldBase
		HTTPClient = oldClient
	}()

	pl, err := FetchPlaylist(context.Background(), "9999", "")
	if err != nil {
		t.Fatal(err)
	}
	if pl.Name != "发烧试音集" || len(pl.Tracks) != total {
		t.Fatalf("got %q with %d tracks, want %d", pl.Name, len(pl.Tracks), total)
	}
	if pl.Tracks[0].Name != "Track 0" || pl.Tracks[total-1].Name != fmt.Sprintf("Track %d", total-1) {
		t.Fatalf("order broken: first=%q last=%q", pl.Tracks[0].Name, pl.Tracks[total-1].Name)
	}
}

// MatchTracks prefers official studio audio over Live/Instrumental uploads and
// preserves original playlist ordering.
func TestMatchTracksPrefersStudioATV(t *testing.T) {
	src := []SourceTrack{
		{ID: 1, Name: "晴天", Artists: []string{"周杰伦"}, DurationMs: 269000},
		{ID: 2, Name: "NoMatchSongXYZ", Artists: []string{"UnknownArtist"}, DurationMs: 180000},
	}
	fakeSearch := func(query string, limit int) ([]ytm.Track, error) {
		if strings.Contains(query, "晴天") {
			return []ytm.Track{
				{ID: "unplay", Name: "晴天", Artists: []ytm.Artist{{Name: "周杰伦"}}, Duration: 269000, VideoType: ytm.VideoATV, Unplayable: true},
				{ID: "live1", Name: "晴天 (Live)", Artists: []ytm.Artist{{Name: "周杰伦"}}, Duration: 290000, VideoType: ytm.VideoOMV},
				{ID: "inst1", Name: "晴天 (伴奏)", Artists: []ytm.Artist{{Name: "周杰伦"}}, Duration: 269000, VideoType: ytm.VideoATV},
				{ID: "atv1", Name: "晴天", Artists: []ytm.Artist{{Name: "周杰伦"}}, Duration: 269500, VideoType: ytm.VideoATV},
			}, nil
		}
		// Same artist and identical duration, but completely different title must be rejected.
		return []ytm.Track{
			{ID: "wrong", Name: "Different Song", Artists: []ytm.Artist{{Name: "UnknownArtist"}}, Duration: 180000, VideoType: ytm.VideoATV},
		}, nil
	}

	got, err := MatchTracks(context.Background(), src, 2, fakeSearch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Matched || got[0].Track.ID != "atv1" {
		t.Fatalf("track 0 matched %+v, want atv1", got[0])
	}
	if got[1].Matched {
		t.Fatalf("track 1 should not match unrelated result: %+v", got[1])
	}

	// SanitizeMusicU rejects injection characters and extracts MUSIC_U from Cookie headers.
	if tok, err := SanitizeMusicU("os=pc; MUSIC_U=abcdef012345; __csrf=1"); err != nil || tok != "abcdef012345" {
		t.Fatalf("SanitizeMusicU valid: %q, %v", tok, err)
	}
	if _, err := SanitizeMusicU("bad\r\nheader"); err == nil {
		t.Fatal("SanitizeMusicU accepted CRLF")
	}
	u, _ := url.Parse("https://music.163.com/playlist/1234567/userid")
	if extractIDFromURL(u) != "1234567" {
		t.Fatalf("path ID extraction failed")
	}
}
