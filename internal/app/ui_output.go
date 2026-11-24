package app

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"

	"ytb-music/internal/bridge"
)

const (
	outputLocal    = "local"
	outputRoon     = "roon"
	outputHQPlayer = "hqplayer"
)

// Replaceable hooks for hermetic UI tests.
var (
	probeHQPlayer   = bridge.ProbeHQPlayer
	newBridgeServer = bridge.NewServer
)

type roonTokenStore struct {
	mu     sync.RWMutex
	tokens map[string]string
}

type audioChainState struct {
	open bool

	srv       *bridge.Server
	hqp       *bridge.HQPlayerBackend
	roonMgr   *bridge.RoonManager
	roon      *bridge.RoonBackend
	roonStore *roonTokenStore

	hqpInput    string
	listenInput string
	zoneLabel   string

	hqpConnected bool
	hqpStatus    string
	hqpTesting   bool
	hqpGen       int

	roonStatus bridge.RoonStatus
	bridgeErr  string
}

func normalizeOutputMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case outputRoon:
		return outputRoon
	case outputHQPlayer:
		return outputHQPlayer
	default:
		return outputLocal
	}
}

func (a *app) syncAudioInputs() {
	if a.settings.HQPlayerAddr == "" {
		a.settings.HQPlayerAddr = bridge.DefaultHQPlayerAddr
	}
	if a.settings.BridgeListen == "" {
		a.settings.BridgeListen = bridge.DefaultListenAddr
	}
	if a.settings.RoonStreamFormat == "" {
		a.settings.RoonStreamFormat = "wav"
	}
	a.settings.OutputMode = normalizeOutputMode(a.settings.OutputMode)
	if a.audio.hqpInput == "" {
		a.audio.hqpInput = a.settings.HQPlayerAddr
	}
	if a.audio.listenInput == "" {
		a.audio.listenInput = a.settings.BridgeListen
	}
}

func (a *app) initAudioChain() {
	a.syncAudioInputs()
	if a.settings.OutputMode != outputLocal {
		if err := a.applyOutputMode(a.settings.OutputMode); err != nil {
			a.settings.OutputMode = outputLocal
			a.toast = err.Error()
		}
	}
}

func (a *app) closeAudioChain() {
	if a.pl != nil {
		a.pl.Stop()
	}
	a.stopUnusedServices(outputLocal)
}

func (a *app) stopUnusedServices(keepMode string) {
	if keepMode != outputHQPlayer && a.audio.hqp != nil {
		hqp := a.audio.hqp
		a.audio.hqp = nil
		hqp.DisconnectAsync(false)
	}
	if keepMode != outputRoon && a.audio.roonMgr != nil {
		mgr := a.audio.roonMgr
		a.audio.roonMgr = nil
		a.audio.roon = nil
		go mgr.Close()
	}
	if keepMode == outputLocal && a.audio.srv != nil {
		srv := a.audio.srv
		a.audio.srv = nil
		go func() { _ = srv.Close() }()
	}
}

func (a *app) postSettingsOrMain(fn func()) {
	if a.win == nil && a.settingsWin == nil {
		if a.postHook != nil {
			a.postHook(fn)
		}
		return
	}
	win := a.win
	if win == nil {
		win = a.settingsWin
	}
	win.Update(func() {
		fn()
		if a.settingsWin != nil {
			a.settingsWin.Invalidate()
		}
		a.redraw()
	})
}

func (a *app) ensureBridgeServer() (*bridge.Server, error) {
	a.syncAudioInputs()
	if a.audio.srv != nil {
		return a.audio.srv, nil
	}
	srv, err := newBridgeServer(a.settings.BridgeListen)
	if err != nil {
		a.audio.bridgeErr = "启动本地音频桥接服务失败：" + err.Error()
		return nil, err
	}
	a.audio.bridgeErr = ""
	a.audio.srv = srv
	return srv, nil
}

func (a *app) ensureHQPlayerBackend() (*bridge.HQPlayerBackend, error) {
	srv, err := a.ensureBridgeServer()
	if err != nil {
		return nil, err
	}
	if a.audio.hqp == nil {
		a.audio.hqp = bridge.NewHQPlayerBackend(srv, a.settings.HQPlayerAddr, func(ok bool, detail string) {
			a.postSettingsOrMain(func() {
				a.audio.hqpConnected = ok
				a.audio.hqpStatus = detail
			})
		})
	} else {
		a.audio.hqp.SetAddr(a.settings.HQPlayerAddr)
	}
	return a.audio.hqp, nil
}

func (a *app) ensureRoonBackend() (*bridge.RoonBackend, error) {
	srv, err := a.ensureBridgeServer()
	if err != nil {
		return nil, err
	}
	if a.audio.roonMgr == nil {
		store := &roonTokenStore{tokens: maps.Clone(a.settings.RoonTokens)}
		if store.tokens == nil {
			store.tokens = map[string]string{}
		}
		a.audio.roonStore = store

		mgr := bridge.NewRoonManager(srv, bridge.RoonCallbacks{
			GetToken: func(coreID string) string {
				store.mu.RLock()
				defer store.mu.RUnlock()
				return store.tokens[coreID]
			},
			SaveToken: func(coreID, token string) {
				store.mu.Lock()
				store.tokens[coreID] = token
				store.mu.Unlock()
				a.postSettingsOrMain(func() {
					if a.settings.RoonTokens == nil {
						a.settings.RoonTokens = map[string]string{}
					}
					if a.settings.RoonTokens[coreID] != token {
						a.settings.RoonTokens[coreID] = token
						a.settings.save()
					}
				})
			},
			OnChange: func(st bridge.RoonStatus) {
				a.postSettingsOrMain(func() {
					a.audio.roonStatus = st
					// Auto-pick an HQPlayer zone when none has been chosen yet.
					if a.settings.RoonZoneID == "" && len(st.Zones) > 0 {
						chosen := st.Zones[0]
						for _, z := range st.Zones {
							low := strings.ToLower(z.DisplayName)
							if strings.Contains(low, "hqplayer") || strings.Contains(low, "hqp") {
								chosen = z
								break
							}
						}
						a.selectRoonZone(chosen.ZoneID, chosen.DisplayName)
					}
				})
			},
		})
		a.audio.roonMgr = mgr
		mgr.Start()
	}
	if a.audio.roon == nil {
		a.audio.roon = bridge.NewRoonBackend(srv, a.audio.roonMgr, a.settings.RoonZoneID)
	} else {
		a.audio.roon.SetZoneID(a.settings.RoonZoneID)
	}
	return a.audio.roon, nil
}

func (a *app) selectRoonZone(id, name string) {
	a.settings.RoonZoneID = id
	a.settings.RoonZoneName = name
	if a.audio.roon != nil {
		a.audio.roon.SetZoneID(id)
	}
	a.changed()
}

func (a *app) setOutputMode(mode string) {
	mode = normalizeOutputMode(mode)
	if a.settings.OutputMode == mode && ((mode == outputLocal && a.pl.Backend() == nil) || (mode != outputLocal && a.pl.Backend() != nil && a.pl.Backend().Name() == mode)) {
		return
	}
	if err := a.applyOutputMode(mode); err != nil {
		a.toast = err.Error()
		return
	}
	a.settings.OutputMode = mode
	a.changed()
}

func (a *app) applyOutputMode(mode string) error {
	mode = normalizeOutputMode(mode)
	switch mode {
	case outputLocal:
		// Cancel any in-flight external load FIRST before disconnecting sockets.
		a.pl.SetBackend(nil)
		a.stopUnusedServices(outputLocal)
		return nil
	case outputHQPlayer:
		b, err := a.ensureHQPlayerBackend()
		if err != nil {
			return err
		}
		a.pl.SetBackend(b)
		a.stopUnusedServices(outputHQPlayer)
		return nil
	case outputRoon:
		b, err := a.ensureRoonBackend()
		if err != nil {
			return err
		}
		a.pl.SetBackend(b)
		a.stopUnusedServices(outputRoon)
		return nil
	}
	return nil
}

func (a *app) applyHQPlayerAddr() {
	addr := strings.TrimSpace(a.audio.hqpInput)
	if addr == "" {
		addr = bridge.DefaultHQPlayerAddr
	}
	prev := a.settings.HQPlayerAddr
	a.audio.hqpInput = addr
	a.settings.HQPlayerAddr = addr

	if addr != prev && a.audio.hqp != nil {
		oldHQP := a.audio.hqp
		a.audio.hqp = nil
		if a.settings.OutputMode == outputHQPlayer {
			if newHQP, err := a.ensureHQPlayerBackend(); err == nil {
				// Reopen any active track on the new HQPlayer endpoint before stopping the old one.
				a.pl.SetBackend(newHQP)
			}
		}
		oldHQP.DisconnectAsync(true)
	}
	a.changed()
	a.testHQPlayerConn()
}

func (a *app) applyBridgeListen() {
	addr := strings.TrimSpace(a.audio.listenInput)
	if addr == "" {
		addr = bridge.DefaultListenAddr
	}
	if a.audio.srv != nil {
		if err := a.audio.srv.Rebind(addr); err != nil {
			a.audio.bridgeErr = "监听地址绑定失败：" + err.Error()
			return
		}
		addr = a.audio.srv.Addr()
	}
	a.audio.bridgeErr = ""
	a.audio.listenInput = addr
	a.settings.BridgeListen = addr
	a.changed()
	a.toast = "已更新音频桥接监听地址：" + addr
}

func (a *app) testHQPlayerConn() {
	a.syncAudioInputs()
	addr := strings.TrimSpace(a.audio.hqpInput)
	if addr == "" {
		addr = a.settings.HQPlayerAddr
	}
	a.audio.hqpGen++
	gen := a.audio.hqpGen
	a.audio.hqpTesting = true
	a.audio.hqpStatus = "正在连接 " + addr + "…"

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		prod, err := probeHQPlayer(ctx, addr)
		a.postSettingsOrMain(func() {
			if gen != a.audio.hqpGen {
				return
			}
			a.audio.hqpTesting = false
			if err != nil {
				a.audio.hqpConnected = false
				a.audio.hqpStatus = err.Error()
				return
			}
			a.audio.hqpConnected = true
			a.audio.hqpStatus = "已连接 " + prod
		})
	}()
}

func (a *app) currentRoonStreamURL() string {
	a.syncAudioInputs()
	peer := a.audio.roonStatus.CoreAddr
	if srv := a.audio.srv; srv != nil {
		if a.settings.RoonStreamFormat == "aac" {
			return srv.RoonAACURL(peer)
		}
		return srv.RoonWAVURL(peer)
	}
	suffix := "/roon/live.wav"
	if a.settings.RoonStreamFormat == "aac" {
		suffix = "/roon/live.aac"
	}
	return "http://" + a.settings.BridgeListen + suffix
}

// outputButton sits in the bottom player bar and opens the audio chain switcher popover.
func (a *app) outputButton(c *ui.Context) {
	p := a.pal
	mode := normalizeOutputMode(a.settings.OutputMode)
	icon := iconSpeaker
	color := p.muted
	if mode != outputLocal {
		icon = iconCast
		color = p.accent
	}
	b := iconButton(c.Key("output-button"), icon, 16, color, "输出设备")
	if b.Clicked() {
		a.audio.open = !a.audio.open
		if a.audio.open {
			a.syncAudioInputs()
		}
	}

	pop := popoverColors(p.dark)
	ui.PopoverBase(c.Key("output-popover"), b, &a.audio.open, func(panel ui.Element) {
		panel.AttachTo(b, ui.AnchorTopEnd, ui.AnchorBottomEnd).Margin(0, 0, 10, 0).Width(276).Padding(8).Gap(4).
			Radius(13).Background(pop.fill).Border(0.5, pop.edge).Shadow(0, 10, 28, -4, ui.RGBA(0, 0, 0, 0.25)).
			Role(ui.RoleMenu).Label("输出设备").TransformOrigin(0.9, 1).
			Transition(ui.ElementTransition{Animation: &ui.Snappy, Enter: &ui.Motion{Scale: 0.96, Y: 4}, Exit: &ui.Motion{Scale: 0.98}})

		ui.Text(c, "音频输出链路").FontSize(11.5).FontWeight(600).TextColor(p.faint).Padding(4, 8, 2, 8)

		a.outputOptionRow(c, outputLocal, "本机输出", "系统默认音频设备", mode == outputLocal)

		roonSub := "44.1kHz 24-bit · 经 Roon 推送至 HQPlayer"
		if st := a.audio.roonStatus; st.Paired {
			if a.settings.RoonZoneName != "" {
				roonSub = st.CoreName + " → " + a.settings.RoonZoneName
			} else {
				roonSub = "已配对 " + st.CoreName
			}
		} else if st.Listeners > 0 {
			roonSub = "Roon 正在拉取无损音频流"
		}
		a.outputOptionRow(c, outputRoon, "Roon 联动桥接", roonSub, mode == outputRoon)

		if mode == outputRoon {
			ui.Column(c.Key("roon-quick")).Padding(4, 8, 6, 26).Gap(4).Children(func() {
				for _, z := range a.audio.roonStatus.Zones {
					zb := ui.ButtonBase(c.Key("zone-" + z.ZoneID)).Height(26).PaddingX(8).Radius(6).Gap(6).
						AlignItems(ui.Center).Justify(ui.Start).FocusRing(false).Label(z.DisplayName)
					if zb.Hovered() {
						zb.Background(p.hover)
					}
					sel := a.settings.RoonZoneID == z.ZoneID
					zb.Children(func() {
						ui.Text(c, z.DisplayName).FontSize(12).TextColor(p.text).Grow(1).SingleLine()
						if sel {
							ui.Icon(c, iconCheck).Size(12, 12).TextColor(p.accent)
						}
					})
					if zb.Clicked() {
						a.selectRoonZone(z.ZoneID, z.DisplayName)
					}
				}
				if ui.Button(c.Key("copy-roon-url"), "复制电台流地址").Clicked() {
					u := a.currentRoonStreamURL()
					_ = copyText(u)
					a.toast = "已复制电台地址：" + u
				}
			})
		}

		hqpSub := "44.1kHz 24-bit WAV · " + a.settings.HQPlayerAddr
		if a.audio.hqpStatus != "" && mode == outputHQPlayer {
			hqpSub = a.audio.hqpStatus
		}
		a.outputOptionRow(c, outputHQPlayer, "HQPlayer 直推", hqpSub, mode == outputHQPlayer)

		ui.Box(c).Height(0.5).Margin(4, 6).Background(pop.edge)
		cfgBtn := ui.ButtonBase(c.Key("audio-settings")).Role(ui.RoleMenuItem).Label("音频链路设置…").
			Height(30).PaddingX(8).Gap(8).Radius(7).AlignItems(ui.Center).Justify(ui.Start).FocusRing(false)
		if cfgBtn.Hovered() {
			cfgBtn.Background(p.press)
		}
		cfgBtn.Children(func() {
			ui.Icon(c, iconSettings).Size(14, 14).TextColor(p.muted)
			ui.Text(c, "音频链路设置…").FontSize(12.5).Grow(1)
		})
		if cfgBtn.Clicked() {
			a.audio.open = false
			a.openSettings()
		}
	})
}

func (a *app) outputOptionRow(c *ui.Context, modeID, title, subtitle string, active bool) {
	p := a.pal
	b := ui.ButtonBase(c.Key("out-" + modeID)).Role(ui.RoleMenuItem).Label(title).
		Padding(6, 8).Gap(9).Radius(8).AlignItems(ui.Center).Justify(ui.Start).FocusRing(false)
	if active {
		b.Background(p.accent.Alpha(0.1))
	} else if b.Hovered() || b.FocusVisible() {
		b.Background(p.press)
	}
	ic := iconSpeaker
	if modeID != outputLocal {
		ic = iconCast
	}
	icColor := p.muted
	if active {
		icColor = p.accent
	}
	b.Children(func() {
		ui.Icon(c, ic).Size(15, 15).TextColor(icColor).Shrink(0)
		ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Gap(1).Children(func() {
			ui.Text(c, title).FontSize(13).FontWeight(600).SingleLine()
			ui.Text(c, subtitle).FontSize(11).TextColor(p.muted).SingleLine()
		})
		if active {
			ui.Icon(c, iconCheck).Size(14, 14).TextColor(p.accent).Shrink(0)
		}
	})
	if b.Clicked() {
		a.setOutputMode(modeID)
	}
}

// audioChainSettings renders the audiophile Roon + HQPlayer section inside Settings.
func (a *app) audioChainSettings(c *ui.Context, group func(string, func()), row func(string, string, func())) {
	a.syncAudioInputs()
	p := paletteFor(c.Theme())
	s := &a.settings

	group("发烧音频链路 (Roon / HQPlayer)", func() {
		modes := []string{outputLocal, outputRoon, outputHQPlayer}
		modeIdx := 0
		for i, m := range modes {
			if m == normalizeOutputMode(s.OutputMode) {
				modeIdx = i
			}
		}
		modeDesc := "使用系统默认音频混音器输出"
		switch modes[modeIdx] {
		case outputRoon:
			modeDesc = "输出原生 44.1kHz 24-bit 无损流至 Roon Live Radio，再由 Roon 转发至 HQPlayer 升频与 DSD 调制"
		case outputHQPlayer:
			modeDesc = "绕过系统 48kHz 重采样，通过 TCP 4321 将原生 44.1kHz 24-bit WAV 直接推送至 HQPlayer 引擎"
		}
		row("输出通道", modeDesc, func() {
			if segmented(c.Key("audio-output-mode"), &modeIdx, "本机", "Roon", "HQPlayer") {
				a.setOutputMode(modes[modeIdx])
			}
		})

		if modes[modeIdx] == outputHQPlayer {
			ui.Column(c.Key("hqp-settings")).Padding(12, 16).Gap(8).Children(func() {
				ui.Text(c, "HQPlayer 网络控制地址（TCP 4321，需开启 HQPlayer 主界面允许网络控制图标）").FontSize(12).TextColor(p.muted)
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					in := ui.TextInput(c.Key("hqp-addr"), &a.audio.hqpInput).
						Placeholder("127.0.0.1:4321 或局域网 IP:4321").
						Label("HQPlayer 地址").FontSize(12.5).Grow(1)
					if in.Submitted() {
						a.applyHQPlayerAddr()
					}
					if ui.Button(c.Key("hqp-save"), "保存").Clicked() {
						a.applyHQPlayerAddr()
					}
					if ui.Button(c.Key("hqp-probe"), "测试连接").Disabled(a.audio.hqpTesting).Clicked() {
						a.testHQPlayerConn()
					}
				})
				if a.audio.hqpStatus != "" {
					col := p.muted
					if !a.audio.hqpConnected && !a.audio.hqpTesting {
						col = p.accent
					}
					ui.Text(c, a.audio.hqpStatus).FontSize(11.5).TextColor(col)
				}
			})
		}

		if modes[modeIdx] == outputRoon {
			st := a.audio.roonStatus
			statusDesc := st.Detail
			if statusDesc == "" {
				statusDesc = "首次使用请在 Roon「设置 → 扩展」中点击启用 YTB Music Bridge，并在 My Live Radio 添加下方流地址（名称含 YTB Music）"
			}
			row("Roon Core 状态", statusDesc, func() {
				if ui.Button(c.Key("roon-rescan"), "重新发现").Clicked() {
					_, _ = a.ensureRoonBackend()
					if a.audio.roonMgr != nil {
						a.audio.roonMgr.Wake()
					}
				}
			})

			if len(st.Zones) > 0 {
				row("Roon 输出区域", "自动关联至 HQPlayer 区域或其他 Roon Ready 终端", func() {
					labels := make([]string, len(st.Zones))
					chosen := st.Zones[0].DisplayName
					for i, z := range st.Zones {
						labels[i] = z.DisplayName
						if z.ZoneID == s.RoonZoneID {
							chosen = z.DisplayName
						}
					}
					a.audio.zoneLabel = chosen
					if ui.Select(c.Key("roon-zone"), &a.audio.zoneLabel, labels).Label("Roon 输出区域").Width(180).Changed() {
						for _, z := range st.Zones {
							if z.DisplayName == a.audio.zoneLabel {
								a.selectRoonZone(z.ZoneID, z.DisplayName)
								break
							}
						}
					}
				})
			}

			row("电台流格式", "WAV 24-bit 传输原生 44.1kHz PCM；AAC 传输零重编码比特直通帧（静音联动，音量由 Roon 控制）", func() {
				fmts := []string{"wav", "aac"}
				fIdx := 0
				if s.RoonStreamFormat == "aac" {
					fIdx = 1
				}
				if segmented(c.Key("roon-stream-fmt"), &fIdx, "WAV 24-bit", "AAC 原生") {
					s.RoonStreamFormat = fmts[fIdx]
					a.changed()
				}
			})

			streamURL := a.currentRoonStreamURL()
			row("Roon 电台流地址", streamURL, func() {
				if ui.Button(c.Key("roon-copy"), "复制地址").Clicked() {
					_ = copyText(streamURL)
					a.toast = "已复制 Roon 电台流地址"
				}
			})
		}

		if modes[modeIdx] != outputLocal {
			ui.Column(c.Key("bridge-listen-row")).Padding(12, 16).Gap(6).Children(func() {
				ui.Text(c, "本地 HTTP 音频桥接监听地址（Roon / HQPlayer 在同机保持 127.0.0.1:17890；跨局域网主机可改为 0.0.0.0:17890）").
					FontSize(11.5).TextColor(p.muted).LineHeight(1.4)
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					in := ui.TextInput(c.Key("bridge-listen"), &a.audio.listenInput).
						Placeholder("127.0.0.1:17890").
						Label("桥接监听地址").FontSize(12.5).Grow(1)
					if in.Submitted() {
						a.applyBridgeListen()
					}
					if ui.Button(c.Key("bridge-apply"), "应用端口").Clicked() {
						a.applyBridgeListen()
					}
				})
				if a.audio.bridgeErr != "" {
					ui.Text(c, a.audio.bridgeErr).FontSize(11.5).TextColor(p.accent)
				}
			})
		}
	})
}
