// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package html

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// These replacements permit compatibility with old numeric entities that
// assumed Windows-1252 encoding.
// https://html.spec.whatwg.org/multipage/parsing.html#numeric-character-reference-end-state
var replacementTable = [...]rune{
	'\u20AC', // First entry is what 0x80 should be replaced with.
	'\u0081',
	'\u201A',
	'\u0192',
	'\u201E',
	'\u2026',
	'\u2020',
	'\u2021',
	'\u02C6',
	'\u2030',
	'\u0160',
	'\u2039',
	'\u0152',
	'\u008D',
	'\u017D',
	'\u008F',
	'\u0090',
	'\u2018',
	'\u2019',
	'\u201C',
	'\u201D',
	'\u2022',
	'\u2013',
	'\u2014',
	'\u02DC',
	'\u2122',
	'\u0161',
	'\u203A',
	'\u0153',
	'\u009D',
	'\u017E',
	'\u0178', // Last entry is 0x9F.
	// 0x00->'\uFFFD' is handled programmatically.
	// 0x0D->'\u000D' is a no-op.
}

// unescapeEntity reads an entity like "&lt;" from b[src:] and writes the
// corresponding "<" to b[dst:], returning the incremented dst and src cursors.
// Precondition: b[src] == '&' && dst <= src.
// attribute should be true if parsing an attribute value.
func unescapeEntity(b []byte, dst, src int, attribute bool) (dst1, src1 int) {
	// https://html.spec.whatwg.org/multipage/syntax.html#consume-a-character-reference

	// i starts at 1 because we already know that s[0] == '&'.
	i, s := 1, b[src:]

	if len(s) <= 1 {
		b[dst] = b[src]
		return dst + 1, src + 1
	}

	if s[i] == '#' {
		if len(s) <= 3 { // We need to have at least "&#.".
			b[dst] = b[src]
			return dst + 1, src + 1
		}
		i++
		c := s[i]
		hex := false
		if c == 'x' || c == 'X' {
			hex = true
			i++
		}

		x := '\x00'
		for i < len(s) {
			c = s[i]
			i++
			if hex {
				if '0' <= c && c <= '9' {
					x = 16*x + rune(c) - '0'
					continue
				} else if 'a' <= c && c <= 'f' {
					x = 16*x + rune(c) - 'a' + 10
					continue
				} else if 'A' <= c && c <= 'F' {
					x = 16*x + rune(c) - 'A' + 10
					continue
				}
			} else if '0' <= c && c <= '9' {
				x = 10*x + rune(c) - '0'
				continue
			}
			if c != ';' {
				i--
			}
			break
		}

		if i <= 3 { // No characters matched.
			b[dst] = b[src]
			return dst + 1, src + 1
		}

		if 0x80 <= x && x <= 0x9F {
			// Replace characters from Windows-1252 with UTF-8 equivalents.
			x = replacementTable[x-0x80]
		} else if x == 0 || (0xD800 <= x && x <= 0xDFFF) || x > 0x10FFFF {
			// Replace invalid characters with the replacement character.
			x = '\uFFFD'
		}

		return dst + utf8.EncodeRune(b[dst:], x), src + i
	}

	// Consume the maximum number of characters possible, with the
	// consumed characters matching one of the named references.

	for i < len(s) {
		c := s[i]
		i++
		// Lower-cased characters are more common in entities, so we check for them first.
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			continue
		}
		if c != ';' {
			i--
		}
		break
	}

	entityName := s[1:i]
	if len(entityName) == 0 {
		// No-op.
	} else if attribute && entityName[len(entityName)-1] != ';' && len(s) > i && s[i] == '=' {
		// No-op.
	} else if x := entity[string(entityName)]; x != 0 {
		return dst + utf8.EncodeRune(b[dst:], x), src + i
	} else if x := entity2[string(entityName)]; x[0] != 0 {
		dst1 := dst + utf8.EncodeRune(b[dst:], x[0])
		return dst1 + utf8.EncodeRune(b[dst1:], x[1]), src + i
	} else if !attribute {
		maxLen := len(entityName) - 1
		if maxLen > longestEntityWithoutSemicolon {
			maxLen = longestEntityWithoutSemicolon
		}
		for j := maxLen; j > 1; j-- {
			if x := entity[string(entityName[:j])]; x != 0 {
				return dst + utf8.EncodeRune(b[dst:], x), src + j + 1
			}
		}
	}

	dst1, src1 = dst+i, src+i
	copy(b[dst:dst1], b[src:src1])
	return dst1, src1
}

// unescape unescapes b's entities in-place, so that "a&lt;b" becomes "a<b".
// attribute should be true if parsing an attribute value.
func unescape(b []byte, attribute bool) []byte {
	populateMapsOnce.Do(populateMaps)
	i := bytes.IndexByte(b, '&')

	if i < 0 {
		return b
	}

	dst, src := unescapeEntity(b, i, i, attribute)
	for len(b[src:]) > 0 {
		if b[src] == '&' {
			i = 0
		} else {
			i = bytes.IndexByte(b[src:], '&')
		}
		if i < 0 {
			dst += copy(b[dst:], b[src:])
			break
		}

		if i > 0 {
			copy(b[dst:], b[src:src+i])
		}
		dst, src = unescapeEntity(b, dst+i, src+i, attribute)
	}
	return b[:dst]
}

// lower lower-cases the A-Z bytes in b in-place, so that "aBc" becomes "abc".
func lower(b []byte) []byte {
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return b
}

// escapeComment is like func escape but escapes its input bytes less often.
// Per https://github.com/golang/go/issues/58246 some HTML comments are (1)
// meaningful and (2) contain angle brackets that we'd like to avoid escaping
// unless we have to.
//
// "We have to" includes the '&' byte, since that introduces other escapes.
//
// It also includes those bytes (not including EOF) that would otherwise end
// the comment. Per the summary table at the bottom of comment_test.go, this is
// the '>' byte that, per above, we'd like to avoid escaping unless we have to.
//
// Studying the summary table (and T actions in its '>' column) closely, we
// only need to escape in states 43, 44, 49, 51 and 52. State 43 is at the
// start of the comment data. State 52 is after a '!'. The other three states
// are after a '-'.
//
// Our algorithm is thus to escape every '&' and to escape '>' if and only if:
//   - The '>' is after a '!' or '-' (in the unescaped data) or
//   - The '>' is at the start of the comment data (after the opening "<!--").
func escapeComment(w writer, s string) error {
	// When modifying this function, consider manually increasing the
	// maxSuffixLen constant in func TestComments, from 6 to e.g. 9 or more.
	// That increase should only be temporary, not committed, as it
	// exponentially affects the test running time.

	if len(s) == 0 {
		return nil
	}

	// Loop:
	//   - Grow j such that s[i:j] does not need escaping.
	//   - If s[j] does need escaping, output s[i:j] and an escaped s[j],
	//     resetting i and j to point past that s[j] byte.
	i := 0
	for j := 0; j < len(s); j++ {
		escaped := ""
		switch s[j] {
		case '&':
			escaped = "&amp;"

		case '>':
			if j > 0 {
				if prev := s[j-1]; (prev != '!') && (prev != '-') {
					continue
				}
			}
			escaped = "&gt;"

		default:
			continue
		}

		if i < j {
			if _, err := w.WriteString(s[i:j]); err != nil {
				return err
			}
		}
		if _, err := w.WriteString(escaped); err != nil {
			return err
		}
		i = j + 1
	}

	if i < len(s) {
		if _, err := w.WriteString(s[i:]); err != nil {
			return err
		}
	}
	return nil
}

// escapeCommentString is to EscapeString as escapeComment is to escape.
func escapeCommentString(s string) string {
	if strings.IndexAny(s, "&>") == -1 {
		return s
	}
	var buf bytes.Buffer
	escapeComment(&buf, s)
	return buf.String()
}

var htmlEscaper = strings.NewReplacer(
	`&`, "&amp;",
	`'`, "&#39;", // "&#39;" is shorter than "&apos;" and apos was not in HTML until HTML5.
	`<`, "&lt;",
	`>`, "&gt;",
	`"`, "&#34;", // "&#34;" is shorter than "&quot;".
	"\r", "&#13;",
)

func escape(w writer, s string) error {
	_, err := htmlEscaper.WriteString(w, s)
	return err
}

// EscapeString escapes special characters like "<" to become "&lt;". It
// escapes only five such characters: <, >, &, ' and ".
// UnescapeString(EscapeString(s)) == s always holds, but the converse isn't
// always true.
func EscapeString(s string) string {
	return htmlEscaper.Replace(s)
}

// UnescapeString unescapes entities like "&lt;" to become "<". It unescapes a
// larger range of entities than EscapeString escapes. For example, "&aacute;"
// unescapes to "á", as does "&#225;" and "&#xE1;".
// UnescapeString(EscapeString(s)) == s always holds, but the converse isn't
// always true.
func UnescapeString(s string) string {
	populateMapsOnce.Do(populateMaps)
	i := strings.IndexByte(s, '&')

	if i < 0 {
		return s
	}

	b := []byte(s)
	dst, src := unescapeEntity(b, i, i, false)
	for len(s[src:]) > 0 {
		if s[src] == '&' {
			i = 0
		} else {
			i = strings.IndexByte(s[src:], '&')
		}
		if i < 0 {
			dst += copy(b[dst:], s[src:])
			break
		}

		if i > 0 {
			copy(b[dst:], s[src:src+i])
		}
		dst, src = unescapeEntity(b, dst+i, src+i, false)
	}
	return string(b[:dst])
}
