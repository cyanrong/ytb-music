package netease

import (
	"strings"
	"unicode"
)

// hasCJKOrKana reports whether s contains Han ideographs, Hiragana, or Katakana.
func hasCJKOrKana(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) {
			return true
		}
	}
	return false
}

// hasKana reports whether s contains Japanese Hiragana or Katakana.
func hasKana(s string) bool {
	for _, r := range composeKana(s) {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana) {
			return true
		}
	}
	return false
}

// isPureLatinTitle reports whether s has ASCII/Latin letters and no CJK or Kana.
func isPureLatinTitle(s string) bool {
	hasLat := false
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) {
			return false
		}
		if unicode.In(r, unicode.Latin) || (r >= 0xFF21 && r <= 0xFF5A) {
			hasLat = true
		}
	}
	return hasLat
}

// composeKana combines NFD-decomposed Japanese dakuten (U+3099) and
// handakuten (U+309A) marks with their preceding Kana base rune so macOS-uploaded
// NetEase titles (e.g. "ハ\u3099") match standard NFC ("バ").
func composeKana(s string) string {
	if !strings.ContainsAny(s, "\u3099\u309a\u309b\u309c") {
		return s
	}
	runes := []rune(s)
	out := make([]rune, 0, len(runes))
	for _, r := range runes {
		if len(out) > 0 && (r == 0x3099 || r == 0x309B) {
			prev := out[len(out)-1]
			if composed, ok := addDakuten(prev); ok {
				out[len(out)-1] = composed
				continue
			}
		}
		if len(out) > 0 && (r == 0x309A || r == 0x309C) {
			prev := out[len(out)-1]
			if composed, ok := addHandakuten(prev); ok {
				out[len(out)-1] = composed
				continue
			}
		}
		out = append(out, r)
	}
	return string(out)
}

func addDakuten(r rune) (rune, bool) {
	switch {
	// Hiragana & Katakana ka..ko, sa..so, ta..to (odd/even pairs depending on block)
	case (r >= 0x304B && r <= 0x3062 && r%2 == 1) || (r >= 0x30AB && r <= 0x30C2 && r%2 == 1):
		return r + 1, true
	case r == 0x3064 || r == 0x3066 || r == 0x3068 || r == 0x30C4 || r == 0x30C6 || r == 0x30C8:
		return r + 1, true
	// ha, hi, fu, he, ho (step of 3: base, dakuten, handakuten)
	case (r >= 0x306F && r <= 0x307B && (r-0x306F)%3 == 0) || (r >= 0x30CF && r <= 0x30DB && (r-0x30CF)%3 == 0):
		return r + 1, true
	case r == 0x3046: // う -> ゔ
		return 0x3094, true
	case r == 0x30A6: // ウ -> ヴ
		return 0x30F4, true
	}
	return r, false
}

func addHandakuten(r rune) (rune, bool) {
	if (r >= 0x306F && r <= 0x307B && (r-0x306F)%3 == 0) || (r >= 0x30CF && r <= 0x30DB && (r-0x30CF)%3 == 0) {
		return r + 2, true
	}
	return r, false
}

// kanjiFold maps Traditional Chinese and Japanese Shinjitai characters to
// Simplified Chinese equivalents so cross-region titles ("恋愛"/"恋爱",
// "極楽浄土"/"极乐净土", "花澤香菜"/"花泽香菜", "躍動"/"跃动") compare equal.
var kanjiFold = map[rune]rune{
	'戀': '恋', '愛': '爱', '極': '极', '楽': '乐', '樂': '乐', '浄': '净', '淨': '净',
	'桜': '樱', '櫻': '樱', '躍': '跃', '動': '动', '頹': '颓', '頽': '颓', '廃': '废',
	'廢': '废', '嶽': '岳', '斷': '断', '靜': '静', '獨': '独', '節': '节', '觀': '观',
	'観': '观', '嚴': '严', '厳': '严', '夢': '梦', '鄉': '乡', '郷': '乡', '風': '风',
	'鳥': '鸟', '雲': '云', '澤': '泽', '沢': '泽', '葉': '叶', '華': '华', '語': '语',
	'東': '东', '陣': '阵', '強': '强', '貫': '贯', '穩': '稳', '穏': '稳', '時': '时',
	'間': '间', '繪': '绘', '絵': '绘', '銀': '银', '劍': '剑', '剣': '剑', '滅': '灭',
	'進': '进', '擊': '击', '撃': '击', '軌': '轨', '跡': '迹', '蒼': '苍', '藍': '蓝',
	'紅': '红', '蓮': '莲', '龍': '龙', '竜': '龙', '國': '国', '學': '学', '園': '园',
	'戰': '战', '戦': '战', '門': '门', '開': '开', '閉': '闭', '見': '见', '貝': '贝',
	'車': '车', '長': '长', '馬': '马', '魚': '鱼', '麥': '麦', '黒': '黑', '齒': '齿',
	'歯': '齿', '靈': '灵', '霊': '灵', '聲': '声', '聽': '听', '聴': '听', '實': '实',
	'実': '实', '發': '发', '発': '发', '變': '变', '変': '变', '歸': '归', '帰': '归',
	'當': '当', '錄': '录', '録': '录', '徹': '彻', '憶': '忆', '憂': '忧', '懷': '怀',
	'懐': '怀', '憐': '怜', '惡': '恶', '悪': '恶', '惱': '恼', '悩': '恼', '驚': '惊',
	'戲': '戏', '戯': '戏', '戶': '户', '戸': '户', '執': '执', '擇': '择', '択': '择',
	'擁': '拥', '晝': '昼', '曉': '晓', '暁': '晓', '闇': '暗', '殺': '杀', '雜': '杂',
	'雑': '杂', '樹': '树', '橋': '桥', '歡': '欢', '歓': '欢', '殘': '残', '淚': '泪',
	'涙': '泪', '淵': '渊', '滿': '满', '満': '满', '濱': '滨', '浜': '滨', '燈': '灯',
	'燒': '烧', '焼': '烧', '熱': '热', '獻': '献', '貓': '猫', '瑤': '瑶', '畫': '画',
	'癡': '痴', '禮': '礼', '祕': '秘', '約': '约', '純': '纯', '紙': '纸', '終': '终',
	'絆': '绊', '絕': '绝', '絶': '绝', '綠': '绿', '緑': '绿', '緣': '缘', '縁': '缘',
	'覺': '觉', '覚': '觉', '記': '记', '詩': '诗', '誠': '诚', '誰': '谁', '謎': '谜',
	'謝': '谢', '譚': '谭', '轉': '转', '転': '转', '輕': '轻', '軽': '轻', '遠': '远',
	'遙': '遥', '鈴': '铃', '鐵': '铁', '鉄': '铁', '鎖': '锁', '鏡': '镜', '陽': '阳',
	'陰': '阴', '階': '阶', '際': '际', '隱': '隐', '隠': '隐', '霧': '雾', '響': '响',
	'頁': '页', '頂': '顶', '順': '顺', '須': '须', '頌': '颂', '豫': '予', '預': '予',
	'顏': '颜', '顔': '颜', '願': '愿', '飛': '飞', '鳴': '鸣', '鷗': '鸥', '鴎': '鸥',
	'黃': '黄', '黙': '默', '齊': '齐', '斉': '齐', '環': '环', '輪': '轮', '廻': '回',
}

func foldRune(r rune) rune {
	// Normalize Katakana (U+30A1..U+30F6) to Hiragana (U+3041..U+3096) so
	// Hiragana and Katakana spellings compare equal.
	if r >= 0x30A1 && r <= 0x30F6 {
		r -= 0x60
	}
	if mapped, ok := kanjiFold[r]; ok && mapped != 0 {
		return mapped
	}
	return r
}

// toHiragana converts Katakana runes in composed s to Hiragana.
func toHiragana(s string) string {
	s = composeKana(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 0x30A1 && r <= 0x30F6 {
			r -= 0x60
		}
		b.WriteRune(r)
	}
	return b.String()
}

var kanaDigraphs = map[string]string{
	"きゃ": "kya", "きゅ": "kyu", "きょ": "kyo",
	"ぎゃ": "gya", "ぎゅ": "gyu", "ぎょ": "gyo",
	"しゃ": "sya", "しゅ": "syu", "しょ": "syo",
	"じゃ": "zya", "じゅ": "zyu", "じょ": "zyo",
	"ちゃ": "tya", "ちゅ": "tyu", "ちょ": "tyo",
	"にゃ": "nya", "にゅ": "nyu", "にょ": "nyo",
	"ひゃ": "hya", "ひゅ": "hyu", "ひょ": "hyo",
	"びゃ": "bya", "びゅ": "byu", "びょ": "byo",
	"ぴゃ": "pya", "ぴゅ": "pyu", "ぴょ": "pyo",
	"みゃ": "mya", "みゅ": "myu", "みょ": "myo",
	"りゃ": "rya", "りゅ": "ryu", "りょ": "ryo",
	"ふぁ": "fa", "ふぃ": "fi", "ふぇ": "fe", "ふぉ": "fo",
	"てぃ": "ti", "でぃ": "di", "とぅ": "tu", "どぅ": "du",
	"ゔぁ": "va", "ゔぃ": "vi", "ゔぇ": "ve", "ゔぉ": "vo",
}

var kanaMonographs = map[rune]string{
	'あ': "a", 'い': "i", 'う': "u", 'え': "e", 'お': "o",
	'か': "ka", 'き': "ki", 'く': "ku", 'け': "ke", 'こ': "ko",
	'が': "ga", 'ぎ': "gi", 'ぐ': "gu", 'げ': "ge", 'ご': "go",
	'さ': "sa", 'し': "si", 'す': "su", 'せ': "se", 'そ': "so",
	'ざ': "za", 'じ': "zi", 'ず': "zu", 'ぜ': "ze", 'ぞ': "zo",
	'た': "ta", 'ち': "ti", 'つ': "tu", 'て': "te", 'と': "to",
	'だ': "da", 'ぢ': "zi", 'づ': "zu", 'で': "de", 'ど': "do",
	'な': "na", 'に': "ni", 'ぬ': "nu", 'ね': "ne", 'の': "no",
	'は': "ha", 'ひ': "hi", 'ふ': "hu", 'へ': "he", 'ほ': "ho",
	'ば': "ba", 'び': "bi", 'ぶ': "bu", 'べ': "be", 'ぼ': "bo",
	'ぱ': "pa", 'ぴ': "pi", 'ぷ': "pu", 'ぺ': "pe", 'ぽ': "po",
	'ま': "ma", 'み': "mi", 'む': "mu", 'め': "me", 'も': "mo",
	'や': "ya", 'ゆ': "yu", 'よ': "yo",
	'ら': "ra", 'り': "ri", 'る': "ru", 'れ': "re", 'ろ': "ro",
	'わ': "wa", 'ゐ': "i", 'ゑ': "e", 'を': "o", 'ん': "n",
	'ゔ': "vu", 'ぁ': "a", 'ぃ': "i", 'ぅ': "u", 'ぇ': "e", 'ぉ': "o",
	'ゃ': "ya", 'ゅ': "yu", 'ょ': "yo", 'ゎ': "wa",
}

// kanaChunkToPhonetic converts a contiguous Hiragana run into canonical
// phonetic Romaji (collapsing Hepburn/Kunrei differences and long vowels).
func kanaChunkToPhonetic(hira []rune) string {
	var b strings.Builder
	sokuon := false
	for i := 0; i < len(hira); i++ {
		r := hira[i]
		if r == 'っ' {
			sokuon = true
			continue
		}
		if r == 'ー' {
			continue
		}
		syl := ""
		if i+1 < len(hira) {
			if dg, ok := kanaDigraphs[string(hira[i:i+2])]; ok {
				syl = dg
				i++
			}
		}
		if syl == "" {
			syl = kanaMonographs[r]
		}
		if syl == "" {
			sokuon = false
			continue
		}
		if sokuon && len(syl) > 0 {
			b.WriteByte(syl[0])
			sokuon = false
		}
		b.WriteString(syl)
	}
	return collapseRomajiVowels(b.String())
}

var romajiReplacer = strings.NewReplacer(
	"tsch", "ty",
	"sh", "sy",
	"ch", "ty",
	"ts", "t",
	"fu", "hu",
	"ji", "zi",
	"ja", "zya",
	"ju", "zyu",
	"jo", "zyo",
	"si", "si",
	"sya", "sya",
	"syi", "si",
	"wo", "o",
	"l", "r",
)

// normalizePhoneticRomaji converts an ASCII/Romaji title into the same
// canonical phonetic space as kanaChunkToPhonetic so Hepburn ("itsudemo",
// "kyousha", "yousei") and Kunrei ("itudemo", "kyousya", "yosei") match.
func normalizePhoneticRomaji(s string) string {
	var clean strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			clean.WriteRune(r)
		}
	}
	t := clean.String()
	t = strings.ReplaceAll(t, "shi", "si")
	t = strings.ReplaceAll(t, "chi", "ti")
	t = strings.ReplaceAll(t, "tsu", "tu")
	t = romajiReplacer.Replace(t)
	return collapseRomajiVowels(t)
}

func collapseRomajiVowels(s string) string {
	s = strings.ReplaceAll(s, "ou", "o")
	s = strings.ReplaceAll(s, "oo", "o")
	s = strings.ReplaceAll(s, "uu", "u")
	s = strings.ReplaceAll(s, "aa", "a")
	s = strings.ReplaceAll(s, "ii", "i")
	s = strings.ReplaceAll(s, "ee", "e")
	s = strings.ReplaceAll(s, "nn", "n")
	return s
}

type kanaSegment struct {
	alts []string // phonetic alternatives (e.g. particle は -> "ha" or "wa")
}

// extractKanaSkeleton splits a Japanese title (pure Kana or mixed Kanji+Kana)
// into ordered phonetic Kana segments and reports whether any non-Kana letters
// (such as Kanji) also appeared.
func extractKanaSkeleton(s string) (segs []kanaSegment, hadKanji bool, totalChars int) {
	h := []rune(toHiragana(s))
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		primary := kanaChunkToPhonetic(cur)
		if primary != "" {
			seg := kanaSegment{alts: []string{primary}}
			if string(cur) == "は" {
				seg.alts = []string{"wa", "ha"}
			} else if strings.HasPrefix(string(cur), "は") {
				rest := kanaChunkToPhonetic(cur[1:])
				seg.alts = []string{"wa" + rest, "ha" + rest}
			}
			segs = append(segs, seg)
			totalChars += len(primary)
		}
		cur = cur[:0]
	}
	for _, r := range h {
		if (r >= 0x3041 && r <= 0x3096) || r == 'ー' {
			cur = append(cur, r)
			continue
		}
		flush()
		if unicode.In(r, unicode.Han) {
			hadKanji = true
		} else if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			// Embedded Latin word inside a Japanese title (e.g. "初雪のスカイロード" vs "Hatsuyuki no sky road").
			p := normalizePhoneticRomaji(string(r))
			if p != "" {
				if len(segs) > 0 && len(segs[len(segs)-1].alts) == 1 && isASCIIAlphaNum(segs[len(segs)-1].alts[0]) {
					// Merge contiguous Latin runes below.
				}
			}
		}
	}
	flush()
	return segs, hadKanji, totalChars
}

func isASCIIAlphaNum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return len(s) > 0
}

// kanaMatchesRomaji reports whether a Japanese Kana or mixed Kanji+Kana title
// matches a candidate's Romaji title. Pure-Kana titles require full/substring
// phonetic equivalence; mixed Kanji+Kana titles require all Kana particle and
// okurigana segments to appear in order with sufficient phonetic length.
func kanaMatchesRomaji(jpTitle, candTitle string) (matched bool, strong bool) {
	if !hasKana(jpTitle) {
		return false, false
	}
	rom := normalizePhoneticRomaji(candTitle)
	if len(rom) < 4 {
		return false, false
	}
	segs, hadKanji, totalChars := extractKanaSkeleton(jpTitle)
	if len(segs) == 0 {
		return false, false
	}
	if !hadKanji {
		// Pure Kana (plus optional spaces/punctuation): compare joined phonetic string.
		var full strings.Builder
		for _, sg := range segs {
			full.WriteString(sg.alts[0])
		}
		jpRom := full.String()
		if len(jpRom) < 4 {
			return false, false
		}
		if rom == jpRom || strings.Contains(rom, jpRom) || (len(rom) >= 5 && strings.Contains(jpRom, rom)) {
			return true, true
		}
		return false, false
	}

	// Mixed Kanji + Kana: verify every Kana chunk appears in order in rom,
	// with gaps where the Kanji readings sit.
	if totalChars < 3 || (len(segs) == 1 && totalChars < 5) {
		return false, false
	}
	pos := 0
	for i, sg := range segs {
		foundAt := -1
		matchedAltLen := 0
		for _, alt := range sg.alts {
			idx := strings.Index(rom[pos:], alt)
			if idx >= 0 && (foundAt < 0 || idx < foundAt) {
				foundAt = idx
				matchedAltLen = len(alt)
			}
		}
		if foundAt < 0 {
			return false, false
		}
		// If the Japanese title ended with this Kana chunk (e.g. "...を見る" -> "omiru",
		// "...になるアイツ" -> "ninaruaitu") and candTitle has no trailing subtitle,
		// prefer matching near the end or continuing cleanly.
		_ = i
		pos += foundAt + matchedAltLen
	}
	return true, totalChars >= 6 && len(segs) >= 2
}
