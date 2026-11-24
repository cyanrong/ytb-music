package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"ytb-music/internal/netease"
	"ytb-music/internal/ytm"
)

// Replaceable hooks for hermetic UI tests.
var (
	fetchNetEasePlaylist = netease.FetchPlaylist
	matchNetEaseTracks   = netease.MatchTracks
	importSearchTracks   netease.SearchFunc
	readClipboardText    = func() string { return mygo.Clipboard.ReadText() }
)

const maskedCookiePlaceholder = "••••••••••••••••"

type importState struct {
	open bool
	gen  int

	input          string
	customName     string
	target         int // 0: local playlist, 1: YouTube Music cloud playlist
	showCookie     bool
	musicUInput    string
	musicUMasked   bool
	rememberCookie bool

	busy    bool
	cancel  context.CancelFunc
	stage   string
	done    int
	total   int
	current string
	err     string

	finished        bool
	resultID        string
	resultName      string
	matchedCount    int
	totalCount      int
	unmatched       []string
	unmatchedScroll ui.ScrollState
}

// openImport opens the NetEase playlist import dialog.
func (a *app) openImport() {
	a.showMain()
	a.nowPlaying = false
	d := &a.imp
	if d.open && d.busy {
		return
	}
	d.open = true
	d.err = ""
	if !d.busy {
		d.finished = false
		if a.profile != nil {
			d.target = 1
		} else {
			d.target = 0
		}
		if a.settings.NetEaseMusicU != "" && d.musicUInput == "" {
			d.musicUInput = maskedCookiePlaceholder
			d.musicUMasked = true
			d.rememberCookie = true
		}
	}
}

func (a *app) cancelImport() {
	d := &a.imp
	d.gen++
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.busy = false
	d.stage = ""
	d.current = ""
}

func (a *app) resolveImportCookie() (string, error) {
	d := &a.imp
	raw := strings.TrimSpace(d.musicUInput)
	wasMasked := d.musicUMasked && raw == maskedCookiePlaceholder
	var clean string
	var err error
	switch {
	case raw == "":
		clean = ""
	case wasMasked:
		decrypted, oerr := openSecret(a.settings.NetEaseMusicU)
		if oerr != nil {
			return "", oerr
		}
		clean, err = netease.SanitizeMusicU(decrypted)
	default:
		clean, err = netease.SanitizeMusicU(raw)
	}
	if err != nil {
		return "", err
	}
	if d.rememberCookie && clean != "" {
		if wasMasked {
			return clean, nil
		}
		sealed, serr := sealSecret(clean)
		if serr != nil {
			return "", fmt.Errorf("无法安全加密 Cookie: %w", serr)
		}
		prev := a.settings.NetEaseMusicU
		a.settings.NetEaseMusicU = sealed
		if saveErr := a.settings.save(); saveErr != nil {
			a.settings.NetEaseMusicU = prev
			return "", fmt.Errorf("无法保存 Cookie: %w", saveErr)
		}
		d.musicUInput = maskedCookiePlaceholder
		d.musicUMasked = true
	} else if !d.rememberCookie && a.settings.NetEaseMusicU != "" {
		prev := a.settings.NetEaseMusicU
		a.settings.NetEaseMusicU = ""
		if saveErr := a.settings.save(); saveErr != nil {
			a.settings.NetEaseMusicU = prev
			return "", fmt.Errorf("无法清除已存 Cookie: %w", saveErr)
		}
	}
	return clean, nil
}

func (a *app) startImport() {
	d := &a.imp
	if d.busy {
		return
	}
	rawInput := strings.TrimSpace(d.input)
	if rawInput == "" {
		d.err = "请粘贴网易云歌单分享链接、文本或数字 ID"
		return
	}
	if _, _, err := netease.ParsePlaylistInput(rawInput); err != nil {
		d.err = "未识别到有效的网易云歌单链接或 ID"
		return
	}
	if d.target == 1 && a.profile == nil {
		d.err = "当前未登录 YouTube 账号，请先登录或切换为“本地歌单”"
		return
	}

	musicU, err := a.resolveImportCookie()
	if err != nil {
		if strings.Contains(err.Error(), "Cookie:") {
			d.err = err.Error()
		} else {
			d.err = "MUSIC_U Cookie 格式不正确，请检查是否包含非法字符"
		}
		return
	}
	customName := strings.TrimSpace(d.customName)
	cloudSync := d.target == 1
	var uid string
	if a.profile != nil {
		uid = a.profile.UserID
	}

	d.gen++
	gen := d.gen
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.busy = true
	d.finished = false
	d.err = ""
	d.stage = "正在获取网易云歌单曲目…"
	d.done, d.total = 0, 0
	d.current = ""
	d.unmatched = nil

	go func() {
		defer cancel()
		src, err := fetchNetEasePlaylist(ctx, rawInput, musicU)
		if err != nil {
			a.post(func() {
				if d.gen != gen {
					return
				}
				d.busy = false
				d.cancel = nil
				d.err = "读取网易云歌单失败：" + friendlyImportErr(err)
			})
			return
		}
		if len(src.Tracks) == 0 {
			a.post(func() {
				if d.gen != gen {
					return
				}
				d.busy = false
				d.cancel = nil
				d.err = "该网易云歌单为空，或需要填写 MUSIC_U Cookie 才能访问私人歌单"
			})
			return
		}

		total := len(src.Tracks)
		a.post(func() {
			if d.gen != gen {
				return
			}
			d.total = total
			d.done = 0
			d.stage = fmt.Sprintf("正在匹配 YouTube Music 曲目 (0 / %d)…", total)
		})

		var progMu sync.Mutex
		var lastPost time.Time
		results, err := matchNetEaseTracks(ctx, src.Tracks, 6, importSearchTracks, func(done, tot int, last netease.MatchResult) {
			now := time.Now()
			progMu.Lock()
			shouldPost := done == 1 || done == tot || now.Sub(lastPost) >= 60*time.Millisecond
			if shouldPost {
				lastPost = now
			}
			progMu.Unlock()
			if !shouldPost {
				return
			}
			title := last.Source.Name
			if ar := last.Source.DisplayArtist(); ar != "" {
				title += " - " + ar
			}
			a.post(func() {
				if d.gen != gen {
					return
				}
				d.done = done
				d.total = tot
				d.current = title
				d.stage = fmt.Sprintf("正在匹配 YouTube Music 曲目 (%d / %d)…", done, tot)
			})
		})
		if err != nil || ctx.Err() != nil {
			return
		}

		seen := make(map[string]bool, len(results))
		matched := make([]ytm.Track, 0, len(results))
		var unmatched []string
		matchedRaw := 0
		for _, r := range results {
			if !r.Matched || r.Track.ID == "" {
				line := r.Source.Name
				if ar := r.Source.DisplayArtist(); ar != "" {
					line += " - " + ar
				}
				unmatched = append(unmatched, line)
				continue
			}
			matchedRaw++
			if !seen[r.Track.ID] {
				seen[r.Track.ID] = true
				matched = append(matched, r.Track)
			}
		}

		if len(matched) == 0 {
			a.post(func() {
				if d.gen != gen {
					return
				}
				d.busy = false
				d.cancel = nil
				d.err = "未能在 YouTube Music 匹配到可播放曲目，请检查网络设置后重试"
			})
			return
		}

		finalName := customName
		if finalName == "" {
			finalName = strings.TrimSpace(src.Name)
		}
		if finalName == "" {
			finalName = "网易云导入歌单"
		}

		if !cloudSync {
			a.post(func() {
				if d.gen != gen {
					return
				}
				desc := fmt.Sprintf("导入自网易云歌单 #%s", src.ID)
				pl, perr := a.createLocalPlaylist(finalName, desc, src.CoverURL, matched)
				d.busy = false
				d.cancel = nil
				if perr != nil {
					d.err = "保存本地歌单失败：" + perr.Error()
					return
				}
				d.finished = true
				d.resultID = pl.ID
				d.resultName = pl.Name
				d.matchedCount = matchedRaw
				d.totalCount = total
				d.unmatched = unmatched
				a.toast = fmt.Sprintf("已导入本地歌单“%s”", pl.Name)
			})
			return
		}

		// Cloud sync to YouTube Music playlist in batches of 50 video IDs.
		a.post(func() {
			if d.gen != gen {
				return
			}
			d.stage = "正在创建 YouTube Music 歌单…"
			d.current = finalName
		})
		created, err := createPlaylist(finalName)
		if err != nil {
			a.post(func() {
				if d.gen != gen {
					return
				}
				d.busy = false
				d.cancel = nil
				d.err = "创建 YouTube 歌单失败，请确认登录状态或切换为本地歌单"
			})
			return
		}

		const batchSize = 50
		ids := make([]string, len(matched))
		for i := range matched {
			ids[i] = matched[i].ID
		}
		for start := 0; start < len(ids); start += batchSize {
			if ctx.Err() != nil {
				return
			}
			end := min(start+batchSize, len(ids))
			sent := end
			a.post(func() {
				if d.gen != gen {
					return
				}
				d.done = sent
				d.total = len(ids)
				d.stage = fmt.Sprintf("正在同步至 YouTube 歌单 (%d / %d)…", sent, len(ids))
			})
			if err := addToPlaylist(created.ID, ids[start:end]...); err != nil && !errors.Is(err, ytm.ErrInPlaylist) {
				a.post(func() {
					if d.gen != gen {
						return
					}
					d.busy = false
					d.cancel = nil
					d.err = fmt.Sprintf("写入第 %d-%d 首歌曲时出错，已创建部分歌单", start+1, end)
				})
				return
			}
		}

		a.post(func() {
			if d.gen != gen {
				return
			}
			created.Editable = true
			created.Name = finalName
			if created.CoverURL == "" {
				if src.CoverURL != "" {
					created.CoverURL = src.CoverURL
				} else if len(matched) > 0 {
					created.CoverURL = matched[0].Album.PicURL
				}
			}
			created.TrackCount = len(matched)
			sidebarEntry := *created
			sidebarEntry.Tracks = nil
			at := 0
			if len(a.mine) > 0 && a.mine[0].SpecialType == 5 {
				at = 1
			}
			a.mine = slices.Insert(a.mine, at, sidebarEntry)
			full := *created
			full.Tracks = matched
			a.playlists[created.ID] = &full
			if a.profile != nil && a.profile.UserID == uid {
				a.loadMine(uid)
			}
			d.busy = false
			d.cancel = nil
			d.finished = true
			d.resultID = created.ID
			d.resultName = finalName
			d.matchedCount = matchedRaw
			d.totalCount = total
			d.unmatched = unmatched
			a.toast = fmt.Sprintf("已同步歌单“%s”", finalName)
		})
	}()
}

func friendlyImportErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "401") || strings.Contains(msg, "403") || strings.Contains(msg, "20001"):
		return "该歌单需要权限，请展开下方高级选项填入网易云 MUSIC_U Cookie"
	case strings.Contains(msg, "404") || strings.Contains(msg, "not found"):
		return "找不到该歌单，请检查链接或歌单 ID 是否正确"
	default:
		return msg
	}
}

func (a *app) importDialog(c *ui.Context) {
	p := a.pal
	d := &a.imp
	wasOpen := d.open
	ui.DialogBase(c.Key("netease-import"), &d.open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.RGBA(0, 0, 0, 0.38))
		panel.Width(460).Padding(24).Radius(18).Gap(14).
			Background(p.bg).Border(1, p.hairline).Shadow(0, 24, 60, -12, ui.RGBA(0, 0, 0, 0.42)).
			Transition(ui.ElementTransition{Animation: &ui.Snappy, Enter: &ui.Motion{Scale: 0.94}, Exit: &ui.Motion{Scale: 0.96}})

		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Gap(3).Children(func() {
				ui.Text(c, "导入网易云音乐歌单").FontSize(17).FontWeight(700)
				ui.Text(c, "支持粘贴分享口令、163cn.tv 短链、网页链接或纯数字歌单 ID").FontSize(12).TextColor(p.muted)
			})
			if iconButton(c.Key("close"), iconX, 15, p.muted, "关闭").Clicked() {
				a.cancelImport()
				d.open = false
			}
		})

		if d.finished {
			a.importDoneBody(c, d, p)
			return
		}
		if d.busy {
			a.importBusyBody(c, d, p)
			return
		}

		// Source URL / ID input with one-click clipboard paste.
		ui.Column(c).Gap(6).Children(func() {
			ui.Row(c).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "歌单链接或 ID").FontSize(12.5).FontWeight(600).Grow(1)
				if ui.Button(c.Key("paste"), "从剪贴板粘贴").Clicked() {
					if clip := strings.TrimSpace(readClipboardText()); clip != "" {
						d.input = clip
						d.err = ""
					}
				}
			})
			in := ui.TextInput(c.Key("playlist-input"), &d.input).
				Placeholder("例如 https://music.163.com/playlist?id=123456 或分享口令").
				Label("歌单链接或 ID").FontSize(13).AutoFocus()
			if in.Changed() {
				d.err = ""
			}
			if in.Submitted() {
				a.startImport()
			}
		})

		// Destination: Local offline vs YouTube Music cloud.
		ui.Row(c).AlignItems(ui.Center).Gap(12).Children(func() {
			ui.Text(c, "保存位置").FontSize(12.5).FontWeight(600).Grow(1)
			if segmented(c.Key("import-target"), &d.target, "本地歌单", "同步至 YouTube") {
				d.err = ""
			}
		})
		if d.target == 0 {
			ui.Text(c, "保存在本机，无需登录 Google 账号即可播放、搜索、编辑与跨重启保留").FontSize(11.5).TextColor(p.muted).Margin(-6, 0, 0, 0)
		} else if a.profile == nil {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Margin(-4, 0, 0, 0).Children(func() {
				ui.Text(c, "尚未登录 YouTube 账号，同步云端前需先登录").FontSize(11.5).TextColor(p.accent).Grow(1)
				if ui.Button(c.Key("go-login"), "去登录").Clicked() {
					d.open = false
					a.openLogin()
				}
			})
		} else {
			ui.Text(c, "将在你的 YouTube Music 账号下创建同名歌单并自动批量添加曲目").FontSize(11.5).TextColor(p.muted).Margin(-6, 0, 0, 0)
		}

		// Optional custom playlist name.
		ui.Column(c).Gap(5).Children(func() {
			ui.Text(c, "自定义歌单名称（可选）").FontSize(12.5).FontWeight(600)
			ui.TextInput(c.Key("custom-name"), &d.customName).
				Placeholder("留空则沿用网易云原歌单名称").
				Label("自定义歌单名称").FontSize(13)
		})

		// Collapsible NetEase MUSIC_U cookie for private / >1000-track playlists.
		adv := ui.ButtonBase(c.Key("toggle-cookie")).Height(26).Gap(6).AlignItems(ui.Center).Justify(ui.Start).FocusRing(false).Label("高级选项")
		adv.Children(func() {
			arrow := iconForward
			if d.showCookie {
				arrow = iconDown
			}
			ui.Icon(c, arrow).Size(12, 12).TextColor(p.muted)
			ui.Text(c, "高级选项：私人歌单 / 超千首完整曲目 Cookie").FontSize(12).TextColor(p.muted)
		})
		if adv.Clicked() {
			d.showCookie = !d.showCookie
		}
		if d.showCookie {
			ui.Column(c.Key("cookie-panel")).Padding(10, 12).Radius(10).Background(p.hover).Gap(8).Children(func() {
				in := ui.TextInput(c.Key("music-u"), &d.musicUInput).
					Placeholder("粘贴 MUSIC_U 值或完整网易云 Cookie（可选）").
					Label("MUSIC_U Cookie").FontSize(12.5)
				if in.Changed() {
					d.musicUMasked = false
				}
				rememberLabel := "在本机记住此 Cookie（保存在本地配置）"
				if secretStorageSupported {
					rememberLabel = "在本机记住此 Cookie（系统凭据加密存储）"
				}
				ui.Row(c).AlignItems(ui.Center).Children(func() {
					ui.Text(c, rememberLabel).FontSize(12).TextColor(p.muted).Grow(1)
					ui.Switch(c.Key("remember-cookie"), &d.rememberCookie).Label("记住 Cookie")
				})
			})
		}

		if d.err != "" {
			ui.Text(c, d.err).FontSize(12).TextColor(p.accent).LineHeight(1.4)
		}

		ui.Row(c).Gap(10).Justify(ui.End).Margin(4, 0, 0, 0).Children(func() {
			if pillButton(c.Key("cancel"), nil, "取消", p.hover.Over(p.bg), p.text).Clicked() {
				d.open = false
			}
			start := pillButton(c.Key("start"), iconImport, "开始导入", p.accent, p.onAccent)
			if strings.TrimSpace(d.input) == "" {
				start.Disabled(true).Opacity(0.5)
			}
			if start.Clicked() {
				a.startImport()
			}
		})
	})
	if wasOpen && !d.open && d.busy {
		a.cancelImport()
	}
}

func (a *app) importBusyBody(c *ui.Context, d *importState, p palette) {
	ui.Column(c).Padding(12, 0, 6, 0).Gap(10).Children(func() {
		ui.Text(c, d.stage).FontSize(13.5).FontWeight(600)
		frac := float32(0.08)
		if d.total > 0 {
			frac = max(0.04, min(1, float32(d.done)/float32(d.total)))
		}
		ui.Box(c).Height(6).Radius(999).Background(p.hover).Draw(func(pt *ui.Painter, r ui.Rect) {
			pt.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W * frac, H: r.H}, p.accent, r.H/2)
		})
		if d.current != "" {
			ui.Text(c, d.current).FontSize(12).TextColor(p.muted).SingleLine()
		}
		ui.Row(c).Justify(ui.End).Margin(8, 0, 0, 0).Children(func() {
			if pillButton(c.Key("stop"), nil, "取消导入", p.hover.Over(p.bg), p.text).Clicked() {
				a.cancelImport()
			}
		})
	})
}

func (a *app) importDoneBody(c *ui.Context, d *importState, p palette) {
	ui.Column(c).Gap(10).Children(func() {
		ui.Box(c).Padding(12, 14).Radius(12).Background(p.hover).Children(func() {
			ui.Column(c).Gap(4).Children(func() {
				ui.Textf(c, "“%s”导入完成", d.resultName).FontSize(14).FontWeight(700)
				ui.Textf(c, "共成功匹配 %d / %d 首曲目", d.matchedCount, d.totalCount).FontSize(12.5).TextColor(p.muted)
			})
		})
		if len(d.unmatched) > 0 {
			ui.Textf(c, "未能匹配曲目（%d 首）：", len(d.unmatched)).FontSize(12).FontWeight(600).TextColor(p.muted)
			ui.Scroll(c.Key("unmatched")).TrackScroll(&d.unmatchedScroll).MaxHeight(140).Padding(8, 10).Radius(10).
				Background(p.hover).Gap(4).Children(func() {
				limit := min(len(d.unmatched), 80)
				for i := range limit {
					ui.Text(c, d.unmatched[i]).FontSize(12).TextColor(p.muted).SingleLine()
				}
				if len(d.unmatched) > limit {
					ui.Textf(c, "…以及另外 %d 首", len(d.unmatched)-limit).FontSize(11.5).TextColor(p.faint)
				}
			})
		}
		ui.Row(c).Gap(10).Justify(ui.End).Margin(4, 0, 0, 0).Children(func() {
			if pillButton(c.Key("again"), nil, "继续导入", p.hover.Over(p.bg), p.text).Clicked() {
				d.finished = false
				d.input = ""
				d.customName = ""
				d.err = ""
			}
			if pillButton(c.Key("open-pl"), nil, "打开歌单", p.accent, p.onAccent).Clicked() {
				id := d.resultID
				d.open = false
				d.finished = false
				a.open(route{page: pagePlaylist, id: id})
			}
		})
	})
}
