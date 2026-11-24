package localmusic

import (
	"bytes"
	"encoding/binary"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// decodeTextAuto decodes text bytes from UTF-8 (with or without BOM),
// UTF-16LE/BE (with or without BOM), GB18030, or Shift-JIS, stripping trailing
// NUL padding.
func decodeTextAuto(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	// Check BOMs first.
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return cleanDecodedString(string(trimTrailingZeroBytes(b[3:])))
	}
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		return cleanDecodedString(decodeUTF16Bytes(b[2:], binary.LittleEndian))
	}
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		return cleanDecodedString(decodeUTF16Bytes(b[2:], binary.BigEndian))
	}

	// Heuristic BOM-less UTF-16 detection when NUL bytes alternate regularly.
	if len(b) >= 4 && len(b)%2 == 0 {
		evenZero, oddZero := 0, 0
		pairs := len(b) / 2
		for i := 0; i < len(b); i += 2 {
			if b[i] == 0 && b[i+1] != 0 {
				evenZero++
			} else if b[i] != 0 && b[i+1] == 0 {
				oddZero++
			}
		}
		if oddZero*2 >= pairs && evenZero == 0 {
			return cleanDecodedString(decodeUTF16Bytes(b, binary.LittleEndian))
		}
		if evenZero*2 >= pairs && oddZero == 0 {
			return cleanDecodedString(decodeUTF16Bytes(b, binary.BigEndian))
		}
	}

	b = trimTrailingZeroBytes(b)
	if len(b) == 0 {
		return ""
	}
	if isPureASCII(b) {
		return cleanDecodedString(string(b))
	}
	if utf8.Valid(b) {
		return cleanDecodedString(string(b))
	}

	return cleanDecodedString(decodeLegacyCJKOrLatin1(b))
}

func decodeLegacyCJKOrLatin1(b []byte) string {
	gbBytes, gbErr := simplifiedchinese.GB18030.NewDecoder().Bytes(b)
	gbOK := gbErr == nil && !bytes.ContainsRune(gbBytes, utf8.RuneError)

	sjBytes, sjErr := japanese.ShiftJIS.NewDecoder().Bytes(b)
	sjOK := sjErr == nil && !bytes.ContainsRune(sjBytes, utf8.RuneError)

	switch {
	case gbOK && !sjOK:
		return string(gbBytes)
	case sjOK && !gbOK:
		return string(sjBytes)
	case gbOK && sjOK:
		if preferShiftJIS(b, sjBytes) {
			return string(sjBytes)
		}
		return string(gbBytes)
	default:
		// ISO-8859-1 fallback.
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		return string(runes)
	}
}

// preferShiftJIS distinguishes Shift-JIS Japanese text from GB18030/GBK Chinese
// text when both decoders succeed without RuneError.
func preferShiftJIS(raw, sjUTF8 []byte) bool {
	fullKana := 0
	halfKana := 0
	for _, r := range string(sjUTF8) {
		switch {
		case (r >= 0x3041 && r <= 0x3096) || (r >= 0x30A1 && r <= 0x30FA) || r == 'ー':
			fullKana++
		case r >= 0xFF61 && r <= 0xFF9F:
			halfKana++
		}
	}
	if fullKana > 0 && fullKana >= halfKana {
		return true
	}
	if halfKana > fullKana {
		return false
	}
	// Pure Kanji without Kana: check lead bytes. Standard GB2312 Chinese uses
	// 0xB0..0xF7 pairs, whereas Shift-JIS Kanji uses 0x88..0x9F (which in GBK
	// falls into rare GBK-3 archaic characters).
	sjKanjiLeads, gbCommonPairs := 0, 0
	for i := 0; i+1 < len(raw); i++ {
		c1, c2 := raw[i], raw[i+1]
		if c1 < 0x80 {
			continue
		}
		if c1 >= 0x88 && c1 <= 0x9F && c2 >= 0x40 && c2 <= 0xFC && c2 != 0x7F {
			sjKanjiLeads++
		}
		if c1 >= 0xB0 && c1 <= 0xF7 && c2 >= 0xA1 && c2 <= 0xFE {
			gbCommonPairs++
		}
		i++
	}
	return sjKanjiLeads > 0 && gbCommonPairs == 0
}

func decodeUTF16Bytes(b []byte, order binary.ByteOrder) string {
	n := len(b) &^ 1
	for n >= 2 && b[n-2] == 0 && b[n-1] == 0 {
		n -= 2
	}
	u16 := make([]uint16, n/2)
	for i := range u16 {
		u16[i] = order.Uint16(b[i*2 : i*2+2])
	}
	return string(utf16.Decode(u16))
}

func trimTrailingZeroBytes(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

func isPureASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

func cleanDecodedString(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.TrimSpace(s)
}

// kanjiFold maps common Traditional Chinese and Japanese Shinjitai characters
// to Simplified Chinese equivalents so album/artist matching across Roon and
// local tags tolerates regional script variants.
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
	'鄧': '邓', '麗': '丽', '張': '张', '陳': '陈', '劉': '刘', '楊': '杨', '趙': '赵',
	'吳': '吴', '鄭': '郑', '孫': '孙', '羅': '罗', '許': '许', '韓': '韩', '馮': '冯',
	'蕭': '萧', '蔣': '蒋', '盧': '卢', '蘇': '苏', '呂': '吕', '鍾': '钟', '陸': '陆',
	'賈': '贾', '韋': '韦', '鄒': '邹', '閆': '闫', '賀': '贺', '顧': '顾', '龔': '龚',
	'萬': '万', '錢': '钱', '湯': '汤', '倫': '伦', '傑': '杰', '榮': '荣', '謙': '谦',
	'賢': '贤', '寶': '宝', '島': '岛', '選': '选', '輯': '辑', '專': '专', '紀': '纪',
	'經': '经', '歲': '岁',
}

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
	case (r >= 0x304B && r <= 0x3062 && r%2 == 1) || (r >= 0x30AB && r <= 0x30C2 && r%2 == 1):
		return r + 1, true
	case r == 0x3064 || r == 0x3066 || r == 0x3068 || r == 0x30C4 || r == 0x30C6 || r == 0x30C8:
		return r + 1, true
	case (r >= 0x306F && r <= 0x307B && (r-0x306F)%3 == 0) || (r >= 0x30CF && r <= 0x30DB && (r-0x30CF)%3 == 0):
		return r + 1, true
	case r == 0x3046:
		return 0x3094, true
	case r == 0x30A6:
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

// normMatchKey normalizes a title, album, or artist string for case-,
// punctuation-, whitespace-, and CJK/Kana-insensitive comparison.
func normMatchKey(s string) string {
	if s == "" {
		return ""
	}
	s = composeKana(s)
	var b strings.Builder
	for _, r := range s {
		// Fullwidth ASCII -> halfwidth ASCII.
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		r = unicode.ToLower(r)
		// Katakana -> Hiragana.
		if r >= 0x30A1 && r <= 0x30F6 {
			r -= 0x60
		}
		if mapped, ok := kanjiFold[r]; ok {
			r = mapped
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var editionSuffixRe = regexp.MustCompile(`(?i)[\(\[（【]\s*(deluxe|remaster(?:ed)?|anniversary|expanded|special|bonus|edition|version|remaster\s*\d{4}|\d{4}\s*remaster|sacd|dsd|hi-?res|flac|disc\s*\d+|cd\s*\d+)[^\)\]）】]*[\)\]）】]`)

func stripEditionNotes(s string) string {
	return strings.TrimSpace(editionSuffixRe.ReplaceAllString(s, ""))
}
