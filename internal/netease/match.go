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

// AlbumResolveFunc searches YouTube Music for an album matching query and
// returns its full track list, or nil when none matches.
type AlbumResolveFunc func(ctx context.Context, query string) (*ytm.AlbumProfile, error)

// Optional hooks overridden in offline unit tests; when nil in live mode
// (search == nil), MatchTracks uses NetEase album metadata, Japanese-locale
// YouTube Music search, and YouTube Music album track-index resolution.
var (
	FetchAlbumMetaFn   func(ctx context.Context, albumID int64) (*AlbumMeta, error)
	SearchJapaneseFn   SearchFunc
	ResolveYTMAlbumFn  AlbumResolveFunc
)

// MatchResult records how a single NetEase track resolved on YouTube Music.
type MatchResult struct {
	Index   int
	Source  SourceTrack
	Track   ytm.Track
	Matched bool
	Score   int
}

const minAcceptScore = 30

type neAlbumCacheEntry struct {
	mu   sync.Mutex
	done bool
	meta *AlbumMeta
}

type ytmAlbumCacheEntry struct {
	mu   sync.Mutex
	done bool
	al   *ytm.AlbumProfile
}

type matchSession struct {
	search       SearchFunc
	jaSearch     SearchFunc
	fetchNEMeta  func(ctx context.Context, albumID int64) (*AlbumMeta, error)
	resolveAlbum AlbumResolveFunc

	neMu     sync.Mutex
	neAlbums map[int64]*neAlbumCacheEntry

	ytmMu     sync.Mutex
	ytmAlbums map[string]*ytmAlbumCacheEntry
}

func newMatchSession(search SearchFunc) *matchSession {
	live := search == nil
	if search == nil {
		search = ytm.Search
	}
	s := &matchSession{
		search:       search,
		fetchNEMeta:  FetchAlbumMetaFn,
		jaSearch:     SearchJapaneseFn,
		resolveAlbum: ResolveYTMAlbumFn,
		neAlbums:     map[int64]*neAlbumCacheEntry{},
		ytmAlbums:    map[string]*ytmAlbumCacheEntry{},
	}
	if live {
		if s.fetchNEMeta == nil {
			s.fetchNEMeta = FetchAlbumMeta
		}
		if s.jaSearch == nil {
			s.jaSearch = func(q string, limit int) ([]ytm.Track, error) {
				return ytm.SearchInLang(q, limit, "ja")
			}
		}
		if s.resolveAlbum == nil {
			s.resolveAlbum = defaultResolveYTMAlbum
		}
	}
	return s
}

func defaultResolveYTMAlbum(ctx context.Context, query string) (*ytm.AlbumProfile, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	page, err := ytm.SearchPage(query, ytm.SearchAlbums, 4, 0)
	if err != nil || len(page.Albums) == 0 {
		return nil, err
	}
	for _, cand := range page.Albums {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if cand.ID == "" {
			continue
		}
		al, err := ytm.AlbumDetail(cand.ID)
		if err == nil && al != nil && len(al.Songs) > 0 {
			return al, nil
		}
	}
	return nil, nil
}

func (ms *matchSession) enrichFromNetEaseAlbum(ctx context.Context, st *SourceTrack) bool {
	if ms.fetchNEMeta == nil || st.AlbumID <= 0 {
		return false
	}
	ms.neMu.Lock()
	ent := ms.neAlbums[st.AlbumID]
	if ent == nil {
		ent = &neAlbumCacheEntry{}
		ms.neAlbums[st.AlbumID] = ent
	}
	ms.neMu.Unlock()

	ent.mu.Lock()
	if !ent.done {
		meta, err := ms.fetchNEMeta(ctx, st.AlbumID)
		if err == nil {
			ent.meta = meta
		}
		ent.done = true
	}
	meta := ent.meta
	ent.mu.Unlock()

	if meta == nil {
		return false
	}
	beforeA := len(st.ArtistAliases)
	beforeAl := len(st.AlbumAliases)
	st.ArtistAliases = appendUniqueTrimmed(st.ArtistAliases, meta.ArtistAliases...)
	st.AlbumAliases = appendUniqueTrimmed(st.AlbumAliases, meta.AlbumAliases...)
	if st.AlbumSize == 0 && meta.Size > 0 {
		st.AlbumSize = meta.Size
	}
	return len(st.ArtistAliases) > beforeA || len(st.AlbumAliases) > beforeAl
}

func (ms *matchSession) lookupYTMAlbum(ctx context.Context, cacheKey string, queries []string) *ytm.AlbumProfile {
	if ms.resolveAlbum == nil || len(queries) == 0 {
		return nil
	}
	ms.ytmMu.Lock()
	ent := ms.ytmAlbums[cacheKey]
	if ent == nil {
		ent = &ytmAlbumCacheEntry{}
		ms.ytmAlbums[cacheKey] = ent
	}
	ms.ytmMu.Unlock()

	ent.mu.Lock()
	defer ent.mu.Unlock()
	if !ent.done {
		for _, q := range queries {
			if ctx.Err() != nil {
				break
			}
			al, err := ms.resolveAlbum(ctx, q)
			if err == nil && al != nil && len(al.Songs) > 0 {
				ent.al = al
				break
			}
		}
		ent.done = true
	}
	return ent.al
}

// MatchTracks resolves src against YouTube Music with bounded concurrency,
// preserving the original playlist order in the returned slice.
func MatchTracks(ctx context.Context, src []SourceTrack, workers int, search SearchFunc, onProgress func(done, total int, last MatchResult)) ([]MatchResult, error) {
	if workers < 1 {
		workers = 4
	}
	total := len(src)
	results := make([]MatchResult, total)
	if total == 0 {
		return results, nil
	}
	ms := newMatchSession(search)

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
				res := ms.matchOne(ctx, idx, src[idx])
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

type fetchedQuery struct {
	cands []ytm.Track
}

func (ms *matchSession) matchOne(ctx context.Context, idx int, st SourceTrack) MatchResult {
	res := MatchResult{Index: idx, Source: st}
	if st.Missing || strings.TrimSpace(st.Name) == "" {
		return res
	}
	st.Name = composeKana(st.Name)
	st.Alias = composeKana(st.Alias)

	var best ytm.Track
	bestScore := -1000
	var history []fetchedQuery

	evalCands := func(cands []ytm.Track) {
		for candIdx, c := range cands {
			if !c.Playable() {
				continue
			}
			sc, ok := scoreCandidate(st, c, candIdx)
			if !ok {
				continue
			}
			if sc > bestScore {
				bestScore = sc
				best = c
			}
		}
	}

	queries := buildQueries(st)
	for _, q := range queries {
		if ctx.Err() != nil {
			return res
		}
		cands, err := ms.search(q, 8)
		if err != nil || len(cands) == 0 {
			continue
		}
		history = append(history, fetchedQuery{cands: cands})
		evalCands(cands)
		if bestScore >= 65 {
			break
		}
	}

	// Cross-script Japanese / CJK disambiguation: enrich artist/album Romaji
	// aliases from NetEase, re-evaluate already-fetched candidates, and try
	// Japanese-locale search + album track-index resolution when needed.
	if bestScore < 65 && hasCJKOrKana(st.Name+" "+st.Alias+" "+st.DisplayArtist()+" "+st.Album) {
		if ms.enrichFromNetEaseAlbum(ctx, &st) {
			for _, fq := range history {
				evalCands(fq.cands)
			}
			if bestScore < 65 {
				for _, q := range buildAliasQueries(st, queries) {
					if ctx.Err() != nil {
						return res
					}
					cands, err := ms.search(q, 8)
					if err != nil || len(cands) == 0 {
						continue
					}
					evalCands(cands)
					if bestScore >= 65 {
						break
					}
				}
			}
		}

		// When signed-in cookies or English locale romanized YTM titles, query
		// YouTube Music with hl="ja" so Japanese tracks return Kanji/Kana titles.
		if bestScore < minAcceptScore && ms.jaSearch != nil {
			jaLimit := min(len(queries), 2)
			for _, q := range queries[:jaLimit] {
				if ctx.Err() != nil {
					return res
				}
				cands, err := ms.jaSearch(q, 8)
				if err != nil || len(cands) == 0 {
					continue
				}
				evalCands(cands)
				if bestScore >= 65 {
					break
				}
			}
		}

		// Final fallback for Chinese-translated Japanese album tracks whose
		// YouTube Music counterparts are pure Romaji: locate the album on YTM
		// and match by 1-based track index + duration agreement.
		if bestScore < minAcceptScore && st.Album != "" && st.TrackNo > 0 {
			if tr, sc, ok := ms.matchViaAlbum(ctx, st); ok && sc > bestScore {
				best = tr
				bestScore = sc
			}
		}
	}

	if bestScore >= minAcceptScore && best.Playable() {
		res.Track = best
		res.Matched = true
		res.Score = bestScore
	}
	return res
}

func (ms *matchSession) matchViaAlbum(ctx context.Context, st SourceTrack) (ytm.Track, int, bool) {
	if st.DurationMs <= 0 || st.TrackNo <= 0 || st.Album == "" {
		return ytm.Track{}, 0, false
	}
	var qs []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, e := range qs {
			if strings.EqualFold(e, s) {
				return
			}
		}
		qs = append(qs, s)
	}
	if len(st.Artists) > 0 {
		add(st.Artists[0] + " " + st.Album)
	}
	if len(st.ArtistAliases) > 0 {
		add(st.ArtistAliases[0] + " " + st.Album)
	}
	for _, alAlias := range st.AlbumAliases {
		if len(st.Artists) > 0 {
			add(st.Artists[0] + " " + alAlias)
		}
		add(alAlias)
	}
	add(st.Album)

	cacheKey := st.Album
	if st.AlbumID > 0 {
		cacheKey = "ne:" + string(rune(st.AlbumID)) + ":" + st.Album
	} else if len(st.Artists) > 0 {
		cacheKey = st.Artists[0] + "|" + st.Album
	}
	// Use explicit numeric string for AlbumID cache key.
	if st.AlbumID > 0 {
		cacheKey = formatAlbumKey(st.AlbumID)
	}
	al := ms.lookupYTMAlbum(ctx, cacheKey, qs)
	if al == nil || len(al.Songs) == 0 {
		return ytm.Track{}, 0, false
	}
	if st.AlbumSize > 0 && absInt(len(al.Songs)-st.AlbumSize) > 2 {
		return ytm.Track{}, 0, false
	}

	// Primary check: exact 1-based track position on the album + duration within ±5s.
	targetIdx := st.TrackNo - 1
	if targetIdx >= 0 && targetIdx < len(al.Songs) {
		cand := al.Songs[targetIdx]
		if cand.Playable() && !hasVersionConflict(st, cand) && cand.Duration > 0 && absInt64(st.DurationMs-cand.Duration) <= 5000 {
			return cand, 68, true
		}
	}

	// Secondary check: allow ±1 index shift (e.g. digital intro/bonus track)
	// only when a single nearby track matches duration within ±2s.
	lo := max(0, targetIdx-1)
	hi := min(len(al.Songs)-1, targetIdx+1)
	matchedIdx := -1
	for i := lo; i <= hi; i++ {
		cand := al.Songs[i]
		if !cand.Playable() || hasVersionConflict(st, cand) || cand.Duration <= 0 {
			continue
		}
		if absInt64(st.DurationMs-cand.Duration) <= 2000 {
			if matchedIdx >= 0 {
				return ytm.Track{}, 0, false // ambiguous duration
			}
			matchedIdx = i
		}
	}
	if matchedIdx >= 0 {
		return al.Songs[matchedIdx], 58, true
	}
	return ytm.Track{}, 0, false
}

func formatAlbumKey(id int64) string {
	var buf [24]byte
	i := len(buf)
	v := id
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return "album:" + string(buf[i:])
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
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
	for _, al := range allTrackAliases(st) {
		if primaryArtist != "" {
			add(primaryArtist + " " + al)
		}
	}
	if st.Album != "" && hasCJKOrKana(st.Name) {
		add(st.Album + " " + st.Name)
	}
	add(st.Name)
	return out
}

func buildAliasQueries(st SourceTrack, already []string) []string {
	var out []string
	seen := func(s string) bool {
		for _, e := range already {
			if strings.EqualFold(e, s) {
				return true
			}
		}
		for _, e := range out {
			if strings.EqualFold(e, s) {
				return true
			}
		}
		return false
	}
	for _, arAlias := range st.ArtistAliases {
		q := strings.TrimSpace(arAlias + " " + st.Name)
		if q != "" && !seen(q) {
			out = append(out, q)
		}
		for _, tAlias := range allTrackAliases(st) {
			q2 := strings.TrimSpace(arAlias + " " + tAlias)
			if q2 != "" && !seen(q2) {
				out = append(out, q2)
			}
		}
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func allTrackAliases(st SourceTrack) []string {
	var out []string
	if st.Alias != "" && !isArrangementSourceNote(st.Alias) {
		out = append(out, st.Alias)
	}
	for _, a := range st.Aliases {
		if a != "" && !isArrangementSourceNote(a) && !strings.EqualFold(a, st.Alias) {
			out = append(out, a)
		}
	}
	return out
}

var (
	bracketRe = regexp.MustCompile(`[(\[【（「]([^)\]】）」]*)[)\]】）」]`)
	liveRe    = regexp.MustCompile(`(?i)\blive\b|现场|演唱会`)
	instRe    = regexp.MustCompile(`(?i)\b(instrumental|karaoke|off\s*vocal|backing\s*track)\b|伴奏|纯音乐版|卡拉ok`)
	coverRe   = regexp.MustCompile(`(?i)\bcover\b|翻唱`)
	remixRe   = regexp.MustCompile(`(?i)\bremix\b|混音|DJ版`)
)

func hasVersionConflict(src SourceTrack, cand ytm.Track) bool {
	srcFull := src.Name + " " + src.Alias + " " + strings.Join(src.Aliases, " ")
	candFull := cand.Name + " " + cand.Album.Name
	for _, mod := range []*regexp.Regexp{liveRe, instRe, coverRe, remixRe} {
		if mod.MatchString(candFull) && !mod.MatchString(srcFull) {
			return true
		}
	}
	return false
}

func scoreCandidate(src SourceTrack, cand ytm.Track, candIdx int) (int, bool) {
	score := 0
	srcFull := src.Name + " " + src.Alias + " " + strings.Join(src.Aliases, " ")
	candFull := cand.Name + " " + cand.Album.Name

	versionConflict := false
	for _, mod := range []*regexp.Regexp{liveRe, instRe, coverRe, remixRe} {
		sHas := mod.MatchString(srcFull)
		cHas := mod.MatchString(candFull)
		if cHas && !sHas {
			score -= 45
			versionConflict = true
		} else if sHas && cHas {
			score += 15
		}
	}

	cCleanRaw := stripBenignBrackets(cand.Name)
	cTitle := normText(cCleanRaw)

	titleMatched := false
	bestTitleBonus := -15

	rawTitles := append([]string{src.Name}, allTrackAliases(src)...)
	for idx, rawT := range rawTitles {
		cleanS := stripBenignBrackets(rawT)
		sNorm := normText(cleanS)
		isPrimary := idx == 0
		switch {
		case sNorm != "" && cTitle == sNorm:
			bonus := 50
			if isPrimary {
				bonus = 55
			}
			if bonus > bestTitleBonus {
				bestTitleBonus = bonus
			}
			titleMatched = true
		case sNorm != "" && cTitle != "" && len([]rune(sNorm)) >= 2 && len([]rune(cTitle)) >= 2 &&
			(strings.Contains(cTitle, sNorm) || strings.Contains(sNorm, cTitle)):
			bonus := 28
			if isPrimary {
				bonus = 32
			}
			if bonus > bestTitleBonus {
				bestTitleBonus = bonus
			}
			titleMatched = true
		default:
			if matched, strong := kanaMatchesRomaji(cleanS, cCleanRaw); matched {
				bonus := 28
				if strong {
					bonus = 44
				}
				if bonus > bestTitleBonus {
					bestTitleBonus = bonus
				}
				titleMatched = true
			}
		}
	}
	score += bestTitleBonus

	// Artist overlap (including English / Romaji / Traditional aliases).
	artistMatched := false
	cTitleRaw := normText(cand.Name)
	allSrcArtists := make([]string, 0, len(src.Artists)+len(src.ArtistAliases))
	allSrcArtists = append(allSrcArtists, src.Artists...)
	allSrcArtists = append(allSrcArtists, src.ArtistAliases...)
	for _, sa := range allSrcArtists {
		nsa := normText(sa)
		if nsa == "" {
			continue
		}
		for _, ca := range cand.Artists {
			nca := normText(ca.Name)
			if nca != "" && (nca == nsa || (len([]rune(nsa)) >= 2 && len([]rune(nca)) >= 2 && (strings.Contains(nca, nsa) || strings.Contains(nsa, nca)))) {
				score += 35
				artistMatched = true
				break
			}
			if ok, _ := kanaMatchesRomaji(sa, ca.Name); ok {
				score += 35
				artistMatched = true
				break
			}
		}
		if artistMatched {
			break
		}
		if len([]rune(nsa)) >= 2 && strings.Contains(cTitleRaw, nsa) {
			score += 20
			artistMatched = true
			break
		}
	}

	// Album overlap.
	albumMatched := false
	if cand.Album.Name != "" {
		ncAlb := normText(stripBenignBrackets(cand.Album.Name))
		srcAlbums := append([]string{src.Album}, src.AlbumAliases...)
		for _, sa := range srcAlbums {
			nsa := normText(stripBenignBrackets(sa))
			if nsa != "" && ncAlb != "" && (nsa == ncAlb || (len([]rune(nsa)) >= 2 && len([]rune(ncAlb)) >= 2 && (strings.Contains(ncAlb, nsa) || strings.Contains(nsa, ncAlb)))) {
				score += 12
				albumMatched = true
				break
			}
			if ok, _ := kanaMatchesRomaji(sa, cand.Album.Name); ok {
				score += 12
				albumMatched = true
				break
			}
		}
	}

	// Duration proximity.
	durDiff := int64(1 << 60)
	if src.DurationMs > 0 && cand.Duration > 0 {
		durDiff = absInt64(src.DurationMs - cand.Duration)
		switch {
		case durDiff <= 3000:
			score += 25
		case durDiff <= 8000:
			score += 15
		case durDiff <= 15000:
			score += 5
		case durDiff > 35000:
			score -= 25
		}
	}

	// Cross-script Knowledge-Graph Rank-0 match: when NetEase holds a Chinese
	// translation or Kanji title (e.g. "青鸟", "静寂の一人歩き") and YouTube
	// Music's #1 result is in Kana/Romaji ("ブルーバード - Blue Bird",
	// "Seijaku no hitori aruki") by the same artist/album with identical
	// duration (±2.5s), trust YouTube's cross-language top hit.
	if !titleMatched && candIdx == 0 && !versionConflict && durDiff <= 2500 &&
		(artistMatched || albumMatched) && isCrossScriptPair(src.Name, cand.Name) {
		score += 25 // offsets the -15 title miss and adds +10 Rank-0 confidence
		titleMatched = true
	}

	// Prefer official album audio tracks over MVs or user uploads.
	if cand.VideoType == ytm.VideoATV {
		score += 12
	}
	return score, titleMatched
}

// isCrossScriptPair reports whether srcTitle contains CJK/Kana while candTitle
// uses a non-overlapping script (such as pure Romaji or Kana vs pure Han).
func isCrossScriptPair(srcTitle, candTitle string) bool {
	if !hasCJKOrKana(srcTitle) {
		return false
	}
	if isPureLatinTitle(candTitle) {
		return true
	}
	// Pure Han Chinese title on NetEase (e.g. "青鸟") vs Kana/Romaji title on YTM ("ブルーバード - Blue Bird").
	if !hasKana(srcTitle) && hasKana(candTitle) {
		return true
	}
	return false
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
	s = composeKana(s)
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		// Convert fullwidth ASCII to halfwidth.
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		r = foldRune(r)
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
