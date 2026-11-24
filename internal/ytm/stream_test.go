package ytm

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func stubSolver() *solver {
	return &solver{id: "testplayer", sts: 20775}
}

// Signed in at "standard", SongURL resolves TVHTML5 instead of failing on a
// bot-blocked anonymous VISIONOS endpoint, and falls back to WEB_REMIX when
// TVHTML5 marks a track unplayable.
func TestSignedInStandardUsesTVAndFallsBack(t *testing.T) {
	SetCookies("SAPISID=abc123; SID=xyz")
	defer ClearSession()

	origVision, origTV, origWR, origSolver := fetchVisionPlayer, fetchTVPlayer, fetchWebRemixPlayer, getSolver
	defer func() {
		fetchVisionPlayer, fetchTVPlayer, fetchWebRemixPlayer, getSolver = origVision, origTV, origWR, origSolver
		clearStreamCaches()
	}()

	getSolver = func() (*solver, error) { return stubSolver(), nil }
	fetchVisionPlayer = func(id string) (any, error) {
		return map[string]any{
			"playabilityStatus": map[string]any{
				"status": "UNPLAYABLE",
				"reason": "Sign in to confirm you’re not a bot",
			},
		}, nil
	}
	fetchTVPlayer = func(id string, sts int) (any, error) {
		if id == "tv_blocked" {
			return map[string]any{
				"playabilityStatus": map[string]any{"status": "UNPLAYABLE", "reason": "Unavailable on TV"},
			}, nil
		}
		return map[string]any{
			"videoDetails":      map[string]any{"videoId": id},
			"playabilityStatus": map[string]any{"status": "OK"},
			"streamingData": map[string]any{
				"adaptiveFormats": []any{
					map[string]any{
						"itag":          float64(140),
						"url":           "https://rr1.googlevideo.com/videoplayback?itag=140&c=TVHTML5",
						"contentLength": "3456789",
					},
				},
			},
		}, nil
	}
	fetchWebRemixPlayer = func(id string, sts int) (any, error) {
		return map[string]any{
			"videoDetails":      map[string]any{"videoId": id},
			"playabilityStatus": map[string]any{"status": "OK"},
			"streamingData": map[string]any{
				"adaptiveFormats": []any{
					map[string]any{
						"itag":          float64(140),
						"url":           "https://rr1.googlevideo.com/videoplayback?itag=140&c=WEB_REMIX",
						"contentLength": "3456000",
					},
				},
			},
		}, nil
	}

	st, err := SongURL("song_ok", "standard")
	if err != nil {
		t.Fatalf("SongURL(song_ok): %v", err)
	}
	if !strings.Contains(st.URL, "c=TVHTML5") || st.UserAgent != tvSamsungUA || st.Level != "standard" {
		t.Fatalf("got %+v", st)
	}

	fb, err := SongURL("tv_blocked", "standard")
	if err != nil {
		t.Fatalf("SongURL(tv_blocked): %v", err)
	}
	if !strings.Contains(fb.URL, "c=WEB_REMIX") || fb.UserAgent != UserAgent {
		t.Fatalf("fallback got %+v", fb)
	}
}

// Signed in without Premium, asking for "exhigh" falls back to 128 kbps AAC
// when WEB_REMIX offers only itag 140 and anonymous VISIONOS is bot-gated.
func TestSignedInNonPremiumExhighFallsBack(t *testing.T) {
	SetCookies("SAPISID=free_user; SID=xyz")
	defer ClearSession()

	origVision, origTV, origWR, origSolver := fetchVisionPlayer, fetchTVPlayer, fetchWebRemixPlayer, getSolver
	defer func() {
		fetchVisionPlayer, fetchTVPlayer, fetchWebRemixPlayer, getSolver = origVision, origTV, origWR, origSolver
		clearStreamCaches()
		forgetPremium()
	}()

	getSolver = func() (*solver, error) { return stubSolver(), nil }
	fetchVisionPlayer = func(id string) (any, error) {
		return map[string]any{
			"playabilityStatus": map[string]any{"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm you’re not a bot"},
		}, nil
	}
	fetchTVPlayer = func(id string, sts int) (any, error) {
		cipher := url.Values{"url": {"https://rr1.googlevideo.com/videoplayback?itag=140&c=TVHTML5"}}
		return map[string]any{
			"playabilityStatus": map[string]any{"status": "OK"},
			"streamingData": map[string]any{
				"adaptiveFormats": []any{
					map[string]any{"itag": float64(140), "signatureCipher": cipher.Encode(), "contentLength": "2000000"},
				},
			},
		}, nil
	}
	fetchWebRemixPlayer = func(id string, sts int) (any, error) {
		return map[string]any{
			"playabilityStatus": map[string]any{"status": "OK"},
			"streamingData": map[string]any{
				"adaptiveFormats": []any{
					map[string]any{"itag": float64(140), "url": "https://rr1.googlevideo.com/videoplayback?itag=140&c=WEB_REMIX", "contentLength": "2000000"},
				},
			},
		}, nil
	}

	st, err := SongURL("free_song", "exhigh")
	if err != nil {
		t.Fatalf("SongURL(exhigh): %v", err)
	}
	if st.Level != "standard" || !strings.Contains(st.URL, "itag=140") {
		t.Fatalf("got %+v", st)
	}
}

// Bot-check challenges report ErrBotCheck alongside ErrUnavailable and are
// never cached across retries once the route recovers.
func TestBotCheckNotCached(t *testing.T) {
	ClearSession()
	origVision := fetchVisionPlayer
	defer func() {
		fetchVisionPlayer = origVision
		clearStreamCaches()
	}()

	calls := 0
	fetchVisionPlayer = func(id string) (any, error) {
		calls++
		if calls == 1 {
			// Old VISIONOS returned UNPLAYABLE for bot challenges rather than LOGIN_REQUIRED.
			return map[string]any{
				"playabilityStatus": map[string]any{
					"status": "UNPLAYABLE",
					"reason": "Sign in to confirm you’re not a bot",
				},
			}, nil
		}
		return map[string]any{
			"playabilityStatus": map[string]any{"status": "OK"},
			"streamingData": map[string]any{
				"adaptiveFormats": []any{
					map[string]any{"itag": float64(140), "url": "https://rr1.googlevideo.com/videoplayback?itag=140", "contentLength": "1000"},
				},
			},
		}, nil
	}

	_, err := SongURL("bot_test", "standard")
	if !errors.Is(err, ErrBotCheck) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first call err = %v, want ErrBotCheck & ErrUnavailable", err)
	}
	st, err := SongURL("bot_test", "standard")
	if err != nil || st == nil || calls != 2 {
		t.Fatalf("second call st=%+v err=%v calls=%d", st, err, calls)
	}
}

// Brand-channel datasyncId values add X-Goog-PageId and user-bound SAPISIDHASH,
// and clear when cookies change.
func TestSignRequestDataSyncID(t *testing.T) {
	SetCookies("SAPISID=sap1; __Secure-1PAPISID=p1")
	defer ClearSession()

	noteDataSyncID("page999||user777")
	h := http.Header{}
	signRequest(h, "https://www.youtube.com")
	if got := h.Get("X-Goog-PageId"); got != "page999" {
		t.Fatalf("X-Goog-PageId = %q, want page999", got)
	}
	if authz := h.Get("Authorization"); !strings.Contains(authz, "_u") {
		t.Fatalf("Authorization = %q, want _u suffix", authz)
	}

	SetCookies("SAPISID=sap2")
	h2 := http.Header{}
	signRequest(h2, "https://www.youtube.com")
	if got := h2.Get("X-Goog-PageId"); got != "" {
		t.Fatalf("after cookie change, X-Goog-PageId = %q, want empty", got)
	}
}

// SongURL populates codec, sample rate, and bitrate from InnerTube formats, while SpecBadge, SpecDetail, EffectiveKbps, and lossless/hi-res helpers format online AAC, PCM, and DSD streams.
func TestStreamSpecMetadataAndFormatting(t *testing.T) {
	p, err := parsePlayerResp("vid_spec", map[string]any{
		"videoDetails":      map[string]any{"videoId": "vid_spec"},
		"playabilityStatus": map[string]any{"status": "OK"},
		"streamingData": map[string]any{
			"adaptiveFormats": []any{
				map[string]any{
					"itag":            float64(140),
					"url":             "https://rr1.googlevideo.com/videoplayback?itag=140",
					"contentLength":   "3200000",
					"averageBitrate":  float64(129600),
					"audioSampleRate": "44100",
				},
				map[string]any{
					"itag":            float64(141),
					"url":             "https://rr1.googlevideo.com/videoplayback?itag=141",
					"contentLength":   "6400000",
					"audioSampleRate": "48000",
				},
			},
		},
	}, UserAgent, nil)
	if err != nil {
		t.Fatalf("parsePlayerResp: %v", err)
	}
	if p.standard == nil || p.standard.Codec != "AAC" || p.standard.SampleRate != 44100 || p.standard.Bitrate != 130 {
		t.Fatalf("standard stream = %+v", p.standard)
	}
	if p.exhigh == nil || p.exhigh.Codec != "AAC" || p.exhigh.SampleRate != 48000 || p.exhigh.Bitrate != 256 {
		t.Fatalf("exhigh stream = %+v", p.exhigh)
	}

	var nilStream *Stream
	if nilStream.SpecBadge() != "" || nilStream.SpecDetail(time.Minute) != "" || nilStream.IsLosslessStream() || nilStream.IsHiResStream() {
		t.Fatal("nil Stream should report empty badge/detail and false lossless/hi-res")
	}

	cases := []struct {
		st       *Stream
		dur      time.Duration
		lossless bool
		hires    bool
		badge    string
		detail   string
	}{
		{
			st:     &Stream{Codec: "AAC", Level: "standard", SampleRate: 44100, Bitrate: 128},
			dur:    3 * time.Minute,
			badge:  "AAC 128k",
			detail: "AAC 128kbps · 44.1kHz",
		},
		{
			st:     &Stream{Codec: "AAC", Level: "exhigh", SampleRate: 44100, Bitrate: 256},
			dur:    3 * time.Minute,
			badge:  "AAC 256k",
			detail: "AAC 256kbps · 44.1kHz",
		},
		{
			st:     &Stream{Codec: "MP3", SampleRate: 44100, Bitrate: 320},
			dur:    3 * time.Minute,
			badge:  "MP3 320k",
			detail: "MP3 320kbps · 44.1kHz",
		},
		{
			st:       &Stream{Codec: "WAV", SampleRate: 44100, BitDepth: 16, Bitrate: 1411},
			dur:      3 * time.Minute,
			lossless: true,
			hires:    false,
			badge:    "WAV 16/44.1",
			detail:   "WAV 44.1kHz/16b · 1411kbps",
		},
		{
			st:       &Stream{Codec: "FLAC", SampleRate: 96000, BitDepth: 24, Bitrate: 2845},
			dur:      4 * time.Minute,
			lossless: true,
			hires:    true,
			badge:    "FLAC 24/96",
			detail:   "FLAC 96kHz/24b · 2845kbps",
		},
		{
			st:       &Stream{Codec: "DSD128", SampleRate: 5644800, BitDepth: 1, IsDSD: true},
			dur:      5 * time.Minute,
			lossless: true,
			hires:    true,
			badge:    "DSD128",
			detail:   "DSD128 · 5.64MHz / 1b",
		},
	}
	for _, tc := range cases {
		if got := tc.st.IsLosslessStream(); got != tc.lossless {
			t.Errorf("%+v IsLosslessStream() = %v, want %v", tc.st, got, tc.lossless)
		}
		if got := tc.st.IsHiResStream(); got != tc.hires {
			t.Errorf("%+v IsHiResStream() = %v, want %v", tc.st, got, tc.hires)
		}
		if got := tc.st.SpecBadge(); got != tc.badge {
			t.Errorf("%+v SpecBadge() = %q, want %q", tc.st, got, tc.badge)
		}
		if got := tc.st.SpecDetail(tc.dur); got != tc.detail {
			t.Errorf("%+v SpecDetail() = %q, want %q", tc.st, got, tc.detail)
		}
	}

	sized := &Stream{Size: 4_800_000}
	if got := sized.EffectiveKbps(200 * time.Second); got != 192 {
		t.Fatalf("EffectiveKbps from Size = %d, want 192", got)
	}
}
