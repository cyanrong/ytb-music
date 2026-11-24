package bridge

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// xmlFrameReader extracts complete XML documents (`<?xml ...?><Root .../>` or
// `<?xml ...?><Root>...</Root>`) from HQPlayer's undelimited TCP byte stream.
type xmlFrameReader struct {
	r   io.Reader
	buf []byte
	tmp [4096]byte
}

func newXMLFrameReader(r io.Reader) *xmlFrameReader {
	return &xmlFrameReader{r: r, buf: make([]byte, 0, 8192)}
}

// Next blocks until one complete XML document is available or an error occurs.
func (xr *xmlFrameReader) Next() (rootTag string, doc []byte, err error) {
	for {
		if start, end, tag, ok := scanCompleteXMLDoc(xr.buf); ok {
			out := make([]byte, end-start)
			copy(out, xr.buf[start:end])
			rem := copy(xr.buf, xr.buf[end:])
			xr.buf = xr.buf[:rem]
			return tag, out, nil
		}
		if len(xr.buf) > 1<<20 {
			return "", nil, errors.New("hqplayer: oversized XML frame")
		}
		n, rerr := xr.r.Read(xr.tmp[:])
		if n > 0 {
			xr.buf = append(xr.buf, xr.tmp[:n]...)
			continue
		}
		if rerr != nil {
			return "", nil, rerr
		}
	}
}

// scanCompleteXMLDoc finds the first complete XML document in b.
func scanCompleteXMLDoc(b []byte) (start, end int, rootTag string, ok bool) {
	n := len(b)
	i := 0
	for i < n && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	if i >= n {
		return 0, 0, "", false
	}
	start = i

	// Skip optional XML declaration(s) or comments before root element.
	for i < n {
		for i < n && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
			i++
		}
		if i >= n || b[i] != '<' {
			return 0, 0, "", false
		}
		if i+1 >= n {
			return 0, 0, "", false
		}
		switch b[i+1] {
		case '?':
			endPI := bytes.Index(b[i+2:], []byte("?>"))
			if endPI < 0 {
				return 0, 0, "", false
			}
			i += 2 + endPI + 2
		case '!':
			if i+3 < n && b[i+2] == '-' && b[i+3] == '-' {
				endCmt := bytes.Index(b[i+4:], []byte("-->"))
				if endCmt < 0 {
					return 0, 0, "", false
				}
				i += 4 + endCmt + 3
			} else {
				endDecl := bytes.IndexByte(b[i+2:], '>')
				if endDecl < 0 {
					return 0, 0, "", false
				}
				i += 2 + endDecl + 1
			}
		default:
			goto scanElements
		}
	}
	return 0, 0, "", false

scanElements:
	depth := 0
	for i < n {
		if b[i] != '<' {
			i++
			continue
		}
		if i+1 >= n {
			return 0, 0, "", false
		}
		// Comment or CDATA inside root element.
		if b[i+1] == '!' {
			if i+3 < n && b[i+2] == '-' && b[i+3] == '-' {
				endCmt := bytes.Index(b[i+4:], []byte("-->"))
				if endCmt < 0 {
					return 0, 0, "", false
				}
				i += 4 + endCmt + 3
				continue
			}
			if i+8 < n && string(b[i:i+9]) == "<![CDATA[" {
				endCD := bytes.Index(b[i+9:], []byte("]]>"))
				if endCD < 0 {
					return 0, 0, "", false
				}
				i += 9 + endCD + 3
				continue
			}
		}
		if b[i+1] == '?' {
			endPI := bytes.Index(b[i+2:], []byte("?>"))
			if endPI < 0 {
				return 0, 0, "", false
			}
			i += 2 + endPI + 2
			continue
		}
		closing := b[i+1] == '/'
		j := i + 1
		if closing {
			j++
		}
		nameStart := j
		for j < n && b[j] != '>' && b[j] != '/' && b[j] != ' ' && b[j] != '\t' && b[j] != '\r' && b[j] != '\n' {
			j++
		}
		if j >= n {
			return 0, 0, "", false
		}
		tagName := string(b[nameStart:j])
		if depth == 0 && !closing && rootTag == "" {
			rootTag = tagName
		}

		// Scan until '>' respecting attribute quotes.
		var quote byte
		for j < n {
			ch := b[j]
			if quote != 0 {
				if ch == quote {
					quote = 0
				}
				j++
				continue
			}
			if ch == '"' || ch == '\'' {
				quote = ch
				j++
				continue
			}
			if ch == '>' {
				break
			}
			j++
		}
		if j >= n {
			return 0, 0, "", false
		}
		selfClosing := !closing && j > i+1 && b[j-1] == '/'
		i = j + 1
		switch {
		case closing:
			depth--
			if depth <= 0 {
				return start, i, rootTag, true
			}
		case selfClosing:
			if depth == 0 {
				return start, i, rootTag, true
			}
		default:
			depth++
		}
	}
	return 0, 0, "", false
}

// parseXMLAttrs parses the root element's attributes and trimmed inner text
// from a complete XML frame.
func parseXMLAttrs(doc []byte) (name string, attrs map[string]string, err error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		tok, terr := dec.Token()
		if terr != nil {
			return "", nil, terr
		}
		if se, ok := tok.(xml.StartElement); ok {
			attrs = make(map[string]string, len(se.Attr)+1)
			for _, a := range se.Attr {
				attrs[a.Name.Local] = a.Value
			}
			var text strings.Builder
			depth := 1
			for depth > 0 {
				inner, ierr := dec.Token()
				if ierr != nil {
					break
				}
				switch v := inner.(type) {
				case xml.StartElement:
					if depth == 1 && v.Name.Local == "metadata" {
						for _, a := range v.Attr {
							attrs["meta_"+a.Name.Local] = a.Value
						}
					}
					depth++
				case xml.EndElement:
					depth--
				case xml.CharData:
					if depth == 1 {
						text.Write(v)
					}
				}
			}
			if body := strings.TrimSpace(text.String()); body != "" {
				attrs["_text"] = body
			}
			return se.Name.Local, attrs, nil
		}
	}
}

func xmlEscapeAttr(s string) string {
	var buf strings.Builder
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
