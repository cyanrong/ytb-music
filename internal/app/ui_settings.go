package app

import (
	"errors"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"ytb-music/internal/ytm"
)

// setProxy and reloadHome apply proxy routes and retry the home page; tests
// swap them out.
var (
	setProxy   = ytm.SetProxy
	reloadHome = func(a *app) { a.loadHome() }
)

func defaultAutoProxyDesc() string {
	if runtime.GOOS == "windows" {
		return "自动跟随环境变量或 Windows 系统代理"
	}
	return "自动跟随 HTTP_PROXY / HTTPS_PROXY 环境变量"
}

func formatAutoProxyDesc(activeURL string, src ytm.ProxySource) string {
	switch {
	case activeURL != "" && src == ytm.ProxySourceEnv:
		return "当前使用环境变量代理：" + activeURL
	case activeURL != "" && src == ytm.ProxySourcePAC:
		return "当前使用系统自动代理脚本：" + activeURL
	case activeURL != "":
		return "当前使用系统代理：" + activeURL
	case runtime.GOOS == "windows":
		return "自动跟随环境变量或 Windows 系统代理（当前直连）"
	default:
		return "自动跟随 HTTP_PROXY / HTTPS_PROXY 环境变量（当前直连）"
	}
}

// refreshAutoProxyDesc resolves the active automatic route off the UI thread
// so WinHTTP / PAC lookups never block rendering.
func (a *app) refreshAutoProxyDesc() {
	if a.autoProxyDesc == "" {
		a.autoProxyDesc = defaultAutoProxyDesc()
	}
	win := a.settingsWin
	if win == nil {
		win = a.win
	}
	if win == nil {
		return
	}
	a.autoProxyGen++
	gen := a.autoProxyGen
	go func() {
		activeURL, src := ytm.EffectiveProxy()
		desc := formatAutoProxyDesc(activeURL, src)
		win.Update(func() {
			if gen != a.autoProxyGen {
				return
			}
			if a.autoProxyDesc != desc {
				a.autoProxyDesc = desc
				if a.settingsWin != nil {
					a.settingsWin.Invalidate()
				}
			}
		})
	}()
}

func hasProxyPassword(raw string) bool {
	u, err := ytm.ParseProxy(raw)
	if err != nil || u == nil || u.User == nil {
		return false
	}
	_, ok := u.User.Password()
	return ok
}

func (a *app) syncProxyState() {
	a.proxyErr = ""
	if a.autoProxyDesc == "" {
		a.autoProxyDesc = defaultAutoProxyDesc()
	}
	switch strings.ToLower(strings.TrimSpace(a.settings.Proxy)) {
	case "":
		a.proxyMode = 0
	case "direct", "none":
		a.proxyMode = 2
	default:
		a.proxyMode = 1
		if norm, err := ytm.NormalizeProxy(a.settings.Proxy); err == nil {
			a.settings.Proxy = norm
		}
		a.lastCustomProxy = a.settings.Proxy
		a.proxyInput = ytm.RedactedProxy(a.settings.Proxy)
		a.proxyMasked = hasProxyPassword(a.settings.Proxy)
	}
}

// restoreRedactedPassword preserves the stored proxy password only when the
// masked "xxxxx" placeholder is submitted with the exact same scheme, host,
// port, and username; any endpoint change requires re-entering the password.
func restoreRedactedPassword(input, saved string) (string, error) {
	inU, err := ytm.ParseProxy(input)
	if err != nil || inU == nil || inU.User == nil {
		return input, nil
	}
	inPass, inHas := inU.User.Password()
	if !inHas || inPass != "xxxxx" {
		return input, nil
	}
	svU, err2 := ytm.ParseProxy(saved)
	if err2 != nil || svU == nil || svU.User == nil {
		return "", errors.New("masked password has no saved source")
	}
	svPass, svHas := svU.User.Password()
	if !svHas || inU.Scheme != svU.Scheme || !strings.EqualFold(inU.Host, svU.Host) || inU.User.Username() != svU.User.Username() {
		return "", errors.New("proxy endpoint changed while password remained masked")
	}
	cp := *inU
	cp.User = url.UserPassword(svU.User.Username(), svPass)
	return cp.String(), nil
}

func (a *app) onProxyRouteChanged() {
	a.changeVideoHeight()
	if a.art != nil {
		a.art.clearFailed()
	}
	if a.homeErr != "" || a.homeLoading {
		reloadHome(a)
	}
}

func (a *app) applyCustomProxy() {
	raw := strings.TrimSpace(a.proxyInput)
	if raw == "" || strings.EqualFold(raw, "direct") || strings.EqualFold(raw, "none") {
		a.proxyErr = "请输入有效的代理地址，例如 127.0.0.1:7890 或 socks5://127.0.0.1:1080"
		return
	}
	if a.proxyMasked {
		restored, maskErr := restoreRedactedPassword(raw, a.lastCustomProxy)
		if maskErr != nil {
			a.proxyErr = "已更改代理服务器或账号，请重新输入完整的代理密码"
			return
		}
		raw = restored
	}
	norm, err := ytm.NormalizeProxy(raw)
	if err != nil {
		a.proxyErr = "代理地址格式不正确，请填写有效的 http:// 或 socks5:// 主机与端口"
		return
	}
	prev, _ := ytm.NormalizeProxy(a.settings.Proxy)
	if err := setProxy(norm); err != nil {
		a.proxyErr = "代理地址格式不正确，请填写有效的 http:// 或 socks5:// 主机与端口"
		return
	}
	a.proxyErr = ""
	a.settings.Proxy = norm
	a.lastCustomProxy = norm
	a.proxyInput = ytm.RedactedProxy(norm)
	a.proxyMasked = hasProxyPassword(norm)
	a.changed()
	if prev != norm {
		a.onProxyRouteChanged()
	} else if a.homeErr != "" {
		reloadHome(a)
	}
}

// openSettings shows the settings window, opening it the first time.
func (a *app) openSettings() {
	if a.settingsWin != nil {
		a.settingsWin.Show()
		a.settingsWin.Focus()
		a.measureCache()
		a.refreshAutoProxyDesc()
		return
	}
	a.syncProxyState()
	a.measureCache()
	a.settingsWin = mygo.NewWindow(mygo.WindowOptions{
		Title:           "设置",
		Width:           560,
		Height:          720,
		MinWidth:        480,
		MinHeight:       420,
		StateKey:        "settings",
		DisableMaximize: true,
		DisableMinimize: true,
		// The app's menu has nothing for this window: on Linux and
		// Windows it stays out of sight until Alt shows it.
		AutoHideMenuBar: true,
		BackgroundColor: "light-dark(#f4f4f6, #1c1c1f)",
		Content:         ui.View(a.settingsView),
	})
	a.settingsWin.OnClosed(func() {
		a.settingsWin = nil
		a.syncDock()
	})
	a.refreshAutoProxyDesc()
	a.syncDock()
}

// changed applies the settings everywhere and keeps them.
func (a *app) changed() {
	a.settings.save()
	if a.win != nil {
		a.settings.applyAppearance()
	}
	if a.notch != nil {
		a.notch.setEnabled(a.settings.Notch)
	}
	if a.settingsWin != nil {
		a.settingsWin.Invalidate()
	}
	a.redraw()
}

func (a *app) settingsView(c *ui.Context) {
	t := *c.Theme()
	p := paletteFor(&t)
	t.Accent, t.AccentText = p.accent, p.onAccent
	c.SetTheme(&t)
	c.Root().Background(p.sidebar)
	c.OnShortcut(ui.Cmd, ui.KeyW, func() { a.settingsWin.Close() })
	s := &a.settings

	ui.Scroll(c).Fill().Padding(24, 28, 32, 28).Gap(8).Children(func() {
		group := func(title string, rows func()) {
			ui.Text(c, title).FontSize(12).FontWeight(600).TextColor(p.muted).Margin(14, 4, 4, 4)
			ui.Column(c).Radius(12).Background(p.bg).Border(1, p.hairline).Dividers(1, p.hairline).Children(rows)
		}
		row := func(label, desc string, control func()) {
			ui.Row(c).Padding(12, 16).Gap(16).AlignItems(ui.Center).MinHeight(48).Children(func() {
				ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Gap(3).Children(func() {
					ui.Text(c, label).FontSize(13.5).FontWeight(500)
					if desc != "" {
						ui.Text(c, desc).FontSize(12).TextColor(p.muted).LineHeight(1.4)
					}
				})
				control()
			})
		}

		// Only on macOS: elsewhere the icon is always in the notification
		// area.
		if runtime.GOOS == "darwin" {
			group("通用", func() {
				row("应用图标", "显示在菜单栏时，关闭窗口后可从菜单栏图标打开", func() {
					labels := make([]string, len(appIcons))
					chosen := appIcons[0].label
					for i, ic := range appIcons {
						labels[i] = ic.label
						if ic.id == s.AppIcon {
							chosen = ic.label
						}
					}
					if ui.Select(c.Key("app-icon"), &chosen, labels).Label("应用图标").Width(150).Changed() {
						for _, ic := range appIcons {
							if ic.label == chosen {
								s.AppIcon = ic.id
							}
						}
						a.changed()
						a.changeAppIcon()
					}
				})
			})
		}

		// Only on macOS, the one with a notch player.
		if a.notch != nil {
			group("刘海播放器", func() {
				row("显示刘海播放器", "在屏幕顶部的刘海处显示迷你播放器，鼠标悬停即可展开", func() {
					if ui.Switch(c.Key("notch"), &s.Notch).Label("显示刘海播放器").Changed() {
						a.changed()
					}
				})
				row("播放时显示实时活动", "在刘海两侧显示封面与音频波形，可能遮挡刘海旁的菜单栏图标", func() {
					if ui.Switch(c.Key("activity"), &s.NotchActivity).Label("播放时显示实时活动").Disabled(!s.Notch).Changed() {
						a.changed()
					}
				})
				row("在无刘海的屏幕上常驻显示", "关闭时，将鼠标移至屏幕顶部中央即可唤出，不会遮挡菜单栏", func() {
					if ui.Switch(c.Key("virtual"), &s.NotchVirtual).Label("在无刘海的屏幕上常驻显示").Disabled(!s.Notch).Changed() {
						a.changed()
					}
				})
			})
		}

		group("播放", func() {
			row("音质", "高音质需要 YouTube Music Premium，没有时自动使用标准音质；从下一首歌开始生效", func() {
				labels := make([]string, len(qualityLevels))
				chosen := qualityLevels[0].label
				for i, q := range qualityLevels {
					labels[i] = q.label
					if q.level == s.Quality {
						chosen = q.label
					}
				}
				if ui.Select(c.Key("quality"), &chosen, labels).Label("音质").Width(150).Changed() {
					for _, q := range qualityLevels {
						if q.label == chosen {
							s.Quality = q.level
						}
					}
					a.changed()
					a.preparePremium()
				}
			})
			row("视频画质", "自动：按实测网速选跟得上的最高画质，最高 1080p", func() {
				labels := make([]string, len(videoHeights))
				chosen := videoHeights[0].label
				for i, h := range videoHeights {
					labels[i] = h.label
					if h.height == s.VideoHeight {
						chosen = h.label
					}
				}
				if ui.Select(c.Key("video-height"), &chosen, labels).Label("视频画质").Width(150).Changed() {
					for _, h := range videoHeights {
						if h.label == chosen {
							s.VideoHeight = h.height
						}
					}
					a.changed()
					a.changeVideoHeight()
				}
			})
			row("音量均衡", "按 YouTube 测得的响度调整每首歌的音量，让歌曲听起来一样响", func() {
				if ui.Switch(c.Key("normalize"), &s.Normalize).Label("音量均衡").Changed() {
					a.pl.SetVolume(a.volume)
					a.changed()
				}
			})
		})

		group("网络", func() {
			customDesc := "作用于目录、音频流、封面与歌词请求（Google 登录窗口使用系统网络设置）"
			directDesc := "目录、音频流、封面与歌词直接连接（Google 登录窗口使用系统网络设置）"
			switch runtime.GOOS {
			case "windows":
				customDesc = "作用于目录、音频流、封面与歌词请求（无密码代理在重启应用后对 Google 登录窗口生效）"
				directDesc = "目录、音频流、封面与歌词直接连接（重启应用后对 Google 登录窗口生效）"
			case "darwin":
				customDesc = "作用于目录、音频流、封面与歌词请求（Google 登录窗口与 MV 视频使用 macOS 系统网络设置）"
				directDesc = "目录、音频流、封面与歌词直接连接（Google 登录窗口与 MV 视频使用 macOS 系统网络设置）"
			}
			desc := customDesc
			switch a.proxyMode {
			case 0:
				if a.autoProxyDesc != "" {
					desc = a.autoProxyDesc
				} else {
					desc = defaultAutoProxyDesc()
				}
			case 2:
				desc = directDesc
			}
			row("网络代理", desc, func() {
				mode := a.proxyMode
				if segmented(c.Key("proxy-mode"), &mode, "跟随系统", "自定义", "直连") {
					a.proxyMode = mode
					a.proxyErr = ""
					prev := s.Proxy
					switch mode {
					case 0:
						s.Proxy = ""
						_ = setProxy("")
						a.changed()
						a.refreshAutoProxyDesc()
						if prev != "" {
							a.onProxyRouteChanged()
						} else if a.homeErr != "" {
							reloadHome(a)
						}
					case 1:
						if a.proxyInput == "" && a.lastCustomProxy != "" {
							a.proxyInput = ytm.RedactedProxy(a.lastCustomProxy)
							a.proxyMasked = hasProxyPassword(a.lastCustomProxy)
						}
					case 2:
						s.Proxy = "direct"
						_ = setProxy("direct")
						a.changed()
						if prev != "direct" {
							a.onProxyRouteChanged()
						} else if a.homeErr != "" {
							reloadHome(a)
						}
					}
				}
			})
			if a.proxyMode == 1 {
				ui.Column(c.Key("proxy-custom")).Padding(12, 16).Gap(8).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						in := ui.TextInput(c.Key("proxy-url"), &a.proxyInput).
							Placeholder("127.0.0.1:7890 或 socks5://127.0.0.1:1080").
							Label("代理地址").FontSize(12.5).Grow(1)
						if in.Changed() {
							a.proxyMasked = false
						}
						if in.Submitted() {
							a.applyCustomProxy()
						}
						if ui.Button(c.Key("proxy-apply"), "应用").Clicked() {
							a.applyCustomProxy()
						}
					})
					if a.proxyErr != "" {
						ui.Text(c, a.proxyErr).FontSize(11.5).TextColor(p.accent)
					} else if s.Proxy != "" && s.Proxy != "direct" {
						ui.Text(c, "已启用代理："+ytm.RedactedProxy(s.Proxy)).FontSize(11.5).TextColor(p.muted)
					}
				})
			}
		})

		if a.captionsShown() {
			group("播客", func() { a.captionSettings(row, c) })
		}

		group("缓存", func() {
			row("歌曲缓存", "听过的歌曲保存在本机，再次播放无需等待，也不耗流量；"+a.cacheUsage(), func() {
				labels := make([]string, len(cacheLimits))
				chosen := cacheLimits[2].label
				for i, l := range cacheLimits {
					labels[i] = l.label
					if l.mb == s.CacheMB {
						chosen = l.label
					}
				}
				if ui.Select(c.Key("cache"), &chosen, labels).Label("歌曲缓存").Width(150).Changed() {
					for _, l := range cacheLimits {
						if l.label == chosen {
							s.CacheMB = l.mb
						}
					}
					a.changed()
					a.applyCacheLimit()
				}
			})
			row("清除缓存", "删除保存在本机的全部歌曲", func() {
				if ui.Button(c.Key("clear-cache"), "清除").Disabled(a.cacheUsed <= 0).Clicked() {
					a.clearCache()
				}
			})
		})

		group("外观", func() {
			row("外观", "", func() {
				modes := []string{"system", "light", "dark"}
				i := 0
				for k, m := range modes {
					if m == s.Appearance {
						i = k
					}
				}
				if segmented(c.Key("appearance"), &i, "跟随系统", "浅色", "深色") {
					s.Appearance = modes[i]
					a.changed()
				}
			})
		})

		group("实验性功能", func() {
			row("进度条烟雾特效", "播放时进度条化作拖着烟雾的光点，随音乐起伏；会多用一些电量", func() {
				if ui.Switch(c.Key("smoke"), &s.Smoke).Label("进度条烟雾特效").Changed() {
					a.changed()
				}
			})
			row("烟雾背景", "播放页的背景化作封面色调的烟雾，缓缓升起，随低音起伏；仍在打磨，会多用一些电量", func() {
				if ui.Switch(c.Key("fog"), &s.Fog).Label("烟雾背景").Changed() {
					a.changed()
				}
			})
		})

		if a.profile == nil {
			group("账号", func() {
				row("未登录", "登录后可使用你的歌单、喜欢的音乐与个性化推荐", func() {
					if ui.PrimaryButton(c.Key("login"), "登录 YouTube").Clicked() {
						a.showMain()
						a.openLogin()
					}
				})
			})
		} else {
			group("账号", func() {
				prof := a.profile
				ui.Row(c).Padding(12, 16).Gap(12).AlignItems(ui.Center).Children(func() {
					a.avatar(c, prof.AvatarURL, 36, p.press)
					ui.Column(c).Grow(1).Gap(2).Children(func() {
						ui.Text(c, prof.Nickname).FontSize(13.5).FontWeight(600)
						ui.Text(c, "已登录 YouTube").FontSize(12).TextColor(p.muted)
					})
					if ui.Button(c.Key("logout"), "退出登录").Clicked() {
						a.signOut()
					}
				})
			})
		}

		if a.updates {
			group("更新", func() {
				desc := "每天检查一次是否有新版本"
				if t := lastUpdateCheck(); !t.IsZero() {
					desc += "；上次检查：" + commentTime(t, time.Now())
				}
				row("自动检查更新", desc, func() {
					on := updateChecks()
					if ui.Switch(c.Key("update-checks"), &on).Label("自动检查更新").Changed() {
						setUpdateChecks(on)
					}
				})
				row("自动下载并安装", "在后台下载新版本，下次打开时生效", func() {
					on := updateDownloads()
					if ui.Switch(c.Key("update-downloads"), &on).Label("自动下载并安装").Disabled(!updateChecks()).Changed() {
						setUpdateDownloads(on)
					}
				})
				row("当前版本 "+mygo.App.Version(), "", func() {
					if ui.Button(c.Key("check-updates"), "检查更新").Clicked() {
						checkForUpdates()
					}
				})
			})
		}
	})
}
