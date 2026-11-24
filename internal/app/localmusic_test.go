package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"ytb-music/internal/bridge"
	"ytb-music/internal/localmusic"
	"ytb-music/internal/player"
	"ytb-music/internal/ytm"
)

type stubLocalOutputSink struct {
	mu      sync.Mutex
	playing bool
	pos     time.Duration
	dur     time.Duration
}

func (s *stubLocalOutputSink) Activate() {}
func (s *stubLocalOutputSink) Play() {
	s.mu.Lock()
	s.playing = true
	s.mu.Unlock()
}
func (s *stubLocalOutputSink) Pause() {
	s.mu.Lock()
	s.playing = false
	s.mu.Unlock()
}
func (s *stubLocalOutputSink) Playing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.playing
}
func (s *stubLocalOutputSink) Seek(t time.Duration) {
	s.mu.Lock()
	s.pos = t
	s.mu.Unlock()
}
func (s *stubLocalOutputSink) Position() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pos
}
func (s *stubLocalOutputSink) Duration() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dur
}
func (s *stubLocalOutputSink) SetSinkVolume(float64)  {}
func (s *stubLocalOutputSink) OnSinkEnd(func())       {}
func (s *stubLocalOutputSink) DirectTransport() bool  { return true }
func (s *stubLocalOutputSink) Close()                 {}

type stubLocalOutputBackend struct {
	mu      sync.Mutex
	lastSrc player.TrackSource
	opens   int
}

func (b *stubLocalOutputBackend) Name() string { return "stub-local" }

func (b *stubLocalOutputBackend) Open(_ context.Context, src player.TrackSource) (player.OutputSink, error) {
	// Exercise OpenPCM so the local pure-Go decoder is verified end-to-end.
	if src.OpenPCM != nil {
		dec, err := src.OpenPCM()
		if err != nil {
			return nil, err
		}
		buf := make([]float32, 64)
		_, _ = dec.Read(buf)
		_ = dec.Close()
	}
	b.mu.Lock()
	b.lastSrc = src
	b.opens++
	b.mu.Unlock()
	return &stubLocalOutputSink{dur: src.Duration}, nil
}

func (b *stubLocalOutputBackend) snapshot() (player.TrackSource, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSrc, b.opens
}

func newLocalTestApp(t *testing.T) (*app, chan func(), *stubLocalOutputBackend) {
	t.Helper()
	tmp := t.TempDir()
	posts := make(chan func(), 256)
	a := newTestApp()
	a.settings = defaultSettings()
	a.settings.path = filepath.Join(tmp, "settings.json")
	a.localPath = filepath.Join(tmp, "local_playlists.json")
	a.postHook = func(fn func()) {
		select {
		case posts <- fn:
		default:
		}
	}
	a.art = newArtCache(a.post)
	a.pl = player.New(a.post)
	a.pl.SetVolume(0.6)
	be := &stubLocalOutputBackend{}
	a.pl.SetBackend(be)

	lib := localmusic.NewLibrary(filepath.Join(tmp, "local_library.json"), filepath.Join(tmp, "local_covers"))
	a.setLocalLibrary(lib)
	t.Cleanup(func() {
		a.closeLocalLibrary()
		a.pl.Stop()
		setLocalCoverLoader(nil)
	})
	return a, posts, be
}

func drainLocalPosts(t *testing.T, posts chan func(), tt *ui.Tester, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		for len(posts) > 0 {
			fn := <-posts
			fn()
		}
		if tt != nil {
			tt.Frame()
		}
		if cond() {
			for len(posts) > 0 {
				(<-posts)()
			}
			if tt != nil {
				tt.Frame()
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for async UI condition")
		}
		select {
		case fn := <-posts:
			fn()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func makeCoverPNG(c1, c2, accent color.RGBA) []byte {
	const sz = 160
	img := image.NewRGBA(image.Rect(0, 0, sz, sz))
	cx, cy := float64(sz)/2, float64(sz)/2
	for y := range sz {
		for x := range sz {
			t := (float64(x)*0.65 + float64(y)*0.35) / float64(sz)
			r := uint8(float64(c1.R)*(1-t) + float64(c2.R)*t)
			g := uint8(float64(c1.G)*(1-t) + float64(c2.G)*t)
			b := uint8(float64(c1.B)*(1-t) + float64(c2.B)*t)
			dx, dy := float64(x)-cx, float64(y)-cy
			dist := math.Hypot(dx, dy)
			if dist > 26 && dist < 58 {
				ring := 0.5 + 0.5*math.Sin(dist*0.55)
				mix := 0.28 * ring
				r = uint8(float64(r)*(1-mix) + float64(accent.R)*mix)
				g = uint8(float64(g)*(1-mix) + float64(accent.G)*mix)
				b = uint8(float64(b)*(1-mix) + float64(accent.B)*mix)
			} else if dist <= 13 {
				r, g, b = accent.R, accent.G, accent.B
			}
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func makeID3v23(title, artist, album, year, genre string) []byte {
	var body bytes.Buffer
	addText := func(id, val string) {
		if val == "" {
			return
		}
		payload := append([]byte{3}, []byte(val)...) // UTF-8
		body.WriteString(id[:4])
		_ = binary.Write(&body, binary.BigEndian, uint32(len(payload)))
		body.Write([]byte{0, 0})
		body.Write(payload)
	}
	addText("TIT2", title)
	addText("TPE1", artist)
	addText("TPE2", artist)
	addText("TALB", album)
	addText("TYER", year)
	addText("TCON", genre)

	n := body.Len()
	var hdr [10]byte
	copy(hdr[0:3], "ID3")
	hdr[3] = 3
	hdr[6] = byte((n >> 21) & 0x7F)
	hdr[7] = byte((n >> 14) & 0x7F)
	hdr[8] = byte((n >> 7) & 0x7F)
	hdr[9] = byte(n & 0x7F)
	return append(hdr[:], body.Bytes()...)
}

func makeWAV24(sr, frames int, id3 []byte) []byte {
	const ch = 2
	pcm := make([]byte, frames*ch*3)
	for i := range frames {
		v := int32(math.Round(0.35 * float64((1<<23)-1) * math.Sin(2*math.Pi*440*float64(i)/float64(sr))))
		offL := (i * 2) * 3
		offR := (i*2 + 1) * 3
		pcm[offL], pcm[offL+1], pcm[offL+2] = byte(v), byte(v>>8), byte(v>>16)
		nv := -v
		pcm[offR], pcm[offR+1], pcm[offR+2] = byte(nv), byte(nv>>8), byte(nv>>16)
	}

	var chunks bytes.Buffer
	var fmtChunk [40]byte
	blockAlign := ch * 3
	binary.LittleEndian.PutUint16(fmtChunk[0:2], 0xFFFE)
	binary.LittleEndian.PutUint16(fmtChunk[2:4], ch)
	binary.LittleEndian.PutUint32(fmtChunk[4:8], uint32(sr))
	binary.LittleEndian.PutUint32(fmtChunk[8:12], uint32(sr*blockAlign))
	binary.LittleEndian.PutUint16(fmtChunk[12:14], uint16(blockAlign))
	binary.LittleEndian.PutUint16(fmtChunk[14:16], 24)
	binary.LittleEndian.PutUint16(fmtChunk[16:18], 22)
	binary.LittleEndian.PutUint16(fmtChunk[18:20], 24)
	binary.LittleEndian.PutUint32(fmtChunk[20:24], 0x03)
	binary.LittleEndian.PutUint16(fmtChunk[24:26], 0x0001)
	copy(fmtChunk[28:40], []byte{0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})

	chunks.WriteString("fmt ")
	_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(fmtChunk)))
	chunks.Write(fmtChunk[:])
	if len(id3) > 0 {
		chunks.WriteString("id3 ")
		_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(id3)))
		chunks.Write(id3)
		if len(id3)%2 != 0 {
			chunks.WriteByte(0)
		}
	}
	chunks.WriteString("data")
	_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(pcm)))
	chunks.Write(pcm)
	if len(pcm)%2 != 0 {
		chunks.WriteByte(0)
	}

	var riff bytes.Buffer
	riff.WriteString("RIFF")
	_ = binary.Write(&riff, binary.LittleEndian, uint32(4+chunks.Len()))
	riff.WriteString("WAVE")
	riff.Write(chunks.Bytes())
	return riff.Bytes()
}

func makeWAV16(sr, frames int, id3 []byte) []byte {
	const ch = 2
	pcm := make([]byte, frames*ch*2)
	for i := range frames {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(sr)))
		binary.LittleEndian.PutUint16(pcm[(i*2)*2:], uint16(v))
		binary.LittleEndian.PutUint16(pcm[(i*2+1)*2:], uint16(-v))
	}
	var chunks bytes.Buffer
	var fmtChunk [16]byte
	blockAlign := ch * 2
	binary.LittleEndian.PutUint16(fmtChunk[0:2], 1)
	binary.LittleEndian.PutUint16(fmtChunk[2:4], ch)
	binary.LittleEndian.PutUint32(fmtChunk[4:8], uint32(sr))
	binary.LittleEndian.PutUint32(fmtChunk[8:12], uint32(sr*blockAlign))
	binary.LittleEndian.PutUint16(fmtChunk[12:14], uint16(blockAlign))
	binary.LittleEndian.PutUint16(fmtChunk[14:16], 16)
	chunks.WriteString("fmt ")
	_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(fmtChunk)))
	chunks.Write(fmtChunk[:])
	if len(id3) > 0 {
		chunks.WriteString("id3 ")
		_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(id3)))
		chunks.Write(id3)
		if len(id3)%2 != 0 {
			chunks.WriteByte(0)
		}
	}
	chunks.WriteString("data")
	_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(pcm)))
	chunks.Write(pcm)

	var riff bytes.Buffer
	riff.WriteString("RIFF")
	_ = binary.Write(&riff, binary.LittleEndian, uint32(4+chunks.Len()))
	riff.WriteString("WAVE")
	riff.Write(chunks.Bytes())
	return riff.Bytes()
}

func makeDSF(dsdRate, numBlocks int, id3 []byte) []byte {
	const (
		channels  = 2
		blockSize = 4096
	)
	totalSamplesPerCh := uint64(numBlocks * blockSize * 8)
	dataPayload := bytes.Repeat([]byte{0x69}, numBlocks*blockSize*channels)
	dataChunkSize := uint64(12 + len(dataPayload))
	var metaOff uint64
	if len(id3) > 0 {
		metaOff = 28 + 52 + dataChunkSize
	}
	totalFileSize := uint64(28+52) + dataChunkSize + uint64(len(id3))

	var out bytes.Buffer
	out.WriteString("DSD ")
	_ = binary.Write(&out, binary.LittleEndian, uint64(28))
	_ = binary.Write(&out, binary.LittleEndian, totalFileSize)
	_ = binary.Write(&out, binary.LittleEndian, metaOff)

	out.WriteString("fmt ")
	_ = binary.Write(&out, binary.LittleEndian, uint64(52))
	_ = binary.Write(&out, binary.LittleEndian, uint32(1))
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))
	_ = binary.Write(&out, binary.LittleEndian, uint32(2))
	_ = binary.Write(&out, binary.LittleEndian, uint32(channels))
	_ = binary.Write(&out, binary.LittleEndian, uint32(dsdRate))
	_ = binary.Write(&out, binary.LittleEndian, uint32(1))
	_ = binary.Write(&out, binary.LittleEndian, totalSamplesPerCh)
	_ = binary.Write(&out, binary.LittleEndian, uint32(blockSize))
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))

	out.WriteString("data")
	_ = binary.Write(&out, binary.LittleEndian, dataChunkSize)
	out.Write(dataPayload)
	if len(id3) > 0 {
		out.Write(id3)
	}
	return out.Bytes()
}

func writeSampleMusicFolder(t *testing.T) string {
	t.Helper()
	musicRoot := filepath.Join(t.TempDir(), "AudiophileMusic")

	// 1. DSD128 SACD album with .lrc lyrics and folder cover.
	milesDir := filepath.Join(musicRoot, "Miles Davis - Kind of Blue SACD")
	if err := os.MkdirAll(milesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dsfID3 := makeID3v23("So What (DSD128)", "Miles Davis", "Kind of Blue SACD", "1959", "Jazz")
	if err := os.WriteFile(filepath.Join(milesDir, "01 - So What.dsf"), makeDSF(5644800, 2, dsfID3), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(milesDir, "01 - So What.lrc"), []byte("[00:01.00]Modal trumpet intro\n[00:05.00]Walking bassline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(milesDir, "cover.png"), makeCoverPNG(color.RGBA{18, 42, 86, 255}, color.RGBA{9, 18, 38, 255}, color.RGBA{245, 213, 138, 255}), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Hi-Res 24/96 WAV album.
	evansDir := filepath.Join(musicRoot, "Bill Evans Trio - Waltz for Debby")
	if err := os.MkdirAll(evansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wavID3 := makeID3v23("Waltz for Debby", "Bill Evans Trio", "Waltz for Debby", "1961", "Post-Bop")
	if err := os.WriteFile(filepath.Join(evansDir, "01 - Waltz for Debby.wav"), makeWAV24(96000, 4800, wavID3), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evansDir, "cover.png"), makeCoverPNG(color.RGBA{72, 34, 52, 255}, color.RGBA{24, 14, 24, 255}, color.RGBA{235, 175, 110, 255}), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Redbook 16/44.1 WAV + CUE sheet album.
	postRockDir := filepath.Join(musicRoot, "Sigur Ros - Agaetis byrjun")
	if err := os.MkdirAll(postRockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(postRockDir, "disc.wav"), makeWAV16(44100, 44100*4, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	cueContent := `REM GENRE "Post-Rock"
REM DATE 1999
PERFORMER "Sigur Rós"
TITLE "Ágætis byrjun"
FILE "disc.wav" WAVE
  TRACK 01 AUDIO
    TITLE "Svefn-g-englar"
    PERFORMER "Sigur Rós"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Starálfur"
    PERFORMER "Sigur Rós"
    INDEX 01 00:02:00
`
	if err := os.WriteFile(filepath.Join(postRockDir, "disc.cue"), []byte(cueContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(postRockDir, "cover.png"), makeCoverPNG(color.RGBA{20, 62, 58, 255}, color.RGBA{12, 26, 30, 255}, color.RGBA{132, 224, 198, 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	return musicRoot
}

// Navigating to Local Music, adding a watched folder, filtering by DSD/Hi-Res and genre, opening a local album, and clicking a track starts playback through Player.Local.
func TestLocalLibraryBrowseFilterAndPlayback(t *testing.T) {
	musicRoot := writeSampleMusicFolder(t)

	origPick := pickMusicFolders
	pickMusicFolders = func(*mygo.Window) ([]string, error) { return []string{musicRoot}, nil }
	defer func() { pickMusicFolders = origPick }()

	var revealedPath string
	origReveal := showInFolder
	showInFolder = func(p string) { revealedPath = p }
	defer func() { showInFolder = origReveal }()

	origSearch := searchResults
	searchResults = func(string, ytm.SearchType, int, int) (ytm.SearchResult, error) {
		return ytm.SearchResult{}, errors.New("offline")
	}
	defer func() { searchResults = origSearch }()

	a, posts, be := newLocalTestApp(t)
	tt := ui.NewTester(a.view, 1180, 780)
	tt.SetDark(true)
	tt.Frame()

	if err := tt.Click("本地音乐"); err != nil {
		t.Fatalf("click sidebar 本地音乐: %v", err)
	}
	tt.Frame()
	if !tt.HasText("选择音乐文件夹…") {
		t.Fatal("expected onboarding empty state before folders are added")
	}

	if err := tt.Click("+ 添加文件夹"); err != nil {
		t.Fatalf("click + 添加文件夹: %v", err)
	}
	drainLocalPosts(t, posts, tt, func() bool {
		return !a.local.picking && !a.local.scanning && a.local.lib.Stats().Albums == 3
	})

	if !tt.HasText("Kind of Blue SACD") || !tt.HasText("Waltz for Debby") || !tt.HasText("Ágætis byrjun") {
		t.Fatalf("expected all 3 indexed albums on Album Wall, texts=%v", tt.Texts())
	}

	// Filter by DSD focus chip.
	if err := tt.Click("DSD"); err != nil {
		t.Fatalf("click DSD focus chip: %v", err)
	}
	tt.Frame()
	if !tt.HasText("Kind of Blue SACD") || tt.HasText("Waltz for Debby") || tt.HasText("Ágætis byrjun") {
		t.Fatalf("DSD focus did not isolate DSD album: %v", tt.Texts())
	}

	// Filter by Hi-Res focus chip (includes DSD128 + 24/96 WAV, excludes 16/44.1 CUE).
	if err := tt.Click("Hi-Res"); err != nil {
		t.Fatalf("click Hi-Res focus chip: %v", err)
	}
	tt.Frame()
	if !tt.HasText("Kind of Blue SACD") || !tt.HasText("Waltz for Debby") || tt.HasText("Ágætis byrjun") {
		t.Fatalf("Hi-Res focus did not filter out 16/44.1 album: %v", tt.Texts())
	}

	if err := tt.Click("全部"); err != nil {
		t.Fatalf("click 全部 focus chip: %v", err)
	}
	tt.Frame()

	// Switch to Genres lens and select "Jazz".
	if err := tt.Click("流派"); err != nil {
		t.Fatalf("click 流派 tab: %v", err)
	}
	tt.Frame()
	if err := tt.Click("Jazz"); err != nil {
		t.Fatalf("click Jazz genre card: %v", err)
	}
	tt.Frame()
	if a.local.tab != localTabAlbums || a.local.genre != "Jazz" || !tt.HasText("流派: Jazz ×") {
		t.Fatalf("genre click did not filter Album Wall by Jazz: tab=%d genre=%q", a.local.tab, a.local.genre)
	}
	if !tt.HasText("Kind of Blue SACD") || tt.HasText("Waltz for Debby") {
		t.Fatalf("Jazz genre filter showed unexpected albums: %v", tt.Texts())
	}

	// Open the DSD album detail page and start playback.
	if err := tt.Click("Kind of Blue SACD"); err != nil {
		t.Fatalf("click album card Kind of Blue SACD: %v", err)
	}
	tt.Frame()
	if a.route.page != pageAlbum || !localmusic.IsLocalAlbum(a.route.id) {
		t.Fatalf("expected local album route, got %+v", a.route)
	}
	if !tt.HasText("DSD128 · 5.64MHz / 1bit") {
		t.Fatalf("expected DSD128 technical spec pill on local album page: %v", tt.Texts())
	}
	if err := tt.Click("在文件夹中显示"); err != nil {
		t.Fatalf("click 在文件夹中显示 on album header: %v", err)
	}
	if !strings.HasSuffix(revealedPath, "Miles Davis - Kind of Blue SACD") {
		t.Fatalf("unexpected revealed album folder: %q", revealedPath)
	}

	if err := tt.Click("So What (DSD128)"); err != nil {
		t.Fatalf("click local track row So What (DSD128): %v", err)
	}
	drainLocalPosts(t, posts, tt, func() bool {
		_, opens := be.snapshot()
		_, lyrOK := a.pl.Lyrics()
		return opens > 0 && a.pl.Ready() && lyrOK
	})

	cur := a.pl.Current()
	if cur == nil || cur.Name != "So What (DSD128)" {
		t.Fatalf("expected So What (DSD128) playing, got %+v", cur)
	}
	gotSrc, _ := be.snapshot()
	if !strings.HasSuffix(gotSrc.LocalPath, "01 - So What.dsf") || !gotSrc.NativeHQP {
		t.Fatalf("unexpected TrackSource passed to backend: LocalPath=%q NativeHQP=%v", gotSrc.LocalPath, gotSrc.NativeHQP)
	}
	if st := a.pl.Stream(); st == nil || !st.IsDSD || st.SpecBadge() != "DSD128" {
		t.Fatalf("unexpected player Stream() for DSD128 track: %+v", st)
	}
	if lines, ok := a.pl.Lyrics(); !ok || len(lines) == 0 || lines[0].Text != "Modal trumpet intro" {
		t.Fatalf("expected .lrc lyrics loaded via Player.Local, got (%+v, %v)", lines, ok)
	}

	// Global search surfaces local matches even when YouTube is offline.
	a.search("Waltz for Debby")
	drainLocalPosts(t, posts, tt, func() bool {
		d := a.searches[a.route]
		return d != nil && !d.page.loading
	})
	if !tt.HasText("本地曲库匹配") || !tt.HasText("Waltz for Debby") {
		t.Fatalf("expected global search to surface local library match: %v", tt.Texts())
	}
}

// Liking a local track while signed out persists without opening the login dialog, and right-clicking a local track adds it to a local playlist and copies its file path.
func TestLocalLikeOfflineAndLocalPlaylistMenu(t *testing.T) {
	musicRoot := writeSampleMusicFolder(t)

	var copied string
	origCopy := copyText
	copyText = func(s string) error { copied = s; return nil }
	defer func() { copyText = origCopy }()

	origLike := likeSong
	likeSong = func(string, bool) error {
		t.Error("likeSong cloud API should not be called for local tracks")
		return nil
	}
	defer func() { likeSong = origLike }()

	a, posts, _ := newLocalTestApp(t)
	a.profile = nil // Explicitly signed out.
	a.local.lib.AddFolders(musicRoot)
	if err := a.local.lib.Scan(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	createdPl, err := a.createLocalPlaylist("深夜试音碟", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	a.open(route{page: pageLocal})
	a.local.tab = localTabTracks
	tt := ui.NewTester(a.view, 1180, 780)
	tt.SetDark(true)
	tt.Frame()

	rowRect, ok := tt.Find("Waltz for Debby")
	if !ok {
		t.Fatalf("could not find track row Waltz for Debby in %v", tt.Texts())
	}
	tt.Move(rowRect.X+rowRect.W/2, rowRect.Y+rowRect.H/2)
	tt.Frame()

	if err := tt.Click("喜欢"); err != nil {
		t.Fatalf("click 喜欢 on local track while signed out: %v", err)
	}
	tt.Advance(time.Second)
	tt.Frame()

	if a.login.open {
		t.Fatal("liking a local track while signed out should not open the YouTube login dialog")
	}
	waltzTracks := a.local.lib.Tracks("Waltz for Debby", "", "")
	if len(waltzTracks) != 1 {
		t.Fatalf("expected 1 Waltz track, got %d", len(waltzTracks))
	}
	waltzYTM := waltzTracks[0].ToYTMTrack()
	if !a.isLiked(&waltzYTM) || !slices.Contains(a.settings.LocalLiked, waltzYTM.ID) {
		t.Fatalf("expected local track persisted in settings.LocalLiked, got %v", a.settings.LocalLiked)
	}
	rawSettings, err := os.ReadFile(a.settings.path)
	if err != nil {
		t.Fatalf("expected settings.json saved: %v", err)
	}
	var diskSettings settings
	if err := json.Unmarshal(rawSettings, &diskSettings); err != nil || !slices.Contains(diskSettings.LocalLiked, waltzYTM.ID) {
		t.Fatalf("settings.json did not persist LocalLiked: %+v (err=%v)", diskSettings.LocalLiked, err)
	}

	// Right-click context menu on the local track.
	if err := tt.RightClick("Waltz for Debby"); err != nil {
		t.Fatalf("right-click Waltz for Debby: %v", err)
	}
	menuItems := tt.Menu()
	if slices.Contains(menuItems, "复制链接") {
		t.Fatalf("local track menu should replace 复制链接 with local file actions: %v", menuItems)
	}
	if !slices.Contains(menuItems, "在文件夹中显示") || !slices.Contains(menuItems, "复制文件路径") {
		t.Fatalf("local track menu missing file actions: %v", menuItems)
	}
	if err := tt.ChooseMenuItem("复制文件路径"); err != nil {
		t.Fatalf("choose 复制文件路径: %v", err)
	}
	tt.Frame()
	if !strings.HasSuffix(copied, "01 - Waltz for Debby.wav") {
		t.Fatalf("copied path = %q, want suffix 01 - Waltz for Debby.wav", copied)
	}

	// Add local track to local playlist via right-click submenu.
	if err := tt.RightClick("Waltz for Debby"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("添加到歌单", "深夜试音碟"); err != nil {
		t.Fatalf("choose 添加到歌单 -> 深夜试音碟: %v (menu=%v)", err, tt.Menu())
	}
	drainLocalPosts(t, posts, tt, func() bool { return true })
	gotPl := a.playlists[createdPl.ID]
	if gotPl == nil || len(gotPl.Tracks) != 1 || gotPl.Tracks[0].ID != waltzYTM.ID {
		t.Fatalf("expected local track added to local playlist 深夜试音碟, got %+v", gotPl)
	}
}

// Clicking "从 Roon 同步流派" merges Roon genre classifications into matching local albums and makes them browsable by genre.
func TestLocalRoonGenreSync(t *testing.T) {
	musicDir := filepath.Join(t.TempDir(), "Ambient")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Genre tag intentionally omitted on disk so Roon is the sole source of genres.
	id3 := makeID3v23("Xtal", "Aphex Twin", "Selected Ambient Works 85-92", "1992", "")
	if err := os.WriteFile(filepath.Join(musicDir, "01 - Xtal.wav"), makeWAV24(96000, 2400, id3), 0o644); err != nil {
		t.Fatal(err)
	}

	origFetch := fetchRoonGenres
	fetchRoonGenres = func(*bridge.RoonManager, context.Context) ([]bridge.RoonAlbumGenre, error) {
		return []bridge.RoonAlbumGenre{
			{
				Album:  "Selected Ambient Works 85-92 (Remastered)",
				Artist: "Aphex Twin",
				Genres: []string{"IDM", "Ambient Techno"},
			},
		}, nil
	}
	defer func() { fetchRoonGenres = origFetch }()

	a, posts, _ := newLocalTestApp(t)
	a.local.lib.AddFolders(musicDir)
	if err := a.local.lib.Scan(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(a.local.lib.Genres()) != 0 {
		t.Fatalf("expected 0 initial genres, got %v", a.local.lib.Genres())
	}

	a.open(route{page: pageLocal})
	tt := ui.NewTester(a.view, 1180, 780)
	tt.SetDark(true)
	tt.Frame()

	if err := tt.Click("从 Roon 同步流派"); err != nil {
		t.Fatalf("click 从 Roon 同步流派: %v", err)
	}
	drainLocalPosts(t, posts, tt, func() bool {
		return !a.local.syncingRoon && len(a.local.lib.Genres()) == 2
	})

	if err := tt.Click("流派"); err != nil {
		t.Fatalf("click 流派 tab: %v", err)
	}
	tt.Frame()
	if !tt.HasText("IDM") || !tt.HasText("Ambient Techno") {
		t.Fatalf("expected synced Roon genres rendered in Genres lens: %v", tt.Texts())
	}
	if err := tt.Click("IDM"); err != nil {
		t.Fatalf("click IDM genre card: %v", err)
	}
	tt.Frame()
	if a.local.genre != "IDM" || !tt.HasText("Selected Ambient Works 85-92") {
		t.Fatalf("expected IDM filter to show matched album: genre=%q texts=%v", a.local.genre, tt.Texts())
	}
}

// An idle Local Music page and local album page request zero animation frames once transitions settle.
func TestLocalLibraryIdleFrames(t *testing.T) {
	musicRoot := writeSampleMusicFolder(t)
	a, posts, _ := newLocalTestApp(t)
	a.local.lib.AddFolders(musicRoot)
	if err := a.local.lib.Scan(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	a.open(route{page: pageLocal})

	tt := ui.NewTester(a.view, 1180, 780)
	tt.SetDark(true)
	tt.Frame()
	// Drain async local cover decodes so art fade-ins finish cleanly.
	deadline := time.Now().Add(2 * time.Second)
	for len(a.art.busy) > 0 && time.Now().Before(deadline) {
		select {
		case fn := <-posts:
			fn()
		case <-time.After(10 * time.Millisecond):
		}
	}
	for len(posts) > 0 {
		(<-posts)()
	}

	checkZeroIdle := func(name string) {
		t.Helper()
		tt.Frame()
		tt.Advance(2 * time.Second)
		tt.Frame()
		asked := 0
		for range 60 {
			tt.Advance(time.Second / 60)
			if tt.FramePending() {
				asked++
				tt.Frame()
			}
		}
		if asked != 0 {
			t.Errorf("%s requested %d idle frames over 1s, want 0", name, asked)
		}
	}

	checkZeroIdle("local-albums")
	a.local.tab = localTabGenres
	checkZeroIdle("local-genres")
	a.local.tab = localTabTracks
	checkZeroIdle("local-tracks")
	a.local.tab = localTabFolders
	checkZeroIdle("local-folders")

	albums := a.local.lib.Albums(localmusic.AlbumFilter{})
	if len(albums) > 0 {
		a.open(route{page: pageAlbum, id: albums[0].ID})
		checkZeroIdle("local-album-detail")
	}
}

// With YTB_LOCAL_SHOTS set to a directory, saves full-window dark-mode PNGs of the Local Music views for visual review.
func TestRenderLocalLibraryScreenshots(t *testing.T) {
	outDir := os.Getenv("YTB_LOCAL_SHOTS")
	if outDir == "" {
		t.Skip("YTB_LOCAL_SHOTS not set")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	musicRoot := filepath.Join(t.TempDir(), "HiFiCollection")
	specs := []struct {
		folder, title, artist, album, year, genre string
		dsd                                       bool
		sr                                        int
		c1, c2, acc                               color.RGBA
	}{
		{"Miles Davis - Kind of Blue", "So What", "Miles Davis", "Kind of Blue (SACD)", "1959", "Jazz", true, 5644800, color.RGBA{22, 52, 98, 255}, color.RGBA{10, 20, 42, 255}, color.RGBA{245, 213, 138, 255}},
		{"Bill Evans - Waltz for Debby", "Waltz for Debby", "Bill Evans Trio", "Waltz for Debby", "1961", "Jazz", false, 192000, color.RGBA{78, 36, 56, 255}, color.RGBA{24, 14, 24, 255}, color.RGBA{236, 178, 114, 255}},
		{"Sigur Ros - Agaetis byrjun", "Svefn-g-englar", "Sigur Rós", "Ágætis byrjun", "1999", "Post-Rock", false, 96000, color.RGBA{22, 68, 64, 255}, color.RGBA{11, 26, 30, 255}, color.RGBA{134, 226, 200, 255}},
		{"Keith Jarrett - Koln Concert", "Part I", "Keith Jarrett", "The Köln Concert", "1975", "Jazz", true, 2822400, color.RGBA{64, 52, 34, 255}, color.RGBA{24, 20, 16, 255}, color.RGBA{245, 213, 138, 255}},
		{"Ryuichi Sakamoto - async", "andata", "Ryuichi Sakamoto", "async", "2017", "Ambient", false, 96000, color.RGBA{38, 48, 72, 255}, color.RGBA{16, 20, 32, 255}, color.RGBA{168, 196, 242, 255}},
		{"Mogwai - Happy Songs", "Hunted by a Freak", "Mogwai", "Happy Songs for Happy People", "2003", "Post-Rock", false, 96000, color.RGBA{84, 42, 28, 255}, color.RGBA{28, 16, 14, 255}, color.RGBA{244, 166, 98, 255}},
		{"Nils Frahm - All Melody", "Sunson", "Nils Frahm", "All Melody", "2018", "Modern Classical", false, 96000, color.RGBA{46, 38, 68, 255}, color.RGBA{18, 16, 30, 255}, color.RGBA{210, 178, 242, 255}},
		{"John Coltrane - A Love Supreme", "Acknowledgement", "John Coltrane", "A Love Supreme", "1965", "Jazz", true, 5644800, color.RGBA{58, 44, 28, 255}, color.RGBA{20, 16, 12, 255}, color.RGBA{245, 213, 138, 255}},
	}

	for _, sp := range specs {
		dir := filepath.Join(musicRoot, sp.folder)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		id3 := makeID3v23(sp.title, sp.artist, sp.album, sp.year, sp.genre)
		if sp.dsd {
			if err := os.WriteFile(filepath.Join(dir, "01 - "+sp.title+".dsf"), makeDSF(sp.sr, 2, id3), 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(filepath.Join(dir, "01 - "+sp.title+".wav"), makeWAV24(sp.sr, 3200, id3), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "cover.png"), makeCoverPNG(sp.c1, sp.c2, sp.acc), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	a, posts, be := newLocalTestApp(t)
	a.local.lib.AddFolders(musicRoot)
	if err := a.local.lib.Scan(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Start playing the DSD128 album so the player bar displays the live Hi-Res signal pill.
	dsdAlbums := a.local.lib.Albums(localmusic.AlbumFilter{Query: "Kind of Blue"})
	if len(dsdAlbums) > 0 {
		a.playLocalTracks(dsdAlbums[0].Tracks, 0)
	}

	a.open(route{page: pageLocal})
	tt := ui.NewTester(a.view, 1180, 780)
	tt.SetDark(true)
	tt.SetScale(2)

	drainLocalPosts(t, posts, tt, func() bool {
		_, opens := be.snapshot()
		return opens > 0 && a.pl.Ready()
	})

	waitCovers := func() {
		tt.Frame()
		deadline := time.Now().Add(2 * time.Second)
		for (len(a.art.busy) > 0 || len(posts) > 0) && time.Now().Before(deadline) {
			select {
			case fn := <-posts:
				fn()
			case <-time.After(10 * time.Millisecond):
			}
			tt.Frame()
		}
		tt.Advance(600 * time.Millisecond)
		tt.Frame()
	}

	saveShot := func(filename string) {
		t.Helper()
		waitCovers()
		f, err := os.Create(filepath.Join(outDir, filename))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}

	// Shot 1: Roon-style Local Music Album Wall with DSD128 / WAV 24/192 gold badges.
	saveShot("local-albums-dark.png")

	// Shot 2: Roon-style Genres Lens with 2x2 Jazz album mosaic.
	if err := tt.Click("流派"); err != nil {
		t.Fatal(err)
	}
	saveShot("local-genres-dark.png")

	// Shot 3: Local Album Detail page with DSD128 spec pill, clickable Genre chip, and Reveal in Folder.
	if len(dsdAlbums) > 0 {
		a.open(route{page: pageAlbum, id: dsdAlbums[0].ID})
		saveShot("local-album-detail-dark.png")
	}
}
