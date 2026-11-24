package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"ytb-music/internal/localmusic"
	"ytb-music/internal/ytm"
)

// searchResults and searchSuggest search YouTube Music; tests replace them.
var (
	searchResults = ytm.SearchPage
	searchSuggest = ytm.SearchSuggest
)

// A search asks for searchPage results at a time, about what YouTube sends
// at once, for the first to show as soon as they come: the next follow on
// their own up to searchAhead, and the rest as the list is scrolled down.
const (
	searchPage  = 20
	searchAhead = 60
)

// searchKinds are the kinds of search, as their tabs show them.
var searchKinds = []struct {
	kind        ytm.SearchType
	label, unit string
}{
	{ytm.SearchSongs, "单曲", "首单曲"},
	{ytm.SearchVideos, "视频", "个视频"},
	{ytm.SearchArtists, "歌手", "位歌手"},
	{ytm.SearchAlbums, "专辑", "张专辑"},
	{ytm.SearchPlaylists, "歌单", "个歌单"},
}

// searchData is what a search found, as far as its pages have loaded.
type searchData struct {
	res          ytm.SearchResult
	localSongs   []ytm.Track
	localAlbums  []ytm.AlbumProfile
	localArtists []ytm.ArtistProfile
	page         paged
}

// search searches for q, of the kind last searched for.
func (a *app) search(q string) {
	q = strings.TrimSpace(q)
	if q == "" {
		return
	}
	kind := a.searched.kind
	if kind == 0 {
		kind = ytm.SearchSongs
	}
	a.searchFor(route{page: pageSearch, query: q, kind: kind})
}

// searchFor opens the page of a search, remembering its query. Searched
// for again, it loads anew.
func (a *app) searchFor(r route) {
	a.rememberSearch(r.query)
	a.dismissSuggestions()
	if r == a.route {
		a.nowPlaying = false
		a.loadSearch(r, true)
		return
	}
	a.open(r)
}

// loadSearch loads the first page of a search, unless it has, or again.
func (a *app) loadSearch(r route, again bool) {
	if a.searches[r] != nil && !again {
		if a.local.lib != nil {
			d := a.searches[r]
			d.localSongs, d.localAlbums, d.localArtists = a.local.lib.Search(r.query, 24)
		}
		return
	}
	d := &searchData{}
	if a.local.lib != nil {
		d.localSongs, d.localAlbums, d.localArtists = a.local.lib.Search(r.query, 24)
	}
	a.searches[r] = d
	a.moreResults(r, d)
}

// moreResults loads the next page of a search. A search done again has
// new data, so the last one's pages land where nothing shows them.
func (a *app) moreResults(r route, d *searchData) {
	if d.page.loading {
		return
	}
	d.page.loading, d.page.err = true, ""
	offset := d.res.Len()
	go func() {
		res, err := searchResults(r.query, r.kind, searchPage, offset)
		a.post(func() {
			d.page.loading = false
			if err != nil {
				d.page.err = "搜索失败，请稍后再试"
				return
			}
			d.res.Append(res)
			// A server can claim more while sending none.
			d.page.loaded, d.page.more = true, res.Len() > 0 && d.res.Len() < d.res.Total
			if d.page.more && d.res.Len() < searchAhead && a.searches[r] == d {
				a.moreResults(r, d)
			}
		})
	}()
}

// searchHistoryLimit is how many searches the history keeps.
const searchHistoryLimit = 20

// rememberSearch puts q first in the history of searches, once.
func (a *app) rememberSearch(q string) {
	history := []string{q}
	for _, s := range a.settings.SearchHistory {
		if s != q && len(history) < searchHistoryLimit {
			history = append(history, s)
		}
	}
	a.settings.SearchHistory = history
	a.settings.save()
}

func (a *app) clearSearchHistory() {
	a.settings.SearchHistory = nil
	a.settings.save()
}

func (a *app) searchPage(c *ui.Context) {
	r := a.route
	d := a.searches[r]
	if d == nil {
		a.loadSearch(r, false)
		d = a.searches[r]
	}
	header := func(c *ui.Context) { a.searchHeader(c, d) }
	more := func() { a.moreResults(r, d) }
	if r.kind == ytm.SearchSongs || r.kind == ytm.SearchVideos {
		hasLocalSongs := r.kind == ytm.SearchSongs && len(d.localSongs) > 0
		if !d.page.loaded && d.page.loading && !hasLocalSongs {
			a.trackSkeleton(c, a.pageShimmer(c), true, header)
			return
		}
		tracks := d.res.Songs
		if r.kind == ytm.SearchVideos {
			tracks = d.res.Videos
		} else if hasLocalSongs {
			tracks = append(slices.Clone(d.localSongs), tracks...)
		}
		a.trackRows(c, tracks, true, header, func(c *ui.Context) {
			if !hasLocalSongs || d.page.err == "" {
				a.pageFoot(c, &d.page, more)
			}
			a.searchEmpty(c, d)
		})
		return
	}
	a.scrollPage(c, func() {
		header(c)
		res := &d.res
		hasLocal := false
		switch r.kind {
		case ytm.SearchArtists:
			if len(d.localArtists) > 0 {
				hasLocal = true
				ui.Text(c, "本地曲库匹配").FontSize(15).FontWeight(700).Margin(0, 0, 12, 0)
				ui.Grid(c.Key("local-artists")).Columns(a.columns(c, 132)).GapX(20).GapY(28).Children(func() {
					for i := range d.localArtists {
						a.artistCard(c.Key(d.localArtists[i].ID), &d.localArtists[i])
					}
				})
				if len(res.Artists) > 0 {
					a.section(c, "在线结果")
				}
			}
			if len(res.Artists) > 0 {
				ui.Grid(c.Key("ytm-artists")).Columns(a.columns(c, 132)).GapX(20).GapY(28).Children(func() {
					for i := range res.Artists {
						a.artistCard(c.Key(res.Artists[i].ID), &res.Artists[i])
					}
				})
			}
		case ytm.SearchAlbums:
			if len(d.localAlbums) > 0 {
				hasLocal = true
				ui.Text(c, "本地曲库匹配").FontSize(15).FontWeight(700).Margin(0, 0, 12, 0)
				a.albumGrid(c.Key("local-albums"), d.localAlbums)
				if len(res.Albums) > 0 {
					a.section(c, "在线结果")
				}
			}
			if len(res.Albums) > 0 {
				a.albumGrid(c.Key("ytm-albums"), res.Albums)
			}
		case ytm.SearchPlaylists:
			ui.Grid(c).Columns(a.columns(c, 156)).GapX(20).GapY(28).Children(func() {
				for i := range res.Playlists {
					a.foundPlaylistCard(c.Key(res.Playlists[i].ID), &res.Playlists[i])
				}
			})
		case ytm.SearchUsers:
			for i := range res.Users {
				a.userRow(c.Key(res.Users[i].UserID), &res.Users[i])
			}
		case ytm.SearchPodcasts:
			for i := range res.Podcasts {
				a.podcastRow(c.Key(res.Podcasts[i].ID), &res.Podcasts[i])
			}
		}
		if !d.page.loaded && d.page.loading && !hasLocal {
			a.playlistCardsSkeleton(c, a.pageShimmer(c), a.columns(c, 156), 156, false)
			return
		}
		if !hasLocal || d.page.err == "" {
			a.pageFoot(c, &d.page, more)
		}
		a.searchEmpty(c, d)
	})
}

func (a *app) searchLocalCount(kind ytm.SearchType, d *searchData) int {
	if d == nil {
		return 0
	}
	switch kind {
	case ytm.SearchSongs:
		return len(d.localSongs)
	case ytm.SearchAlbums:
		return len(d.localAlbums)
	case ytm.SearchArtists:
		return len(d.localArtists)
	default:
		return 0
	}
}

func (a *app) searchEmpty(c *ui.Context, d *searchData) {
	if a.searchLocalCount(a.route.kind, d) > 0 {
		return
	}
	if d.page.loaded && d.res.Len() == 0 {
		a.empty(c, "没有找到相关结果")
	}
}

// searchHeader is what was searched for, how many it found, and the tabs
// of what it may look for instead.
func (a *app) searchHeader(c *ui.Context, d *searchData) {
	r := a.route
	sub := "搜索"
	at := 0
	localCnt := a.searchLocalCount(r.kind, d)
	for i, k := range searchKinds {
		if k.kind == r.kind {
			at = i
			switch {
			case d.page.loaded && localCnt > 0:
				sub = fmt.Sprintf("搜索 · 本地 %d · 在线 %d %s", localCnt, d.res.Len(), k.unit)
			case localCnt > 0:
				sub = fmt.Sprintf("搜索 · 本地曲库 %d %s", localCnt, k.unit)
			case d.page.loaded:
				sub = fmt.Sprintf("搜索 · %d %s", d.res.Len(), k.unit)
			}
		}
	}
	a.heading(c, "“"+r.query+"”", sub)
	ui.Row(c).Margin(0, 0, 18, 0).Children(func() {
		labels := make([]string, len(searchKinds))
		for i, k := range searchKinds {
			labels[i] = k.label
		}
		if segmented(c.Key("kind"), &at, labels...) {
			r.kind = searchKinds[at].kind
			a.show(r)
		}
	})
	if r.kind == ytm.SearchSongs && (len(d.localSongs) > 0 || len(d.localAlbums) > 0 || len(d.localArtists) > 0) {
		p := a.pal
		ui.Row(c.Key("local-search-banner")).Gap(8).AlignItems(ui.Center).Margin(0, 0, 12, 0).PaddingX(12).Wrap().Children(func() {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, iconFolder).Size(13.5, 13.5).TextColor(p.accent)
				ui.Text(c, "本地曲库匹配").FontSize(12.5).FontWeight(700).TextColor(p.text)
			})
			for i := range min(len(d.localAlbums), 3) {
				al := d.localAlbums[i]
				chip := ui.ButtonBase(c.Key("s-al:" + al.ID)).Height(24).PaddingX(10).Gap(5).Radius(999).AlignItems(ui.Center).
					Background(p.hover).Border(1, p.hairline).TextColor(p.muted).Label("本地专辑：" + al.Name).FocusRing(false)
				if chip.Hovered() {
					chip.Background(p.press).TextColor(p.text)
				}
				chip.Children(func() {
					ui.Icon(c, iconDisc).Size(12, 12)
					ui.Text(c, al.Name).FontSize(11.5).FontWeight(600).SingleLine()
				})
				if chip.Clicked() {
					a.open(route{page: pageAlbum, id: al.ID})
				}
			}
			for i := range min(len(d.localArtists), 2) {
				ar := d.localArtists[i]
				chip := ui.ButtonBase(c.Key("s-ar:" + ar.ID)).Height(24).PaddingX(10).Gap(5).Radius(999).AlignItems(ui.Center).
					Background(p.hover).Border(1, p.hairline).TextColor(p.muted).Label("本地艺人：" + ar.Name).FocusRing(false)
				if chip.Hovered() {
					chip.Background(p.press).TextColor(p.text)
				}
				chip.Children(func() {
					ui.Icon(c, iconUser).Size(12, 12)
					ui.Text(c, ar.Name).FontSize(11.5).FontWeight(600).SingleLine()
				})
				if chip.Clicked() {
					a.open(route{page: pageArtist, id: ar.ID})
				}
			}
		})
	}
}

// artistCard is an artist's round photo over their name, opening their page.
func (a *app) artistCard(c *ui.Context, ar *ytm.ArtistProfile) {
	p := a.pal
	card := ui.ButtonBase(c).Column().Gap(10).AlignItems(ui.Stretch).FocusRing(false).Label(ar.Name)
	hovered := card.Hovered()
	card.Children(func() {
		photo := ar.Avatar
		if photo == "" {
			photo = ar.PicURL
		}
		art := ui.Box(c).AspectRatio(1).Radius(999)
		if hovered {
			art.Shadow(0, 12, 28, -6, p.cardShade)
		} else {
			art.Shadow(0, 2, 8, -2, p.cardShade)
		}
		art.Children(func() {
			if photo == "" && localmusic.IsLocalArtist(ar.ID) {
				a.localDiscPlaceholder(c, 999)
			} else {
				a.coverFill(c, photo, 400, 999)
			}
		})
		ui.Text(c, ar.Name).FontSize(13).FontWeight(500).SingleLine().AlignSelf(ui.Center)
	})
	if card.Clicked() {
		a.open(route{page: pageArtist, id: ar.ID})
	}
}

// foundPlaylistCard is a playlist a search found: its card, with how many
// songs it has and who made it under its name, as strangers' playlists
// are told apart by their makers.
func (a *app) foundPlaylistCard(c *ui.Context, pl *ytm.Playlist) {
	sub := pl.Subtitle
	if pl.TrackCount > 0 {
		sub = fmt.Sprintf("%d 首", pl.TrackCount)
	}
	if pl.Creator.Nickname != "" && pl.TrackCount > 0 {
		sub += " · " + pl.Creator.Nickname
	}
	if a.coverCard(c, pl.Name, pl.Cover(), pl.PlayCount, sub, func() { a.loadPlaylist(pl.ID, true) }) {
		a.open(route{page: pagePlaylist, id: pl.ID})
	}
}
