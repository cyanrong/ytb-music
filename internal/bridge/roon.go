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

type RoonCallbacks struct {
	GetToken  func(coreID string) string
	SaveToken func(coreID, token string)
	OnChange  func(RoonStatus)
}

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
	wantPlaying   bool
	wantZoneID    string
	startedZoneID string

	stopOnce sync.Once
	stopCh   chan struct{}
	wakeCh   chan struct{}
}

func NewRoonManager(srv *Server, cb RoonCallbacks) *RoonManager {
	m := &RoonManager{
		srv:      srv,
		cb:       cb,
		zonesMap: make(map[string]RoonZone),
		stopCh:   make(chan struct{}),
		wakeCh:   make(chan struct{}, 1),
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
		sess := m.moo
		m.moo = nil
		m.mu.Unlock()
		if sess != nil {
			sess.Close()
		}
	})
}

// SetPlaybackIntent records whether YTB Music wants Roon's selected zone to be
// playing our Live Radio station, retrying automatically when pairing/zones arrive.
func (m *RoonManager) SetPlaybackIntent(wantPlay bool, zoneID string) {
	m.mu.Lock()
	zoneChanged := zoneID != "" && zoneID != m.wantZoneID
	m.wantPlaying = wantPlay
	if zoneID != "" {
		m.wantZoneID = zoneID
	}
	if !wantPlay {
		m.mu.Unlock()
		return
	}
	if zoneChanged {
		m.startedZoneID = ""
	}
	zid := m.wantZoneID
	m.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = m.EnsureStationPlaying(ctx, zid)
	}()
}

func (m *RoonManager) maybeRetryPendingPlay() {
	m.mu.RLock()
	want := m.wantPlaying
	zid := m.wantZoneID
	paired := m.status.Paired
	hasZones := len(m.zonesMap) > 0
	m.mu.RUnlock()
	if !want || !paired || !hasZones {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = m.EnsureStationPlaying(ctx, zid)
	}()
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
		}
		for _, id := range payload.ZonesRemoved {
			delete(m.zonesMap, id)
			if m.startedZoneID == id {
				m.startedZoneID = ""
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

// EnsureStationPlaying locates the "YTB Music" station in Roon's My Live Radio
// hierarchy and starts playback on the resolved target zone when needed.
func (m *RoonManager) EnsureStationPlaying(ctx context.Context, zoneID string) error {
	m.mu.RLock()
	sess := m.moo
	paired := m.status.Paired
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
	alreadyStarted := m.startedZoneID == zoneID && zoneKnown && zoneInfo.State == "playing" && m.srv != nil && m.srv.ActiveListeners() > 0
	m.mu.RUnlock()

	if alreadyStarted || sess == nil || !paired || zoneID == "" {
		return nil
	}

	_, err := sess.Request(ctx, "com.roonlabs.browse:1/browse", map[string]any{
		"hierarchy":         "internet_radio",
		"pop_all":           true,
		"zone_or_output_id": zoneID,
	})
	if err != nil {
		return err
	}
	loadMsg, err := sess.Request(ctx, "com.roonlabs.browse:1/load", map[string]any{
		"hierarchy": "internet_radio",
		"offset":    0,
		"count":     100,
	})
	if err != nil {
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
	if stationKey == "" {
		return nil
	}

	brMsg, err := sess.Request(ctx, "com.roonlabs.browse:1/browse", map[string]any{
		"hierarchy":         "internet_radio",
		"item_key":          stationKey,
		"zone_or_output_id": zoneID,
	})
	if err != nil {
		return err
	}
	var brRes struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(brMsg.Body, &brRes)
	if brRes.Action == "list" {
		actLoad, err := sess.Request(ctx, "com.roonlabs.browse:1/load", map[string]any{
			"hierarchy": "internet_radio",
			"offset":    0,
			"count":     20,
		})
		if err == nil {
			var acts struct {
				Items []struct {
					Title   string `json:"title"`
					ItemKey string `json:"item_key"`
				} `json:"items"`
			}
			if json.Unmarshal(actLoad.Body, &acts) == nil && len(acts.Items) > 0 {
				playKey := acts.Items[0].ItemKey
				for _, act := range acts.Items {
					al := strings.ToLower(act.Title)
					if strings.Contains(al, "play") || strings.Contains(al, "播放") {
						playKey = act.ItemKey
						break
					}
				}
				if _, err := sess.Request(ctx, "com.roonlabs.browse:1/browse", map[string]any{
					"hierarchy":         "internet_radio",
					"item_key":          playKey,
					"zone_or_output_id": zoneID,
				}); err != nil {
					return err
				}
			}
		}
	}

	m.mu.Lock()
	if m.wantPlaying {
		m.startedZoneID = zoneID
	}
	m.mu.Unlock()
	return nil
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
		// If currently playing on Roon, route playback to the newly selected zone.
		b.srv.hubMu.Lock()
		rs := b.srv.activeSink
		b.srv.hubMu.Unlock()
		if rs != nil && rs.Playing() {
			b.mgr.SetPlaybackIntent(true, id)
		}
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
		backend:  b,
		src:      src,
		dec:      dec,
		srcRate:  sr,
		aacRate:  sr,
		duration: src.Duration,
		gain:     src.Gain,
		title:    title,
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
	stateMu        sync.Mutex
	posFrames      int64 // in srcRate frames
	seekGen        uint64
	seekTarget     int64 // in srcRate frames
	playing        bool
	closed         bool
	gain           float64
	aacRate        int
	title          string
	onEnd          func()
	endedOnce      bool

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
	s.playing = true
	s.endedOnce = false
	s.stateMu.Unlock()

	if s.backend.mgr != nil {
		s.backend.mgr.SetPlaybackIntent(true, s.backend.ZoneID())
	}
}

func (s *roonSink) Pause() {
	s.stateMu.Lock()
	s.playing = false
	s.stateMu.Unlock()

	if s.backend.mgr != nil {
		s.backend.mgr.SetPlaybackIntent(false, "")
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

func (s *roonSink) aacSampleRate() int {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.aacRate > 0 {
		return s.aacRate
	}
	return roonTargetRate
}

// pullPCM24 accumulates a full 20ms tick across partial MP4/AAC packet reads
// without holding stateMu during decoder I/O.
func (s *roonSink) pullPCM24(outFrames int) []byte {
	s.stateMu.Lock()
	if s.closed || !s.playing {
		s.stateMu.Unlock()
		return nil
	}
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

	// Loop across decoder reads so packet boundaries never produce short ticks.
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
			samples = buf // full outFrames*2 with zero-padded tail on EOF
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
	s.stateMu.Lock()
	if !s.closed {
		if s.seekGen == wantSeekGen {
			s.posFrames += int64(gotSrcFrames)
		}
		if errors.Is(readErr, io.EOF) && s.seekGen == wantSeekGen && !s.endedOnce {
			s.endedOnce = true
			s.playing = false
			fireEnd = s.onEnd
		}
	}
	s.stateMu.Unlock()

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
					// Align newly opened ADTS cursor with current track position!
					initSample := curFrames
					if s.srcRate > 0 && ar.SampleRate() != s.srcRate {
						initSample = curFrames * int64(ar.SampleRate()) / int64(s.srcRate)
					}
					_ = ar.SeekToSample(initSample)
					s.raw = raw
					s.adts = ar
					s.appliedAACSeek = wantSeekGen
					s.stateMu.Lock()
					s.aacRate = ar.SampleRate()
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
		// Honor mute on passthrough AAC stream while keeping packet cursor aligned.
		pkt = ar.SilentFrame()
	}
	arRate := ar.SampleRate()
	s.ioMu.Unlock()

	var fireEnd func()
	s.stateMu.Lock()
	if !s.closed {
		if len(pkt) > 0 && advancePos && s.seekGen == wantSeekGen {
			step := int64(1024)
			if arRate > 0 && s.srcRate != arRate {
				step = int64(1024) * int64(s.srcRate) / int64(arRate)
			}
			s.posFrames += step
		}
		if errors.Is(err, io.EOF) && advancePos && s.seekGen == wantSeekGen && !s.endedOnce {
			s.endedOnce = true
			s.playing = false
			fireEnd = s.onEnd
		}
	}
	s.stateMu.Unlock()

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
	s.closed = true
	s.playing = false
	s.stateMu.Unlock()

	if s.backend.mgr != nil {
		s.backend.mgr.SetPlaybackIntent(false, "")
	}

	// Release decoders off the UI thread so Close() never blocks behind stalled I/O.
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
