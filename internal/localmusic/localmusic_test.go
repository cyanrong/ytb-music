package localmusic

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// TestFLACStreamInfoVorbisAndPicture checks that FLAC STREAMINFO, Vorbis comments, ReplayGain, and front-cover PICTURE blocks are indexed accurately.
func TestFLACStreamInfoVorbisAndPicture(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "music", "Bill Evans")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}

	iconBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3}
	frontBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'F', 'R', 'O', 'N', 'T', 0xFF, 0xD9}

	flacData := buildSyntheticFLAC(96000, 2, 24, 96000*180, []string{
		"TITLE=Waltz for Debby",
		"ARTIST=Bill Evans",
		"ARTIST=Scott LaFaro",
		"ALBUM=Waltz for Debby",
		"ALBUMARTIST=Bill Evans Trio",
		"GENRE=Jazz",
		"STYLE=Post-Bop",
		"DATE=1961-06-25",
		"COMPOSER=Bill Evans",
		"TRACKNUMBER=02/06",
		"DISCNUMBER=1/1",
		"REPLAYGAIN_ALBUM_GAIN=-4.10 dB",
		"REPLAYGAIN_TRACK_GAIN=-6.25 dB",
		"LYRICS=[00:01.00]Instrumental jazz trio",
	}, []flacPicSpec{
		{picType: 1, mime: "image/png", data: iconBytes},
		{picType: 3, mime: "image/jpeg", data: frontBytes},
	})

	flacPath := filepath.Join(musicDir, "02 - Waltz for Debby.flac")
	if err := os.WriteFile(flacPath, flacData, 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "local_library.json"), filepath.Join(root, "covers"))
	if !lib.AddFolders(filepath.Join(root, "music")) {
		t.Fatal("expected AddFolders to add music directory")
	}
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	tracks := lib.Tracks("", "", "hires")
	if len(tracks) != 1 {
		t.Fatalf("expected 1 hi-res track, got %d", len(tracks))
	}
	tr := tracks[0]
	if !IsLocalTrack(tr.ID) || !IsLocalAlbum(tr.AlbumID) {
		t.Fatalf("unexpected ID prefixes: track=%q album=%q", tr.ID, tr.AlbumID)
	}
	if tr.Title != "Waltz for Debby" || tr.Album != "Waltz for Debby" || tr.AlbumArtist != "Bill Evans Trio" {
		t.Fatalf("unexpected core tags: %+v", tr)
	}
	if !slices.Equal(tr.Artists, []string{"Bill Evans", "Scott LaFaro"}) {
		t.Fatalf("unexpected artists: %v", tr.Artists)
	}
	if !slices.Equal(tr.Genres, []string{"Jazz", "Post-Bop"}) {
		t.Fatalf("unexpected genres: %v", tr.Genres)
	}
	if tr.Year != 1961 || tr.TrackNo != 2 || tr.DiscNo != 1 || tr.Composer != "Bill Evans" {
		t.Fatalf("unexpected numeric/composer metadata: year=%d track=%d disc=%d composer=%q", tr.Year, tr.TrackNo, tr.DiscNo, tr.Composer)
	}
	if tr.DurationMs != 180000 {
		t.Fatalf("expected 180000ms duration, got %d", tr.DurationMs)
	}
	if math.Abs(tr.GainDB-(-6.25)) > 1e-6 {
		t.Fatalf("expected track ReplayGain -6.25 dB, got %v", tr.GainDB)
	}
	if tr.Spec.Codec != "FLAC" || tr.Spec.SampleRate != 96000 || tr.Spec.BitDepth != 24 || tr.Spec.Channels != 2 || !tr.Spec.IsLossless || !tr.Spec.IsHiRes {
		t.Fatalf("unexpected AudioSpec: %+v", tr.Spec)
	}
	if got := tr.Spec.Badge(); got != "FLAC 24/96" {
		t.Fatalf("expected badge %q, got %q", "FLAC 24/96", got)
	}
	if detail := tr.Spec.Detail(); !strings.Contains(detail, "FLAC · 96kHz / 24bit") {
		t.Fatalf("unexpected spec detail: %q", detail)
	}
	if !tr.NativeHQP() {
		t.Fatal("expected standalone FLAC track to support NativeHQP")
	}

	// Verify front cover (type 3) beat earlier icon (type 1) and round-trips via CoverURL.
	coverRaw, err := lib.CoverBytes(CoverURL(tr.CoverID))
	if err != nil {
		t.Fatalf("CoverBytes failed: %v", err)
	}
	if !bytes.Equal(coverRaw, frontBytes) {
		t.Fatalf("expected front cover bytes %x, got %x", frontBytes, coverRaw)
	}

	ytmTr := tr.ToYTMTrack()
	if ytmTr.Name != "Waltz for Debby" || ytmTr.Reason != "FLAC 24/96" || !IsLocalCover(ytmTr.Album.PicURL) {
		t.Fatalf("unexpected ToYTMTrack mapping: %+v", ytmTr)
	}
}

// TestWAVExtensible24BitAndGB18030ID3 checks 24-bit WAVE_FORMAT_EXTENSIBLE WAV parsing, GB18030 ID3v2 fallback, and pure-Go PCM decoding.
func TestWAVExtensible24BitAndGB18030ID3(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "wav")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		sr     = 192000
		frames = 1920 // 10 ms
	)
	pcm := make([]byte, frames*2*3)
	for i := range frames {
		lVal := int32(math.Round(0.5 * float64((1<<23)-1) * math.Sin(2*math.Pi*1000*float64(i)/sr)))
		rVal := -lVal
		putPCM24LE(pcm[(i*2)*3:], lVal)
		putPCM24LE(pcm[(i*2+1)*3:], rVal)
	}

	gbTitle, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("春江花月夜"))
	if err != nil {
		t.Fatal(err)
	}
	gbArtist, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("中央民族乐团"))
	if err != nil {
		t.Fatal(err)
	}
	id3Chunk := buildID3v23Tag([][2][]byte{
		{[]byte("TIT2"), append([]byte{0}, gbTitle...)}, // encoding 0 (ISO-8859-1) carrying GB18030 bytes
		{[]byte("TPE1"), append([]byte{0}, gbArtist...)},
		{[]byte("TALB"), append([]byte{3}, []byte("中国古典民乐")...)},
		{[]byte("TYER"), append([]byte{0}, []byte("1998")...)},
	})

	wavBytes := buildExtensibleWAV24(sr, 2, pcm, id3Chunk)
	wavPath := filepath.Join(musicDir, "river.wav")
	if err := os.WriteFile(wavPath, wavBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolders(musicDir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	tracks := lib.Tracks("", "", "")
	if len(tracks) != 1 {
		t.Fatalf("expected 1 WAV track, got %d", len(tracks))
	}
	tr := tracks[0]
	if tr.Title != "春江花月夜" || len(tr.Artists) == 0 || tr.Artists[0] != "中央民族乐团" || tr.Album != "中国古典民乐" {
		t.Fatalf("GB18030 ID3v2 tags decoded incorrectly: title=%q artists=%v album=%q", tr.Title, tr.Artists, tr.Album)
	}
	if tr.Spec.Badge() != "WAV 24/192" || !tr.Spec.IsHiRes || !tr.Spec.IsLossless {
		t.Fatalf("unexpected WAV spec: %+v (badge=%q)", tr.Spec, tr.Spec.Badge())
	}

	dec, openedTr, err := lib.OpenDecoder(tr.ID)
	if err != nil {
		t.Fatalf("OpenDecoder failed: %v", err)
	}
	defer func() {
		if c, ok := dec.(io.Closer); ok {
			if err := c.Close(); err != nil {
				t.Errorf("Close failed: %v", err)
			}
		}
	}()
	if openedTr.ID != tr.ID || dec.SampleRate() != sr || dec.Channels() != 2 || dec.Len() != frames {
		t.Fatalf("unexpected decoder params: sr=%d ch=%d len=%d", dec.SampleRate(), dec.Channels(), dec.Len())
	}

	buf := make([]float32, 96*2)
	n, err := dec.Read(buf)
	if err != nil || n != len(buf) {
		t.Fatalf("Read failed: n=%d err=%v", n, err)
	}
	// Frame 48 is 1/4 cycle of 1000 Hz at 192 kHz -> sin(pi/2) = +0.5 on L, -0.5 on R.
	if math.Abs(float64(buf[48*2])-0.5) > 1e-4 || math.Abs(float64(buf[48*2+1])+0.5) > 1e-4 {
		t.Fatalf("unexpected decoded 24-bit WAV samples at quarter wave: L=%f R=%f", buf[48*2], buf[48*2+1])
	}

	if err := dec.SetPosition(48); err != nil {
		t.Fatalf("SetPosition failed: %v", err)
	}
	one := make([]float32, 2)
	if _, err := dec.Read(one); err != nil {
		t.Fatalf("Read after seek failed: %v", err)
	}
	if math.Abs(float64(one[0])-0.5) > 1e-4 {
		t.Fatalf("expected +0.5 after seeking to frame 48, got %f", one[0])
	}
}

// TestAIFFExtendedFloatAndPCMDecode checks AIFF 80-bit IEEE 754 sample rate parsing and 24-bit big-endian + AIFC sowt PCM decoding.
func TestAIFFExtendedFloatAndPCMDecode(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "aiff")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		sr     = 88200
		frames = 400
	)
	pcmBE := make([]byte, frames*2*3)
	for i := range frames {
		v := int32((i - 200) * 16384)
		putPCM24BE(pcmBE[(i*2)*3:], v)
		putPCM24BE(pcmBE[(i*2+1)*3:], -v)
	}
	aiffBytes := buildAIFFFile("AIFF", "", sr, 2, 24, frames, 16, pcmBE, "Autumn Leaves", "Cannonball Adderley")
	aiffPath := filepath.Join(musicDir, "01 - Autumn Leaves.aiff")
	if err := os.WriteFile(aiffPath, aiffBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Also write a 16-bit little-endian "sowt" AIFC file.
	pcmSowt := make([]byte, frames*2*2)
	posSample, negSample := int16(8192), int16(-8192)
	for i := range frames {
		binary.LittleEndian.PutUint16(pcmSowt[(i*2)*2:], uint16(posSample))
		binary.LittleEndian.PutUint16(pcmSowt[(i*2+1)*2:], uint16(negSample))
	}
	aifcBytes := buildAIFFFile("AIFC", "sowt", 44100, 2, 16, frames, 0, pcmSowt, "Sowt Track", "Cannonball Adderley")
	aifcPath := filepath.Join(musicDir, "02 - Sowt.aif")
	if err := os.WriteFile(aifcPath, aifcBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolders(musicDir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	hiRes := lib.Tracks("", "", "hires")
	if len(hiRes) != 1 {
		t.Fatalf("expected 1 hi-res AIFF track, got %d", len(hiRes))
	}
	tr := hiRes[0]
	if tr.Title != "Autumn Leaves" || tr.Spec.Badge() != "AIFF 24/88.2" {
		t.Fatalf("unexpected AIFF metadata: title=%q badge=%q", tr.Title, tr.Spec.Badge())
	}

	dec, _, err := lib.OpenDecoder(tr.ID)
	if err != nil {
		t.Fatalf("OpenDecoder AIFF failed: %v", err)
	}
	defer func() {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	if dec.SampleRate() != sr || dec.Len() != frames {
		t.Fatalf("unexpected AIFF decoder params: sr=%d len=%d", dec.SampleRate(), dec.Len())
	}
	if err := dec.SetPosition(300); err != nil {
		t.Fatalf("AIFF SetPosition failed: %v", err)
	}
	pair := make([]float32, 2)
	if _, err := dec.Read(pair); err != nil {
		t.Fatalf("AIFF Read failed: %v", err)
	}
	want := float32((300-200)*16384) / float32(1<<23)
	if math.Abs(float64(pair[0]-want)) > 1e-5 || math.Abs(float64(pair[1]+want)) > 1e-5 {
		t.Fatalf("AIFF sample mismatch at frame 300: got (%f, %f), want (%f, %f)", pair[0], pair[1], want, -want)
	}

	// Verify sowt AIFC decodes little-endian 16-bit PCM accurately.
	allTracks := lib.Tracks("Sowt Track", "", "")
	if len(allTracks) != 1 {
		t.Fatalf("expected sowt track, got %d", len(allTracks))
	}
	sowtDec, _, err := lib.OpenDecoder(allTracks[0].ID)
	if err != nil {
		t.Fatalf("OpenDecoder AIFC sowt failed: %v", err)
	}
	defer func() {
		if c, ok := sowtDec.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	if _, err := sowtDec.Read(pair); err != nil {
		t.Fatalf("sowt Read failed: %v", err)
	}
	if math.Abs(float64(pair[0])-0.25) > 1e-4 || math.Abs(float64(pair[1])+0.25) > 1e-4 {
		t.Fatalf("sowt sample mismatch: got (%f, %f), want (0.25, -0.25)", pair[0], pair[1])
	}
}

// TestDSFMetadataAndPreviewDecode checks DSD64 .dsf and DSD128 .dff indexing and pure-Go 88.2kHz decimated PCM preview decoding.
func TestDSFMetadataAndPreviewDecode(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "dsd")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}

	id3Tag := buildID3v23Tag([][2][]byte{
		{[]byte("TIT2"), append([]byte{3}, []byte("Blue in Green (DSD)")...)},
		{[]byte("TPE1"), append([]byte{3}, []byte("Miles Davis")...)},
		{[]byte("TALB"), append([]byte{3}, []byte("Kind of Blue SACD")...)},
	})
	// 2 blocks of 4096 bytes per channel = 65536 DSD samples (~23.2 ms @ 2.8224 MHz -> 2048 PCM frames @ 88.2 kHz).
	dsfBytes := buildSyntheticDSF(2822400, 2, 2, 1000.0, id3Tag)
	dsfPath := filepath.Join(musicDir, "03 - Blue in Green.dsf")
	if err := os.WriteFile(dsfPath, dsfBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	dffBytes := buildSyntheticDFF(5644800, 2, 8192, "So What (DSD128)", "Miles Davis")
	dffPath := filepath.Join(musicDir, "01 - So What.dff")
	if err := os.WriteFile(dffPath, dffBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolders(musicDir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	dsdTracks := lib.Tracks("", "", "dsd")
	if len(dsdTracks) != 2 {
		t.Fatalf("expected 2 DSD tracks, got %d", len(dsdTracks))
	}

	var dsfTr, dffTr LocalTrack
	for _, tr := range dsdTracks {
		if strings.HasSuffix(tr.Path, ".dsf") {
			dsfTr = tr
		} else {
			dffTr = tr
		}
	}

	if dsfTr.Title != "Blue in Green (DSD)" || dsfTr.Spec.Codec != "DSD64" || dsfTr.Spec.Badge() != "DSD64" || !dsfTr.Spec.IsDSD || !dsfTr.Spec.IsHiRes {
		t.Fatalf("unexpected DSF metadata: %+v", dsfTr)
	}
	if !strings.Contains(dsfTr.Spec.Detail(), "DSD64 · 2.82MHz / 1bit") {
		t.Fatalf("unexpected DSF detail: %q", dsfTr.Spec.Detail())
	}
	if dffTr.Title != "So What (DSD128)" || dffTr.Spec.Codec != "DSD128" || !strings.Contains(dffTr.Spec.Detail(), "DSD128 · 5.64MHz / 1bit") {
		t.Fatalf("unexpected DFF metadata: %+v (detail=%q)", dffTr, dffTr.Spec.Detail())
	}

	dec, _, err := lib.OpenDecoder(dsfTr.ID)
	if err != nil {
		t.Fatalf("OpenDecoder DSF failed: %v", err)
	}
	defer func() {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	if dec.SampleRate() != 88200 || dec.Channels() != 2 || dec.Len() != 2048 {
		t.Fatalf("unexpected DSF preview decoder params: sr=%d ch=%d len=%d", dec.SampleRate(), dec.Channels(), dec.Len())
	}

	pcm := make([]float32, 2048*2)
	got, err := dec.Read(pcm)
	if err != nil || got != len(pcm) {
		t.Fatalf("DSF Read failed: got=%d err=%v", got, err)
	}
	var sumSq float64
	var maxAbs float32
	for _, s := range pcm[128:] { // skip 4-tap filter warmup
		if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
			t.Fatal("DSD preview produced NaN/Inf sample")
		}
		abs := float32(math.Abs(float64(s)))
		if abs > maxAbs {
			maxAbs = abs
		}
		sumSq += float64(s) * float64(s)
	}
	rms := math.Sqrt(sumSq / float64(len(pcm)-128))
	if rms < 0.05 || maxAbs > 0.55 {
		t.Fatalf("unexpected DSD64 decimated 1kHz tone level: rms=%f maxAbs=%f", rms, maxAbs)
	}

	if err := dec.SetPosition(1024); err != nil {
		t.Fatalf("DSF SetPosition failed: %v", err)
	}
	tail := make([]float32, 1024*2)
	n2, err := dec.Read(tail)
	if err != nil || n2 != len(tail) {
		t.Fatalf("DSF Read after seek failed: n=%d err=%v", n2, err)
	}
	eofBuf := make([]float32, 16)
	if n3, err := dec.Read(eofBuf); n3 != 0 || err != io.EOF {
		t.Fatalf("expected EOF at end of DSF stream, got n=%d err=%v", n3, err)
	}
}

// TestGBKCueSheetWAVSlicingAndClipDecoder checks GBK-encoded Chinese CUE parsing, whole-disc suppression, parent disc folder cover lookup, and frame-accurate clipDecoder bounds.
func TestGBKCueSheetWAVSlicingAndClipDecoder(t *testing.T) {
	root := t.TempDir()
	albumRoot := filepath.Join(root, "music", "民歌蔡琴")
	discDir := filepath.Join(albumRoot, "CD1")
	if err := os.MkdirAll(discDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Place cover.jpg in parent albumRoot while audio + CUE live inside CD1.
	folderCoverBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'C', 'O', 'V', 'E', 'R', 0xFF, 0xD9}
	if err := os.WriteFile(filepath.Join(albumRoot, "cover.jpg"), folderCoverBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Synthesize a 2.5-second 44.1 kHz 16-bit stereo WAV:
	// [0.0s, 1.0s) -> +0.25 (+8192)
	// [1.0s, 2.5s) -> -0.50 (-16384)
	const sr = 44100
	const totalFrames = sr * 5 / 2 // 110250 frames (2500 ms)
	pcm16 := make([]byte, totalFrames*4)
	for i := range totalFrames {
		sample := int16(8192)
		if i >= sr {
			sample = -16384
		}
		binary.LittleEndian.PutUint16(pcm16[i*4:], uint16(sample))
		binary.LittleEndian.PutUint16(pcm16[i*4+2:], uint16(sample))
	}
	wavPath := filepath.Join(discDir, "CDImage.wav")
	if err := os.WriteFile(wavPath, buildPCM16WAV(sr, 2, pcm16), 0o644); err != nil {
		t.Fatal(err)
	}

	cueUTF8 := strings.Join([]string{
		`REM GENRE "Folk"`,
		`REM DATE 1996`,
		`PERFORMER "蔡琴"`,
		`TITLE "民歌蔡琴"`,
		`FILE "CDImage.wav" WAVE`,
		`  TRACK 01 AUDIO`,
		`    TITLE "被遗忘的时光"`,
		`    PERFORMER "蔡琴"`,
		`    INDEX 01 00:00:00`,
		`  TRACK 02 AUDIO`,
		`    TITLE "渡口"`,
		`    PERFORMER "蔡琴"`,
		`    INDEX 01 00:01:00`,
		"",
	}, "\r\n")
	cueGBK, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(cueUTF8))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(discDir, "CDImage.cue"), cueGBK, 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolders(filepath.Join(root, "music"))
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	// Whole-disc CDImage.wav must be consumed by CUE, leaving exactly 2 sliced tracks.
	tracks := lib.Tracks("", "", "")
	if len(tracks) != 2 {
		t.Fatalf("expected exactly 2 CUE tracks (whole-disc WAV suppressed), got %d", len(tracks))
	}
	t1, t2 := tracks[0], tracks[1]
	if t1.Title != "被遗忘的时光" || t2.Title != "渡口" || t1.Album != "民歌蔡琴" || t1.AlbumArtist != "蔡琴" {
		t.Fatalf("GBK CUE metadata mismatch: t1=%+v t2=%+v", t1, t2)
	}
	if t1.DurationMs != 1000 || t2.DurationMs != 1500 {
		t.Fatalf("unexpected CUE slice durations: t1=%dms t2=%dms", t1.DurationMs, t2.DurationMs)
	}
	if !t2.Spec.IsCUE || t2.NativeHQP() || t2.Spec.Badge() != "WAV 16/44.1 · CUE" {
		t.Fatalf("unexpected CUE spec/flags: spec=%+v badge=%q nativeHQP=%v", t2.Spec, t2.Spec.Badge(), t2.NativeHQP())
	}
	// Parent folder cover.jpg must have been discovered for CD1 subfolder.
	gotCover, err := lib.CoverBytes(t2.CoverID)
	if err != nil || !bytes.Equal(gotCover, folderCoverBytes) {
		t.Fatalf("expected parent folder cover to attach to CD1 CUE tracks, err=%v", err)
	}

	// Verify clipDecoder bounds & seeking on Track 2 ([1000ms, 2500ms) -> 66150 frames of -0.50).
	dec, _, err := lib.OpenDecoder(t2.ID)
	if err != nil {
		t.Fatalf("OpenDecoder on CUE track 2 failed: %v", err)
	}
	defer func() {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	const wantFrames = sr * 3 / 2 // 66150
	if dec.Len() != wantFrames {
		t.Fatalf("expected clipDecoder Len=%d, got %d", wantFrames, dec.Len())
	}

	allSamples := make([]float32, (wantFrames+500)*2)
	totalRead := 0
	for {
		n, rerr := dec.Read(allSamples[totalRead:])
		totalRead += n
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("clipDecoder Read error: %v", rerr)
		}
	}
	if totalRead != wantFrames*2 {
		t.Fatalf("expected %d samples from clipDecoder, got %d", wantFrames*2, totalRead)
	}
	for i := 0; i < totalRead; i++ {
		if math.Abs(float64(allSamples[i])+0.5) > 1e-4 {
			t.Fatalf("clipDecoder sample[%d] = %f, expected -0.50 (track 2 region only)", i, allSamples[i])
		}
	}
	if err := dec.SetPosition(wantFrames + 1); err == nil {
		t.Fatal("expected out-of-bounds SetPosition on clipDecoder to fail")
	}
	if err := dec.SetPosition(100); err != nil {
		t.Fatalf("valid SetPosition on clipDecoder failed: %v", err)
	}
	check := make([]float32, 2)
	if n, err := dec.Read(check); err != nil || n != 2 || math.Abs(float64(check[0])+0.5) > 1e-4 {
		t.Fatalf("Read after clipDecoder seek failed: n=%d err=%v val=%f", n, err, check[0])
	}
}

// TestBilingualAndShiftedCueLyrics checks Shift-JIS / UTF-8 bilingual duplicate-timestamp .lrc splitting and CUE whole-disc .lrc clipping/shifting.
func TestBilingualAndShiftedCueLyrics(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "music")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Standalone track with Shift-JIS Japanese + UTF-8/bilingual LRC.
	pcm := make([]byte, 44100*4*4) // 4 seconds
	wavPath := filepath.Join(musicDir, "hikaru.wav")
	if err := os.WriteFile(wavPath, buildPCM16WAV(44100, 2, pcm), 0o644); err != nil {
		t.Fatal(err)
	}
	bilingualLRC := strings.Join([]string{
		"[00:00.50]最後のキスは",
		"[00:00.50]最后的吻",
		"[00:02.25]タバコのflavorがした",
		"[00:02.25]带着香烟的味道",
	}, "\n")
	if err := os.WriteFile(filepath.Join(musicDir, "hikaru.lrc"), []byte(bilingualLRC), 0o644); err != nil {
		t.Fatal(err)
	}

	// CUE sheet in a separate folder referencing a whole-disc .lrc that needs shifting & clipping.
	cueDir := filepath.Join(root, "music", "cue_lrc")
	if err := os.MkdirAll(cueDir, 0o755); err != nil {
		t.Fatal(err)
	}
	discPCM := make([]byte, 44100*10*4) // 10 seconds
	if err := os.WriteFile(filepath.Join(cueDir, "disc.wav"), buildPCM16WAV(44100, 2, discPCM), 0o644); err != nil {
		t.Fatal(err)
	}
	// CUE references "disc.ape" while disk has "disc.wav" to also verify CUE extension fallback.
	sjCueText := "PERFORMER \"宇多田ヒカル\"\r\nTITLE \"First Love\"\r\nFILE \"disc.ape\" WAVE\r\n  TRACK 01 AUDIO\r\n    TITLE \"Automatic\"\r\n    INDEX 01 00:00:00\r\n  TRACK 02 AUDIO\r\n    TITLE \"First Love\"\r\n    INDEX 01 00:04:00\r\n"
	sjCueBytes, err := japanese.ShiftJIS.NewEncoder().Bytes([]byte(sjCueText))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cueDir, "disc.cue"), sjCueBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	wholeDiscLRC := strings.Join([]string{
		"[00:01.00]It's automatic",
		"[00:01.00]不由自主",
		"[00:05.50]You are always gonna be my love",
		"[00:05.50]你永远会是我的爱",
		"[00:08.00]いつか誰かとまた恋に落ちても",
		"[00:08.00]就算有一天再和别人相恋",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cueDir, "disc.lrc"), []byte(wholeDiscLRC), 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolders(musicDir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	hikaru := lib.Tracks("hikaru", "", "")
	if len(hikaru) != 1 {
		t.Fatalf("expected hikaru track, got %d", len(hikaru))
	}
	lrc, trans := lib.Lyrics(hikaru[0].ID)
	if !strings.Contains(lrc, "[00:00.50]最後のキスは") || !strings.Contains(lrc, "[00:02.25]タバコのflavorがした") {
		t.Fatalf("unexpected primary LRC: %q", lrc)
	}
	if !strings.Contains(trans, "[00:00.50]最后的吻") || !strings.Contains(trans, "[00:02.25]带着香烟的味道") {
		t.Fatalf("unexpected translated LRC: %q", trans)
	}

	// Track 2 ("First Love") starts at 00:04.00 (4000ms), so 00:05.50 shifts to 00:01.50 and 00:01.00 is clipped out.
	cueTracks := lib.Tracks("First Love", "", "cue")
	var tr2 *LocalTrack
	for i := range cueTracks {
		if cueTracks[i].TrackNo == 2 {
			tr2 = &cueTracks[i]
		}
	}
	if tr2 == nil || tr2.AlbumArtist != "宇多田ヒカル" {
		t.Fatalf("Shift-JIS CUE track 2 missing or artist wrong: %+v", tr2)
	}
	cLrc, cTrans := lib.Lyrics(tr2.ID)
	if strings.Contains(cLrc, "automatic") {
		t.Fatalf("expected track 1 lyrics before ClipStartMs to be clipped out, got %q", cLrc)
	}
	if !strings.Contains(cLrc, "[00:01.50]You are always gonna be my love") || !strings.Contains(cLrc, "[00:04.00]いつか誰かとまた恋に落ちても") {
		t.Fatalf("expected shifted CUE primary lyrics, got %q", cLrc)
	}
	if !strings.Contains(cTrans, "[00:01.50]你永远会是我的爱") || !strings.Contains(cTrans, "[00:04.00]就算有一天再和别人相恋") {
		t.Fatalf("expected shifted CUE translation lyrics, got %q", cTrans)
	}
}

// TestIncrementalScanCacheAndApplyRoonGenres checks that unchanged files skip re-parsing on subsequent scans and that ApplyRoonGenres merges normalized/CJK genres persistently.
func TestIncrementalScanCacheAndApplyRoonGenres(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "music")
	alb1Dir := filepath.Join(musicDir, "Teresa Teng")
	alb2Dir := filepath.Join(musicDir, "Bill Evans")
	if err := os.MkdirAll(alb1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(alb2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	f1 := filepath.Join(alb1Dir, "01 - 甜蜜蜜.flac")
	f2 := filepath.Join(alb2Dir, "01 - My Foolish Heart.flac")
	if err := os.WriteFile(f1, buildSyntheticFLAC(44100, 2, 16, 44100*60, []string{
		"TITLE=甜蜜蜜",
		"ARTIST=邓丽君",
		"ALBUM=岛国之情歌第六集 (Remastered)",
		"ALBUMARTIST=邓丽君",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, buildSyntheticFLAC(96000, 2, 24, 96000*60, []string{
		"TITLE=My Foolish Heart",
		"ARTIST=Bill Evans Trio",
		"ALBUM=Waltz for Debby!",
		"ALBUMARTIST=Bill Evans Trio",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	var parsedCount atomic.Int32
	prevHook := onFileParsedHook
	onFileParsedHook = func(string) { parsedCount.Add(1) }
	defer func() { onFileParsedHook = prevHook }()

	idxPath := filepath.Join(root, "local_library.json")
	covDir := filepath.Join(root, "covers")
	lib := NewLibrary(idxPath, covDir)
	lib.AddFolders(musicDir)

	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan failed: %v", err)
	}
	if got := parsedCount.Load(); got != 2 {
		t.Fatalf("expected initial scan to parse 2 files, parsed %d", got)
	}

	// Second scan with identical (size, mtimeNs) must reuse cache completely.
	parsedCount.Store(0)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("second Scan failed: %v", err)
	}
	if got := parsedCount.Load(); got != 0 {
		t.Fatalf("expected incremental scan to parse 0 unchanged files, parsed %d", got)
	}

	// Touch only f1 -> incremental scan should re-parse only f1.
	future := time.Unix(1_750_000_000, 0)
	if err := os.Chtimes(f1, future, future); err != nil {
		t.Fatal(err)
	}
	parsedCount.Store(0)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("third Scan failed: %v", err)
	}
	if got := parsedCount.Load(); got != 1 {
		t.Fatalf("expected incremental scan after touching 1 file to parse 1 file, parsed %d", got)
	}

	// Merge Roon genres using Traditional Chinese ("鄧麗君" / "島國之情歌第六集") and stripped punctuation/artist ("waltz for debby" / "Bill Evans").
	matched := lib.ApplyRoonGenres([]RoonAlbumGenre{
		{Album: "島國之情歌第六集", Artist: "鄧麗君", Genres: []string{"Mandopop", "Vocal"}},
		{Album: "waltz for debby", Artist: "Bill Evans", Genres: []string{"Jazz", "Cool Jazz"}},
		{Album: "Nonexistent Album", Artist: "Nobody", Genres: []string{"Rock"}},
	})
	if matched != 2 {
		t.Fatalf("expected ApplyRoonGenres to match 2 local albums, matched %d", matched)
	}

	// Reload into a brand-new Library instance and verify Roon genres, folder browse, and search persisted.
	reloaded := NewLibrary(idxPath, covDir)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	st := reloaded.Stats()
	if st.Albums != 2 || st.Tracks != 2 || st.Genres != 4 || st.HiResAlbums != 1 {
		t.Fatalf("unexpected reloaded stats: %+v", st)
	}
	mandoAlbums := reloaded.Albums(AlbumFilter{Genre: "Mandopop"})
	if len(mandoAlbums) != 1 || mandoAlbums[0].AlbumArtist != "邓丽君" {
		t.Fatalf("expected Mandopop filter to return Teresa Teng album, got %+v", mandoAlbums)
	}

	rootNode := reloaded.BrowseFolder("")
	if len(rootNode.Subdirs) != 1 || rootNode.Subdirs[0].TrackCount != 2 {
		t.Fatalf("unexpected root BrowseFolder: %+v", rootNode)
	}
	musicNode := reloaded.BrowseFolder(musicDir)
	if len(musicNode.Subdirs) != 2 || musicNode.Parent != "" {
		t.Fatalf("unexpected musicDir BrowseFolder: %+v", musicNode)
	}

	songs, albums, artists := reloaded.Search("鄧麗君", 10)
	if len(songs) != 1 || len(albums) != 1 || len(artists) != 1 {
		t.Fatalf("expected CJK-folded Search for '鄧麗君' to hit track/album/artist, got songs=%d albums=%d artists=%d", len(songs), len(albums), len(artists))
	}
}

// TestALACWavPackAndMP3XingParsing checks pure-Go metadata and spec extraction for ALAC M4A, WavPack + APEv2, and Xing VBR MP3 files.
func TestALACWavPackAndMP3XingParsing(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "formats")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}

	coverBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'A', 'L', 'A', 'C', 0xFF, 0xD9}
	m4aPath := filepath.Join(musicDir, "01 - Koln.m4a")
	if err := os.WriteFile(m4aPath, buildSyntheticALACM4A(96000, 24, 2, 120000, "The Köln Concert", "Keith Jarrett", coverBytes), 0o644); err != nil {
		t.Fatal(err)
	}

	wvCover := []byte{0x89, 'P', 'N', 'G', 'W', 'V', 'C', 'O', 'V'}
	wvPath := filepath.Join(musicDir, "02 - Goldberg.wv")
	if err := os.WriteFile(wvPath, buildSyntheticWavPack(88200, 24, 2, 88200*90, "Aria", "Glenn Gould", "Goldberg Variations", wvCover), 0o644); err != nil {
		t.Fatal(err)
	}

	mp3Path := filepath.Join(musicDir, "03 - Nocturne.mp3")
	if err := os.WriteFile(mp3Path, buildSyntheticXingMP3(1000, "Nocturne Op.9 No.2", "Maurizio Pollini"), 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolders(musicDir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	alacTracks := lib.Tracks("Köln", "", "hires")
	if len(alacTracks) != 1 {
		t.Fatalf("expected 1 ALAC hi-res track, got %d", len(alacTracks))
	}
	if alacTracks[0].Spec.Badge() != "ALAC 24/96" || alacTracks[0].DurationMs != 120000 || alacTracks[0].NativeHQP() {
		t.Fatalf("unexpected ALAC track: spec=%+v badge=%q dur=%d nativeHQP=%v", alacTracks[0].Spec, alacTracks[0].Spec.Badge(), alacTracks[0].DurationMs, alacTracks[0].NativeHQP())
	}
	gotAlacCov, err := lib.CoverBytes(alacTracks[0].CoverID)
	if err != nil || !bytes.Equal(gotAlacCov, coverBytes) {
		t.Fatalf("expected ALAC covr atom to round-trip, err=%v", err)
	}

	wvTracks := lib.Tracks("Aria", "", "hires")
	if len(wvTracks) != 1 || wvTracks[0].Spec.Badge() != "WV 24/88.2" || wvTracks[0].DurationMs != 90000 {
		t.Fatalf("unexpected WavPack track: %+v", wvTracks)
	}
	gotWVCov, err := lib.CoverBytes(wvTracks[0].CoverID)
	if err != nil || !bytes.Equal(gotWVCov, wvCover) {
		t.Fatalf("expected APEv2 Cover Art (Front) to round-trip, err=%v", err)
	}

	mp3Tracks := lib.Tracks("Nocturne", "", "")
	if len(mp3Tracks) != 1 || mp3Tracks[0].Spec.Codec != "MP3" || mp3Tracks[0].Spec.IsLossless {
		t.Fatalf("unexpected MP3 track: %+v", mp3Tracks)
	}
	// 1000 frames * 1152 samples / 44100 Hz = 26122 ms
	if mp3Tracks[0].DurationMs != 26122 {
		t.Fatalf("expected Xing VBR MP3 duration 26122ms, got %d", mp3Tracks[0].DurationMs)
	}
}

// TestSaveSerializesConcurrentMutationsAndPreservesIndexOnRenameFailure checks that Save serializes snapshots without clobbering newer state and preserves the last good index when atomic rename fails (fixing Save's shared .part race and destructive os.Remove fallback).
func TestSaveSerializesConcurrentMutationsAndPreservesIndexOnRenameFailure(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "music")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	flacPath := filepath.Join(musicDir, "01 - Blue Train.flac")
	if err := os.WriteFile(flacPath, buildSyntheticFLAC(96000, 2, 24, 96000*30, []string{
		"TITLE=Blue Train",
		"ARTIST=John Coltrane",
		"ALBUM=Blue Train",
		"ALBUMARTIST=John Coltrane",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	idxDir := filepath.Join(root, "state")
	idxPath := filepath.Join(idxDir, "local_library.json")
	covDir := filepath.Join(root, "covers")
	lib := NewLibrary(idxPath, covDir)
	lib.AddFolder(musicDir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan failed: %v", err)
	}
	goodBytes, err := os.ReadFile(idxPath)
	if err != nil || len(goodBytes) == 0 {
		t.Fatalf("expected valid initial index file, err=%v", err)
	}

	// Simulate rename failure and verify the previous valid index is untouched and temp files are cleaned up.
	prevRename := renameIndexFile
	renameIndexFile = func(_, _ string) error {
		return errors.New("simulated rename failure")
	}
	extraDir := filepath.Join(root, "extra")
	if err := os.MkdirAll(extraDir, 0o755); err != nil {
		renameIndexFile = prevRename
		t.Fatal(err)
	}
	lib.AddFolder(extraDir)
	saveErr := lib.Save()
	renameIndexFile = prevRename
	if saveErr == nil {
		t.Fatal("expected Save to fail when renameIndexFile fails")
	}

	afterFailBytes, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("expected previous index file to survive rename failure, got err=%v", err)
	}
	if !bytes.Equal(afterFailBytes, goodBytes) {
		t.Fatal("expected previous good index bytes to remain intact on rename failure")
	}
	entries, err := os.ReadDir(idxDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".local_library-") {
			t.Fatalf("leaked temporary index file after failed Save: %s", ent.Name())
		}
	}

	// Concurrent Save / AddFolder / RemoveFolder / ApplyRoonGenres must never race or leave stale snapshots on disk.
	var wg sync.WaitGroup
	genres := []string{"Hard Bop", "Modal Jazz", "Soul Jazz", "Avant-Garde Jazz"}
	for i := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			sub := filepath.Join(root, fmt.Sprintf("concurrent-%d", worker))
			lib.AddFolder(sub)
			lib.ApplyRoonGenres([]RoonAlbumGenre{{
				Album:  "Blue Train",
				Artist: "John Coltrane",
				Genres: []string{genres[worker%len(genres)]},
			}})
			if worker%2 == 1 {
				lib.RemoveFolder(sub)
			}
			if err := lib.Save(); err != nil {
				t.Errorf("concurrent Save failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	wantFolders := lib.Folders()
	reloaded := NewLibrary(idxPath, covDir)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load after concurrent saves failed: %v", err)
	}
	if !slices.Equal(reloaded.Folders(), wantFolders) {
		t.Fatalf("persisted folders %v do not match latest in-memory folders %v", reloaded.Folders(), wantFolders)
	}
	reloadedAlbums := reloaded.Albums(AlbumFilter{Query: "Blue Train"})
	if len(reloadedAlbums) != 1 || len(reloadedAlbums[0].Genres) != len(genres) {
		t.Fatalf("expected all %d concurrent Roon genres persisted, got %+v", len(genres), reloadedAlbums)
	}
	entries, _ = os.ReadDir(idxDir)
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".local_library-") {
			t.Fatalf("leaked temporary index file after concurrent saves: %s", ent.Name())
		}
	}
}

// TestDSFDecoderBoundsPayloadAndRejectsCorruptHeaders checks that newDSFDecoder validates channels, sampleRate, blockSize, and payload bytes before allocating buffers or computing frame length (fixing unbounded DSF header allocations).
func TestDSFDecoderBoundsPayloadAndRejectsCorruptHeaders(t *testing.T) {
	dir := t.TempDir()
	valid := buildSyntheticDSF(2822400, 2, 2, 1000.0, nil)

	writeMutated := func(name string, mut func([]byte) []byte) string {
		b := mut(bytes.Clone(valid))
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// 1. Huge blockSize (1 GiB) must be rejected before allocating blockBuf.
	hugeBlockPath := writeMutated("huge_block.dsf", func(b []byte) []byte {
		binary.LittleEndian.PutUint32(b[28+44:28+48], 1<<30)
		return b
	})
	if dec, err := OpenTrackDecoder(&LocalTrack{Path: hugeBlockPath}); err == nil {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
		t.Fatal("expected DSF with 1GiB blockSize to be rejected")
	}

	// 2. Zero blockSize must be rejected.
	zeroBlockPath := writeMutated("zero_block.dsf", func(b []byte) []byte {
		binary.LittleEndian.PutUint32(b[28+44:28+48], 0)
		return b
	})
	if dec, err := OpenTrackDecoder(&LocalTrack{Path: zeroBlockPath}); err == nil {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
		t.Fatal("expected DSF with zero blockSize to be rejected")
	}

	// 3. Out-of-range channel count (9) and non-DSD sampleRate (44100) must be rejected.
	badChanPath := writeMutated("bad_chan.dsf", func(b []byte) []byte {
		binary.LittleEndian.PutUint32(b[28+24:28+28], 9)
		return b
	})
	if dec, err := OpenTrackDecoder(&LocalTrack{Path: badChanPath}); err == nil {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
		t.Fatal("expected DSF with 9 channels to be rejected")
	}

	badRatePath := writeMutated("bad_rate.dsf", func(b []byte) []byte {
		binary.LittleEndian.PutUint32(b[28+28:28+32], 44100)
		return b
	})
	if dec, err := OpenTrackDecoder(&LocalTrack{Path: badRatePath}); err == nil {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
		t.Fatal("expected DSF with 44.1kHz PCM rate to be rejected")
	}

	// 4. Truncated payload (< channels * blockSize) must be rejected.
	truncPath := writeMutated("trunc_payload.dsf", func(b []byte) []byte {
		return b[:28+52+12+1024] // less than 2 * 4096 = 8192 bytes
	})
	if dec, err := OpenTrackDecoder(&LocalTrack{Path: truncPath}); err == nil {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
		t.Fatal("expected DSF with truncated audio block to be rejected")
	}

	// 5. Inflated sampleCount (MaxUint64) on a 2-block DSF must clamp Len() to the actual 2048 PCM frames.
	inflatedPath := writeMutated("inflated_samples.dsf", func(b []byte) []byte {
		binary.LittleEndian.PutUint64(b[28+36:28+44], math.MaxUint64)
		binary.LittleEndian.PutUint64(b[28+52+4:28+52+12], math.MaxUint64)
		return b
	})
	dec, err := OpenTrackDecoder(&LocalTrack{Path: inflatedPath})
	if err != nil {
		t.Fatalf("expected DSF with clamped sampleCount to open, got err=%v", err)
	}
	defer func() {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	if dec.Len() != 2048 {
		t.Fatalf("expected clamped DSF decoder Len=2048 frames, got %d", dec.Len())
	}
	buf := make([]float32, 4096*2)
	n, rerr := dec.Read(buf)
	if n != 2048*2 || (rerr != nil && rerr != io.EOF) {
		t.Fatalf("expected 4096 float32 samples from clamped 2-block DSF, got n=%d err=%v", n, rerr)
	}
	if n2, err2 := dec.Read(buf[:16]); n2 != 0 || err2 != io.EOF {
		t.Fatalf("expected EOF after clamped DSF payload, got n=%d err=%v", n2, err2)
	}
}

// TestBinaryTagParsersRejectOverflowingSubchunkLengths checks that DFF, RF64 WAV, and MP4 binary parsers validate 64-bit subchunk lengths before int conversion so huge uint64 sizes cannot overflow offsets and panic.
func TestBinaryTagParsersRejectOverflowingSubchunkLengths(t *testing.T) {
	dir := t.TempDir()

	for _, badSz := range []uint64{0x7fffffffffffffff, 0xffffffffffffffff} {
		// Malformed DFF with overflowing subchunks inside both PROP and DIIN.
		var prop bytes.Buffer
		prop.WriteString("SND ")
		prop.WriteString("FS  ")
		_ = binary.Write(&prop, binary.BigEndian, uint64(4))
		_ = binary.Write(&prop, binary.BigEndian, uint32(2822400))
		prop.WriteString("CHNL")
		_ = binary.Write(&prop, binary.BigEndian, uint64(6))
		_ = binary.Write(&prop, binary.BigEndian, uint16(1))
		prop.WriteString("SLFT")
		prop.WriteString("CMPR")
		_ = binary.Write(&prop, binary.BigEndian, badSz)
		prop.WriteString("DSD ")

		var diin bytes.Buffer
		diin.WriteString("DITI")
		_ = binary.Write(&diin, binary.BigEndian, badSz)
		diin.WriteString("boom")

		var frm bytes.Buffer
		frm.WriteString("DSD ")
		writeChunk64 := func(id string, body []byte) {
			frm.WriteString(id)
			_ = binary.Write(&frm, binary.BigEndian, uint64(len(body)))
			frm.Write(body)
			if len(body)%2 != 0 {
				frm.WriteByte(0)
			}
		}
		writeChunk64("PROP", prop.Bytes())
		writeChunk64("DIIN", diin.Bytes())
		frm.WriteString("DSD ")
		_ = binary.Write(&frm, binary.BigEndian, badSz)
		frm.Write(bytes.Repeat([]byte{0xAA}, 64))

		var dff bytes.Buffer
		dff.WriteString("FRM8")
		_ = binary.Write(&dff, binary.BigEndian, uint64(frm.Len()))
		dff.Write(frm.Bytes())

		dffPath := filepath.Join(dir, fmt.Sprintf("overflow_%x.dff", badSz))
		if err := os.WriteFile(dffPath, dff.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		meta, err := parseAudioFile(dffPath, int64(dff.Len()))
		if err != nil || meta == nil || meta.spec.SampleRate != 2822400 {
			t.Fatalf("expected malformed DFF subchunk (0x%x) to parse cleanly without panic, err=%v", badSz, err)
		}
		dec, err := OpenTrackDecoder(&LocalTrack{Path: dffPath})
		if err != nil {
			t.Fatalf("expected malformed DFF subchunk (0x%x) decoder open without panic, err=%v", badSz, err)
		}
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}

		// Malformed RF64 WAV with overflowing ds64 data size.
		var rf64 bytes.Buffer
		rf64.WriteString("RF64")
		_ = binary.Write(&rf64, binary.LittleEndian, uint32(0xFFFFFFFF))
		rf64.WriteString("WAVE")
		rf64.WriteString("ds64")
		_ = binary.Write(&rf64, binary.LittleEndian, uint32(28))
		var ds64 [28]byte
		binary.LittleEndian.PutUint64(ds64[0:8], badSz)
		binary.LittleEndian.PutUint64(ds64[8:16], badSz)
		rf64.Write(ds64[:])
		rf64.WriteString("fmt ")
		_ = binary.Write(&rf64, binary.LittleEndian, uint32(16))
		var fmt16 [16]byte
		binary.LittleEndian.PutUint16(fmt16[0:2], 1)
		binary.LittleEndian.PutUint16(fmt16[2:4], 2)
		binary.LittleEndian.PutUint32(fmt16[4:8], 44100)
		binary.LittleEndian.PutUint32(fmt16[8:12], 176400)
		binary.LittleEndian.PutUint16(fmt16[12:14], 4)
		binary.LittleEndian.PutUint16(fmt16[14:16], 16)
		rf64.Write(fmt16[:])
		rf64.WriteString("data")
		_ = binary.Write(&rf64, binary.LittleEndian, uint32(0xFFFFFFFF))
		rf64.Write(make([]byte, 64))

		wavPath := filepath.Join(dir, fmt.Sprintf("overflow_%x.wav", badSz))
		if err := os.WriteFile(wavPath, rf64.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := parseAudioFile(wavPath, int64(rf64.Len())); err != nil {
			t.Fatalf("expected malformed RF64 WAV (0x%x) to clamp cleanly without panic, err=%v", badSz, err)
		}

		// Malformed MP4 with 64-bit extended box size overflow.
		var mp4 bytes.Buffer
		mp4.Write(mp4Box("ftyp", []byte("M4A \x00\x00\x00\x00")))
		_ = binary.Write(&mp4, binary.BigEndian, uint32(1))
		mp4.WriteString("mdat")
		_ = binary.Write(&mp4, binary.BigEndian, badSz)
		mp4Path := filepath.Join(dir, fmt.Sprintf("overflow_%x.m4a", badSz))
		if err := os.WriteFile(mp4Path, mp4.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _ = parseAudioFile(mp4Path, int64(mp4.Len()))
	}

	// Also verify 32-bit max-length fields (0x7fffffff / 0xffffffff) in FLAC Vorbis/PICTURE, ID3v2, and WavPack APEv2.
	for _, bad32 := range []uint32{0x7fffffff, 0xffffffff} {
		var badVC [12]byte
		binary.LittleEndian.PutUint32(badVC[0:4], 0)
		binary.LittleEndian.PutUint32(badVC[4:8], 1)
		binary.LittleEndian.PutUint32(badVC[8:12], bad32)
		parseVorbisComment(badVC[:], &parsedMeta{})

		var badPic [36]byte
		binary.BigEndian.PutUint32(badPic[0:4], 3)
		binary.BigEndian.PutUint32(badPic[4:8], bad32)
		parseFLACPicture(badPic[:], &parsedMeta{})

		var badID3 bytes.Buffer
		badID3.Write([]byte{'I', 'D', '3', 3, 0, 0x40, 0, 0, 0, 20})
		_ = binary.Write(&badID3, binary.BigEndian, bad32)
		badID3.WriteString("TIT2")
		_ = binary.Write(&badID3, binary.BigEndian, bad32)
		badID3.Write([]byte{0, 0, 3, 'X', 0, 0})
		parseID3v2(badID3.Bytes(), &parsedMeta{})

		var badAPE bytes.Buffer
		_ = binary.Write(&badAPE, binary.LittleEndian, bad32)
		_ = binary.Write(&badAPE, binary.LittleEndian, uint32(0))
		badAPE.WriteString("Title\x00val")
		var footer [32]byte
		copy(footer[0:8], "APETAGEX")
		binary.LittleEndian.PutUint32(footer[12:16], uint32(badAPE.Len()+32))
		binary.LittleEndian.PutUint32(footer[16:20], bad32)
		badAPE.Write(footer[:])
		parseAPEv2Tail(bytes.NewReader(badAPE.Bytes()), int64(badAPE.Len()), &parsedMeta{})
	}
}

// TestInFlightScanDoesNotRestoreConcurrentlyRemovedFolder checks that removing a watched folder while Scan is in flight prevents its files and CUEs from being committed or persisted (fixing Scan overwriting RemoveFolder).
func TestInFlightScanDoesNotRestoreConcurrentlyRemovedFolder(t *testing.T) {
	root := t.TempDir()
	keepDir := filepath.Join(root, "keep")
	dropDir := filepath.Join(root, "drop")
	if err := os.MkdirAll(keepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(keepDir, "01 - Keep.flac"), buildSyntheticFLAC(44100, 2, 16, 44100*10, []string{
		"TITLE=Keep Song",
		"ARTIST=Keep Artist",
		"ALBUM=Keep Album",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropDir, "01 - Drop.flac"), buildSyntheticFLAC(44100, 2, 16, 44100*10, []string{
		"TITLE=Drop Song",
		"ARTIST=Drop Artist",
		"ALBUM=Drop Album",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	dropPCM := make([]byte, 44100*2*4)
	if err := os.WriteFile(filepath.Join(dropDir, "disc.wav"), buildPCM16WAV(44100, 2, dropPCM), 0o644); err != nil {
		t.Fatal(err)
	}
	dropCue := "PERFORMER \"Drop Artist\"\nTITLE \"Drop Cue Album\"\nFILE \"disc.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"Drop Cue Track\"\n    INDEX 01 00:00:00\n"
	if err := os.WriteFile(filepath.Join(dropDir, "disc.cue"), []byte(dropCue), 0o644); err != nil {
		t.Fatal(err)
	}

	idxPath := filepath.Join(root, "index.json")
	covDir := filepath.Join(root, "covers")
	lib := NewLibrary(idxPath, covDir)
	lib.AddFolders(keepDir, dropDir)

	var removedOnce sync.Once
	prevHook := onFileParsedHook
	onFileParsedHook = func(string) {
		removedOnce.Do(func() {
			lib.RemoveFolder(dropDir)
		})
	}
	defer func() { onFileParsedHook = prevHook }()

	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	tracks := lib.Tracks("", "", "")
	if len(tracks) != 1 || tracks[0].Title != "Keep Song" {
		t.Fatalf("expected only Keep Song after concurrent RemoveFolder, got %+v", tracks)
	}

	reloaded := NewLibrary(idxPath, covDir)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	reloadedTracks := reloaded.Tracks("", "", "")
	if len(reloadedTracks) != 1 || reloadedTracks[0].Title != "Keep Song" {
		t.Fatalf("expected persisted index to exclude concurrently removed folder, got %+v", reloadedTracks)
	}
}

// TestUnreadableMusicRootPreservesCachedEntriesUnlikeEmptyRoot checks that an unavailable music root returns an error from Scan while preserving its cached tracks, whereas a readable empty root removes deleted tracks.
func TestUnreadableMusicRootPreservesCachedEntriesUnlikeEmptyRoot(t *testing.T) {
	root := t.TempDir()
	localRoot := filepath.Join(root, "local")
	nasRoot := filepath.Join(root, "nas")
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nasRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(localRoot, "01 - Local1.flac"), buildSyntheticFLAC(44100, 2, 16, 44100*15, []string{
		"TITLE=Local One",
		"ARTIST=Artist A",
		"ALBUM=Local Album",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nasRoot, "01 - NAS.flac"), buildSyntheticFLAC(96000, 2, 24, 96000*20, []string{
		"TITLE=NAS Master",
		"ARTIST=Artist B",
		"ALBUM=NAS Album",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	nasPCM := make([]byte, 44100*2*4)
	if err := os.WriteFile(filepath.Join(nasRoot, "nasdisc.wav"), buildPCM16WAV(44100, 2, nasPCM), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nasRoot, "nasdisc.cue"), []byte("PERFORMER \"Artist B\"\nTITLE \"NAS Cue\"\nFILE \"nasdisc.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"NAS Cue Track\"\n    INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	idxPath := filepath.Join(root, "index.json")
	covDir := filepath.Join(root, "covers")
	lib := NewLibrary(idxPath, covDir)
	lib.AddFolders(localRoot, nasRoot)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan failed: %v", err)
	}
	if got := len(lib.Tracks("", "", "")); got != 3 {
		t.Fatalf("expected 3 initial tracks across local + NAS roots, got %d", got)
	}

	// Take nasRoot offline while adding a second track to readable localRoot.
	if err := os.RemoveAll(nasRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "02 - Local2.flac"), buildSyntheticFLAC(44100, 2, 16, 44100*15, []string{
		"TITLE=Local Two",
		"ARTIST=Artist A",
		"ALBUM=Local Album",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	scanErr := lib.Scan(context.Background(), nil)
	if scanErr == nil || !strings.Contains(scanErr.Error(), nasRoot) {
		t.Fatalf("expected Scan to return descriptive error mentioning offline root %q, got %v", nasRoot, scanErr)
	}
	if got := len(lib.Tracks("", "", "")); got != 4 {
		t.Fatalf("expected 2 local tracks + 2 preserved NAS tracks (total 4), got %d", got)
	}
	if len(lib.Tracks("NAS Master", "", "")) != 1 || len(lib.Tracks("NAS Cue Track", "", "cue")) != 1 {
		t.Fatal("expected both standalone and CUE tracks under offline NAS root to be preserved")
	}

	// Recreate nasRoot as a genuinely empty readable folder -> Scan should succeed and prune NAS tracks.
	if err := os.MkdirAll(nasRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("expected Scan on empty readable folder to succeed, got %v", err)
	}
	if got := len(lib.Tracks("", "", "")); got != 2 {
		t.Fatalf("expected empty readable nasRoot to prune deleted NAS tracks leaving 2 local tracks, got %d", got)
	}
}

// TestIncrementalScanInvalidatesChangedFolderSidecarCovers checks that adding, replacing, or deleting folder cover artwork updates cached standalone and CUE tracks without re-parsing unchanged audio files, while preserving embedded covers.
func TestIncrementalScanInvalidatesChangedFolderSidecarCovers(t *testing.T) {
	root := t.TempDir()
	flacDir := filepath.Join(root, "music", "SidecarFLAC")
	cueDir := filepath.Join(root, "music", "SidecarCUE")
	embDir := filepath.Join(root, "music", "EmbeddedFLAC")
	for _, d := range []string{flacDir, cueDir, embDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(flacDir, "01 - Track.flac"), buildSyntheticFLAC(44100, 2, 16, 44100*10, []string{
		"TITLE=Sidecar Song",
		"ARTIST=Trio",
		"ALBUM=Sidecar Album",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	pcm := make([]byte, 44100*2*4)
	if err := os.WriteFile(filepath.Join(cueDir, "disc.wav"), buildPCM16WAV(44100, 2, pcm), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cueDir, "disc.cue"), []byte("PERFORMER \"Trio\"\nTITLE \"Sidecar CUE Album\"\nFILE \"disc.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"Sidecar CUE Song\"\n    INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	embBytes := []byte{0x89, 'P', 'N', 'G', 'E', 'M', 'B', 'E', 'D'}
	if err := os.WriteFile(filepath.Join(embDir, "01 - Embedded.flac"), buildSyntheticFLAC(44100, 2, 16, 44100*10, []string{
		"TITLE=Embedded Song",
		"ARTIST=Trio",
		"ALBUM=Embedded Album",
	}, []flacPicSpec{{picType: 3, mime: "image/png", data: embBytes}}), 0o644); err != nil {
		t.Fatal(err)
	}

	var parsedCount atomic.Int32
	prevHook := onFileParsedHook
	onFileParsedHook = func(string) { parsedCount.Add(1) }
	defer func() { onFileParsedHook = prevHook }()

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolder(filepath.Join(root, "music"))
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan failed: %v", err)
	}
	if tr := lib.Tracks("Sidecar Song", "", "")[0]; tr.CoverID != "" {
		t.Fatalf("expected initially empty CoverID, got %q", tr.CoverID)
	}
	if tr := lib.Tracks("Sidecar CUE Song", "", "cue")[0]; tr.CoverID != "" {
		t.Fatalf("expected initially empty CUE CoverID, got %q", tr.CoverID)
	}

	// 1. Add cover.jpg in all three folders -> standalone & CUE tracks gain coverA without re-parsing FLACs; embedded track keeps embBytes.
	coverA := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'C', 'O', 'V', 'A', 0xFF, 0xD9}
	for _, d := range []string{flacDir, cueDir, embDir} {
		if err := os.WriteFile(filepath.Join(d, "cover.jpg"), coverA, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parsedCount.Store(0)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan after adding cover.jpg failed: %v", err)
	}
	if got := parsedCount.Load(); got != 0 {
		t.Fatalf("expected 0 audio files re-parsed when only sidecar cover changed, got %d", got)
	}
	for _, q := range []string{"Sidecar Song", "Sidecar CUE Song"} {
		tr := lib.Tracks(q, "", "")[0]
		gotBytes, err := lib.CoverBytes(tr.CoverID)
		if err != nil || !bytes.Equal(gotBytes, coverA) {
			t.Fatalf("expected %s to pick up newly added coverA, err=%v", q, err)
		}
	}
	embTr := lib.Tracks("Embedded Song", "", "")[0]
	if gotEmb, err := lib.CoverBytes(embTr.CoverID); err != nil || !bytes.Equal(gotEmb, embBytes) {
		t.Fatalf("expected embedded cover to take precedence over folder cover, err=%v", err)
	}

	// 2. Replace cover.jpg with coverB (and bump mtime) -> standalone & CUE tracks switch to coverB.
	coverB := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'C', 'O', 'V', 'E', 'R', '_', 'B', 0xFF, 0xD9}
	t2 := time.Unix(1_760_000_000, 0)
	for _, d := range []string{flacDir, cueDir} {
		cp := filepath.Join(d, "cover.jpg")
		if err := os.WriteFile(cp, coverB, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(cp, t2, t2); err != nil {
			t.Fatal(err)
		}
	}
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan after replacing cover.jpg failed: %v", err)
	}
	for _, q := range []string{"Sidecar Song", "Sidecar CUE Song"} {
		tr := lib.Tracks(q, "", "")[0]
		gotBytes, err := lib.CoverBytes(tr.CoverID)
		if err != nil || !bytes.Equal(gotBytes, coverB) {
			t.Fatalf("expected %s to update to replaced coverB, err=%v", q, err)
		}
	}

	// 3. Remove cover.jpg -> standalone & CUE tracks clear CoverID, while embedded cover remains intact.
	for _, d := range []string{flacDir, cueDir, embDir} {
		if err := os.Remove(filepath.Join(d, "cover.jpg")); err != nil {
			t.Fatal(err)
		}
	}
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan after deleting cover.jpg failed: %v", err)
	}
	if tr := lib.Tracks("Sidecar Song", "", "")[0]; tr.CoverID != "" {
		t.Fatalf("expected Sidecar Song CoverID cleared after deleting cover.jpg, got %q", tr.CoverID)
	}
	if tr := lib.Tracks("Sidecar CUE Song", "", "cue")[0]; tr.CoverID != "" {
		t.Fatalf("expected Sidecar CUE Song CoverID cleared after deleting cover.jpg, got %q", tr.CoverID)
	}
	embTr = lib.Tracks("Embedded Song", "", "")[0]
	if gotEmb, err := lib.CoverBytes(embTr.CoverID); err != nil || !bytes.Equal(gotEmb, embBytes) {
		t.Fatalf("expected embedded cover to survive deleting folder cover.jpg, err=%v", err)
	}
}

// TestIncrementalCueScanRevisitsUnresolvedFileReferences checks that incremental CUE scans re-evaluate previously missing FILE entries when their audio arrives later and suppress the newly resolved whole-side audio file.
func TestIncrementalCueScanRevisitsUnresolvedFileReferences(t *testing.T) {
	root := t.TempDir()
	discDir := filepath.Join(root, "vinyl")
	if err := os.MkdirAll(discDir, 0o755); err != nil {
		t.Fatal(err)
	}

	pcm := make([]byte, 44100*4*4) // 4 seconds
	sideAPath := filepath.Join(discDir, "SideA.wav")
	sideBPath := filepath.Join(discDir, "SideB.wav")
	if err := os.WriteFile(sideAPath, buildPCM16WAV(44100, 2, pcm), 0o644); err != nil {
		t.Fatal(err)
	}

	cueText := strings.Join([]string{
		`PERFORMER "Keith Jarrett"`,
		`TITLE "The Köln Concert"`,
		`FILE "SideA.wav" WAVE`,
		`  TRACK 01 AUDIO`,
		`    TITLE "Part I"`,
		`    INDEX 01 00:00:00`,
		`FILE "SideB.wav" WAVE`,
		`  TRACK 02 AUDIO`,
		`    TITLE "Part IIa"`,
		`    INDEX 01 00:00:00`,
		`  TRACK 03 AUDIO`,
		`    TITLE "Part IIb"`,
		`    INDEX 01 00:02:00`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(discDir, "concert.cue"), []byte(cueText), 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolder(discDir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan failed: %v", err)
	}
	if got := len(lib.Tracks("", "", "")); got != 1 {
		t.Fatalf("expected 1 CUE track when only SideA.wav exists, got %d", got)
	}

	// Copy SideB.wav into place without modifying concert.cue or SideA.wav.
	if err := os.WriteFile(sideBPath, buildPCM16WAV(44100, 2, pcm), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("incremental Scan after SideB.wav arrived failed: %v", err)
	}

	allTracks := lib.Tracks("", "", "")
	if len(allTracks) != 3 {
		t.Fatalf("expected 3 sliced CUE tracks (and SideB.wav suppressed as standalone), got %d: %+v", len(allTracks), allTracks)
	}
	for _, tr := range allTracks {
		if !tr.Spec.IsCUE {
			t.Fatalf("expected newly arrived SideB.wav to be suppressed by CUE, found standalone track %+v", tr)
		}
	}
}

// TestCueFrameExactSlicingAndScopedWholeDiscLyrics checks 1/75s CD-frame sample accuracy (00:00:01 -> sample 588 at 44.1kHz) and ensures CUE whole-disc lyrics never leak across sibling .cue files in the same directory.
func TestCueFrameExactSlicingAndScopedWholeDiscLyrics(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "multi_cue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Synthesize Disc1.wav (44.1kHz stereo, 2 CD frames = 1176 samples):
	// Frames [0, 588) = +0.25 (+8192); frames [588, 1176) = -0.50 (-16384).
	// If truncated to 13ms (573 samples), Track 2 would erroneously read 15 samples of +0.25!
	const (
		sr       = 44100
		cdFrame1 = 588 // exact 44100 / 75
		total    = 1176
	)
	disc1PCM := make([]byte, total*4)
	for i := range total {
		v := int16(8192)
		if i >= cdFrame1 {
			v = -16384
		}
		binary.LittleEndian.PutUint16(disc1PCM[i*4:], uint16(v))
		binary.LittleEndian.PutUint16(disc1PCM[i*4+2:], uint16(v))
	}
	if err := os.WriteFile(filepath.Join(dir, "Disc1.wav"), buildPCM16WAV(sr, 2, disc1PCM), 0o644); err != nil {
		t.Fatal(err)
	}
	disc1Cue := strings.Join([]string{
		`PERFORMER "Glenn Gould"`,
		`TITLE "Disc One"`,
		`FILE "Disc1.wav" WAVE`,
		`  TRACK 01 AUDIO`,
		`    TITLE "Prelude One"`,
		`    INDEX 01 00:00:00`,
		`  TRACK 02 AUDIO`,
		`    TITLE "Fugue One"`,
		`    INDEX 01 00:00:01`,
		"",
	}, "\n")
	disc1CuePath := filepath.Join(dir, "Disc1.cue")
	if err := os.WriteFile(disc1CuePath, []byte(disc1Cue), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Disc1.lrc"), []byte("[00:00.00]Disc one opening lyric\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sibling Disc2.cue + Disc2.wav in the SAME folder WITHOUT Disc2.lrc.
	disc2PCM := make([]byte, sr*2*4)
	if err := os.WriteFile(filepath.Join(dir, "Disc2.wav"), buildPCM16WAV(sr, 2, disc2PCM), 0o644); err != nil {
		t.Fatal(err)
	}
	disc2CuePath := filepath.Join(dir, "Disc2.cue")
	if err := os.WriteFile(disc2CuePath, []byte("PERFORMER \"Glenn Gould\"\nTITLE \"Disc Two\"\nFILE \"Disc2.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"Sonata Two\"\n    INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolder(dir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	tr1 := lib.Tracks("Prelude One", "", "cue")[0]
	tr2 := lib.Tracks("Fugue One", "", "cue")[0]
	if !tr1.Spec.HasCueFrames || tr1.Spec.ClipEndFrames != 1 || !tr2.Spec.HasCueFrames || tr2.Spec.ClipStartFrames != 1 {
		t.Fatalf("unexpected CUE frame specs: tr1=%+v tr2=%+v", tr1.Spec, tr2.Spec)
	}
	if tr1.CuePath != filepath.Clean(disc1CuePath) {
		t.Fatalf("expected CuePath=%q, got %q", filepath.Clean(disc1CuePath), tr1.CuePath)
	}

	dec1, _, err := lib.OpenDecoder(tr1.ID)
	if err != nil {
		t.Fatalf("OpenDecoder tr1 failed: %v", err)
	}
	defer func() {
		if c, ok := dec1.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	if dec1.Len() != cdFrame1 {
		t.Fatalf("expected Track 1 exact 1-CD-frame length %d, got %d", cdFrame1, dec1.Len())
	}

	dec2, _, err := lib.OpenDecoder(tr2.ID)
	if err != nil {
		t.Fatalf("OpenDecoder tr2 failed: %v", err)
	}
	defer func() {
		if c, ok := dec2.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	if dec2.Len() != total-cdFrame1 {
		t.Fatalf("expected Track 2 exact length %d starting at sample 588, got %d", total-cdFrame1, dec2.Len())
	}
	firstPair := make([]float32, 2)
	if n, err := dec2.Read(firstPair); err != nil || n != 2 {
		t.Fatalf("Track 2 Read failed: n=%d err=%v", n, err)
	}
	if math.Abs(float64(firstPair[0])+0.5) > 1e-4 {
		t.Fatalf("expected Track 2 sample 0 at exact frame 588 to be -0.50 (not pre-boundary +0.25), got %f", firstPair[0])
	}

	// Verify Disc1 track reads Disc1.lrc while Disc2 track in the same directory does NOT read Disc1.lrc.
	lrc1, _ := lib.Lyrics(tr1.ID)
	if !strings.Contains(lrc1, "Disc one opening lyric") {
		t.Fatalf("expected Disc1 track to resolve Disc1.lrc, got %q", lrc1)
	}
	disc2Tr := lib.Tracks("Sonata Two", "", "cue")[0]
	if disc2Tr.CuePath != filepath.Clean(disc2CuePath) {
		t.Fatalf("expected Disc2 CuePath=%q, got %q", filepath.Clean(disc2CuePath), disc2Tr.CuePath)
	}
	lrc2, trans2 := lib.Lyrics(disc2Tr.ID)
	if lrc2 != "" || trans2 != "" {
		t.Fatalf("expected Disc2 track without Disc2.lrc to return empty lyrics instead of sibling Disc1.lrc, got (%q, %q)", lrc2, trans2)
	}
}

// TestLibraryRevisionAndTrackByIDWithoutDecoder checks monotonic Revision increments across library mutations and verifies TrackByID returns cloned metadata for native HQPlayer formats (.wv) without opening a PCM decoder.
func TestLibraryRevisionAndTrackByIDWithoutDecoder(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "wv")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wvCover := []byte{0x89, 'P', 'N', 'G', 'H', 'Q', 'P', 'W', 'V'}
	wvPath := filepath.Join(musicDir, "01 - Goldberg.wv")
	if err := os.WriteFile(wvPath, buildSyntheticWavPack(96000, 24, 2, 96000*60, "Aria", "Glenn Gould", "Goldberg Variations", wvCover), 0o644); err != nil {
		t.Fatal(err)
	}

	idxPath := filepath.Join(root, "index.json")
	covDir := filepath.Join(root, "covers")
	lib := NewLibrary(idxPath, covDir)
	if rev := lib.Revision(); rev != 0 {
		t.Fatalf("expected initial Revision 0, got %d", rev)
	}

	if !lib.AddFolder(musicDir) {
		t.Fatal("expected AddFolder to return true")
	}
	revAfterAdd := lib.Revision()
	if revAfterAdd <= 0 {
		t.Fatalf("expected Revision > 0 after AddFolder, got %d", revAfterAdd)
	}
	// Duplicate AddFolder is a no-op and must not bump Revision.
	if lib.AddFolder(musicDir) || lib.Revision() != revAfterAdd {
		t.Fatalf("expected duplicate AddFolder not to bump Revision (got %d, want %d)", lib.Revision(), revAfterAdd)
	}

	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	revAfterScan := lib.Revision()
	if revAfterScan <= revAfterAdd {
		t.Fatalf("expected Scan to increment Revision (%d -> %d)", revAfterAdd, revAfterScan)
	}

	// TrackByID must succeed on .wv without opening a Go PCM decoder, and return an isolated clone.
	all := lib.Tracks("", "", "")
	if len(all) != 1 {
		t.Fatalf("expected 1 WavPack track, got %d", len(all))
	}
	tr, ok := lib.TrackByID(all[0].ID)
	if !ok || tr == nil {
		t.Fatal("expected TrackByID to find WavPack track")
	}
	if !tr.NativeHQP() || tr.Spec.Codec != "WV" || tr.CoverID == "" {
		t.Fatalf("unexpected WavPack TrackByID metadata: %+v", tr)
	}
	tr.Artists[0] = "Mutated Caller Copy"
	trAgain, ok := lib.TrackByID(all[0].ID)
	if !ok || trAgain.Artists[0] != "Glenn Gould" {
		t.Fatalf("expected TrackByID to return a deep clone, got %+v", trAgain)
	}

	// ApplyRoonGenres bumps Revision only when matching at least one album.
	if matched := lib.ApplyRoonGenres([]RoonAlbumGenre{{Album: "Unmatched", Artist: "Nobody", Genres: []string{"Pop"}}}); matched != 0 || lib.Revision() != revAfterScan {
		t.Fatalf("expected unmatched ApplyRoonGenres not to bump Revision")
	}
	if matched := lib.ApplyRoonGenres([]RoonAlbumGenre{{Album: "Goldberg Variations", Artist: "Glenn Gould", Genres: []string{"Classical", "Baroque"}}}); matched != 1 {
		t.Fatalf("expected ApplyRoonGenres to match 1 album, matched %d", matched)
	}
	revAfterRoon := lib.Revision()
	if revAfterRoon <= revAfterScan {
		t.Fatalf("expected matched ApplyRoonGenres to bump Revision (%d -> %d)", revAfterScan, revAfterRoon)
	}

	// Reloading into a fresh Library increments its Revision from 0 -> 1.
	reloaded := NewLibrary(idxPath, covDir)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if reloaded.Revision() == 0 {
		t.Fatal("expected Load on persisted index to increment Revision")
	}

	// RemoveFolder bumps Revision.
	if !lib.RemoveFolder(musicDir) || lib.Revision() <= revAfterRoon {
		t.Fatalf("expected RemoveFolder to bump Revision above %d, got %d", revAfterRoon, lib.Revision())
	}
}

// TestConcurrentScanAndApplyRoonGenresOnCueSheet checks that an in-flight Scan and concurrent ApplyRoonGenres isolate CUE track slices without racing or dropping genres (fixing unsynchronized prevCues sharing with applyPersistedRoonGenresLocked).
func TestConcurrentScanAndApplyRoonGenresOnCueSheet(t *testing.T) {
	root := t.TempDir()
	cueDir := filepath.Join(root, "music", "cue_album")
	flacDir := filepath.Join(root, "music", "standalone")
	for _, d := range []string{cueDir, flacDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pcm := make([]byte, 44100*2*4)
	if err := os.WriteFile(filepath.Join(cueDir, "disc.wav"), buildPCM16WAV(44100, 2, pcm), 0o644); err != nil {
		t.Fatal(err)
	}
	cueText := strings.Join([]string{
		`PERFORMER "Bill Evans Trio"`,
		`TITLE "Sunday at the Village Vanguard"`,
		`FILE "disc.wav" WAVE`,
		`  TRACK 01 AUDIO`,
		`    TITLE "Gloria's Step"`,
		`    INDEX 01 00:00:00`,
		`  TRACK 02 AUDIO`,
		`    TITLE "Alice in Wonderland"`,
		`    INDEX 01 00:01:00`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cueDir, "disc.cue"), []byte(cueText), 0o644); err != nil {
		t.Fatal(err)
	}
	flacPath := filepath.Join(flacDir, "01 - Trigger.flac")
	if err := os.WriteFile(flacPath, buildSyntheticFLAC(44100, 2, 16, 44100*10, []string{
		"TITLE=Trigger",
		"ARTIST=Soloist",
		"ALBUM=Trigger Album",
	}, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolder(filepath.Join(root, "music"))
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan failed: %v", err)
	}

	roonSets := [][]string{
		{"Jazz"},
		{"Post-Bop"},
		{"Cool Jazz"},
		{"Live Jazz"},
	}
	prevHook := onFileParsedHook
	onFileParsedHook = func(string) {
		for _, gs := range roonSets {
			lib.ApplyRoonGenres([]RoonAlbumGenre{{
				Album:  "Sunday at the Village Vanguard",
				Artist: "Bill Evans Trio",
				Genres: gs,
			}})
		}
	}
	defer func() { onFileParsedHook = prevHook }()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
				lib.ApplyRoonGenres([]RoonAlbumGenre{{
					Album:  "Sunday at the Village Vanguard",
					Artist: "Bill Evans Trio",
					Genres: roonSets[i%len(roonSets)],
				}})
				_ = lib.Tracks("", "", "cue")
			}
		}
	}()

	for round := range 4 {
		cov := []byte{0xFF, 0xD8, 0xFF, 0xE0, byte('A' + round), 0xFF, 0xD9}
		covPath := filepath.Join(cueDir, "cover.jpg")
		if err := os.WriteFile(covPath, cov, 0o644); err != nil {
			close(done)
			wg.Wait()
			t.Fatal(err)
		}
		ts := time.Unix(1_760_000_000+int64(round)*10, 0)
		_ = os.Chtimes(covPath, ts, ts)
		_ = os.Chtimes(flacPath, ts, ts)
		if err := lib.Scan(context.Background(), nil); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("concurrent Scan round %d failed: %v", round, err)
		}
	}
	close(done)
	wg.Wait()

	cueTracks := lib.Tracks("", "", "cue")
	if len(cueTracks) != 2 {
		t.Fatalf("expected 2 CUE tracks after concurrent scans, got %d", len(cueTracks))
	}
	for _, tr := range cueTracks {
		if tr.CoverID == "" {
			t.Fatalf("expected CUE track CoverID populated, got %+v", tr)
		}
		for _, wantG := range []string{"Jazz", "Post-Bop", "Cool Jazz", "Live Jazz"} {
			if !slices.Contains(tr.Genres, wantG) {
				t.Fatalf("CUE track %q missing merged Roon genre %q in %v", tr.Title, wantG, tr.Genres)
			}
		}
	}
}

// TestDataOnlyCueDoesNotSuppressSiblingAudioFile checks that a data-only MODE1/2352 CUE sheet keeps AllRefs tracked without hiding a basename-matching standalone audio file (fixing premature Consumed marking before AUDIO track extraction).
func TestDataOnlyCueDoesNotSuppressSiblingAudioFile(t *testing.T) {
	root := t.TempDir()
	dataOnlyDir := filepath.Join(root, "music", "data_only")
	mixedDir := filepath.Join(root, "music", "mixed_cue")
	for _, d := range []string{dataOnlyDir, mixedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pcm := make([]byte, 44100*2*4)
	// 1. Data-only CUE ("game.bin" -> basename fallback matches "game.wav"), zero AUDIO tracks.
	if err := os.WriteFile(filepath.Join(dataOnlyDir, "game.wav"), buildPCM16WAV(44100, 2, pcm), 0o644); err != nil {
		t.Fatal(err)
	}
	dataCue := strings.Join([]string{
		`FILE "game.bin" BINARY`,
		`  TRACK 01 MODE1/2352`,
		`    INDEX 01 00:00:00`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dataOnlyDir, "game.cue"), []byte(dataCue), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Mixed multi-FILE CUE: FILE 1 is MODE1/2352 matching bonus_data.wav; FILE 2 has an AUDIO track on disc_audio.wav.
	if err := os.WriteFile(filepath.Join(mixedDir, "bonus_data.wav"), buildPCM16WAV(44100, 2, pcm), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mixedDir, "disc_audio.wav"), buildPCM16WAV(44100, 2, pcm), 0o644); err != nil {
		t.Fatal(err)
	}
	mixedCue := strings.Join([]string{
		`PERFORMER "OST Composer"`,
		`TITLE "Enhanced Disc"`,
		`FILE "bonus_data.bin" BINARY`,
		`  TRACK 01 MODE1/2352`,
		`    INDEX 01 00:00:00`,
		`FILE "disc_audio.wav" WAVE`,
		`  TRACK 02 AUDIO`,
		`    TITLE "Main Theme"`,
		`    INDEX 01 00:00:00`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(mixedDir, "mixed.cue"), []byte(mixedCue), 0o644); err != nil {
		t.Fatal(err)
	}

	var parsedCount, cueParsedCount atomic.Int64
	prevHook := onFileParsedHook
	prevCueHook := onCueParsedHook
	onFileParsedHook = func(string) { parsedCount.Add(1) }
	onCueParsedHook = func(string) { cueParsedCount.Add(1) }
	defer func() {
		onFileParsedHook = prevHook
		onCueParsedHook = prevCueHook
	}()

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolder(filepath.Join(root, "music"))
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan failed: %v", err)
	}
	if cueParsedCount.Load() == 0 {
		t.Fatal("expected initial scan to parse CUE files")
	}

	if got := lib.Tracks("game", "", ""); len(got) != 1 || got[0].Spec.IsCUE {
		t.Fatalf("expected standalone game.wav to remain visible alongside MODE1/2352 game.cue, got %+v", got)
	}
	if got := lib.Tracks("bonus_data", "", ""); len(got) != 1 || got[0].Spec.IsCUE {
		t.Fatalf("expected standalone bonus_data.wav from data-only FILE block to remain visible, got %+v", got)
	}
	if got := lib.Tracks("Main Theme", "", "cue"); len(got) != 1 {
		t.Fatalf("expected AUDIO FILE block to yield Main Theme CUE track, got %+v", got)
	}
	if got := lib.Tracks("disc_audio", "", ""); len(got) != 0 {
		t.Fatalf("expected whole-disc disc_audio.wav to be suppressed by its AUDIO CUE track, got %+v", got)
	}

	// Incremental rescan must reuse both the data-only CUE cache and standalone file cache without re-parsing.
	parsedCount.Store(0)
	cueParsedCount.Store(0)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("incremental Scan failed: %v", err)
	}
	if got := parsedCount.Load(); got != 0 {
		t.Fatalf("expected 0 standalone files re-parsed on unchanged incremental scan, got %d", got)
	}
	if got := cueParsedCount.Load(); got != 0 {
		t.Fatalf("expected 0 CUE sheets re-parsed on unchanged incremental scan, got %d", got)
	}
	if got := lib.Tracks("game", "", ""); len(got) != 1 || got[0].Spec.IsCUE {
		t.Fatalf("expected game.wav to remain visible after incremental scan, got %+v", got)
	}
}

// TestDSD1024AndDSD2048x48TagAndPreviewDecode checks that 44.1kHz- and 48kHz-family DSD1024 and DSD2048 DSF/DFF streams parse and decimate cleanly up to 98.304 MHz while rejecting higher rates (fixing the 45.1584 MHz ceiling rejecting DSD1024x48).
func TestDSD1024AndDSD2048x48TagAndPreviewDecode(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name      string
		rate      int
		isDFF     bool
		wantCodec string
		wantOutSR int
	}{
		{name: "dsd1024_44.dsf", rate: 45158400, isDFF: false, wantCodec: "DSD1024", wantOutSR: 88200},
		{name: "dsd1024_48.dsf", rate: 49152000, isDFF: false, wantCodec: "DSD1024", wantOutSR: 96000},
		{name: "dsd1024_48.dff", rate: 49152000, isDFF: true, wantCodec: "DSD1024", wantOutSR: 96000},
		{name: "dsd2048_44.dsf", rate: 90316800, isDFF: false, wantCodec: "DSD2048", wantOutSR: 88200},
		{name: "dsd2048_48.dsf", rate: 98304000, isDFF: false, wantCodec: "DSD2048", wantOutSR: 96000},
		{name: "dsd2048_48.dff", rate: 98304000, isDFF: true, wantCodec: "DSD2048", wantOutSR: 96000},
	}

	for _, tc := range cases {
		var raw []byte
		if tc.isDFF {
			raw = buildSyntheticDFF(tc.rate, 2, 8192, tc.name, "High Rate DSD")
		} else {
			raw = buildSyntheticDSF(tc.rate, 2, 1, 1000.0, nil)
		}
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		meta, err := parseAudioFile(p, int64(len(raw)))
		if err != nil {
			t.Fatalf("parseAudioFile(%s @ %d Hz) failed: %v", tc.name, tc.rate, err)
		}
		if meta.spec.SampleRate != tc.rate || meta.spec.Codec != tc.wantCodec || !meta.spec.IsDSD {
			t.Fatalf("%s spec = %+v, want rate=%d codec=%s", tc.name, meta.spec, tc.rate, tc.wantCodec)
		}

		dec, err := OpenTrackDecoder(&LocalTrack{Path: p, Spec: meta.spec})
		if err != nil {
			t.Fatalf("OpenTrackDecoder(%s @ %d Hz) failed: %v", tc.name, tc.rate, err)
		}
		if dec.SampleRate() != tc.wantOutSR || dec.Len() <= 0 {
			if c, ok := dec.(io.Closer); ok {
				_ = c.Close()
			}
			t.Fatalf("%s decoder sr=%d len=%d, want sr=%d len>0", tc.name, dec.SampleRate(), dec.Len(), tc.wantOutSR)
		}
		buf := make([]float32, 32)
		n, rerr := dec.Read(buf)
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
		if n == 0 || (rerr != nil && rerr != io.EOF) {
			t.Fatalf("%s Read failed: n=%d err=%v", tc.name, n, rerr)
		}
	}

	// Above 98304000 Hz must still be rejected by both parser and decoder.
	overBytes := buildSyntheticDSF(2822400, 2, 1, 1000.0, nil)
	binary.LittleEndian.PutUint32(overBytes[28+28:28+32], 98304001)
	overPath := filepath.Join(dir, "too_high.dsf")
	if err := os.WriteFile(overPath, overBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseAudioFile(overPath, int64(len(overBytes))); err == nil {
		t.Fatal("expected DSF above 98304000 Hz to be rejected by parseAudioFile")
	}
	if dec, err := OpenTrackDecoder(&LocalTrack{Path: overPath}); err == nil {
		if c, ok := dec.(io.Closer); ok {
			_ = c.Close()
		}
		t.Fatal("expected DSF above 98304000 Hz to be rejected by OpenTrackDecoder")
	}
}

// TestFolderCoverDeduplicatesInFlightReadsAndFallsBackOnUnreadableSidecar checks that parallel scan workers read an album sidecar cover once and fall through to the next candidate when the preferred cover is unreadable (fixing duplicate concurrent cover reads and caching unreadable sidecars).
func TestFolderCoverDeduplicatesInFlightReadsAndFallsBackOnUnreadableSidecar(t *testing.T) {
	root := t.TempDir()
	albumDir := filepath.Join(root, "music", "Octet Album")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("%02d - Movement %d.flac", i, i)
		if err := os.WriteFile(filepath.Join(albumDir, name), buildSyntheticFLAC(96000, 2, 24, 96000*10, []string{
			fmt.Sprintf("TITLE=Movement %d", i),
			"ARTIST=Chamber Octet",
			"ALBUM=Octet Sessions",
			"ALBUMARTIST=Chamber Octet",
			fmt.Sprintf("TRACKNUMBER=%d", i),
		}, nil), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	coverJPGBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'C', 'O', 'V', 'J', 'P', 'G', 0xFF, 0xD9}
	folderPNGBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 'F', 'L', 'D', 'R'}
	coverJPGPath := filepath.Clean(filepath.Join(albumDir, "cover.jpg"))
	folderPNGPath := filepath.Clean(filepath.Join(albumDir, "folder.png"))
	if err := os.WriteFile(coverJPGPath, coverJPGBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(folderPNGPath, folderPNGBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		blockCoverJPG atomic.Bool
		coverReads    atomic.Int32
		folderReads   atomic.Int32
	)
	blockCoverJPG.Store(true)

	origReadCover := readCoverFile
	readCoverFile = func(p string) ([]byte, error) {
		switch canonicalPathKey(p) {
		case canonicalPathKey(coverJPGPath):
			coverReads.Add(1)
			if blockCoverJPG.Load() {
				return nil, os.ErrPermission
			}
			return os.ReadFile(p)
		case canonicalPathKey(folderPNGPath):
			folderReads.Add(1)
			time.Sleep(10 * time.Millisecond) // hold in-flight so all 8 scan workers converge
			return os.ReadFile(p)
		default:
			return os.ReadFile(p)
		}
	}
	defer func() { readCoverFile = origReadCover }()

	lib := NewLibrary(filepath.Join(root, "index.json"), filepath.Join(root, "covers"))
	lib.AddFolder(filepath.Join(root, "music"))
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan failed: %v", err)
	}

	if got := coverReads.Load(); got != 1 {
		t.Fatalf("expected unreadable cover.jpg to be probed once across 8 workers, got %d", got)
	}
	if got := folderReads.Load(); got != 1 {
		t.Fatalf("expected fallback folder.png to be read & hashed once across 8 workers, got %d", got)
	}

	tracks := lib.Tracks("", "", "")
	if len(tracks) != 8 {
		t.Fatalf("expected 8 indexed tracks, got %d", len(tracks))
	}
	for _, tr := range tracks {
		gotCov, err := lib.CoverBytes(tr.CoverID)
		if err != nil || !bytes.Equal(gotCov, folderPNGBytes) {
			t.Fatalf("track %q did not receive fallback folder.png cover: err=%v", tr.Title, err)
		}
	}

	// Second incremental scan while cover.jpg remains unreadable must preserve folder.png.
	folderReads.Store(0)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("second Scan failed: %v", err)
	}
	if got := folderReads.Load(); got != 1 {
		t.Fatalf("expected incremental fallback check to read folder.png once across 8 cached tracks, got %d", got)
	}
	for _, tr := range lib.Tracks("", "", "") {
		gotCov, err := lib.CoverBytes(tr.CoverID)
		if err != nil || !bytes.Equal(gotCov, folderPNGBytes) {
			t.Fatalf("track %q lost fallback folder.png cover on incremental scan: err=%v", tr.Title, err)
		}
	}

	// Once cover.jpg becomes readable, the next incremental scan upgrades all 8 tracks to cover.jpg.
	blockCoverJPG.Store(false)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("third Scan failed: %v", err)
	}
	for _, tr := range lib.Tracks("", "", "") {
		gotCov, err := lib.CoverBytes(tr.CoverID)
		if err != nil || !bytes.Equal(gotCov, coverJPGBytes) {
			t.Fatalf("track %q did not upgrade to recovered cover.jpg: err=%v", tr.Title, err)
		}
	}
}

// --- Synthetic Audiophile Test Fixture Builders ---

type flacPicSpec struct {
	picType uint32
	mime    string
	data    []byte
}

func buildSyntheticFLAC(sr, ch, bps int, totalSamples int64, comments []string, pics []flacPicSpec) []byte {
	var out bytes.Buffer
	out.WriteString("fLaC")

	totalBlocks := 1
	if len(comments) > 0 {
		totalBlocks++
	}
	totalBlocks += len(pics)
	curBlock := 0
	writeBlock := func(btype byte, body []byte) {
		curBlock++
		hdr := btype
		if curBlock == totalBlocks {
			hdr |= 0x80
		}
		n := len(body)
		out.Write([]byte{hdr, byte(n >> 16), byte(n >> 8), byte(n)})
		out.Write(body)
	}

	var si [34]byte
	binary.BigEndian.PutUint16(si[0:2], 4096)
	binary.BigEndian.PutUint16(si[2:4], 4096)
	v := (uint64(sr)&0xFFFFF)<<44 | (uint64(ch-1)&0x07)<<41 | (uint64(bps-1)&0x1F)<<36 | (uint64(totalSamples) & 0xFFFFFFFFF)
	binary.BigEndian.PutUint64(si[10:18], v)
	writeBlock(0, si[:])

	if len(comments) > 0 {
		var vc bytes.Buffer
		vendor := "ytb-test"
		_ = binary.Write(&vc, binary.LittleEndian, uint32(len(vendor)))
		vc.WriteString(vendor)
		_ = binary.Write(&vc, binary.LittleEndian, uint32(len(comments)))
		for _, c := range comments {
			_ = binary.Write(&vc, binary.LittleEndian, uint32(len(c)))
			vc.WriteString(c)
		}
		writeBlock(4, vc.Bytes())
	}

	for _, p := range pics {
		var pb bytes.Buffer
		_ = binary.Write(&pb, binary.BigEndian, p.picType)
		_ = binary.Write(&pb, binary.BigEndian, uint32(len(p.mime)))
		pb.WriteString(p.mime)
		_ = binary.Write(&pb, binary.BigEndian, uint32(0)) // desc len
		_ = binary.Write(&pb, binary.BigEndian, uint32(500))
		_ = binary.Write(&pb, binary.BigEndian, uint32(500))
		_ = binary.Write(&pb, binary.BigEndian, uint32(24))
		_ = binary.Write(&pb, binary.BigEndian, uint32(0))
		_ = binary.Write(&pb, binary.BigEndian, uint32(len(p.data)))
		pb.Write(p.data)
		writeBlock(6, pb.Bytes())
	}
	return out.Bytes()
}

func buildID3v23Tag(frames [][2][]byte) []byte {
	var body bytes.Buffer
	for _, fr := range frames {
		body.Write(fr[0][:4])
		_ = binary.Write(&body, binary.BigEndian, uint32(len(fr[1])))
		body.Write([]byte{0, 0})
		body.Write(fr[1])
	}
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

func putPCM24LE(dst []byte, v int32) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v >> 16)
}

func putPCM24BE(dst []byte, v int32) {
	dst[0] = byte(v >> 16)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v)
}

func buildExtensibleWAV24(sr, ch int, pcm []byte, id3 []byte) []byte {
	var chunks bytes.Buffer
	// 40-byte WAVE_FORMAT_EXTENSIBLE fmt chunk
	var fmtChunk [40]byte
	blockAlign := ch * 3
	binary.LittleEndian.PutUint16(fmtChunk[0:2], 0xFFFE)
	binary.LittleEndian.PutUint16(fmtChunk[2:4], uint16(ch))
	binary.LittleEndian.PutUint32(fmtChunk[4:8], uint32(sr))
	binary.LittleEndian.PutUint32(fmtChunk[8:12], uint32(sr*blockAlign))
	binary.LittleEndian.PutUint16(fmtChunk[12:14], uint16(blockAlign))
	binary.LittleEndian.PutUint16(fmtChunk[14:16], 24)
	binary.LittleEndian.PutUint16(fmtChunk[16:18], 22)
	binary.LittleEndian.PutUint16(fmtChunk[18:20], 24) // wValidBitsPerSample
	binary.LittleEndian.PutUint32(fmtChunk[20:24], 0x03)
	// KSDATAFORMAT_SUBTYPE_PCM GUID: 00000001-0000-0010-8000-00AA00389B71
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

func buildPCM16WAV(sr, ch int, pcm []byte) []byte {
	var chunks bytes.Buffer
	var fmtChunk [16]byte
	blockAlign := ch * 2
	binary.LittleEndian.PutUint16(fmtChunk[0:2], 1)
	binary.LittleEndian.PutUint16(fmtChunk[2:4], uint16(ch))
	binary.LittleEndian.PutUint32(fmtChunk[4:8], uint32(sr))
	binary.LittleEndian.PutUint32(fmtChunk[8:12], uint32(sr*blockAlign))
	binary.LittleEndian.PutUint16(fmtChunk[12:14], uint16(blockAlign))
	binary.LittleEndian.PutUint16(fmtChunk[14:16], 16)

	chunks.WriteString("fmt ")
	_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(fmtChunk)))
	chunks.Write(fmtChunk[:])
	chunks.WriteString("data")
	_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(pcm)))
	chunks.Write(pcm)

	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(4+chunks.Len()))
	out.WriteString("WAVE")
	out.Write(chunks.Bytes())
	return out.Bytes()
}

func encodeIEEE80(rate float64) [10]byte {
	var out [10]byte
	if rate <= 0 {
		return out
	}
	frac, exp := math.Frexp(rate)
	// Frexp returns frac in [0.5, 1.0) such that rate = frac * 2^exp.
	// Extended precision puts an explicit 1 at bit 63: mant = frac * 2^64, biasedExp = exp - 1 + 16383.
	biased := uint16(exp - 1 + 16383)
	mant := uint64(math.Ldexp(frac, 64))
	out[0] = byte(biased >> 8)
	out[1] = byte(biased)
	binary.BigEndian.PutUint64(out[2:10], mant)
	return out
}

func buildAIFFFile(formType, compType string, sr, ch, bits, frames, ssndOffset int, pcm []byte, name, auth string) []byte {
	var chunks bytes.Buffer

	writeChunk := func(id string, body []byte) {
		chunks.WriteString(id)
		_ = binary.Write(&chunks, binary.BigEndian, uint32(len(body)))
		chunks.Write(body)
		if len(body)%2 != 0 {
			chunks.WriteByte(0)
		}
	}

	if name != "" {
		writeChunk("NAME", []byte(name))
	}
	if auth != "" {
		writeChunk("AUTH", []byte(auth))
	}

	// Put SSND before COMM when ssndOffset > 0 to verify chunk-order independence.
	var ssnd bytes.Buffer
	_ = binary.Write(&ssnd, binary.BigEndian, uint32(ssndOffset))
	_ = binary.Write(&ssnd, binary.BigEndian, uint32(0))
	if ssndOffset > 0 {
		ssnd.Write(make([]byte, ssndOffset))
	}
	ssnd.Write(pcm)
	if ssndOffset > 0 {
		writeChunk("SSND", ssnd.Bytes())
	}

	var comm bytes.Buffer
	_ = binary.Write(&comm, binary.BigEndian, int16(ch))
	_ = binary.Write(&comm, binary.BigEndian, uint32(frames))
	_ = binary.Write(&comm, binary.BigEndian, int16(bits))
	f80 := encodeIEEE80(float64(sr))
	comm.Write(f80[:])
	if formType == "AIFC" {
		ct := compType
		if len(ct) < 4 {
			ct = "NONE"
		}
		comm.WriteString(ct[:4])
		comm.Write([]byte{0, 0})
	}
	writeChunk("COMM", comm.Bytes())

	if ssndOffset == 0 {
		writeChunk("SSND", ssnd.Bytes())
	}

	var form bytes.Buffer
	form.WriteString("FORM")
	_ = binary.Write(&form, binary.BigEndian, uint32(4+chunks.Len()))
	form.WriteString(formType[:4])
	form.Write(chunks.Bytes())
	return form.Bytes()
}

// buildSyntheticDSF generates a valid DSF file using a 1st-order 1-bit delta-sigma
// modulator encoding a stereo sine wave at toneHz (50% SACD modulation depth).
func buildSyntheticDSF(dsdRate, channels, numBlocks int, toneHz float64, id3 []byte) []byte {
	const blockSize = 4096
	totalSamplesPerCh := uint64(numBlocks * blockSize * 8)

	dataPayload := make([]byte, numBlocks*blockSize*channels)
	var errAccL, errAccR float64
	sampleIdx := 0
	for b := range numBlocks {
		ch0 := dataPayload[b*blockSize*channels : b*blockSize*channels+blockSize]
		var ch1 []byte
		if channels >= 2 {
			ch1 = dataPayload[b*blockSize*channels+blockSize : b*blockSize*channels+2*blockSize]
		}
		for byteIdx := range blockSize {
			var byteL, byteR uint8
			for bit := range 8 {
				x := 0.5 * math.Sin(2*math.Pi*toneHz*float64(sampleIdx)/float64(dsdRate))
				sampleIdx++

				vL := x + errAccL
				var qL float64
				if vL >= 0 {
					byteL |= 1 << bit // LSB-first
					qL = 1.0
				} else {
					qL = -1.0
				}
				errAccL = vL - qL

				vR := -x + errAccR
				var qR float64
				if vR >= 0 {
					byteR |= 1 << bit
					qR = 1.0
				} else {
					qR = -1.0
				}
				errAccR = vR - qR
			}
			ch0[byteIdx] = byteL
			if ch1 != nil {
				ch1[byteIdx] = byteR
			}
		}
	}

	dataChunkSize := uint64(12 + len(dataPayload))
	metaOff := uint64(0)
	if len(id3) > 0 {
		metaOff = 28 + 52 + dataChunkSize
	}
	totalFileSize := uint64(28+52) + dataChunkSize + uint64(len(id3))

	var out bytes.Buffer
	// "DSD " chunk (28 bytes)
	out.WriteString("DSD ")
	_ = binary.Write(&out, binary.LittleEndian, uint64(28))
	_ = binary.Write(&out, binary.LittleEndian, totalFileSize)
	_ = binary.Write(&out, binary.LittleEndian, metaOff)

	// "fmt " chunk (52 bytes)
	out.WriteString("fmt ")
	_ = binary.Write(&out, binary.LittleEndian, uint64(52))
	_ = binary.Write(&out, binary.LittleEndian, uint32(1)) // format version
	_ = binary.Write(&out, binary.LittleEndian, uint32(0)) // format ID: DSD raw
	_ = binary.Write(&out, binary.LittleEndian, uint32(2)) // stereo channel type
	_ = binary.Write(&out, binary.LittleEndian, uint32(channels))
	_ = binary.Write(&out, binary.LittleEndian, uint32(dsdRate))
	_ = binary.Write(&out, binary.LittleEndian, uint32(1)) // LSB-first 1-bit
	_ = binary.Write(&out, binary.LittleEndian, totalSamplesPerCh)
	_ = binary.Write(&out, binary.LittleEndian, uint32(blockSize))
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))

	// "data" chunk
	out.WriteString("data")
	_ = binary.Write(&out, binary.LittleEndian, dataChunkSize)
	out.Write(dataPayload)
	if len(id3) > 0 {
		out.Write(id3)
	}
	return out.Bytes()
}

func buildSyntheticDFF(dsdRate, channels, bytesPerCh int, title, artist string) []byte {
	writeDFFChunk := func(dst *bytes.Buffer, id string, body []byte) {
		dst.WriteString(id)
		_ = binary.Write(dst, binary.BigEndian, uint64(len(body)))
		dst.Write(body)
		if len(body)%2 != 0 {
			dst.WriteByte(0)
		}
	}

	var propBody bytes.Buffer
	propBody.WriteString("SND ")
	var fsBuf [4]byte
	binary.BigEndian.PutUint32(fsBuf[:], uint32(dsdRate))
	writeDFFChunk(&propBody, "FS  ", fsBuf[:])

	var chBuf bytes.Buffer
	_ = binary.Write(&chBuf, binary.BigEndian, uint16(channels))
	for range channels {
		chBuf.WriteString("SLFT")
	}
	writeDFFChunk(&propBody, "CHNL", chBuf.Bytes())
	writeDFFChunk(&propBody, "CMPR", append([]byte("DSD "), 0x0E, 'n', 'o', 't', ' ', 'c', 'o', 'm', 'p', 'r', 'e', 's', 's', 'e', 'd'))

	var diinBody bytes.Buffer
	if title != "" {
		var tb bytes.Buffer
		_ = binary.Write(&tb, binary.BigEndian, uint32(len(title)))
		tb.WriteString(title)
		writeDFFChunk(&diinBody, "DITI", tb.Bytes())
	}
	if artist != "" {
		var ab bytes.Buffer
		_ = binary.Write(&ab, binary.BigEndian, uint32(len(artist)))
		ab.WriteString(artist)
		writeDFFChunk(&diinBody, "DIAR", ab.Bytes())
	}

	dsdPayload := bytes.Repeat([]byte{0xAA}, bytesPerCh*channels)

	var frmBody bytes.Buffer
	frmBody.WriteString("DSD ")
	writeDFFChunk(&frmBody, "PROP", propBody.Bytes())
	writeDFFChunk(&frmBody, "DIIN", diinBody.Bytes())
	writeDFFChunk(&frmBody, "DSD ", dsdPayload)

	var out bytes.Buffer
	out.WriteString("FRM8")
	_ = binary.Write(&out, binary.BigEndian, uint64(frmBody.Len()))
	out.Write(frmBody.Bytes())
	return out.Bytes()
}

func mp4Box(typ string, body []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(8+len(body)))
	b.WriteString(typ[:4])
	b.Write(body)
	return b.Bytes()
}

func mp4IlstData(itemType string, dataType uint32, val []byte) []byte {
	var d bytes.Buffer
	_ = binary.Write(&d, binary.BigEndian, dataType)
	_ = binary.Write(&d, binary.BigEndian, uint32(0))
	d.Write(val)
	return mp4Box(itemType, mp4Box("data", d.Bytes()))
}

func buildSyntheticALACM4A(sr, bitDepth, ch int, durationMs int64, title, artist string, cover []byte) []byte {
	var mvhd bytes.Buffer
	mvhd.Write([]byte{0, 0, 0, 0}) // version 0
	mvhd.Write(make([]byte, 8))    // creation/mod
	_ = binary.Write(&mvhd, binary.BigEndian, uint32(1000))
	_ = binary.Write(&mvhd, binary.BigEndian, uint32(durationMs))
	mvhd.Write(make([]byte, 80))

	var hdlr bytes.Buffer
	hdlr.Write(make([]byte, 8))
	hdlr.WriteString("soun")
	hdlr.Write(make([]byte, 12))

	// ALACSpecificConfig (4-byte fullbox header + 24-byte cookie)
	var alacCfg [28]byte
	binary.BigEndian.PutUint32(alacCfg[4:8], 4096)
	alacCfg[9] = byte(bitDepth)
	alacCfg[10] = 40
	alacCfg[11] = 10
	alacCfg[12] = 14
	alacCfg[13] = byte(ch)
	binary.BigEndian.PutUint32(alacCfg[24:28], uint32(sr))

	var alacEntry bytes.Buffer
	alacEntry.Write(make([]byte, 16))
	_ = binary.Write(&alacEntry, binary.BigEndian, uint16(ch))
	_ = binary.Write(&alacEntry, binary.BigEndian, uint16(bitDepth))
	alacEntry.Write(make([]byte, 4))
	_ = binary.Write(&alacEntry, binary.BigEndian, uint32(48000)<<16)
	alacEntry.Write(mp4Box("alac", alacCfg[:]))

	var stsd bytes.Buffer
	stsd.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(&stsd, binary.BigEndian, uint32(1))
	stsd.Write(mp4Box("alac", alacEntry.Bytes()))

	stbl := mp4Box("stbl", stsdBytes(stsd.Bytes()))
	minf := mp4Box("minf", stbl)
	mdia := mp4Box("mdia", append(mp4Box("hdlr", hdlr.Bytes()), minf...))
	trak := mp4Box("trak", mdia)

	var ilst bytes.Buffer
	ilst.Write(mp4IlstData("\xa9nam", 1, []byte(title)))
	ilst.Write(mp4IlstData("\xa9ART", 1, []byte(artist)))
	ilst.Write(mp4IlstData("aART", 1, []byte(artist)))
	ilst.Write(mp4IlstData("\xa9alb", 1, []byte(title)))
	ilst.Write(mp4IlstData("trkn", 0, []byte{0, 0, 0, 1, 0, 4, 0, 0}))
	if len(cover) > 0 {
		ilst.Write(mp4IlstData("covr", 13, cover))
	}
	meta := mp4Box("meta", append([]byte{0, 0, 0, 0}, mp4Box("ilst", ilst.Bytes())...))
	udta := mp4Box("udta", meta)

	var moovBody bytes.Buffer
	moovBody.Write(mp4Box("mvhd", mvhd.Bytes()))
	moovBody.Write(trak)
	moovBody.Write(udta)

	var file bytes.Buffer
	file.Write(mp4Box("ftyp", []byte("M4A \x00\x00\x00\x00M4A mp42")))
	file.Write(mp4Box("moov", moovBody.Bytes()))
	return file.Bytes()
}

func stsdBytes(body []byte) []byte { return mp4Box("stsd", body) }

func buildSyntheticWavPack(sr, bitDepth, ch int, totalSamples uint32, title, artist, album string, cover []byte) []byte {
	var wvh [32]byte
	copy(wvh[0:4], "wvpk")
	binary.LittleEndian.PutUint32(wvh[4:8], 24)
	binary.LittleEndian.PutUint16(wvh[8:10], 0x0410)
	binary.LittleEndian.PutUint32(wvh[12:16], totalSamples)
	srIdx := uint32(9)
	for i, r := range wvSampleRates {
		if r == sr {
			srIdx = uint32(i)
			break
		}
	}
	bytesPerSample := uint32(bitDepth/8 - 1)
	var monoFlag uint32
	if ch == 1 {
		monoFlag = 0x04
	}
	flags := bytesPerSample | monoFlag | (srIdx << 23)
	binary.LittleEndian.PutUint32(wvh[24:28], flags)

	var apeItems bytes.Buffer
	itemCount := uint32(0)
	writeItem := func(itemFlags uint32, key string, val []byte) {
		itemCount++
		_ = binary.Write(&apeItems, binary.LittleEndian, uint32(len(val)))
		_ = binary.Write(&apeItems, binary.LittleEndian, itemFlags)
		apeItems.WriteString(key)
		apeItems.WriteByte(0)
		apeItems.Write(val)
	}
	writeItem(0, "Title", []byte(title))
	writeItem(0, "Artist", []byte(artist))
	writeItem(0, "Album", []byte(album))
	if len(cover) > 0 {
		writeItem(1<<1, "Cover Art (Front)", append([]byte("cover.png\x00"), cover...))
	}

	var footer [32]byte
	copy(footer[0:8], "APETAGEX")
	binary.LittleEndian.PutUint32(footer[8:12], 2000)
	binary.LittleEndian.PutUint32(footer[12:16], uint32(apeItems.Len()+32))
	binary.LittleEndian.PutUint32(footer[16:20], itemCount)

	var out bytes.Buffer
	out.Write(wvh[:])
	out.Write(apeItems.Bytes())
	out.Write(footer[:])
	return out.Bytes()
}

func buildSyntheticXingMP3(totalFrames uint32, title, artist string) []byte {
	id3 := buildID3v23Tag([][2][]byte{
		{[]byte("TIT2"), append([]byte{3}, []byte(title)...)},
		{[]byte("TPE1"), append([]byte{3}, []byte(artist)...)},
	})
	// MPEG1 Layer III, 192 kbps (idx 11), 44100 Hz (idx 0), stereo -> frameSize = 144*192000/44100 = 626 bytes
	frame1 := make([]byte, 626)
	frame1[0] = 0xFF
	frame1[1] = 0xFB // MPEG1 Layer III no CRC
	frame1[2] = 0xB0 // 192kbps, 44.1kHz, no pad
	frame1[3] = 0x00 // stereo
	// Xing header at offset 36
	copy(frame1[36:40], "Xing")
	binary.BigEndian.PutUint32(frame1[40:44], 0x03) // frames + bytes
	binary.BigEndian.PutUint32(frame1[44:48], totalFrames)
	binary.BigEndian.PutUint32(frame1[48:52], totalFrames*626)

	frame2 := make([]byte, 626)
	copy(frame2[0:4], frame1[0:4])

	var out bytes.Buffer
	out.Write(id3)
	out.Write(frame1)
	out.Write(frame2)
	return out.Bytes()
}

// Rescanning an unchanged music folder neither increments Revision nor rewrites local_library.json on disk.
func TestScanNoOpSkipsRevisionBumpAndIndexRewrite(t *testing.T) {
	root := t.TempDir()
	musicDir := filepath.Join(root, "music")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(musicDir, "01 - Track.mp3"), buildSyntheticXingMP3(200, "Quiet Check", "Artist"), 0o644); err != nil {
		t.Fatal(err)
	}

	idxPath := filepath.Join(root, "local_library.json")
	lib := NewLibrary(idxPath, filepath.Join(root, "covers"))
	lib.AddFolder(musicDir)
	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("initial Scan: %v", err)
	}
	rev1 := lib.Revision()
	st1, err := os.Stat(idxPath)
	if err != nil {
		t.Fatalf("stat index after initial Scan: %v", err)
	}
	past := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(idxPath, past, past); err != nil {
		t.Fatal(err)
	}
	stBackdated, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := lib.Scan(context.Background(), nil); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if got := lib.Revision(); got != rev1 {
		t.Fatalf("unchanged Scan incremented Revision from %d to %d", rev1, got)
	}
	st2, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(stBackdated.ModTime()) || st2.Size() != st1.Size() {
		t.Fatalf("unchanged Scan rewrote index file: modTime %v -> %v", stBackdated.ModTime(), st2.ModTime())
	}
}
