package netease

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"ytb-music/internal/ytm"
)

// SearchFunc searches YouTube Music for tracks matching query.
type SearchFunc func(query string, limit int) ([]ytm.Track, error)

// MatchResult records how a single NetEase track resolved on YouTube Music.
type MatchResult struct {
	Index   int
	Source  SourceTrack
	Track   ytm.Track
	Matched bool
	Score   int
}

const minAcceptScore = 30

// MatchTracks resolves src against YouTube Music with bounded concurrency,
// preserving the original playlist order in the returned slice.
func MatchTracks(ctx context.Context, src []SourceTrack, workers int, search SearchFunc, onProgress func(done, total int, last MatchResult)) ([]MatchResult, error) {
	if search == nil {
		search = ytm.Search
	}
	if workers < 1 {
		workers = 4
	}
	total := len(src)
	results := make([]MatchResult, total)
	if total == 0 {
		return results, nil
	}

	jobs := make(chan int, total)
	for i := range total {
		jobs <- i
	}
	close(jobs)

	var (
		wg       sync.WaitGroup
		progMu   sync.Mutex
		finished int
	)
	for range min(workers, total) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				res := matchOne(ctx, idx, src[idx], search)
				results[idx] = res
				progMu.Lock()
				finished++
				done := finished
				if onProgress != nil && ctx.Err() == nil {
					onProgress(done, total, res)
				}
				progMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

func matchOne(ctx context.Context, idx int, st SourceTrack, search SearchFunc) MatchResult {
	res := MatchResult{Index: idx, Source: st}
	if st.Missing || strings.TrimSpace(st.Name) == "" {
		return res
	}
	queries := buildQueries(st)
	var best ytm.Track
	bestScore := -1000

	for _, q := range queries {
		if ctx.Err() != nil {
			return res
		}
		cands, err := search(q, 8)
		if err != nil || len(cands) == 0 {
			continue
		}
		for _, c := range cands {
			if !c.Playable() {
				continue
			}
			sc, hasTitle := scoreCandidate(st, c)
			if !hasTitle {
				continue
			}
			if sc > bestScore {
				bestScore = sc
				best = c
			}
		}
		if bestScore >= 65 {
			break
		}
	}
	if bestScore >= minAcceptScore && best.Playable() {
		res.Track = best
		res.Matched = true
		res.Score = bestScore
	}
	return res
}

func buildQueries(st SourceTrack) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, existing := range out {
			if strings.EqualFold(existing, s) {
				return
			}
		}
		out = append(out, s)
	}
	primaryArtist := ""
	if len(st.Artists) > 0 {
		primaryArtist = st.Artists[0]
	}
	if primaryArtist != "" {
		add(primaryArtist + " " + st.Name)
	}
	if st.Alias != "" && primaryArtist != "" {
		add(primaryArtist + " " + st.Alias)
	}
	add(st.Name)
	return out
}

var (
	bracketRe = regexp.MustCompile(`[(\[【（「]([^)\]】）」]*)[)\]】）」]`)
	liveRe    = regexp.MustCompile(`(?i)\blive\b|现场|演唱会`)
	instRe    = regexp.MustCompile(`(?i)\b(instrumental|karaoke|off\s*vocal|backing\s*track)\b|伴奏|纯音乐版|卡拉ok`)
	coverRe   = regexp.MustCompile(`(?i)\bcover\b|翻唱`)
	remixRe   = regexp.MustCompile(`(?i)\bremix\b|混音|DJ版`)
)

func scoreCandidate(src SourceTrack, cand ytm.Track) (int, bool) {
	score := 0
	srcFull := src.Name + " " + src.Alias
	candFull := cand.Name + " " + cand.Album.Name

	// Penalize version mismatches (e.g. matching a Live or Instrumental upload
	// when the NetEase track is the studio vocal version).
	for _, mod := range []*regexp.Regexp{liveRe, instRe, coverRe, remixRe} {
		sHas := mod.MatchString(srcFull)
		cHas := mod.MatchString(candFull)
		if cHas && !sHas {
			score -= 45
		} else if sHas && cHas {
			score += 15
		}
	}

	sTitle := normText(stripBenignBrackets(src.Name))
	sAlias := normText(stripBenignBrackets(src.Alias))
	cTitle := normText(stripBenignBrackets(cand.Name))

	titleMatched := false
	switch {
	case sTitle != "" && cTitle == sTitle:
		score += 55
		titleMatched = true
	case sAlias != "" && cTitle == sAlias:
		score += 50
		titleMatched = true
	case sTitle != "" && cTitle != "" && (strings.Contains(cTitle, sTitle) || strings.Contains(sTitle, cTitle)):
		score += 32
		titleMatched = true
	case sAlias != "" && cTitle != "" && (strings.Contains(cTitle, sAlias) || strings.Contains(sAlias, cTitle)):
		score += 28
		titleMatched = true
	default:
		score -= 15
	}

	// Artist overlap.
	artistMatched := false
	cTitleRaw := normText(cand.Name)
	for _, sa := range src.Artists {
		nsa := normText(sa)
		if nsa == "" {
			continue
		}
		for _, ca := range cand.Artists {
			nca := normText(ca.Name)
			if nca != "" && (nca == nsa || strings.Contains(nca, nsa) || strings.Contains(nsa, nca)) {
				score += 35
				artistMatched = true
				break
			}
		}
		if artistMatched {
			break
		}
		if strings.Contains(cTitleRaw, nsa) {
			score += 20
			artistMatched = true
			break
		}
	}

	// Duration proximity.
	if src.DurationMs > 0 && cand.Duration > 0 {
		diff := src.DurationMs - cand.Duration
		if diff < 0 {
			diff = -diff
		}
		switch {
		case diff <= 3000:
			score += 25
		case diff <= 8000:
			score += 15
		case diff <= 15000:
			score += 5
		case diff > 35000:
			score -= 25
		}
	}

	// Prefer official album audio tracks over MVs or user uploads.
	if cand.VideoType == ytm.VideoATV {
		score += 12
	}
	return score, titleMatched
}

func stripBenignBrackets(s string) string {
	return strings.TrimSpace(bracketRe.ReplaceAllStringFunc(s, func(m string) string {
		if liveRe.MatchString(m) || instRe.MatchString(m) || coverRe.MatchString(m) || remixRe.MatchString(m) {
			return m
		}
		return ""
	}))
}

func normText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		// Convert fullwidth ASCII to halfwidth.
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
