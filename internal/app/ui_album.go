package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"ytb-music/internal/localmusic"
	"ytb-music/internal/ytm"
)

// albumDetail loads an album from NetEase; tests replace it.
var albumDetail = ytm.AlbumDetail

// albumData is what the app has of an album.
type albumData struct {
	album   *ytm.AlbumProfile
	loading bool
	err     string
}

func (a *app) loadAlbum(id string) {
	d := a.albums[id]
	if d == nil {
		d = &albumData{}
		a.albums[id] = d
	}
	if localmusic.IsLocalAlbum(id) {
		if a.local.lib != nil {
			if al, ok := a.local.lib.AlbumByID(id); ok {
				prof := al.ToAlbumProfile()
				d.album = &prof
				d.loading, d.err = false, ""
				return
			}
		}
		d.loading, d.err = false, "本地专辑不存在"
		return
	}
	if d.album != nil || d.loading {
		return
	}
	d.loading, d.err = true, ""
	go func() {
		al, err := albumDetail(id)
		a.post(func() {
			d.loading = false
			if err != nil {
				d.err = "专辑加载失败"
				return
			}
			d.album = al
		})
	}()
}

func (a *app) albumPage(c *ui.Context) {
	id := a.route.id
	d := a.albums[id]
	if d == nil {
		a.loadAlbum(id)
		d = a.albums[id]
	}
	if d.album == nil {
		if !d.loading {
			a.centered(c, d.err, func() { a.loadAlbum(id) })
			return
		}
		s := a.pageShimmer(c)
		a.trackSkeleton(c, s, false, func(c *ui.Context) { a.playlistHeaderSkeleton(c, s) })
		return
	}
	a.trackRows(c, d.album.Songs, false, func(c *ui.Context) { a.albumHeader(c, d.album) })
}

func (a *app) albumHeader(c *ui.Context, al *ytm.AlbumProfile) {
	p := a.pal
	isLocal := localmusic.IsLocalAlbum(al.ID)
	var localAl *localmusic.LocalAlbum
	if isLocal && a.local.lib != nil {
		if got, ok := a.local.lib.AlbumByID(al.ID); ok {
			localAl = got
		}
	}
	ui.Row(c).Gap(28).AlignItems(ui.End).Padding(0, 0, 26, 0).Children(func() {
		ui.Box(c).Size(212, 212).Radius(14).Shadow(0, 18, 40, -10, p.cardShade).Children(func() {
			if isLocal && al.PicURL == "" {
				a.localDiscPlaceholder(c, 14)
			} else {
				a.coverFill(c, al.PicURL, 600, 14)
			}
		})
		ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Gap(8).Children(func() {
			eyebrow := "专辑"
			if isLocal {
				eyebrow = "本地专辑"
			}
			ui.Text(c, eyebrow).FontSize(12).FontWeight(600).TextColor(p.accent)
			ui.Text(c, al.Name).FontSize(30).FontWeight(800).LetterSpacing(-0.6).MaxLines(2).LineHeight(1.2)
			ui.RichText(c).FontSize(13).TextColor(p.muted).Children(func() {
				for i, ar := range al.Artists {
					if i > 0 {
						ui.Text(c, ", ")
					}
					a.link(c.Key(i), ar.Name, p.muted, p.text, route{page: pageArtist, id: ar.ID})
				}
				meta := fmt.Sprintf(" · %d 首", len(al.Songs))
				if localAl != nil && localAl.DiscCount > 1 {
					meta += fmt.Sprintf(" · %d 碟", localAl.DiscCount)
				}
				switch {
				case localAl != nil && localAl.Year > 0:
					meta = fmt.Sprintf(" · %d", localAl.Year) + meta
				case al.PublishTime > 0:
					meta = " · " + time.UnixMilli(al.PublishTime).Format("2006-01-02") + meta
				}
				ui.Text(c, meta)
			})
			if isLocal && localAl != nil {
				if detail := localAl.Spec.Detail(); detail != "" {
					goldFg, goldBg, goldBorder := audiophileGold(p.dark)
					fg, bg, border := p.text, p.hover, p.hairline
					if localAl.Spec.IsHiRes || localAl.Spec.IsDSD {
						fg, bg, border = goldFg, goldBg, goldBorder
					}
					ui.Row(c).AlignSelf(ui.Start).Padding(4, 11).Gap(6).Radius(999).AlignItems(ui.Center).
						Background(bg).Border(1, border).Margin(2, 0, 0, 0).Children(func() {
						ui.Icon(c, iconDisc).Size(13, 13).TextColor(fg)
						ui.Text(c, detail).FontSize(12).FontWeight(600).FontFeatures("tnum").TextColor(fg).SingleLine()
					})
				}
				if len(localAl.Genres) > 0 {
					ui.Row(c).Gap(6).Wrap().Margin(2, 0, 0, 0).Children(func() {
						for _, g := range localAl.Genres {
							genre := g
							chip := ui.ButtonBase(c.Key("genre:" + genre)).Height(24).PaddingX(10).Radius(999).Center().
								Background(p.hover).Border(1, p.hairline).TextColor(p.muted).Label(genre).FocusRing(false)
							if chip.Hovered() {
								chip.Background(p.press).TextColor(p.text)
							}
							chip.Children(func() {
								ui.Text(c, genre).FontSize(11.5).FontWeight(600).SingleLine()
							})
							if chip.Clicked() {
								a.local.genre = genre
								a.local.tab = localTabAlbums
								a.open(route{page: pageLocal})
							}
						}
					})
				}
			} else if d := strings.TrimSpace(al.Description); d != "" {
				ui.Text(c, strings.ReplaceAll(d, "\n", " ")).FontSize(12.5).TextColor(p.muted).MaxLines(2).LineHeight(1.45).Margin(2, 0, 0, 0)
			}
			ui.Row(c).Gap(10).Margin(10, 0, 0, 0).Wrap().Children(func() {
				if pillButton(c.Key("play"), iconPlay, "播放", p.accent, p.onAccent).Clicked() {
					if a.pl.Shuffle() {
						a.pl.ToggleShuffle()
					}
					a.pl.PlayQueue(al.Songs, firstPlayable(al.Songs))
				}
				if isLocal {
					if pillButton(c.Key("shuffle"), iconShuffle, "随机播放", p.hover.Over(p.bg), p.text).Clicked() {
						if !a.pl.Shuffle() {
							a.pl.ToggleShuffle()
						}
						a.pl.PlayQueue(al.Songs, firstPlayable(al.Songs))
					}
					if localAl != nil && localAl.Dir != "" {
						dir := localAl.Dir
						if pillButton(c.Key("reveal"), iconFolderOpen, "在文件夹中显示", p.hover.Over(p.bg), p.text).Clicked() {
							showInFolder(dir)
						}
					}
				}
			})
		})
	})
}

// albumGrid shows albums as cards, each opening its page.
func (a *app) albumGrid(c *ui.Context, albums []ytm.AlbumProfile) {
	ui.Grid(c).Columns(a.columns(c, 156)).GapX(20).GapY(28).Children(func() {
		for i := range albums {
			a.albumCard(c.Key(albums[i].ID), &albums[i])
		}
	})
}

// albumCard is an album's cover, lifting under the pointer, over its name
// and the year it came out.
func (a *app) albumCard(c *ui.Context, al *ytm.AlbumProfile) {
	p := a.pal
	card := ui.ButtonBase(c).Column().Gap(10).AlignItems(ui.Stretch).FocusRing(false).Label(al.Name)
	hovered := card.Hovered()
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
			isLocal := localmusic.IsLocalAlbum(al.ID)
			if isLocal && al.PicURL == "" {
				a.localDiscPlaceholder(c, 10)
			} else {
				a.coverFill(c, al.PicURL, 400, 10)
			}
			if isLocal && al.Kind != "" {
				ui.Row(c).Absolute().Top(8).Right(8).Padding(2.5, 8).Radius(999).AlignItems(ui.Center).
					Background(ui.RGBA(14, 14, 18, 0.74)).Border(0.5, ui.RGBA(245, 213, 138, 0.35)).Children(func() {
					ui.Text(c, al.Kind).FontSize(10.5).FontWeight(700).FontFeatures("tnum").TextColor(ui.Hex("#f5d58a")).SingleLine()
				})
			}
		})
		ui.Text(c, al.Name).FontSize(13).FontWeight(500).MaxLines(2).LineHeight(1.35)
		var meta []string
		if a.route.page != pageArtist {
			meta = append(meta, al.ArtistNames())
		}
		if al.PublishTime > 0 {
			meta = append(meta, time.UnixMilli(al.PublishTime).Format("2006"))
		}
		ui.Text(c, strings.Join(meta, " · ")).FontSize(12).TextColor(p.muted).SingleLine().Margin(-6, 0, 0, 0)
	})
	if card.Clicked() {
		a.open(route{page: pageAlbum, id: al.ID})
	}
}
