package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"ytb-music/internal/player"
)

const (
	soodPort          = 9003
	soodMulticastAddr = "239.255.90.90:9003"
	roonRegistrySvcID = "00720724-5143-4a9b-abac-0e50cba674bb"
	roonExtensionID   = "com.ytbmusic.bridge"
)

// DiscoverSOODFunc locates a Roon Core on the network; tests replace it so UI
// tests never emit multicast/broadcast UDP traffic.
var DiscoverSOODFunc = discoverRoonSOOD

// RoonZone describes an output zone reported by Roon Core (such as an HQPlayer zone).
type RoonZone struct {
	ZoneID      string `json:"zone_id"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"`
	NowPlaying  *struct {
		OneLine struct {
			Line1 string `json:"line1"`
		} `json:"one_line"`
		ThreeLine struct {
			Line1 string `json:"line1"`
			Line2 string `json:"line2"`
		} `json:"three_line"`
	} `json:"now_playing,omitempty"`
}

// RoonStatus summarizes the current Roon Core pairing and Live Radio state.
type RoonStatus struct {
	Discovered bool
	Paired     bool
	CoreID     string
	CoreName   string
	CoreAddr   string
	Zones      []RoonZone
	Listeners  int
	Detail     string
}

// RoonAlbumGenre pairs a Roon library album and artist with its assigned genres.
type RoonAlbumGenre struct {
	Album  string
	Artist string
	Genres []string
}

type RoonCallbacks struct {
	GetToken  func(coreID string) string
	SaveToken func(coreID, token string)
	OnChange  func(RoonStatus)
}

var roonOwnerSeq atomic.Uint64

// RoonManager discovers Roon Core via SOOD UDP multicast/localhost, registers
// the "YTB Music Bridge" extension over MOO/1 WebSocket, tracks output zones,
// and triggers Live Radio playback on the chosen zone.
type RoonManager struct {
	srv *Server
	cb  RoonCallbacks

	mu            sync.RWMutex
	directAddr    string
	status        RoonStatus
	zonesMap      map[string]RoonZone
	moo           *mooSession
	intentOwner   uint64
	intentGen     uint64
	wantPlaying   bool
	wantZoneID    string
	startedZoneID string
	zoneOwned     bool
	startCancel   context.CancelFunc

	browseMu sync.Mutex
	genreMu  sync.Mutex

	stopOnce sync.Once
	stopCh   chan struct{}
	wakeCh   chan struct{}
	startCh  chan struct{}
}

func NewRoonManager(srv *Server, cb RoonCallbacks) *RoonManager {
	m := &RoonManager{
		srv:      srv,
		cb:       cb,
		zonesMap: make(map[string]RoonZone),
		stopCh:   make(chan struct{}),
		wakeCh:   make(chan struct{}, 1),
		startCh:  make(chan struct{}, 1),
		status: RoonStatus{
			Detail: "正在局域网搜索 Roon Core…",
		},
	}
	if srv != nil {
		srv.SetOnListeners(func(cnt int) {
			m.mu.Lock()
			m.status.Listeners = cnt
			st := m.snapshotLocked()
			m.mu.Unlock()
			if m.cb.OnChange != nil {
				m.cb.OnChange(st)
			}
		})
	}
	go m.startWorker()
	return m
}

func (m *RoonManager) Start() {
	go m.loop()
}

func (m *RoonManager) SetDirectCoreAddr(addr string) {
	m.mu.Lock()
	m.directAddr = strings.TrimSpace(addr)
	sess := m.moo
	m.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
	m.Wake()
}

func (m *RoonManager) Wake() {
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
}

func (m *RoonManager) Close() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		if m.srv != nil {
			m.srv.SetOnListeners(nil)
		}
		m.mu.Lock()
		m.wantPlaying = false
		if m.startCancel != nil {
			m.startCancel()
			m.startCancel = nil
		}
		sess := m.moo
		m.moo = nil
		m.mu.Unlock()
		if sess != nil {
			sess.Close()
		}
	})
}

func (m *RoonManager) signalStartWorker() {
	select {
	case m.startCh <- struct{}{}:
	default:
	}
}

// SetPlaybackIntent records whether ownerID wants Roon's selected zone to play
// our Live Radio station, ignoring stale false intents from superseded sinks.
func (m *RoonManager) SetPlaybackIntent(ownerID uint64, wantPlay bool, zoneID string) {
	if ownerID == 0 {
		return
	}
	m.mu.Lock()
	if !wantPlay {
		if m.intentOwner != ownerID {
			m.mu.Unlock()
			return
		}
		m.wantPlaying = false
		m.intentGen++
		if m.startCancel != nil {
			m.startCancel()
			m.startCancel = nil
		}
		m.mu.Unlock()
		return
	}
	m.intentOwner = ownerID
	m.intentGen++
	m.wantPlaying = true
	if zoneID != "" {
		m.wantZoneID = zoneID
	}
	if m.startCancel != nil {
		m.startCancel()
		m.startCancel = nil
	}
	m.mu.Unlock()
	m.signalStartWorker()
}

func (m *RoonManager) UpdateZone(zoneID string) {
	zoneID = strings.TrimSpace(zoneID)
	m.mu.Lock()
	changed := m.wantZoneID != zoneID
	m.wantZoneID = zoneID
	want := m.wantPlaying
	if changed {
		m.intentGen++
		if m.startCancel != nil {
			m.startCancel()
			m.startCancel = nil
		}
	}
	m.mu.Unlock()
	if changed && want {
		m.signalStartWorker()
	}
}

func (m *RoonManager) maybeRetryPendingPlay() {
	m.mu.RLock()
	want := m.wantPlaying
	paired := m.status.Paired
	hasZones := len(m.zonesMap) > 0
	m.mu.RUnlock()
	if want && paired && hasZones {
		m.signalStartWorker()
	}
}

func (m *RoonManager) startWorker() {
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.startCh:
		}
		m.mu.Lock()
		if !m.wantPlaying || !m.status.Paired || len(m.zonesMap) == 0 {
			m.mu.Unlock()
			continue
		}
		gen := m.intentGen
		zid := m.wantZoneID
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		m.startCancel = cancel
		m.mu.Unlock()

		_ = m.ensureStationPlayingSerialized(ctx, gen, zid)
		cancel()
	}
}

func (m *RoonManager) Status() RoonStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshotLocked()
}

func (m *RoonManager) snapshotLocked() RoonStatus {
	st := m.status
	if m.srv != nil {
		st.Listeners = m.srv.ActiveListeners()
	}
	st.Zones = make([]RoonZone, 0, len(m.zonesMap))
	for _, z := range m.zonesMap {
		st.Zones = append(st.Zones, z)
	}
	return st
}

func (m *RoonManager) emit() {
	m.mu.RLock()
	st := m.snapshotLocked()
	m.mu.RUnlock()
	if m.cb.OnChange != nil {
		m.cb.OnChange(st)
	}
}

func (m *RoonManager) loop() {
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}

		m.mu.RLock()
		addr := m.directAddr
		m.mu.RUnlock()

		var coreName string
		if addr == "" {
			discAddr, name, err := DiscoverSOODFunc(2500 * time.Millisecond)
			if err != nil || discAddr == "" {
				m.mu.Lock()
				m.status.Discovered = false
				m.status.Paired = false
				m.status.Detail = "未检测到 Roon Core（可直接复制电台流地址到 Roon 播放）"
				m.mu.Unlock()
				m.emit()
				select {
				case <-m.stopCh:
					return
				case <-m.wakeCh:
				case <-time.After(6 * time.Second):
				}
				continue
			}
			addr, coreName = discAddr, name
		}

		m.runSession(addr, coreName)

		select {
		case <-m.stopCh:
			return
		case <-m.wakeCh:
		case <-time.After(3 * time.Second):
		}
	}
}

func (m *RoonManager) runSession(addr, hintName string) {
	cfg, err := websocket.NewConfig("ws://"+addr+"/api", "http://"+addr)
	if err != nil {
		return
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	rawConn, err := dialer.Dial("tcp", addr)
	if err != nil {
		m.mu.Lock()
		m.status.Discovered = false
		m.status.Paired = false
		m.status.Detail = "连接 Roon Core 失败：" + addr
		m.mu.Unlock()
		m.emit()
		return
	}
	ws, err := websocket.NewClient(cfg, rawConn)
	if err != nil {
		_ = rawConn.Close()
		return
	}

	sess := newMOOSession(ws, m.handleIncomingRequest)
	m.mu.Lock()
	m.moo = sess
	m.startedZoneID = ""
	m.status.Discovered = true
	m.status.CoreAddr = addr
	if hintName != "" {
		m.status.CoreName = hintName
	}
	m.status.Detail = "已发现 Roon Core，正在握手…"
	m.mu.Unlock()
	m.emit()

	defer func() {
		sess.Close()
		m.mu.Lock()
		if m.moo == sess {
			m.moo = nil
		}
		m.status.Paired = false
		m.startedZoneID = ""
		m.zonesMap = make(map[string]RoonZone)
		m.mu.Unlock()
		m.emit()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	infoMsg, err := sess.Request(ctx, "com.roonlabs.registry:1/info", nil)
	cancel()
	if err != nil {
		return
	}
	var info struct {
		CoreID      string `json:"core_id"`
		DisplayName string `json:"display_name"`
	}
	_ = json.Unmarshal(infoMsg.Body, &info)
	if info.DisplayName == "" {
		info.DisplayName = "Roon Core"
	}

	m.mu.Lock()
	m.status.CoreID = info.CoreID
	m.status.CoreName = info.DisplayName
	m.status.Detail = fmt.Sprintf("请在 Roon「设置 → 扩展」中授权 YTB Music Bridge（%s）", info.DisplayName)
	m.mu.Unlock()
	m.emit()

	var savedToken string
	if m.cb.GetToken != nil && info.CoreID != "" {
		savedToken = m.cb.GetToken(info.CoreID)
	}

	regPayload := map[string]any{
		"extension_id":      roonExtensionID,
		"display_name":      "YTB Music Bridge",
		"display_version":   "0.2.0",
		"publisher":         "YTB Music",
		"email":             "bridge@ytbmusic.local",
		"required_services": []string{"com.roonlabs.transport:2", "com.roonlabs.browse:1"},
		"provided_services": []string{"com.roonlabs.ping:1", "com.roonlabs.pairing:1"},
	}
	if savedToken != "" {
		regPayload["token"] = savedToken
	}

	zonesSubscribed := false
	sess.Subscribe("com.roonlabs.registry:1/register", regPayload, func(msg mooMessage) {
		switch msg.Name {
		case "Registered":
			var reg struct {
				CoreID      string `json:"core_id"`
				DisplayName string `json:"display_name"`
				Token       string `json:"token"`
			}
			_ = json.Unmarshal(msg.Body, &reg)
			cid := reg.CoreID
			if cid == "" {
				cid = info.CoreID
			}
			if reg.Token != "" && cid != "" && m.cb.SaveToken != nil {
				m.cb.SaveToken(cid, reg.Token)
			}
			m.mu.Lock()
			m.status.Paired = true
			if reg.DisplayName != "" {
				m.status.CoreName = reg.DisplayName
			}
			m.status.Detail = "已连接 Roon Core：" + m.status.CoreName
			needSub := !zonesSubscribed
			zonesSubscribed = true
			m.mu.Unlock()
			m.emit()

			if needSub {
				m.subscribeZones(sess)
			} else {
				m.maybeRetryPendingPlay()
			}
		}
	})

	select {
	case <-m.stopCh:
	case <-sess.doneCh:
	}
}

func (m *RoonManager) subscribeZones(sess *mooSession) {
	sess.Subscribe("com.roonlabs.transport:2/subscribe_zones", map[string]any{
		"subscription_key": "ytb-zones",
	}, func(msg mooMessage) {
		var payload struct {
			Zones        []RoonZone `json:"zones"`
			ZonesAdded   []RoonZone `json:"zones_added"`
			ZonesChanged []RoonZone `json:"zones_changed"`
			ZonesRemoved []string   `json:"zones_removed"`
		}
		if json.Unmarshal(msg.Body, &payload) != nil {
			return
		}
		m.mu.Lock()
		if msg.Name == "Subscribed" && len(payload.Zones) > 0 {
			m.zonesMap = make(map[string]RoonZone, len(payload.Zones))
			for _, z := range payload.Zones {
				m.zonesMap[z.ZoneID] = z
			}
		}
		for _, z := range payload.ZonesAdded {
			m.zonesMap[z.ZoneID] = z
		}
		for _, z := range payload.ZonesChanged {
			m.zonesMap[z.ZoneID] = z
			if z.ZoneID == m.startedZoneID && m.zoneOwned && !m.wantPlaying {
				if z.State == "stopped" || (z.NowPlaying != nil && !zoneMatchesBridge(z)) {
					m.zoneOwned = false
				}
			}
		}
		for _, id := range payload.ZonesRemoved {
			delete(m.zonesMap, id)
			if m.startedZoneID == id {
				m.startedZoneID = ""
				m.zoneOwned = false
			}
		}
		m.mu.Unlock()
		m.emit()
		m.maybeRetryPendingPlay()
	})
}

func (m *RoonManager) handleIncomingRequest(sess *mooSession, msg mooMessage) {
	switch msg.Name {
	case "com.roonlabs.ping:1/ping":
		_ = sess.Reply(msg.RequestID, "COMPLETE", "Success", nil)
	case "com.roonlabs.pairing:1/subscribe_pairing":
		m.mu.RLock()
		cid := m.status.CoreID
		m.mu.RUnlock()
		_ = sess.Reply(msg.RequestID, "CONTINUE", "Subscribed", map[string]any{
			"paired_core_id": cid,
		})
	case "com.roonlabs.pairing:1/get_pairing":
		m.mu.RLock()
		cid := m.status.CoreID
		m.mu.RUnlock()
		_ = sess.Reply(msg.RequestID, "COMPLETE", "Success", map[string]any{
			"paired_core_id": cid,
		})
	default:
		_ = sess.Reply(msg.RequestID, "COMPLETE", "InvalidRequest", nil)
	}
}

func zoneMatchesBridge(z RoonZone) bool {
	if z.NowPlaying == nil {
		return true
	}
	joined := strings.ToLower(z.NowPlaying.OneLine.Line1 + " " + z.NowPlaying.ThreeLine.Line1 + " " + z.NowPlaying.ThreeLine.Line2)
	if strings.TrimSpace(joined) == "" {
		return true
	}
	return strings.Contains(joined, "ytb") || strings.Contains(joined, "youtube")
}

func isMOOSuccess(msg mooMessage) bool {
	return msg.Name == "Success" || msg.Name == "Loaded"
}

// EnsureStationPlaying locates the "YTB Music" station in Roon's My Live Radio
// hierarchy and starts playback on the resolved target zone when needed.
func (m *RoonManager) EnsureStationPlaying(ctx context.Context, zoneID string) error {
	m.mu.RLock()
	gen := m.intentGen
	m.mu.RUnlock()
	return m.ensureStationPlayingSerialized(ctx, gen, zoneID)
}

func (m *RoonManager) ensureStationPlayingSerialized(ctx context.Context, gen uint64, zoneID string) error {
	m.browseMu.Lock()
	defer m.browseMu.Unlock()

	m.mu.RLock()
	sess := m.moo
	paired := m.status.Paired
	prevStarted := m.startedZoneID
	if zoneID == "" {
		zoneID = m.wantZoneID
	}
	if zoneID == "" {
		for _, z := range m.zonesMap {
			if strings.Contains(strings.ToLower(z.DisplayName), "hqplayer") || strings.Contains(strings.ToLower(z.DisplayName), "hqp") {
				zoneID = z.ZoneID
				break
			}
			if zoneID == "" {
				zoneID = z.ZoneID
			}
		}
	}
	zoneInfo, zoneKnown := m.zonesMap[zoneID]
	alreadyStarted := prevStarted == zoneID && zoneKnown && zoneInfo.State == "playing" && m.srv != nil && m.srv.ActiveListeners() > 0
	stillValid := m.wantPlaying && m.intentGen == gen
	m.mu.RUnlock()

	if !stillValid || alreadyStarted || sess == nil || !paired || zoneID == "" {
		return nil
	}

	popMsg, err := sess.Request(ctx, "com.roonlabs.browse:1/browse", map[string]any{
		"hierarchy":         "internet_radio",
		"pop_all":           true,
		"zone_or_output_id": zoneID,
	})
	if err != nil || !isMOOSuccess(popMsg) {
		return err
	}
	loadMsg, err := sess.Request(ctx, "com.roonlabs.browse:1/load", map[string]any{
		"hierarchy": "internet_radio",
		"offset":    0,
		"count":     100,
	})
	if err != nil || !isMOOSuccess(loadMsg) {
		return err
	}
	var loaded struct {
		Items []struct {
			Title    string `json:"title"`
			Subtitle string `json:"subtitle"`
			ItemKey  string `json:"item_key"`
			Hint     string `json:"hint"`
		} `json:"items"`
	}
	if json.Unmarshal(loadMsg.Body, &loaded) != nil {
		return nil
	}

	var stationKey string
	for _, it := range loaded.Items {
		tl := strings.ToLower(it.Title)
		if strings.Contains(tl, "ytb") || strings.Contains(tl, "youtube") {
			stationKey = it.ItemKey
			break
		}
	}
	if stationKey == "" || ctx.Err() != nil {
		return nil
	}

	brMsg, err := sess.Request(ctx, "com.roonlabs.browse:1/browse", map[string]any{
		"hierarchy":         "internet_radio",
		"item_key":          stationKey,
		"zone_or_output_id": zoneID,
	})
	if err != nil || !isMOOSuccess(brMsg) {
		return err
	}
	var brRes struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(brMsg.Body, &brRes)
	if brRes.Action == "message" {
		return nil
	}
	played := brRes.Action == "none" || brRes.Action == ""
	if brRes.Action == "list" {
		actLoad, err := sess.Request(ctx, "com.roonlabs.browse:1/load", map[string]any{
			"hierarchy": "internet_radio",
			"offset":    0,
			"count":     20,
		})
		if err != nil || !isMOOSuccess(actLoad) {
			return err
		}
		var acts struct {
			Items []struct {
				Title   string `json:"title"`
				ItemKey string `json:"item_key"`
			} `json:"items"`
		}
		if json.Unmarshal(actLoad.Body, &acts) != nil || len(acts.Items) == 0 {
			return nil
		}
		playKey := acts.Items[0].ItemKey
		for _, act := range acts.Items {
			al := strings.ToLower(act.Title)
			if strings.Contains(al, "play") || strings.Contains(al, "播放") {
				playKey = act.ItemKey
				break
			}
		}

		// Re-verify intent was not superseded before issuing Play Now.
		m.mu.RLock()
		okToPlay := m.wantPlaying && m.intentGen == gen && ctx.Err() == nil
		m.mu.RUnlock()
		if !okToPlay {
			return nil
		}

		actMsg, err := sess.Request(ctx, "com.roonlabs.browse:1/browse", map[string]any{
			"hierarchy":         "internet_radio",
			"item_key":          playKey,
			"zone_or_output_id": zoneID,
		})
		if err != nil || !isMOOSuccess(actMsg) {
			return err
		}
		var actRes struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(actMsg.Body, &actRes)
		if actRes.Action == "message" {
			return nil
		}
		played = true
	}

	if !played {
		return nil
	}

	// Record the newly started zone immediately (even if superseded while stopping old zone)
	// and check whether the previous zone is still owned by our bridge before stopping it.
	m.mu.Lock()
	oldZone := m.startedZoneID
	oldOwned := m.zoneOwned
	oldInfo, hasOld := m.zonesMap[oldZone]
	m.startedZoneID = zoneID
	m.zoneOwned = true
	m.mu.Unlock()

	if oldZone != "" && oldZone != zoneID && oldOwned && (!hasOld || zoneMatchesBridge(oldInfo)) {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = sess.Request(stopCtx, "com.roonlabs.transport:2/control", map[string]any{
			"zone_or_output_id": oldZone,
			"control":           "stop",
		})
		cancel()
	}
	return nil
}

const roonGenreSessionKey = "ytb-genre-sync"

type roonBrowseItem struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	ItemKey  string `json:"item_key"`
	Hint     string `json:"hint"`
}

type roonBrowseResult struct {
	Action string `json:"action"`
	List   *struct {
		Title string `json:"title"`
		Count int    `json:"count"`
		Level int    `json:"level"`
	} `json:"list"`
}

type roonLoadResult struct {
	Items  []roonBrowseItem `json:"items"`
	Offset int              `json:"offset"`
	List   *struct {
		Title string `json:"title"`
		Count int    `json:"count"`
		Level int    `json:"level"`
	} `json:"list"`
}

func findRoonAlbumsBucket(items []roonBrowseItem) string {
	for _, it := range items {
		if it.ItemKey == "" || it.Hint == "action" || it.Hint == "header" {
			continue
		}
		t := strings.TrimSpace(it.Title)
		tl := strings.ToLower(t)
		if tl == "albums" || tl == "all albums" || tl == "view all albums" ||
			strings.HasPrefix(tl, "albums (") || strings.HasPrefix(tl, "albums(") ||
			t == "专辑" || t == "全部专辑" || t == "查看全部专辑" ||
			strings.HasPrefix(t, "专辑 (") || strings.HasPrefix(t, "专辑(") || strings.HasPrefix(t, "专辑（") {
			return it.ItemKey
		}
	}
	return ""
}

func isRoonNonAlbumRow(it roonBrowseItem, atRoot bool) bool {
	if it.Hint == "action" || it.Hint == "header" {
		return true
	}
	t := strings.TrimSpace(it.Title)
	if t == "" {
		return true
	}
	if strings.TrimSpace(it.Subtitle) != "" {
		return false
	}
	tl := strings.ToLower(t)
	switch tl {
	case "play genre", "play now", "play all", "shuffle", "start radio", "roon radio",
		"播放流派", "立即播放", "播放全部", "随机播放":
		return true
	}
	if !atRoot {
		switch tl {
		case "artists", "all artists", "composers", "subgenres",
			"艺术家", "全部艺术家", "作曲家", "子流派":
			return true
		}
	}
	return false
}

// FetchLibraryGenres traverses Roon's "genres" browse hierarchy using an isolated
// multi_session_key ("ytb-genre-sync") so Live Radio browsing is never disturbed,
// returning deduplicated (Album, Artist) -> Genres mappings.
func (m *RoonManager) FetchLibraryGenres(ctx context.Context) ([]RoonAlbumGenre, error) {
	m.genreMu.Lock()
	defer m.genreMu.Unlock()

	m.mu.RLock()
	sess := m.moo
	paired := m.status.Paired
	m.mu.RUnlock()
	if sess == nil || !paired {
		return nil, errors.New("尚未连接或授权 Roon Core")
	}

	browseGenres := func(opts map[string]any) (roonBrowseResult, error) {
		req := map[string]any{
			"hierarchy":         "genres",
			"multi_session_key": roonGenreSessionKey,
		}
		for k, v := range opts {
			req[k] = v
		}
		msg, err := sess.Request(ctx, "com.roonlabs.browse:1/browse", req)
		if err != nil {
			return roonBrowseResult{}, err
		}
		if !isMOOSuccess(msg) {
			return roonBrowseResult{}, fmt.Errorf("roon browse genres failed: %s", msg.Name)
		}
		var res roonBrowseResult
		_ = json.Unmarshal(msg.Body, &res)
		return res, nil
	}

	loadAll := func() ([]roonBrowseItem, error) {
		const pageSize = 100
		var all []roonBrowseItem
		offset := 0
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			msg, err := sess.Request(ctx, "com.roonlabs.browse:1/load", map[string]any{
				"hierarchy":         "genres",
				"multi_session_key": roonGenreSessionKey,
				"offset":            offset,
				"count":             pageSize,
			})
			if err != nil {
				return nil, err
			}
			if !isMOOSuccess(msg) {
				return nil, fmt.Errorf("roon load genres failed: %s", msg.Name)
			}
			var page roonLoadResult
			if err := json.Unmarshal(msg.Body, &page); err != nil {
				return nil, err
			}
			if len(page.Items) == 0 {
				break
			}
			if offset > 0 && len(all) > 0 && page.Items[0].ItemKey != "" && page.Items[0].ItemKey == all[0].ItemKey {
				break
			}
			all = append(all, page.Items...)
			offset += len(page.Items)
			if page.List != nil && page.List.Count > 0 {
				if offset >= page.List.Count {
					break
				}
				continue
			}
			if len(page.Items) < pageSize {
				break
			}
		}
		return all, nil
	}

	if _, err := browseGenres(map[string]any{"pop_all": true}); err != nil {
		return nil, err
	}
	genreItems, err := loadAll()
	if err != nil {
		return nil, err
	}

	type albumKey struct {
		album  string
		artist string
	}
	var out []RoonAlbumGenre
	idxByKey := make(map[albumKey]int)
	addAlbumGenre := func(album, artist, genre string) {
		album = strings.TrimSpace(album)
		artist = strings.TrimSpace(artist)
		genre = strings.TrimSpace(genre)
		if album == "" || genre == "" {
			return
		}
		k := albumKey{
			album:  strings.ToLower(album),
			artist: strings.ToLower(artist),
		}
		idx, ok := idxByKey[k]
		if !ok {
			idx = len(out)
			idxByKey[k] = idx
			out = append(out, RoonAlbumGenre{
				Album:  album,
				Artist: artist,
				Genres: []string{genre},
			})
			return
		}
		for _, existing := range out[idx].Genres {
			if strings.EqualFold(existing, genre) {
				return
			}
		}
		out[idx].Genres = append(out[idx].Genres, genre)
	}

	for _, g := range genreItems {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		genreName := strings.TrimSpace(g.Title)
		if genreName == "" || g.ItemKey == "" || isRoonNonAlbumRow(g, true) {
			continue
		}
		brRes, err := browseGenres(map[string]any{"item_key": g.ItemKey})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if brRes.Action != "" && brRes.Action != "list" {
			continue
		}

		children, err := loadAll()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_, _ = browseGenres(map[string]any{"pop_levels": 1})
			continue
		}

		if albumsKey := findRoonAlbumsBucket(children); albumsKey != "" {
			subRes, serr := browseGenres(map[string]any{"item_key": albumsKey})
			if serr == nil && (subRes.Action == "" || subRes.Action == "list") {
				if albumItems, lerr := loadAll(); lerr == nil {
					for _, it := range albumItems {
						if !isRoonNonAlbumRow(it, false) {
							addAlbumGenre(it.Title, it.Subtitle, genreName)
						}
					}
				} else if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				_, _ = browseGenres(map[string]any{"pop_levels": 1})
			} else if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		} else {
			for _, it := range children {
				if !isRoonNonAlbumRow(it, false) {
					addAlbumGenre(it.Title, it.Subtitle, genreName)
				}
			}
		}

		if _, perr := browseGenres(map[string]any{"pop_levels": 1}); perr != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	return out, nil
}

// --- SOOD v2 Discovery ---

func discoverRoonSOOD(timeout time.Duration) (addr, name string, err error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return "", "", err
	}
	defer conn.Close()

	var tidBytes [16]byte
	_, _ = rand.Read(tidBytes[:])
	pkt := buildSOODQuery(map[string]string{
		"query_service_id": roonRegistrySvcID,
		"_tid":             hex.EncodeToString(tidBytes[:]),
	})

	targets := []string{
		soodMulticastAddr,
		"127.0.0.1:9003",
		"255.255.255.255:9003",
	}
	for _, t := range targets {
		if ua, rerr := net.ResolveUDPAddr("udp4", t); rerr == nil {
			_, _ = conn.WriteToUDP(pkt, ua)
		}
	}

	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	localIPs := localInterfaceIPs()
	buf := make([]byte, 2048)
	for {
		n, from, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			return "", "", rerr
		}
		props, ok := parseSOODReply(buf[:n])
		if !ok {
			continue
		}
		if props["service_id"] != roonRegistrySvcID || strings.TrimSpace(props["http_port"]) == "" {
			continue
		}
		port := strings.TrimSpace(props["http_port"])
		if _, perr := strconv.Atoi(port); perr != nil {
			continue
		}
		host := from.IP.String()
		if from.IP.IsLoopback() || localIPs[host] {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, port), props["display_name"], nil
	}
}

func buildSOODQuery(props map[string]string) []byte {
	var b bytes.Buffer
	b.WriteString("SOOD\x02Q")
	for k, v := range props {
		b.WriteByte(byte(len(k)))
		b.WriteString(k)
		var vlen [2]byte
		binary.BigEndian.PutUint16(vlen[:], uint16(len(v)))
		b.Write(vlen[:])
		b.WriteString(v)
	}
	return b.Bytes()
}

func parseSOODReply(pkt []byte) (map[string]string, bool) {
	if len(pkt) < 6 || string(pkt[:6]) != "SOOD\x02R" {
		return nil, false
	}
	props := make(map[string]string)
	i := 6
	for i < len(pkt) {
		klen := int(pkt[i])
		i++
		if klen == 0 || i+klen+2 > len(pkt) {
			break
		}
		key := string(pkt[i : i+klen])
		i += klen
		vlen := int(binary.BigEndian.Uint16(pkt[i : i+2]))
		i += 2
		if vlen == 0xFFFF {
			continue
		}
		if i+vlen > len(pkt) {
			break
		}
		props[key] = string(pkt[i : i+vlen])
		i += vlen
	}
	return props, len(props) > 0
}

func localInterfaceIPs() map[string]bool {
	m := map[string]bool{"127.0.0.1": true, "::1": true}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return m
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP != nil {
			m[ipn.IP.String()] = true
		}
	}
	return m
}

// --- MOO/1 Framing over WebSocket ---

type mooMessage struct {
	Verb      string
	Name      string
	RequestID string
	Headers   map[string]string
	Body      []byte
}

type mooSession struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	reqSeq  atomic.Int64

	mu      sync.Mutex
	waiters map[string]chan mooMessage
	subs    map[string]func(mooMessage)
	onReq   func(*mooSession, mooMessage)
	doneCh  chan struct{}
	once    sync.Once
}

func newMOOSession(ws *websocket.Conn, onReq func(*mooSession, mooMessage)) *mooSession {
	s := &mooSession{
		ws:      ws,
		waiters: make(map[string]chan mooMessage),
		subs:    make(map[string]func(mooMessage)),
		onReq:   onReq,
		doneCh:  make(chan struct{}),
	}
	go s.readLoop()
	return s
}

func (s *mooSession) Close() {
	s.once.Do(func() {
		_ = s.ws.Close()
		close(s.doneCh)
	})
}

func (s *mooSession) sendFrame(verb, name, reqID string, payload any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "MOO/1 %s %s\nRequest-Id: %s\n", verb, name, reqID)
	if len(body) > 0 {
		fmt.Fprintf(&buf, "Content-Length: %d\nContent-Type: application/json\n", len(body))
	}
	buf.WriteByte('\n')
	if len(body) > 0 {
		buf.Write(body)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return websocket.Message.Send(s.ws, buf.Bytes())
}

func (s *mooSession) Reply(reqID, verb, name string, payload any) error {
	return s.sendFrame(verb, name, reqID, payload)
}

func (s *mooSession) Request(ctx context.Context, method string, payload any) (mooMessage, error) {
	reqID := strconv.FormatInt(s.reqSeq.Add(1), 10)
	ch := make(chan mooMessage, 1)
	s.mu.Lock()
	s.waiters[reqID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiters, reqID)
		s.mu.Unlock()
	}()

	if err := s.sendFrame("REQUEST", method, reqID, payload); err != nil {
		return mooMessage{}, err
	}
	select {
	case <-ctx.Done():
		return mooMessage{}, ctx.Err()
	case <-s.doneCh:
		return mooMessage{}, errors.New("roon: connection closed")
	case msg := <-ch:
		return msg, nil
	}
}

func (s *mooSession) Subscribe(method string, payload any, cb func(mooMessage)) {
	reqID := strconv.FormatInt(s.reqSeq.Add(1), 10)
	s.mu.Lock()
	s.subs[reqID] = cb
	s.mu.Unlock()
	_ = s.sendFrame("REQUEST", method, reqID, payload)
}

func (s *mooSession) readLoop() {
	defer s.Close()
	for {
		var raw []byte
		if err := websocket.Message.Receive(s.ws, &raw); err != nil {
			return
		}
		msg, ok := parseMOOFrame(raw)
		if !ok {
			continue
		}
		if msg.Verb == "REQUEST" {
			if s.onReq != nil {
				go s.onReq(s, msg)
			}
			continue
		}
		s.mu.Lock()
		waitCh := s.waiters[msg.RequestID]
		subFn := s.subs[msg.RequestID]
		if msg.Verb == "COMPLETE" && subFn != nil {
			delete(s.subs, msg.RequestID)
		}
		s.mu.Unlock()

		if waitCh != nil {
			select {
			case waitCh <- msg:
			default:
			}
		}
		if subFn != nil {
			subFn(msg)
		}
	}
}

func parseMOOFrame(raw []byte) (mooMessage, bool) {
	sep := bytes.Index(raw, []byte("\n\n"))
	headerEnd := sep
	bodyStart := sep + 2
	if sep < 0 {
		sep = bytes.Index(raw, []byte("\r\n\r\n"))
		if sep < 0 {
			return mooMessage{}, false
		}
		headerEnd = sep
		bodyStart = sep + 4
	}
	lines := strings.Split(strings.ReplaceAll(string(raw[:headerEnd]), "\r\n", "\n"), "\n")
	if len(lines) == 0 {
		return mooMessage{}, false
	}
	first := strings.SplitN(strings.TrimSpace(lines[0]), " ", 3)
	if len(first) < 3 || first[0] != "MOO/1" {
		return mooMessage{}, false
	}
	hdrs := make(map[string]string, len(lines)-1)
	for _, ln := range lines[1:] {
		k, v, ok := strings.Cut(ln, ":")
		if ok {
			hdrs[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return mooMessage{
		Verb:      first[1],
		Name:      first[2],
		RequestID: hdrs["Request-Id"],
		Headers:   hdrs,
		Body:      raw[bodyStart:],
	}, true
}

// --- RoonBackend & roonSink ---

// RoonBackend feeds YTB Music's decoded PCM / lossless ADTS-AAC stream into
// Server's continuous Live Radio broadcaster and coordinates playback with Roon Core.
type RoonBackend struct {
	srv    *Server
	mgr    *RoonManager
	mu     sync.RWMutex
	zoneID string
}

func NewRoonBackend(srv *Server, mgr *RoonManager, zoneID string) *RoonBackend {
	return &RoonBackend{
		srv:    srv,
		mgr:    mgr,
		zoneID: zoneID,
	}
}

func (b *RoonBackend) Name() string { return "roon" }

func (b *RoonBackend) SetZoneID(id string) {
	id = strings.TrimSpace(id)
	b.mu.Lock()
	changed := b.zoneID != id
	b.zoneID = id
	b.mu.Unlock()
	if changed && b.mgr != nil {
		b.mgr.UpdateZone(id)
	}
}

func (b *RoonBackend) ZoneID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.zoneID
}

func (b *RoonBackend) Open(ctx context.Context, src player.TrackSource) (player.OutputSink, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	dec, err := src.OpenPCM()
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		_ = dec.Close()
		return nil, ctx.Err()
	}
	sr := dec.SampleRate()
	if sr <= 0 {
		sr = roonTargetRate
	}
	title := src.Track.Name
	if ar := src.Track.ArtistNames(); ar != "" {
		title = ar + " - " + src.Track.Name
	}

	rs := &roonSink{
		backend:   b,
		src:       src,
		dec:       dec,
		srcRate:   sr,
		aacRate:   roonTargetRate,
		aacSilent: silentADTSFrame44kStereo,
		duration:  src.Duration,
		gain:      src.Gain,
		title:     title,
	}
	return rs, nil
}

type roonSink struct {
	backend  *RoonBackend
	src      player.TrackSource
	srcRate  int
	duration time.Duration

	// stateMu guards lightweight playback state accessed on the UI thread;
	// it is NEVER held across blocking decoder or network reads.
	stateMu   sync.Mutex
	ownerID   uint64
	posFrames int64 // in srcRate frames
	playGen   uint64
	seekGen   uint64
	seekTarget int64 // in srcRate frames
	playing   bool
	closed    bool
	gain      float64
	aacRate   int
	aacSilent []byte
	title     string
	onEnd     func()
	endedOnce bool

	// ioMu guards dec, raw, adts, and decode buffers on the background clock goroutine.
	ioMu           sync.Mutex
	dec            player.PCMDecoder
	raw            player.RawStream
	adts           *aacADTSReader
	adtsTried      bool
	appliedPCMSeek uint64
	appliedAACSeek uint64
	floatBuf       []float32
}

func (s *roonSink) Activate() {
	s.stateMu.Lock()
	if s.ownerID == 0 {
		s.ownerID = roonOwnerSeq.Add(1)
	}
	s.stateMu.Unlock()
	s.backend.srv.setActiveRoonSink(s)
}

func (s *roonSink) DirectTransport() bool { return false }

func (s *roonSink) Duration() time.Duration { return s.duration }

func (s *roonSink) Playing() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.playing && !s.closed
}

func (s *roonSink) Position() time.Duration {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.srcRate <= 0 {
		return 0
	}
	d := time.Duration(float64(s.posFrames) / float64(s.srcRate) * float64(time.Second))
	if s.duration > 0 && d > s.duration {
		d = s.duration
	}
	return d
}

func (s *roonSink) Play() {
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return
	}
	if s.ownerID == 0 {
		s.ownerID = roonOwnerSeq.Add(1)
	}
	oid := s.ownerID
	s.playing = true
	s.playGen++
	s.endedOnce = false
	s.stateMu.Unlock()

	if s.backend.mgr != nil {
		s.backend.mgr.SetPlaybackIntent(oid, true, s.backend.ZoneID())
	}
}

func (s *roonSink) Pause() {
	s.stateMu.Lock()
	oid := s.ownerID
	s.playing = false
	s.playGen++
	s.stateMu.Unlock()

	if s.backend.mgr != nil && oid != 0 {
		s.backend.mgr.SetPlaybackIntent(oid, false, "")
	}
}

func (s *roonSink) Seek(t time.Duration) {
	if t < 0 {
		t = 0
	}
	s.stateMu.Lock()
	if !s.closed {
		target := int64(t.Seconds() * float64(s.srcRate))
		s.posFrames = target
		s.seekTarget = target
		s.seekGen++
		s.playGen++
		s.endedOnce = false
	}
	s.stateMu.Unlock()
}

func (s *roonSink) SetSinkVolume(v float64) {
	s.stateMu.Lock()
	s.gain = v
	s.stateMu.Unlock()
}

func (s *roonSink) OnSinkEnd(fn func()) {
	s.stateMu.Lock()
	s.onEnd = fn
	s.stateMu.Unlock()
}

func (s *roonSink) streamTitle() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.playing {
		return "YTB Music (已暂停)"
	}
	return s.title
}

func (s *roonSink) aacFormat() (int, []byte) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.aacRate > 0 && len(s.aacSilent) > 0 {
		return s.aacRate, s.aacSilent
	}
	return roonTargetRate, silentADTSFrame44kStereo
}

// pullPCM24 accumulates a full 20ms tick across partial MP4/AAC packet reads
// without holding stateMu during decoder I/O.
func (s *roonSink) pullPCM24(outFrames int) []byte {
	s.stateMu.Lock()
	if s.closed || !s.playing {
		s.stateMu.Unlock()
		return nil
	}
	wantPlayGen := s.playGen
	wantSeekGen := s.seekGen
	seekTarget := s.seekTarget
	gain := s.gain
	s.stateMu.Unlock()

	s.ioMu.Lock()
	dec := s.dec
	if dec == nil {
		s.ioMu.Unlock()
		return nil
	}
	if s.appliedPCMSeek != wantSeekGen {
		_ = dec.SetPosition(seekTarget)
		s.appliedPCMSeek = wantSeekGen
	}

	needSrc := outFrames
	if s.srcRate != roonTargetRate {
		needSrc = max(1, int(int64(outFrames)*int64(s.srcRate)/roonTargetRate))
	}
	if cap(s.floatBuf) < needSrc*2 {
		s.floatBuf = make([]float32, needSrc*2)
	}
	buf := s.floatBuf[:needSrc*2]

	filledFloats := 0
	var readErr error
	for filledFloats < len(buf) {
		n, err := dec.Read(buf[filledFloats:])
		filledFloats += n
		if err != nil {
			readErr = err
			break
		}
		if n == 0 {
			readErr = io.EOF
			break
		}
	}
	gotSrcFrames := filledFloats / 2
	if filledFloats < len(buf) {
		clear(buf[filledFloats:])
	}
	var samples []float32
	if gotSrcFrames > 0 {
		if s.srcRate == roonTargetRate {
			samples = buf
		} else {
			samples = resampleLinearStereo(buf, needSrc, outFrames)
		}
	}
	var out []byte
	if len(samples) > 0 {
		out = make([]byte, (len(samples)/2)*6)
		encodePCM24LE(out, samples, gain)
	}
	s.ioMu.Unlock()

	var fireEnd func()
	var clearOwner uint64
	s.stateMu.Lock()
	valid := !s.closed && s.playing && s.playGen == wantPlayGen && s.seekGen == wantSeekGen
	if !valid {
		out = nil
		// If paused mid-read without an explicit seek, resync decoder cursor back to posFrames before next resume!
		if !s.closed && s.seekGen == wantSeekGen && gotSrcFrames > 0 {
			s.seekTarget = s.posFrames
			s.seekGen++
		}
	} else {
		s.posFrames += int64(gotSrcFrames)
		if errors.Is(readErr, io.EOF) && !s.endedOnce {
			s.endedOnce = true
			s.playing = false
			clearOwner = s.ownerID
			fireEnd = s.onEnd
		}
	}
	s.stateMu.Unlock()

	if clearOwner != 0 && s.backend.mgr != nil {
		s.backend.mgr.SetPlaybackIntent(clearOwner, false, "")
	}
	if fireEnd != nil {
		go fireEnd()
	}
	return out
}

func (s *roonSink) pullADTS(advancePos bool) []byte {
	s.stateMu.Lock()
	if s.closed || !s.playing {
		s.stateMu.Unlock()
		return nil
	}
	wantPlayGen := s.playGen
	wantSeekGen := s.seekGen
	seekTarget := s.seekTarget
	curFrames := s.posFrames
	gain := s.gain
	s.stateMu.Unlock()

	s.ioMu.Lock()
	if !s.adtsTried && s.src.OpenRaw != nil {
		s.adtsTried = true
		if raw, err := s.src.OpenRaw(); err == nil {
			if sz, serr := raw.Size(); serr == nil && sz > 0 {
				if ar, aerr := openAACADTSReader(raw, sz); aerr == nil {
					initSample := curFrames
					if s.srcRate > 0 && ar.SampleRate() != s.srcRate {
						initSample = curFrames * int64(ar.SampleRate()) / int64(s.srcRate)
					}
					_ = ar.SeekToSample(initSample)
					s.raw = raw
					s.adts = ar
					s.appliedAACSeek = wantSeekGen
					sf := ar.SilentFrame()
					s.stateMu.Lock()
					s.aacRate = ar.SampleRate()
					s.aacSilent = sf
					s.stateMu.Unlock()
				} else {
					_ = raw.Close()
				}
			} else {
				_ = raw.Close()
			}
		}
	}
	ar := s.adts
	if ar == nil {
		s.ioMu.Unlock()
		return nil
	}
	if s.appliedAACSeek != wantSeekGen {
		sample := seekTarget
		if s.srcRate > 0 && ar.SampleRate() != s.srcRate {
			sample = seekTarget * int64(ar.SampleRate()) / int64(s.srcRate)
		}
		_ = ar.SeekToSample(sample)
		s.appliedAACSeek = wantSeekGen
	}

	pkt, err := ar.NextFrame()
	if len(pkt) > 0 && gain <= 0.0001 {
		pkt = ar.SilentFrame()
	}
	arRate := ar.SampleRate()
	s.ioMu.Unlock()

	var fireEnd func()
	var clearOwner uint64
	s.stateMu.Lock()
	valid := !s.closed && s.playing && s.playGen == wantPlayGen && s.seekGen == wantSeekGen
	if !valid {
		pkt = nil
		if !s.closed && s.seekGen == wantSeekGen {
			s.seekTarget = s.posFrames
			s.seekGen++
		}
	} else {
		if len(pkt) > 0 && advancePos {
			step := int64(1024)
			if arRate > 0 && s.srcRate != arRate {
				step = int64(1024) * int64(s.srcRate) / int64(arRate)
			}
			s.posFrames += step
		}
		if errors.Is(err, io.EOF) && advancePos && !s.endedOnce {
			s.endedOnce = true
			s.playing = false
			clearOwner = s.ownerID
			fireEnd = s.onEnd
		}
	}
	s.stateMu.Unlock()

	if clearOwner != 0 && s.backend.mgr != nil {
		s.backend.mgr.SetPlaybackIntent(clearOwner, false, "")
	}
	if fireEnd != nil {
		go fireEnd()
	}
	return pkt
}

func resampleLinearStereo(in []float32, inFrames, outFrames int) []float32 {
	if inFrames <= 0 || outFrames <= 0 {
		return nil
	}
	out := make([]float32, outFrames*2)
	scale := float64(inFrames-1) / float64(max(1, outFrames-1))
	for i := range outFrames {
		pos := float64(i) * scale
		idx := int(pos)
		frac := float32(pos - float64(idx))
		if idx >= inFrames-1 {
			out[i*2] = in[(inFrames-1)*2]
			out[i*2+1] = in[(inFrames-1)*2+1]
			continue
		}
		out[i*2] = in[idx*2]*(1-frac) + in[(idx+1)*2]*frac
		out[i*2+1] = in[idx*2+1]*(1-frac) + in[(idx+1)*2+1]*frac
	}
	return out
}

func (s *roonSink) Close() {
	s.backend.srv.clearActiveRoonSink(s)
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return
	}
	oid := s.ownerID
	s.closed = true
	s.playing = false
	s.playGen++
	s.stateMu.Unlock()

	if oid != 0 && s.backend.mgr != nil {
		s.backend.mgr.SetPlaybackIntent(oid, false, "")
	}

	go func() {
		s.ioMu.Lock()
		dec := s.dec
		raw := s.raw
		s.dec = nil
		s.raw = nil
		s.adts = nil
		s.ioMu.Unlock()
		if dec != nil {
			_ = dec.Close()
		}
		if raw != nil {
			_ = raw.Close()
		}
	}()
}
