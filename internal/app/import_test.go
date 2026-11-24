package app

import (
	"context"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"ytb-music/internal/netease"
	"ytb-music/internal/ytm"
)

func drainPosts(t *testing.T, ch <-chan func(), until func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !until() {
		select {
		case fn := <-ch:
			fn()
		case <-deadline:
			t.Fatal("timed out waiting for async UI update")
		}
	}
	for {
		select {
		case fn := <-ch:
			fn()
		default:
			return
		}
	}
}

// Importing a NetEase playlist while signed out saves a persistent local playlist, deduplicates matched tracks, and opens its page on click.
func TestImportNetEaseToLocalPlaylist(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "local_playlists.json")
	realLocalPath := localPlaylistsPath
	localPlaylistsPath = func() (string, error) { return localPath, nil }
	defer func() { localPlaylistsPath = realLocalPath }()

	realFetch, realSearch, realClip := fetchNetEasePlaylist, importSearchTracks, readClipboardText
	defer func() {
		fetchNetEasePlaylist = realFetch
		importSearchTracks = realSearch
		readClipboardText = realClip
	}()

	readClipboardText = func() string {
		return "分享歌单 https://music.163.com/#/playlist?id=889900 (@网易云音乐)"
	}
	fetchNetEasePlaylist = func(_ context.Context, raw, _ string) (*netease.Playlist, error) {
		if !strings.Contains(raw, "889900") {
			t.Fatalf("unexpected raw input: %q", raw)
		}
		return &netease.Playlist{
			ID:       "889900",
			Name:     "深夜爵士",
			CoverURL: "",
			Tracks: []netease.SourceTrack{
				{ID: 1, Name: "晴天", Artists: []string{"周杰伦"}, DurationMs: 269000},
				{ID: 2, Name: "不存在的独白", Artists: []string{"无名氏"}, DurationMs: 180000},
			},
		}, nil
	}
	importSearchTracks = func(query string, _ int) ([]ytm.Track, error) {
		if strings.Contains(query, "晴天") {
			return []ytm.Track{
				{ID: "yt-qingtian", Name: "晴天", Artists: []ytm.Artist{{Name: "周杰伦"}}, Duration: 269000, VideoType: ytm.VideoATV},
			}, nil
		}
		return nil, nil
	}

	posts := make(chan func(), 32)
	a := newTestApp()
	a.settings = defaultSettings()
	a.postHook = func(fn func()) { posts <- fn }
	a.initLocalPlaylists()

	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()

	if err := tt.Click("导入网易云歌单"); err != nil {
		t.Fatal(err)
	}
	tt.Advance(300 * time.Millisecond)
	tt.Frame()

	if err := tt.Click("从剪贴板粘贴"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("开始导入"); err != nil {
		t.Fatal(err)
	}

	drainPosts(t, posts, func() bool { return a.imp.finished || a.imp.err != "" })
	tt.Frame()

	if a.imp.err != "" {
		t.Fatalf("import failed: %s", a.imp.err)
	}
	if a.imp.matchedCount != 1 || len(a.imp.unmatched) != 1 {
		t.Fatalf("matched=%d unmatched=%v, want 1 matched and 1 unmatched", a.imp.matchedCount, a.imp.unmatched)
	}

	if err := tt.Click("打开歌单"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.route.page != pagePlaylist || !isLocalPlaylist(a.route.id) {
		t.Fatalf("route = %+v, want opened local playlist", a.route)
	}

	// Verify disk persistence across a fresh app instance, plus deleting the last local playlist without touching cloud rows.
	a2 := editTestApp()
	a2.initLocalPlaylists()
	if len(a2.localPlaylists) != 1 || a2.localPlaylists[0].Name != "深夜爵士" || len(a2.localPlaylists[0].Tracks) != 1 {
		t.Fatalf("reloaded local playlists = %+v, want 深夜爵士 with 1 track", a2.localPlaylists)
	}
	tt2 := ui.NewTester(a2.view, 1200, 800)
	tt2.Frame()
	if err := tt2.RightClick("深夜爵士"); err != nil {
		t.Fatal(err)
	}
	if err := tt2.ChooseMenuItem("删除歌单…"); err != nil {
		t.Fatal(err)
	}
	tt2.Advance(300 * time.Millisecond)
	tt2.Frame()
	if err := tt2.Click("删除"); err != nil {
		t.Fatal(err)
	}
	tt2.Frame()
	if len(a2.localPlaylists) != 0 || len(a2.mine) != 3 {
		t.Fatalf("after deleting local playlist: local=%d mine=%d, want 0 and 3 cloud playlists intact", len(a2.localPlaylists), len(a2.mine))
	}
}

// Importing a NetEase playlist while signed in batches matched track IDs into a newly created YouTube Music cloud playlist.
func TestImportNetEaseToCloudPlaylistBatches(t *testing.T) {
	realFetch, realSearch := fetchNetEasePlaylist, importSearchTracks
	realCreate, realAdd := createPlaylist, addToPlaylist
	defer func() {
		fetchNetEasePlaylist = realFetch
		importSearchTracks = realSearch
		createPlaylist = realCreate
		addToPlaylist = realAdd
	}()

	fetchNetEasePlaylist = func(_ context.Context, _, musicU string) (*netease.Playlist, error) {
		if musicU != "secretToken123" {
			t.Fatalf("musicU = %q, want secretToken123", musicU)
		}
		tracks := make([]netease.SourceTrack, 55)
		for i := range tracks {
			tracks[i] = netease.SourceTrack{
				ID:         int64(i + 1),
				Name:       "Song",
				Artists:    []string{"Artist"},
				DurationMs: 200000,
			}
		}
		return &netease.Playlist{ID: "77777", Name: "百首精选", Tracks: tracks}, nil
	}
	importSearchTracks = func(query string, _ int) ([]ytm.Track, error) {
		return nil, nil
	}
	// Stub matchNetEaseTracks directly so 55 unique video IDs are produced deterministically.
	realMatch := matchNetEaseTracks
	defer func() { matchNetEaseTracks = realMatch }()
	matchNetEaseTracks = func(_ context.Context, src []netease.SourceTrack, _ int, _ netease.SearchFunc, _ func(int, int, netease.MatchResult)) ([]netease.MatchResult, error) {
		out := make([]netease.MatchResult, len(src))
		for i, st := range src {
			out[i] = netease.MatchResult{
				Index:   i,
				Source:  st,
				Matched: true,
				Track:   ytm.Track{ID: "vid-" + string(rune('A'+i/26)) + string(rune('a'+i%26)), Name: st.Name, Duration: 200000},
			}
		}
		return out, nil
	}

	var createdName string
	var batches [][]string
	createPlaylist = func(name string) (*ytm.Playlist, error) {
		createdName = name
		return &ytm.Playlist{ID: "PL_CLOUD_1", Name: name}, nil
	}
	addToPlaylist = func(pid string, songs ...string) error {
		if pid != "PL_CLOUD_1" {
			t.Fatalf("addToPlaylist pid = %q, want PL_CLOUD_1", pid)
		}
		cp := append([]string(nil), songs...)
		batches = append(batches, cp)
		return nil
	}

	posts := make(chan func(), 64)
	a := editTestApp()
	a.postHook = func(fn func()) { posts <- fn }
	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()

	if err := tt.Click("导入网易云歌单"); err != nil {
		t.Fatal(err)
	}
	tt.Advance(300 * time.Millisecond)
	tt.Frame()

	a.imp.input = "77777"
	a.imp.customName = "我的云端精选"
	if err := tt.Click("高级选项"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	a.imp.musicUInput = "MUSIC_U=secretToken123; os=pc"
	tt.Frame()

	if err := tt.Click("开始导入"); err != nil {
		t.Fatal(err)
	}
	drainPosts(t, posts, func() bool { return a.imp.finished || a.imp.err != "" })
	tt.Frame()

	if a.imp.err != "" {
		t.Fatalf("cloud import failed: %s", a.imp.err)
	}
	if createdName != "我的云端精选" {
		t.Fatalf("created playlist %q, want 我的云端精选", createdName)
	}
	if len(batches) != 2 || len(batches[0]) != 50 || len(batches[1]) != 5 {
		t.Fatalf("batches = %d (%v), want 2 batches of 50 and 5", len(batches), len(batches))
	}
}

// A NetEase fetch failure clears busy state and surfaces the error message even after the worker's deferred context cancel runs.
func TestImportNetEaseFailureClearsBusy(t *testing.T) {
	realFetch := fetchNetEasePlaylist
	defer func() { fetchNetEasePlaylist = realFetch }()

	ctxCaptured := make(chan context.Context, 1)
	fetchNetEasePlaylist = func(ctx context.Context, _, _ string) (*netease.Playlist, error) {
		ctxCaptured <- ctx
		return nil, netease.ErrNeedMusicU
	}

	posts := make(chan func(), 8)
	a := newTestApp()
	a.settings = defaultSettings()
	a.postHook = func(fn func()) { posts <- fn }

	tt := ui.NewTester(a.view, 1200, 800)
	tt.Frame()
	if err := tt.Click("导入网易云歌单"); err != nil {
		t.Fatal(err)
	}
	tt.Advance(300 * time.Millisecond)
	tt.Frame()

	a.imp.input = "123456"
	tt.Frame()
	if err := tt.Click("开始导入"); err != nil {
		t.Fatal(err)
	}

	// Wait until the worker's deferred cancel() has actually cancelled fetchCtx BEFORE running the queued UI callback.
	var workerCtx context.Context
	select {
	case workerCtx = <-ctxCaptured:
	case <-time.After(2 * time.Second):
		t.Fatal("fetchNetEasePlaylist was not called")
	}
	select {
	case <-workerCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not cancel its context on exit")
	}
	drainPosts(t, posts, func() bool { return !a.imp.busy })
	tt.Frame()

	if a.imp.busy || !strings.Contains(a.imp.err, "MUSIC_U") {
		t.Fatalf("busy=%v err=%q, want non-busy MUSIC_U error", a.imp.busy, a.imp.err)
	}
}

// With NM_FRAMES set, renders the NetEase import dialog states for visual inspection.
func TestImportDialogFrames(t *testing.T) {
	dir := os.Getenv("NM_FRAMES")
	if dir == "" {
		t.Skip()
	}
	a := editTestApp()
	a.openImport()
	a.imp.input = "https://music.163.com/playlist?id=2829883282"
	a.imp.showCookie = true
	a.imp.musicUInput = maskedCookiePlaceholder
	a.imp.musicUMasked = true
	a.imp.rememberCookie = true

	tt := ui.NewTester(a.view, 1200, 800)
	save := func(name string) {
		tt.Advance(400 * time.Millisecond)
		tt.Frame()
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		_ = png.Encode(f, tt.Image())
	}
	tt.SetDark(true)
	save("import-dialog-dark")
	tt.SetDark(false)
	save("import-dialog-light")
}

// Imported cloud playlists initially placed in saved are promoted to mine once
// their detail page reports Editable, unlocking track reordering and removal,
// and read-only playlists can be forked into an editable local copy.
func TestEditablePromotionAndPlaylistFork(t *testing.T) {
	tmp := t.TempDir()
	origPath := localPlaylistsPath
	localPlaylistsPath = func() (string, error) { return filepath.Join(tmp, "local_playlists.json"), nil }
	defer func() { localPlaylistsPath = origPath }()

	realStart := playlistStart
	defer func() { playlistStart = realStart }()
	playlistStart = func(id string) (*ytm.Playlist, string, error) {
		return &ytm.Playlist{
			ID:       id,
			Name:     "好耶来了可爱喜欢",
			Editable: true,
			Tracks: []ytm.Track{
				{ID: "jp1", Name: "静寂の一人歩き", Artists: []ytm.Artist{{Name: "O-LIFE.JP"}}, Duration: 294000},
				{ID: "jp2", Name: "妖精は真冬の夢を見る", Artists: []ytm.Artist{{Name: "O-LIFE.JP"}}, Duration: 450000},
			},
		}, "", nil
	}

	a := editTestApp()
	a.initLocalPlaylists()
	posts := make(chan func(), 16)
	a.postHook = func(fn func()) { posts <- fn }

	// Simulate an imported cloud playlist misclassified under saved before opening.
	a.saved = []ytm.Playlist{{ID: "PL_IMPORTED", Name: "好耶来了可爱喜欢", Subscribed: true}}
	a.open(route{page: pagePlaylist, id: "PL_IMPORTED"})
	drainPosts(t, posts, func() bool { return a.playlists["PL_IMPORTED"] != nil })

	if a.editable("PL_IMPORTED") == nil || len(a.saved) != 0 {
		t.Fatalf("expected PL_IMPORTED promoted from saved to editable mine; editable=%v saved=%d", a.editable("PL_IMPORTED"), len(a.saved))
	}

	// Forking a read-only playlist to local creates an editable local playlist and reorders tracks.
	readOnly := &ytm.Playlist{
		ID:   "PL_OTHER",
		Name: "东方精选",
		Tracks: []ytm.Track{
			{ID: "t1", Name: "第一首", Duration: 180000},
			{ID: "t2", Name: "第二首", Duration: 200000},
		},
	}
	a.forkPlaylist(readOnly, false)
	if !isLocalPlaylist(a.route.id) || a.editable(a.route.id) == nil {
		t.Fatalf("expected forked local playlist to be active and editable, route=%+v", a.route)
	}
	forkID := a.route.id
	a.moveTop(forkID, &readOnly.Tracks[1])
	got := a.playlists[forkID].Tracks
	if len(got) != 2 || got[0].ID != "t2" || got[1].ID != "t1" {
		t.Fatalf("reordered tracks = %+v, want [t2 t1]", got)
	}
}
