package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"ytb-music/internal/bridge"
	"ytb-music/internal/localmusic"
	"ytb-music/internal/player"
	"ytb-music/internal/ytm"
)

const (
	localTabAlbums = iota
	localTabArtists
	localTabGenres
	localTabTracks
	localTabFolders
)

// Replaceable OS / Roon hooks for deterministic UI tests.
var (
	pickMusicFolders = func(parent *mygo.Window) ([]string, error) {
		return mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:    parent,
			Title:     "选择本地音乐文件夹",
			Directory: true,
			Multiple:  true,
		})
	}
	showInFolder = func(path string) {
		if path != "" {
			mygo.Shell.ShowItemInFolder(path)
		}
	}
	acquireRoonManager = func(a *app) (*bridge.RoonManager, error) {
		if a.audio.roonMgr != nil {
			return a.audio.roonMgr, nil
		}
		if _, err := a.ensureRoonBackend(); err != nil {
			return nil, err
		}
		return a.audio.roonMgr, nil
	}
	fetchRoonGenres = func(mgr *bridge.RoonManager, ctx context.Context) ([]bridge.RoonAlbumGenre, error) {
		if mgr == nil {
			return nil, errors.New("尚未连接或授权 Roon Core")
		}
		return mgr.FetchLibraryGenres(ctx)
	}
)

type localPlaylistPrompt struct {
	open bool
	name string
	add  *ytm.Track
}

type localUIState struct {
	lib         *localmusic.Library
	tab         int    // localTabAlbums..localTabFolders
	filter      string // instant search/filter text
	filterBox   ui.Handle
	genre       string // active genre filter
	focus       string // "", "hires", "dsd", "lossless", "cue"
	sort        string // "recent", "title", "artist", "year", "spec"
	folderPath  string // current browsed directory ("" = all watched roots)
	foldersOpen bool   // watched-folder manager inline panel

	scanning     bool
	scanProgress localmusic.ScanProgress
	scanCancel   context.CancelFunc
	scanGen      int
	picking      bool
	syncingRoon  bool

	tracksList        ui.ListState
	tracksSig         string
	tracksCacheSig    string
	tracksCacheRev    uint64
	tracksCacheReady  bool
	cachedLocalTracks []localmusic.LocalTrack
	cachedYTMTracks   []ytm.Track

	newPl localPlaylistPrompt
}

type localFileRawStream struct {
	*os.File
	size int64
}

func (r *localFileRawStream) Size() (int64, error) { return r.size, nil }

type localPlayerProvider struct {
	lib *localmusic.Library
}

func (p localPlayerProvider) IsLocalTrack(id string) bool {
	return localmusic.IsLocalTrack(id)
}

func (p localPlayerProvider) OpenLocalTrack(id string) (*player.LocalOpenedTrack, error) {
	if p.lib == nil {
		return nil, errors.New("localmusic: library not initialized")
	}
	tr, ok := p.lib.TrackByID(id)
	if !ok || tr == nil {
		return nil, errors.New("localmusic: track not found: " + id)
	}
	var cover []byte
	if tr.CoverID != "" {
		cover, _ = p.lib.CoverBytes(tr.CoverID)
	}
	openPCM := func() (player.PCMDecoder, error) {
		dec, err := localmusic.OpenTrackDecoder(tr)
		if err != nil {
			return nil, err
		}
		return player.WrapPCMDecoder(dec), nil
	}
	var openRaw func() (player.RawStream, error)
	ext := strings.ToLower(filepath.Ext(tr.Path))
	if !tr.Spec.IsCUE && strings.EqualFold(tr.Spec.Codec, "AAC") && (ext == ".m4a" || ext == ".mp4") {
		path := tr.Path
		openRaw = func() (player.RawStream, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			st, err := f.Stat()
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			return &localFileRawStream{File: f, size: st.Size()}, nil
		}
	}
	return &player.LocalOpenedTrack{
		OpenPCM:   openPCM,
		OpenRaw:   openRaw,
		Stream:    tr.ToYTMStream(),
		LocalPath: tr.Path,
		NativeHQP: tr.NativeHQP(),
		Cover:     cover,
	}, nil
}

func (p localPlayerProvider) LocalLyrics(id string) (lrc, trans string, plain []string) {
	if p.lib == nil {
		return "", "", nil
	}
	lrc, trans = p.lib.Lyrics(id)
	return lrc, trans, nil
}

// initLocalLibrary initializes the persistent local music index and cover cache
// on startup, wiring it to the cover loader and player before the window first renders.
func (a *app) initLocalLibrary() {
	userDataDir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil || userDataDir == "" {
		return
	}
	var coverCacheDir string
	if cacheDir, err := mygo.App.Path(mygo.PathCache); err == nil && cacheDir != "" {
		coverCacheDir = filepath.Join(cacheDir, "local_covers")
	} else {
		coverCacheDir = filepath.Join(userDataDir, "local_covers")
	}
	lib := localmusic.NewLibrary(filepath.Join(userDataDir, "local_library.json"), coverCacheDir)
	_ = lib.Load()
	a.setLocalLibrary(lib)
}

// startInitialLocalScan begins the incremental background folder check once the
// main window exists and can deliver completion posts to the UI thread.
func (a *app) startInitialLocalScan() {
	if a.local.lib != nil && len(a.local.lib.Folders()) > 0 {
		a.rescanLocalLibrary()
	}
}

// setLocalLibrary attaches lib to the app, cover cache, and player.
func (a *app) setLocalLibrary(lib *localmusic.Library) {
	a.local.lib = lib
	a.local.tracksCacheReady = false
	a.local.cachedLocalTracks = nil
	a.local.cachedYTMTracks = nil
	if a.local.sort == "" {
		a.local.sort = "recent"
	}
	if lib == nil {
		setLocalCoverLoader(nil)
		if a.pl != nil {
			a.pl.Local = nil
		}
		return
	}
	setLocalCoverLoader(lib.CoverBytes)
	if a.pl != nil {
		a.pl.Local = localPlayerProvider{lib: lib}
	}
}

func (a *app) closeLocalLibrary() {
	if a.local.scanCancel != nil {
		a.local.scanCancel()
		a.local.scanCancel = nil
	}
}

// invalidateLocalPages drops cached local album/artist detail snapshots so
// rescans and Roon genre merges reflect immediately on detail pages.
func (a *app) invalidateLocalPages() {
	for id := range a.albums {
		if localmusic.IsLocalAlbum(id) {
			delete(a.albums, id)
		}
	}
	for id := range a.artists {
		if localmusic.IsLocalArtist(id) {
			delete(a.artists, id)
		}
	}
	switch a.route.page {
	case pageAlbum:
		if localmusic.IsLocalAlbum(a.route.id) {
			a.loadAlbum(a.route.id)
		}
	case pageArtist:
		if localmusic.IsLocalArtist(a.route.id) {
			a.loadArtist(a.route.id)
		}
	}
}

// rescanLocalLibrary triggers an incremental background folder scan.
func (a *app) rescanLocalLibrary() {
	lib := a.local.lib
	if lib == nil {
		return
	}
	if len(lib.Folders()) == 0 {
		a.toast = "请先添加本地音乐文件夹"
		return
	}
	if a.local.scanCancel != nil {
		a.local.scanCancel()
	}
	a.local.scanGen++
	gen := a.local.scanGen
	ctx, cancel := context.WithCancel(context.Background())
	a.local.scanCancel = cancel
	a.local.scanning = true
	a.local.scanProgress = localmusic.ScanProgress{Scanning: true}

	beforeRev := lib.Revision()
	go func() {
		err := lib.Scan(ctx, func(sp localmusic.ScanProgress) {
			a.post(func() {
				if a.local.scanGen != gen || !a.local.scanning {
					return
				}
				a.local.scanProgress = sp
			})
		})
		a.post(func() {
			if a.local.scanGen != gen {
				return
			}
			a.local.scanning = false
			a.local.scanProgress.Scanning = false
			a.local.scanCancel = nil
			if err == nil || lib.Revision() != beforeRev {
				a.invalidateLocalPages()
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				a.toast = "扫描本地曲库失败：" + err.Error()
			}
		})
	}()
}

func (a *app) addLocalFolders() {
	if a.local.picking {
		return
	}
	if a.local.lib == nil {
		a.local.lib = localmusic.NewLibrary("", "")
		a.setLocalLibrary(a.local.lib)
	}
	lib := a.local.lib
	parent := a.win
	a.local.picking = true
	go func() {
		dirs, err := pickMusicFolders(parent)
		a.post(func() {
			a.local.picking = false
			if err != nil || len(dirs) == 0 {
				return
			}
			lib.AddFolders(dirs...)
			a.rescanLocalLibrary()
		})
	}()
}

func (a *app) removeLocalFolder(folder string) {
	lib := a.local.lib
	if lib == nil {
		return
	}
	if !lib.RemoveFolder(folder) {
		return
	}
	if len(lib.Folders()) == 0 {
		a.local.foldersOpen = false
		a.local.folderPath = ""
	} else if a.local.folderPath != "" {
		node := lib.BrowseFolder(a.local.folderPath)
		if len(node.Subdirs) == 0 && len(node.Tracks) == 0 {
			a.local.folderPath = ""
		}
	}
	a.invalidateLocalPages()
	a.toast = "已移除监控目录"
}

func (a *app) syncLocalRoonGenres() {
	if a.local.syncingRoon || a.local.lib == nil {
		return
	}
	if a.local.lib.Stats().Albums == 0 {
		a.toast = "请先导入本地音乐文件夹"
		return
	}
	mgr, mgrErr := acquireRoonManager(a)
	if mgrErr != nil {
		a.toast = "启动 Roon 同步失败：" + mgrErr.Error()
		return
	}
	lib := a.local.lib
	a.local.syncingRoon = true

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		items, err := fetchRoonGenres(mgr, ctx)
		var matched int
		if err == nil && len(items) > 0 {
			conv := make([]localmusic.RoonAlbumGenre, len(items))
			for i, it := range items {
				conv[i] = localmusic.RoonAlbumGenre{
					Album:  it.Album,
					Artist: it.Artist,
					Genres: slices.Clone(it.Genres),
				}
			}
			matched = lib.ApplyRoonGenres(conv)
		}
		a.post(func() {
			a.local.syncingRoon = false
			if err != nil {
				a.toast = "请先在设置中连接并授权 Roon Core（YTB Music Bridge）"
				return
			}
			a.invalidateLocalPages()
			switch {
			case matched > 0:
				a.toast = fmt.Sprintf("已从 Roon 同步 %d 张专辑的流派分类", matched)
			case len(items) == 0:
				a.toast = "Roon 曲库中暂无可同步的流派分类"
			default:
				a.toast = "未找到与本地专辑匹配的 Roon 流派条目"
			}
		})
	}()
}

func toYTMTracks(src []localmusic.LocalTrack) []ytm.Track {
	out := make([]ytm.Track, len(src))
	for i := range src {
		out[i] = src[i].ToYTMTrack()
	}
	return out
}

func (a *app) playLocalTracks(tracks []localmusic.LocalTrack, start int) {
	if len(tracks) == 0 {
		return
	}
	ytmTracks := toYTMTracks(tracks)
	a.pl.PlayQueue(ytmTracks, start)
}

func (a *app) localTrackByID(id string) (*localmusic.LocalTrack, bool) {
	if a.local.lib == nil {
		return nil, false
	}
	return a.local.lib.TrackByID(id)
}

func (a *app) openNewLocalPlaylist(tr *ytm.Track) {
	var cp *ytm.Track
	if tr != nil {
		t := *tr
		cp = &t
	}
	a.local.newPl = localPlaylistPrompt{open: true, add: cp}
}

func (a *app) submitNewLocalPlaylist() {
	d := &a.local.newPl
	name := strings.TrimSpace(d.name)
	if name == "" {
		return
	}
	var tracks []ytm.Track
	var cover string
	if d.add != nil {
		tracks = []ytm.Track{*d.add}
		cover = d.add.Album.PicURL
	}
	pl, err := a.createLocalPlaylist(name, "", cover, tracks)
	if err != nil {
		a.toast = "创建本地歌单失败，请检查磁盘空间"
		return
	}
	d.open = false
	d.name = ""
	d.add = nil
	a.open(route{page: pagePlaylist, id: pl.ID})
	a.toast = "已创建本地歌单"
}

func (a *app) localPlaylistNameDialog(c *ui.Context) {
	p := a.pal
	d := &a.local.newPl
	ui.DialogBase(c.Key("local-playlist-name"), &d.open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.RGBA(0, 0, 0, 0.35))
		panel.Width(360).Padding(24).Radius(18).Gap(16).
			Background(p.bg).Border(1, p.hairline).Shadow(0, 24, 60, -12, ui.RGBA(0, 0, 0, 0.4)).
			Transition(ui.ElementTransition{Animation: &ui.Snappy, Enter: &ui.Motion{Scale: 0.94}, Exit: &ui.Motion{Scale: 0.96}})
		ui.Text(c, "新建本地歌单").FontSize(17).FontWeight(700)
		in := ui.TextInput(c.Key("local-pl-input"), &d.name).Placeholder("歌单名称").Label("本地歌单名称").FontSize(14).AutoFocus()
		if in.Submitted() {
			a.submitNewLocalPlaylist()
		}
		ui.Row(c).Gap(10).Justify(ui.End).Children(func() {
			if pillButton(c.Key("cancel"), nil, "取消", p.hover.Over(p.bg), p.text).Clicked() {
				d.open = false
			}
			ok := pillButton(c.Key("ok"), nil, "创建", p.accent, p.onAccent)
			if strings.TrimSpace(d.name) == "" {
				ok.Disabled(true).Opacity(0.5)
			}
			if ok.Clicked() {
				a.submitNewLocalPlaylist()
			}
		})
	})
}

// localSongPlaylistItems renders playlist submenu options tailored for local
// tracks (allowing local playlists both signed-in and signed-out, while
// omitting YouTube cloud playlists that only accept 11-character video IDs).
func (a *app) localSongPlaylistItems(m *ui.Menu, tr *ytm.Track, pid string) {
	m.Separator()
	m.Submenu("添加到歌单", func(m *ui.Menu) {
		if m.Item("新建歌单…").Chosen() {
			a.openNewLocalPlaylist(tr)
		}
		var locals []ytm.Playlist
		for i := range a.localPlaylists {
			if pl := a.localPlaylists[i]; pl.ID != pid {
				locals = append(locals, pl)
			}
		}
		if len(locals) > 0 {
			m.Separator()
		}
		for i := range locals {
			pl := locals[i]
			if m.Item(pl.Name).Chosen() {
				a.addTo(pl.ID, tr)
			}
		}
	})
	if isLocalPlaylist(pid) && a.editable(pid) != nil {
		if m.Item("移至歌单顶部").Chosen() {
			a.moveTop(pid, tr)
		}
		if m.Item("从歌单中移除").Chosen() {
			a.removeFrom(pid, tr)
		}
	}
}

// audiophileGold returns the warm Roon/HQPlayer gold accent color for Hi-Res & DSD badges.
func audiophileGold(dark bool) (fg, bg, border ui.Color) {
	if dark {
		return ui.Hex("#f5d58a"), ui.RGBA(245, 213, 138, 0.13), ui.RGBA(245, 213, 138, 0.34)
	}
	return ui.Hex("#b87d14"), ui.RGBA(184, 125, 20, 0.10), ui.RGBA(184, 125, 20, 0.28)
}

var localSortOptions = []struct {
	key, label string
}{
	{"recent", "最近添加"},
	{"title", "专辑名"},
	{"artist", "艺人"},
	{"year", "年份"},
	{"spec", "规格"},
}

var localFocusOptions = []struct {
	key, label string
}{
	{"", "全部"},
	{"hires", "Hi-Res"},
	{"dsd", "DSD"},
	{"lossless", "无损"},
	{"cue", "CUE 分轨"},
}

func localSortLabel(key string) string {
	for _, o := range localSortOptions {
		if o.key == key {
			return o.label
		}
	}
	return "最近添加"
}

func localStatsSubtitle(st localmusic.Stats) string {
	if st.Tracks == 0 {
		return "导入本地无损与 DSD 曲库，直通 HQPlayer / Roon"
	}
	parts := []string{
		fmt.Sprintf("%d 张专辑", st.Albums),
		fmt.Sprintf("%d 首曲目", st.Tracks),
	}
	if st.Artists > 0 {
		parts = append(parts, fmt.Sprintf("%d 位艺人", st.Artists))
	}
	if st.HiResAlbums > 0 {
		parts = append(parts, fmt.Sprintf("%d 张 Hi-Res / DSD", st.HiResAlbums))
	}
	return strings.Join(parts, " · ")
}

func (a *app) localPage(c *ui.Context) {
	if a.local.sort == "" {
		a.local.sort = "recent"
	}
	var stats localmusic.Stats
	var folders []string
	if a.local.lib != nil {
		stats = a.local.lib.Stats()
		folders = a.local.lib.Folders()
	}

	// Empty onboarding state when no tracks are indexed and not actively scanning.
	if stats.Tracks == 0 && !a.local.scanning {
		top, bottom := a.insets(c)
		ui.Scroll(c.Key("local-empty")).TrackScroll(&a.pageScroll).Grow(1).
			Padding(top+pageGap, pagePad, bottom+32, pagePad).ScrollbarInsets(top, 0, bottom, 0).Children(func() {
			a.startMark(c)
			defer a.endMark(c)
			a.localToolbarHeader(c, stats, folders)
			if a.local.foldersOpen && len(folders) > 0 {
				a.localFoldersPanel(c, folders)
			}
			a.localEmptyState(c)
		})
		return
	}

	// Virtualized ui.List when on the Tracks lens.
	if a.local.tab == localTabTracks {
		a.localTracksLens(c, stats, folders)
		return
	}

	top, bottom := a.insets(c)
	ui.Scroll(c.Key("local-scroll")).TrackScroll(&a.pageScroll).Grow(1).
		Padding(top+pageGap, pagePad-12, bottom+40, pagePad-12).ScrollbarInsets(top, 0, bottom, 0).Children(func() {
		a.startMark(c)
		defer a.endMark(c)
		ui.Column(c).PaddingX(12).Children(func() {
			a.localTopControls(c, stats, folders)
			switch a.local.tab {
			case localTabAlbums:
				a.localAlbumsLens(c)
			case localTabArtists:
				a.localArtistsLens(c)
			case localTabGenres:
				a.localGenresLens(c)
			case localTabFolders:
				a.localFoldersLens(c)
			}
		})
	})
}

func (a *app) localToolbarHeader(c *ui.Context, stats localmusic.Stats, folders []string) {
	p := a.pal
	ui.Row(c).Gap(16).AlignItems(ui.End).Margin(0, 0, 18, 0).Wrap().Children(func() {
		ui.Column(c).Grow(1).MinWidth(240).Gap(4).Children(func() {
			ui.Text(c, localStatsSubtitle(stats)).FontSize(12).FontWeight(600).TextColor(p.muted).LetterSpacing(0.3)
			ui.Text(c, "本地音乐").FontSize(30).FontWeight(700).LetterSpacing(-0.6)
		})
		ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
			if a.localActionPill(c.Key("add-folder"), iconPlus, "+ 添加文件夹", true, false).Clicked() {
				a.addLocalFolders()
			}
			scanLabel := "重新扫描"
			if a.local.scanning {
				sp := a.local.scanProgress
				if sp.Total > 0 {
					scanLabel = fmt.Sprintf("扫描中 (%d/%d)…", sp.Scanned, sp.Total)
				} else {
					scanLabel = "扫描中…"
				}
			}
			scanBtn := a.localActionPill(c.Key("rescan"), iconReload, scanLabel, false, false).Label("重新扫描")
			if a.local.scanning || len(folders) == 0 {
				scanBtn.Disabled(a.local.scanning)
				if a.local.scanning {
					scanBtn.Opacity(0.7)
				}
			}
			if scanBtn.Clicked() && !a.local.scanning {
				a.rescanLocalLibrary()
			}

			roonLabel := "从 Roon 同步流派"
			if a.local.syncingRoon {
				roonLabel = "正在同步流派…"
			}
			roonBtn := a.localActionPill(c.Key("roon-sync"), iconTag, roonLabel, false, false).Label("从 Roon 同步流派")
			if a.local.syncingRoon {
				roonBtn.Disabled(true).Opacity(0.7)
			}
			if roonBtn.Clicked() && !a.local.syncingRoon {
				a.syncLocalRoonGenres()
			}

			if len(folders) > 0 {
				dirBtn := a.localActionPill(c.Key("folders-mgr"), iconFolder, fmt.Sprintf("目录管理 (%d)", len(folders)), false, a.local.foldersOpen).Label("目录管理")
				if dirBtn.Clicked() {
					a.local.foldersOpen = !a.local.foldersOpen
				}
			}
		})
	})
}

func (a *app) localActionPill(c *ui.Context, icon *ui.SVG, text string, primary, active bool) ui.Element {
	p := a.pal
	bg, fg := p.hover.Over(p.bg), p.text
	if primary {
		bg, fg = p.accent, p.onAccent
	} else if active {
		bg = p.press.Over(p.bg)
	}
	b := ui.ButtonBase(c).Height(32).PaddingX(13).Gap(6).Radius(999).AlignItems(ui.Center).TextColor(fg).FocusRing(false)
	fill := bg
	if b.Hovered() {
		fill = bg.Mix(ui.RGB(255, 255, 255), 0.12)
	}
	b.Background(b.AnimateColor("bg", fill, ui.Animation{Duration: 120 * time.Millisecond}))
	if !primary {
		b.Border(1, p.hairline)
	}
	b.Children(func() {
		if icon != nil && !strings.HasPrefix(text, "+ ") {
			ui.Icon(c, icon).Size(13.5, 13.5)
		}
		ui.Text(c, text).FontSize(12.5).FontWeight(600).SingleLine()
	})
	return b
}

func (a *app) localFoldersPanel(c *ui.Context, folders []string) {
	p := a.pal
	ui.Column(c.Key("folders-panel")).Padding(14, 16).Margin(0, 0, 18, 0).Radius(14).Gap(8).
		Background(p.hover).Border(1, p.hairline).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "监控的音乐目录").FontSize(13).FontWeight(700)
			ui.Spacer(c)
			ui.Textf(c, "共 %d 个根目录", len(folders)).FontSize(12).TextColor(p.muted)
		})
		for _, dir := range folders {
			d := dir
			ui.Row(c.Key("watched:" + d)).Height(34).PaddingX(10).Radius(8).Gap(10).AlignItems(ui.Center).
				Background(p.bg.Alpha(0.55)).Border(1, p.hairline).Children(func() {
				ui.Icon(c, iconFolder).Size(15, 15).TextColor(p.muted)
				ui.Text(c, d).FontSize(12.5).Grow(1).Shrink(1).MinWidth(0).SingleLine()
				if iconButton(c.Key("reveal"), iconFolderOpen, 14, p.muted, "在文件夹中显示").Size(28, 28).Clicked() {
					showInFolder(d)
				}
				rem := ui.ButtonBase(c.Key("remove")).Height(24).PaddingX(10).Radius(6).Center().
					Background(p.hover).TextColor(p.muted).Label("移除目录：" + filepath.Base(d)).FocusRing(false)
				if rem.Hovered() {
					rem.Background(p.accent.Alpha(0.14)).TextColor(p.accent)
				}
				rem.Children(func() { ui.Text(c, "移除").FontSize(12).FontWeight(600) })
				if rem.Clicked() {
					a.removeLocalFolder(d)
				}
			})
		}
	})
}

func (a *app) localEmptyState(c *ui.Context) {
	p := a.pal
	goldFg, goldBg, goldBorder := audiophileGold(p.dark)
	ui.Column(c).Margin(28, 0, 0, 0).Padding(42, 36).Radius(20).Gap(14).AlignItems(ui.Center).
		Background(p.hover).Border(1, p.hairline).Children(func() {
		ui.Box(c).Size(64, 64).Radius(20).Center().Background(goldBg).Border(1, goldBorder).Children(func() {
			ui.Icon(c, iconDisc).Size(30, 30).TextColor(goldFg)
		})
		ui.Text(c, "构建你的本地无损与高解析度音乐馆").FontSize(20).FontWeight(700).LetterSpacing(-0.3).Margin(4, 0, 0, 0)
		ui.Text(c, "自动提取内嵌与目录高清封面、ReplayGain 响度、分盘标签与同步歌词，支持 HQPlayer 原生文件直通与 Roon 流派融合。").
			FontSize(13).TextColor(p.muted).TextAlign(ui.Center).MaxWidth(540).LineHeight(1.5)
		ui.Row(c).Padding(6, 14).Radius(999).Background(goldBg).Border(1, goldBorder).Children(func() {
			ui.Text(c, "FLAC · WAV · AIFF · DSF/DFF (DSD64–512) · CUE+整轨镜像 · ALAC/M4A · MP3 · OGG · .lrc 歌词").
				FontSize(11.5).FontWeight(600).TextColor(goldFg)
		})
		ui.Row(c).Margin(8, 0, 0, 0).Children(func() {
			if pillButton(c.Key("empty-pick"), iconFolderOpen, "选择音乐文件夹…", p.accent, p.onAccent).Clicked() {
				a.addLocalFolders()
			}
		})
	})
}

func (a *app) localTopControls(c *ui.Context, stats localmusic.Stats, folders []string) {
	p := a.pal
	a.localToolbarHeader(c, stats, folders)
	if a.local.foldersOpen && len(folders) > 0 {
		a.localFoldersPanel(c, folders)
	}

	// Five Roon Lenses + Instant Filter input.
	ui.Row(c).Gap(12).AlignItems(ui.Center).Margin(0, 0, 14, 0).Wrap().Children(func() {
		if segmented(c.Key("local-lenses"), &a.local.tab, "专辑", "艺人", "流派", "曲目", "文件夹") {
			a.pageScroll = ui.ScrollState{}
			a.local.tracksList = ui.ListState{}
		}
		ui.Spacer(c)
		filterBox := ui.Row(c).Width(248).Height(30).PaddingX(10).Gap(6).Radius(8).AlignItems(ui.Center)
		if c.Focused(a.local.filterBox) {
			filterBox.Background(p.bg).Border(1, p.accent.Alpha(0.6)).Shadow(0, 0, 0, 2.5, p.accent.Alpha(0.2))
		} else {
			filterBox.Background(p.hover).Border(1, p.hairline)
		}
		filterBox.Children(func() {
			ui.Icon(c, iconSearch).Size(13, 13).TextColor(p.muted)
			ui.TextInputBase(c.Key("local-filter"), &a.local.filter).Bind(&a.local.filterBox).
				Grow(1).MinWidth(0).FontSize(12.5).Placeholder("筛选专辑、艺人、曲目、规格…").Label("筛选本地音乐")
			if a.local.filter != "" {
				clr := ui.ButtonBase(c.Key("clear-filter")).Size(18, 18).Radius(999).Center().Label("清除筛选").FocusRing(false)
				if clr.Hovered() {
					clr.Background(p.press)
				}
				clr.Children(func() { ui.Icon(c, iconX).Size(11, 11).TextColor(p.muted) })
				if clr.Clicked() {
					a.local.filter = ""
				}
			}
		})
	})

	// Focus Chips Row (shown on Albums & Tracks tabs, or whenever an active Genre filter is set).
	showFocus := a.local.tab == localTabAlbums || a.local.tab == localTabTracks
	if showFocus || a.local.genre != "" {
		goldFg, goldBg, goldBorder := audiophileGold(p.dark)
		ui.Row(c.Key("focus-row")).Gap(8).AlignItems(ui.Center).Margin(0, 0, 18, 0).Wrap().Children(func() {
			if showFocus {
				for _, opt := range localFocusOptions {
					o := opt
					active := a.local.focus == o.key
					isGoldOpt := o.key == "hires" || o.key == "dsd"
					chip := ui.ButtonBase(c.Key("focus:" + o.key)).Height(27).PaddingX(12).Radius(999).Center().Label(o.label).FocusRing(false)
					switch {
					case active && isGoldOpt:
						chip.Background(goldBg).Border(1, goldBorder).TextColor(goldFg)
					case active:
						chip.Background(p.accent.Alpha(0.15)).Border(1, p.accent.Alpha(0.45)).TextColor(p.accent)
					case chip.Hovered():
						chip.Background(p.press).Border(1, p.hairline).TextColor(p.text)
					default:
						chip.Background(p.hover).Border(1, p.hairline).TextColor(p.muted)
					}
					chip.Children(func() {
						ui.Text(c, o.label).FontSize(12).FontWeight(600).SingleLine()
					})
					if chip.Clicked() {
						a.local.focus = o.key
						a.pageScroll = ui.ScrollState{}
						a.local.tracksList = ui.ListState{}
					}
				}
			}

			if a.local.genre != "" {
				gLabel := "流派: " + a.local.genre + " ×"
				gChip := ui.ButtonBase(c.Key("active-genre")).Height(27).PaddingX(12).Gap(5).Radius(999).AlignItems(ui.Center).
					Background(p.accent.Alpha(0.14)).Border(1, p.accent.Alpha(0.45)).TextColor(p.accent).
					Label("清除流派筛选").FocusRing(false)
				if gChip.Hovered() {
					gChip.Background(p.accent.Alpha(0.22))
				}
				gChip.Children(func() {
					ui.Text(c, gLabel).FontSize(12).FontWeight(600).SingleLine()
				})
				if gChip.Clicked() {
					a.local.genre = ""
				}
			}

			if a.local.tab == localTabAlbums {
				ui.Spacer(c)
				sortBtn := ui.ButtonBase(c.Key("album-sort")).Height(27).PaddingX(11).Gap(5).Radius(8).AlignItems(ui.Center).
					Background(p.hover).Border(1, p.hairline).TextColor(p.muted).Label("专辑排序").FocusRing(false)
				if sortBtn.Hovered() {
					sortBtn.Background(p.press).TextColor(p.text)
				}
				sortBtn.Children(func() {
					ui.Text(c, "排序："+localSortLabel(a.local.sort)).FontSize(12).FontWeight(500).SingleLine()
					ui.Icon(c, iconUpDown).Size(12, 12)
				})
				sortBtn.Menu(func(m *ui.Menu) {
					for _, opt := range localSortOptions {
						o := opt
						label := o.label
						if a.local.sort == o.key {
							label = "✓ " + label
						}
						if m.Item(label).Chosen() {
							a.local.sort = o.key
							a.pageScroll = ui.ScrollState{}
						}
					}
				})
			}
		})
	}
}

// 1. 专辑墙 (Albums Lens)
func (a *app) localAlbumsLens(c *ui.Context) {
	if a.local.lib == nil {
		return
	}
	albums := a.local.lib.Albums(localmusic.AlbumFilter{
		Query: a.local.filter,
		Genre: a.local.genre,
		Focus: a.local.focus,
		Sort:  a.local.sort,
	})
	if len(albums) == 0 {
		a.empty(c, "没有符合当前筛选条件的本地专辑")
		return
	}
	ui.Grid(c).Columns(a.columns(c, 162)).GapX(20).GapY(28).Children(func() {
		for i := range albums {
			a.localAlbumCard(c.Key(albums[i].ID), &albums[i])
		}
	})
}

func (a *app) localAlbumCard(c *ui.Context, al *localmusic.LocalAlbum) {
	p := a.pal
	card := ui.ButtonBase(c).Column().Gap(10).AlignItems(ui.Stretch).Justify(ui.Start).FocusRing(false).Label(al.Title)
	hovered := card.Hovered()
	tracks := al.Tracks
	card.Children(func() {
		art := ui.Box(c).AspectRatio(1).Radius(10)
		lift := ui.Pose{}
		if hovered {
			lift = ui.Pose{Y: -3}
			art.Shadow(0, 12, 28, -6, p.cardShade)
		} else {
			art.Shadow(0, 2, 8, -2, p.cardShade)
		}
		art.AnimatePose(lift, ui.Snappy)
		art.Children(func() {
			coverURL := localmusic.CoverURL(al.CoverID)
			if coverURL == "" {
				a.localDiscPlaceholder(c, 10)
			} else {
				a.coverFill(c, coverURL, 400, 10)
			}
			if badge := al.Spec.Badge(); badge != "" {
				fg := ui.RGB(245, 245, 247)
				border := ui.RGBA(255, 255, 255, 0.16)
				if al.Spec.IsHiRes || al.Spec.IsDSD {
					fg = ui.Hex("#f5d58a")
					border = ui.RGBA(245, 213, 138, 0.42)
				}
				ui.Row(c).Absolute().Top(8).Right(8).Padding(2.5, 8).Radius(999).AlignItems(ui.Center).
					Background(ui.RGBA(14, 14, 18, 0.74)).Border(0.5, border).Children(func() {
					ui.Text(c, badge).FontSize(10.5).FontWeight(700).FontFeatures("tnum").TextColor(fg).SingleLine()
				})
			}
			a.coverPlayButton(c, hovered, func() {
				a.playLocalTracks(tracks, 0)
			})
		})
		ui.Text(c, al.Title).FontSize(13.5).FontWeight(600).MaxLines(2).LineHeight(1.35)
		var meta []string
		if al.AlbumArtist != "" {
			meta = append(meta, al.AlbumArtist)
		}
		if al.Year > 0 {
			meta = append(meta, fmt.Sprintf("%d", al.Year))
		}
		if len(meta) > 0 {
			ui.Text(c, strings.Join(meta, " · ")).FontSize(12).TextColor(p.muted).SingleLine().Margin(-5, 0, 0, 0)
		}
	})
	if card.Clicked() {
		a.open(route{page: pageAlbum, id: al.ID})
	}
}

func (a *app) localDiscPlaceholder(c *ui.Context, radius float32) {
	p := a.pal
	from, to := ui.Hex("#28282e"), ui.Hex("#1a1a1f")
	if !p.dark {
		from, to = ui.Hex("#ececf1"), ui.Hex("#dcdce4")
	}
	ui.Box(c).Absolute().Top(0).Left(0).Right(0).Bottom(0).Radius(radius).Gradient(from, to, 145).Center().Children(func() {
		ui.Icon(c, iconDisc).Size(44, 44).TextColor(p.faint)
	})
}

// 2. 艺人 (Artists Lens)
func (a *app) localArtistsLens(c *ui.Context) {
	if a.local.lib == nil {
		return
	}
	artists := a.local.lib.Artists(a.local.filter)
	if len(artists) == 0 {
		a.empty(c, "没有匹配的本地艺人")
		return
	}
	p := a.pal
	ui.Grid(c).Columns(a.columns(c, 148)).GapX(20).GapY(28).Children(func() {
		for i := range artists {
			ar := &artists[i]
			card := ui.ButtonBase(c.Key(ar.ID)).Column().Gap(9).AlignItems(ui.Stretch).Justify(ui.Start).FocusRing(false).Label(ar.Name)
			hovered := card.Hovered()
			card.Children(func() {
				art := ui.Box(c).AspectRatio(1).Radius(999)
				lift := ui.Pose{}
				if hovered {
					lift = ui.Pose{Y: -3}
					art.Shadow(0, 12, 28, -6, p.cardShade)
				} else {
					art.Shadow(0, 2, 8, -2, p.cardShade)
				}
				art.AnimatePose(lift, ui.Snappy)
				art.Children(func() {
					if ar.CoverID == "" {
						a.localDiscPlaceholder(c, 999)
					} else {
						a.coverFill(c, localmusic.CoverURL(ar.CoverID), 400, 999)
					}
				})
				ui.Text(c, ar.Name).FontSize(13.5).FontWeight(600).SingleLine().AlignSelf(ui.Center)
				ui.Textf(c, "%d 张专辑 · %d 首", ar.AlbumCount, ar.TrackCount).
					FontSize(12).TextColor(p.muted).SingleLine().AlignSelf(ui.Center).Margin(-5, 0, 0, 0)
			})
			if card.Clicked() {
				a.open(route{page: pageArtist, id: ar.ID})
			}
		}
	})
}

// 3. 流派 (Genres Lens)
func (a *app) localGenresLens(c *ui.Context) {
	if a.local.lib == nil {
		return
	}
	allGenres := a.local.lib.Genres()
	q := strings.ToLower(strings.TrimSpace(a.local.filter))
	var genres []localmusic.GenreSummary
	for _, g := range allGenres {
		if q == "" || strings.Contains(strings.ToLower(g.Name), q) {
			genres = append(genres, g)
		}
	}
	if len(genres) == 0 {
		msg := "暂无流派信息（可点击右上角“从 Roon 同步流派”自动补全）"
		if q != "" {
			msg = "没有匹配的流派"
		}
		a.empty(c, msg)
		return
	}
	p := a.pal
	goldFg, goldBg, goldBorder := audiophileGold(p.dark)
	ui.Grid(c).Columns(a.columns(c, 224)).Gap(16).Children(func() {
		for i := range genres {
			g := genres[i]
			card := ui.ButtonBase(c.Key("genre:" + g.Name)).Padding(12).Radius(14).Gap(14).
				AlignItems(ui.Center).Justify(ui.Start).FocusRing(false).Label(g.Name)
			bg := p.hover
			if card.Hovered() {
				bg = p.press
			}
			card.Background(card.AnimateColor("bg", bg, ui.Animation{Duration: 120 * time.Millisecond})).Border(1, p.hairline)
			card.Children(func() {
				// 2x2 mini mosaic when >= 4 covers, primary cover when 1..3, or disc tile.
				switch {
				case len(g.Covers) >= 4:
					ui.Grid(c).Columns(2).Gap(2).Size(76, 76).Shrink(0).Children(func() {
						for k := range 4 {
							a.cover(c.Key(k), g.Covers[k], 160, 37, 5)
						}
					})
				case len(g.Covers) > 0:
					a.cover(c, g.Covers[0], 200, 76, 10).Shrink(0)
				default:
					ui.Box(c).Size(76, 76).Radius(10).Shrink(0).Center().Background(goldBg).Border(1, goldBorder).Children(func() {
						ui.Icon(c, iconTag).Size(26, 26).TextColor(goldFg)
					})
				}
				ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Gap(5).Children(func() {
					ui.Text(c, g.Name).FontSize(15.5).FontWeight(700).SingleLine()
					ui.Textf(c, "%d 张专辑 · %d 首曲目", g.AlbumCount, g.TrackCount).FontSize(12).TextColor(p.muted).SingleLine()
				})
			})
			if card.Clicked() {
				a.local.genre = g.Name
				a.local.tab = localTabAlbums
				a.pageScroll = ui.ScrollState{}
			}
		}
	})
}

// 4. 曲目 (Virtualized Tracks Lens)
func (a *app) localTracksLens(c *ui.Context, stats localmusic.Stats, folders []string) {
	sig := a.local.filter + "\x00" + a.local.genre + "\x00" + a.local.focus
	if a.local.tracksSig != sig {
		a.local.tracksSig = sig
		a.local.tracksList = ui.ListState{}
	}
	var rev uint64
	if a.local.lib != nil {
		rev = a.local.lib.Revision()
	}
	if !a.local.tracksCacheReady || a.local.tracksCacheSig != sig || a.local.tracksCacheRev != rev {
		var fresh []localmusic.LocalTrack
		if a.local.lib != nil {
			fresh = a.local.lib.Tracks(a.local.filter, a.local.genre, a.local.focus)
		}
		a.local.cachedLocalTracks = fresh
		a.local.cachedYTMTracks = toYTMTracks(fresh)
		a.local.tracksCacheSig = sig
		a.local.tracksCacheRev = rev
		a.local.tracksCacheReady = true
	}
	localTracks := a.local.cachedLocalTracks
	tracks := a.local.cachedYTMTracks

	w, _ := c.Size()
	wide := w-sidebarWidth > 820
	top, bottom := a.insets(c)
	a.underTop, a.underBottom = true, true

	extraFoot := 0
	if len(tracks) == 0 {
		extraFoot = 1
	}
	rows := 1 + len(tracks) + extraFoot
	ui.List(c.Key("local-tracks-list"), &a.local.tracksList, rows, func(rowIdx int) {
		if rowIdx == 0 {
			ui.Column(c).PaddingX(12).Children(func() {
				a.startMark(c)
				a.localTopControls(c, stats, folders)
			})
			if rows == 1 {
				a.endMark(c)
			}
			return
		}
		if rowIdx == rows-1 {
			defer a.endMark(c)
		}
		if len(tracks) == 0 {
			a.empty(c, "没有符合当前筛选条件的本地曲目")
			return
		}
		i := rowIdx - 1
		a.localTrackRow(c, i, &tracks[i], &localTracks[i], tracks, wide)
	}).Grow(1).Padding(top+pageGap, pagePad-12, bottom+32, pagePad-12).ScrollbarInsets(top, 0, bottom, 0)
}

func (a *app) localTrackRow(c *ui.Context, idx int, tr *ytm.Track, lt *localmusic.LocalTrack, queue []ytm.Track, wide bool) {
	p := a.pal
	cur := a.pl.Current()
	playing := cur != nil && cur.ID == tr.ID
	row := ui.ButtonBase(c.Key(tr.ID)).Height(54).PaddingX(12).Gap(14).Radius(10).AlignItems(ui.Center).FocusRing(false).
		Label(tr.Name).ContextMenu(a.songItems(tr, ""))
	hovered := row.Hovered()
	bg := ui.Transparent
	switch {
	case playing:
		bg = p.accent.Alpha(0.09)
	case hovered:
		bg = p.hover
	}
	row.Background(row.AnimateColor("bg", bg, ui.Animation{Duration: 100 * time.Millisecond}))
	goldFg, goldBg, goldBorder := audiophileGold(p.dark)

	row.Children(func() {
		ui.Box(c).Width(26).Center().Children(func() {
			switch {
			case playing:
				a.nowPlayingBars(c)
			case hovered:
				ui.Icon(c, iconPlay).Size(13, 13).TextColor(p.text)
			default:
				ui.Textf(c, "%d", idx+1).FontSize(13).FontFeatures("tnum").TextColor(p.faint)
			}
		})
		a.cover(c, tr.Album.PicURL, 96, 38, 6)
		ui.Column(c).Grow(3).Basis(0).MinWidth(0).Gap(3).Children(func() {
			nameCol := p.text
			if playing {
				nameCol = p.accent
			}
			ui.Text(c, tr.Name).FontSize(14).FontWeight(500).TextColor(nameCol).SingleLine()
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				a.artistLinks(c, tr, p.muted, p.text).FontSize(12.5).SingleLine().Shrink(1).MinWidth(0)
				if tr.Reason != "" {
					hiRes := lt != nil && (lt.Spec.IsHiRes || lt.Spec.IsDSD)
					tag := ui.Text(c, tr.Reason).FontSize(10.5).FontWeight(600).FontFeatures("tnum").Padding(1, 6).Radius(999).SingleLine()
					if hiRes {
						tag.TextColor(goldFg).Background(goldBg).Border(0.5, goldBorder)
					} else {
						tag.TextColor(p.muted).Background(p.hover).Border(0.5, p.hairline)
					}
				}
			})
		})
		if wide {
			album := ui.RichText(c).Grow(2).Basis(0).MinWidth(0).FontSize(12.5).TextColor(p.muted).SingleLine()
			album.Children(func() {
				if tr.Album.ID == "" {
					ui.Text(c, tr.Album.Name)
				} else {
					a.link(c.Key("album"), tr.Album.Name, p.muted, p.text, route{page: pageAlbum, id: tr.Album.ID})
				}
			})
		}
		ui.Box(c).Size(32, 32).Center().Children(func() {
			if a.isLiked(tr) || hovered {
				a.likeButton(c.Key("like"), tr, 15, p.muted)
			}
		})
		ui.Box(c).Size(28, 32).Center().Children(func() {
			if hovered {
				iconButton(c.Key("row-more"), iconMore, 15, p.muted, "曲目操作").Menu(a.songItems(tr, ""))
			}
		})
		ui.Text(c, clock(tr.Length())).FontSize(12.5).FontFeatures("tnum").TextColor(p.faint).Width(44).TextAlign(ui.End)
	})
	if row.Clicked() {
		a.pl.PlayQueue(queue, idx)
	}
}

// 5. 文件夹 (Folders Tree Lens)
func (a *app) localFoldersLens(c *ui.Context) {
	if a.local.lib == nil {
		return
	}
	lib := a.local.lib
	node := lib.BrowseFolder(a.local.folderPath)
	q := strings.ToLower(strings.TrimSpace(a.local.filter))

	subdirs := node.Subdirs
	tracks := node.Tracks
	if q != "" {
		var fs []localmusic.FolderEntry
		for _, s := range subdirs {
			if strings.Contains(strings.ToLower(s.Name), q) || strings.Contains(strings.ToLower(s.Path), q) {
				fs = append(fs, s)
			}
		}
		subdirs = fs
		var ft []localmusic.LocalTrack
		for _, t := range tracks {
			if strings.Contains(strings.ToLower(t.Title), q) || strings.Contains(strings.ToLower(t.Album), q) {
				ft = append(ft, t)
			}
		}
		tracks = ft
	}

	playableInScope := a.tracksUnderFolder(a.local.folderPath, tracks)

	p := a.pal
	// Breadcrumb bar + Play Current Folder + Reveal in Finder/Explorer.
	ui.Row(c.Key("folder-bar")).Gap(8).AlignItems(ui.Center).Margin(0, 0, 14, 0).Wrap().Children(func() {
		crumbs := buildFolderBreadcrumbs(lib.Folders(), a.local.folderPath)
		ui.Row(c).Gap(4).AlignItems(ui.Center).Grow(1).MinWidth(200).Wrap().Children(func() {
			for i, cr := range crumbs {
				crumb := cr
				if i > 0 {
					ui.Text(c, "›").FontSize(13).TextColor(p.faint).PaddingX(2)
				}
				isLast := i == len(crumbs)-1
				b := ui.ButtonBase(c.Key("crumb:" + crumb.path)).Height(26).PaddingX(8).Radius(6).AlignItems(ui.Center).
					Label(crumb.label).FocusRing(false)
				if isLast {
					b.TextColor(p.text).Background(p.hover)
				} else {
					b.TextColor(p.muted)
					if b.Hovered() {
						b.Background(p.hover).TextColor(p.text)
					}
				}
				b.Children(func() {
					weight := 500
					if isLast {
						weight = 700
					}
					ui.Text(c, crumb.label).FontSize(12.5).FontWeight(weight).SingleLine()
				})
				if b.Clicked() && !isLast {
					a.local.folderPath = crumb.path
					a.pageScroll = ui.ScrollState{}
				}
			}
		})

		if len(playableInScope) > 0 {
			if a.localActionPill(c.Key("play-folder"), iconPlay, "播放当前文件夹", false, false).Clicked() {
				a.playLocalTracks(playableInScope, 0)
			}
		}
		if a.local.folderPath != "" {
			if a.localActionPill(c.Key("reveal-folder"), iconFolderOpen, "在文件夹中显示", false, false).Clicked() {
				showInFolder(a.local.folderPath)
			}
		}
	})

	if len(subdirs) == 0 && len(tracks) == 0 {
		a.empty(c, "当前目录下没有子目录或音频曲目")
		return
	}

	if len(subdirs) > 0 {
		ui.Column(c).Gap(4).Margin(0, 0, 16, 0).Children(func() {
			for _, sd := range subdirs {
				sub := sd
				row := ui.ButtonBase(c.Key("subdir:" + sub.Path)).Height(52).PaddingX(12).Gap(12).Radius(10).
					AlignItems(ui.Center).Justify(ui.Start).FocusRing(false).Label(sub.Name)
				hovered := row.Hovered()
				if hovered {
					row.Background(p.hover)
				}
				row.Children(func() {
					if sub.CoverID != "" {
						a.cover(c, localmusic.CoverURL(sub.CoverID), 96, 36, 6)
					} else {
						ui.Box(c).Size(36, 36).Radius(8).Center().Background(p.hover).Border(1, p.hairline).Children(func() {
							ui.Icon(c, iconFolder).Size(18, 18).TextColor(p.accent)
						})
					}
					ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Gap(2).Children(func() {
						ui.Text(c, sub.Name).FontSize(14).FontWeight(600).SingleLine()
						var meta []string
						if sub.SubdirCount > 0 {
							meta = append(meta, fmt.Sprintf("%d 个子目录", sub.SubdirCount))
						}
						meta = append(meta, fmt.Sprintf("%d 首曲目", sub.TrackCount))
						ui.Text(c, strings.Join(meta, " · ")).FontSize(12).TextColor(p.muted).SingleLine()
					})
					if hovered {
						if iconButton(c.Key("open-os"), iconFolderOpen, 14, p.muted, "在文件夹中显示").Size(28, 28).Clicked() {
							showInFolder(sub.Path)
						}
					}
					ui.Icon(c, iconForward).Size(14, 14).TextColor(p.faint)
				})
				if row.Clicked() {
					a.local.folderPath = sub.Path
					a.pageScroll = ui.ScrollState{}
				}
			}
		})
	}

	if len(tracks) > 0 {
		w, _ := c.Size()
		wide := w-sidebarWidth > 820
		ytmQueue := toYTMTracks(tracks)
		ui.Column(c).Margin(0, -12).Children(func() {
			for i := range tracks {
				a.localTrackRow(c, i, &ytmQueue[i], &tracks[i], ytmQueue, wide)
			}
		})
	}
}

type folderCrumb struct {
	label string
	path  string
}

func buildFolderBreadcrumbs(roots []string, current string) []folderCrumb {
	crumbs := []folderCrumb{{label: "全部监控目录", path: ""}}
	if strings.TrimSpace(current) == "" {
		return crumbs
	}
	cleanCur := filepath.Clean(current)
	curLow := strings.ToLower(cleanCur)
	sep := string(filepath.Separator)

	var matchedRoot string
	for _, r := range roots {
		cleanRoot := filepath.Clean(r)
		rLow := strings.ToLower(cleanRoot)
		if curLow == rLow || strings.HasPrefix(curLow, strings.TrimRight(rLow, sep)+sep) {
			if len(cleanRoot) > len(matchedRoot) {
				matchedRoot = cleanRoot
			}
		}
	}
	if matchedRoot == "" {
		return append(crumbs, folderCrumb{label: filepath.Base(cleanCur), path: cleanCur})
	}
	rootLabel := filepath.Base(matchedRoot)
	if rootLabel == "" || rootLabel == "." || rootLabel == sep {
		rootLabel = matchedRoot
	}
	crumbs = append(crumbs, folderCrumb{label: rootLabel, path: matchedRoot})
	if strings.EqualFold(cleanCur, matchedRoot) {
		return crumbs
	}
	rel := strings.TrimLeft(cleanCur[len(matchedRoot):], `/\`)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	acc := matchedRoot
	for _, part := range parts {
		if part == "" {
			continue
		}
		acc = filepath.Join(acc, part)
		crumbs = append(crumbs, folderCrumb{label: part, path: acc})
	}
	return crumbs
}

func (a *app) tracksUnderFolder(dir string, direct []localmusic.LocalTrack) []localmusic.LocalTrack {
	if len(direct) > 0 {
		return direct
	}
	if a.local.lib == nil {
		return nil
	}
	all := a.local.lib.Tracks("", "", "")
	if strings.TrimSpace(dir) == "" {
		return all
	}
	clean := strings.ToLower(filepath.Clean(dir))
	sep := string(filepath.Separator)
	prefix := strings.TrimRight(clean, sep) + sep
	var out []localmusic.LocalTrack
	for _, t := range all {
		td := strings.ToLower(filepath.Clean(t.Dir))
		if td == clean || strings.HasPrefix(td, prefix) {
			out = append(out, t)
		}
	}
	return out
}
