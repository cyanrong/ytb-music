package ytm

import (
	"net/url"
	"os"
	"testing"
)

// The embedded yt-dlp solver loads into jsc and unscrambles YouTube's live
// player script when LIVE is set.
func TestLiveSolver(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("set LIVE to call YouTube")
	}
	PlayerCacheDir = t.TempDir()
	sv, err := currentSolver()
	if err != nil {
		t.Fatalf("currentSolver: %v", err)
	}
	inSig := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdef"
	inN := "abc123XYZ_def456"
	sig, n, err := sv.solve(inSig, inN)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if sig == "" || n == "" || sig == inSig || n == inN {
		t.Fatalf("solver did not transform inputs: sig=%q n=%q", sig, n)
	}
	t.Logf("player %s (sts %d) → sig=%q n=%q", sv.id, sv.sts, sig, n)
}

// A web client's format plays from its URL with the signature its cipher
// scrambled put back under the name the cipher says, and its n replaced by
// what the player makes of it; a URL with neither is taken as it is.
func TestStreamURL(t *testing.T) {
	solve := func(sig, n string) (string, string, error) { return "un" + sig, "un" + n, nil }
	cipher := url.Values{"s": {"AB=C"}, "sp": {"sig"}, "url": {"https://rr1.googlevideo.com/videoplayback?itag=141&n=xyz&ip=1"}}
	got, err := streamURL(map[string]any{"signatureCipher": cipher.Encode()}, solve)
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, got)
	if q.Get("sig") != "unAB=C" || q.Get("n") != "unxyz" || q.Get("itag") != "141" || q.Get("ip") != "1" {
		t.Errorf("%s", got)
	}

	got, err = streamURL(map[string]any{"url": "https://rr1.googlevideo.com/videoplayback?itag=141&n=xyz"}, solve)
	if q := mustQuery(t, got); err != nil || q.Get("n") != "unxyz" || q.Has("sig") || q.Has("signature") {
		t.Errorf("%s, %v", got, err)
	}

	plain := "https://rr1.googlevideo.com/videoplayback?itag=140"
	if got, err := streamURL(map[string]any{"url": plain}, solve); err != nil || got != plain {
		t.Errorf("%s, %v", got, err)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
