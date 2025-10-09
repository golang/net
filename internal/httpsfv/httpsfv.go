// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package httpsfv provides functionality for dealing with HTTP Structured
// Field Values.
package httpsfv

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func isLCAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z')
}

func isAlpha(b byte) bool {
	return isLCAlpha(b) || (b >= 'A' && b <= 'Z')
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isVChar(b byte) bool {
	return b >= 0x21 && b <= 0x7e
}

func isSP(b byte) bool {
	return b == 0x20
}

func isTChar(b byte) bool {
	if isAlpha(b) || isDigit(b) {
		return true
	}
	return slices.Contains([]byte{'!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~'}, b)
}

func countLeftWhitespace(s string) int {
	i := 0
	for _, ch := range []byte(s) {
		if ch != ' ' && ch != '\t' {
			break
		}
		i++
	}
	return i
}

// https://www.rfc-editor.org/rfc/rfc4648#section-8.
func decOctetHex(ch1, ch2 byte) (ch byte, ok bool) {
	decBase16 := func(in byte) (out byte, ok bool) {
		if !isDigit(in) && !(in >= 'a' && in <= 'f') {
			return 0, false
		}
		if isDigit(in) {
			return in - '0', true
		}
		return in - 'a' + 10, true
	}

	if ch1, ok = decBase16(ch1); !ok {
		return 0, ok
	}
	if ch2, ok = decBase16(ch2); !ok {
		return 0, ok
	}
	return ch1<<4 | ch2, true
}

// ParseList parses a list from a given HTTP Structured Field Values.
//
// Given an HTTP SFV string that represents a list, it will call the given
// function using each of the members and parameters contained in the list.
// This allows the caller to extract information out of the list.
//
// This function will return once it encounters the end of the string, or
// something that is not a list. If it cannot consume the entire given
// string, the ok value returned will be false.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-list.
func ParseList(s string, f func(member, param string)) (ok bool) {
	for len(s) != 0 {
		var member, param string
		if len(s) != 0 && s[0] == '(' {
			if member, s, ok = consumeBareInnerList(s, nil); !ok {
				return ok
			}
		} else {
			if member, s, ok = consumeBareItem(s); !ok {
				return ok
			}
		}
		if param, s, ok = consumeParameter(s, nil); !ok {
			return ok
		}
		if f != nil {
			f(member, param)
		}

		s = s[countLeftWhitespace(s):]
		if len(s) == 0 {
			break
		}
		if s[0] != ',' {
			return false
		}
		s = s[1:]
		s = s[countLeftWhitespace(s):]
		if len(s) == 0 {
			return false
		}
	}
	return true
}

// consumeBareInnerList consumes an inner list
// (https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-an-inner-list),
// except for the inner list's top-most parameter.
// For example, given `(a;b c;d);e`, it will consume only `(a;b c;d)`.
func consumeBareInnerList(s string, f func(bareItem, param string)) (consumed, rest string, ok bool) {
	if len(s) == 0 || s[0] != '(' {
		return "", s, false
	}
	rest = s[1:]
	for len(rest) != 0 {
		var bareItem, param string
		rest = rest[countLeftWhitespace(rest):]
		if len(rest) != 0 && rest[0] == ')' {
			rest = rest[1:]
			break
		}
		if bareItem, rest, ok = consumeBareItem(rest); !ok {
			return "", s, ok
		}
		if param, rest, ok = consumeParameter(rest, nil); !ok {
			return "", s, ok
		}
		if len(rest) == 0 || (rest[0] != ')' && !isSP(rest[0])) {
			return "", s, false
		}
		if f != nil {
			f(bareItem, param)
		}
	}
	return s[:len(s)-len(rest)], rest, true
}

// ParseBareInnerList parses a bare inner list from a given HTTP Structured
// Field Values.
//
// We define a bare inner list as an inner list
// (https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-an-inner-list),
// without the top-most parameter of the inner list. For example, given the
// inner list `(a;b c;d);e`, the bare inner list would be `(a;b c;d)`.
//
// Given an HTTP SFV string that represents a bare inner list, it will call the
// given function using each of the bare item and parameter within the bare
// inner list. This allows the caller to extract information out of the bare
// inner list.
//
// This function will return once it encounters the end of the bare inner list,
// or something that is not a bare inner list. If it cannot consume the entire
// given string, the ok value returned will be false.
func ParseBareInnerList(s string, f func(bareItem, param string)) (ok bool) {
	_, rest, ok := consumeBareInnerList(s, f)
	return rest == "" && ok
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-an-item.
func consumeItem(s string, f func(bareItem, param string)) (consumed, rest string, ok bool) {
	var bareItem, param string
	if bareItem, rest, ok = consumeBareItem(s); !ok {
		return "", s, ok
	}
	if param, rest, ok = consumeParameter(rest, nil); !ok {
		return "", s, ok
	}
	if f != nil {
		f(bareItem, param)
	}
	return s[:len(s)-len(rest)], rest, true
}

// ParseItem parses an item from a given HTTP Structured Field Values.
//
// Given an HTTP SFV string that represents an item, it will call the given
// function once, with the bare item and the parameter of the item. This allows
// the caller to extract information out of the item.
//
// This function will return once it encounters the end of the string, or
// something that is not an item. If it cannot consume the entire given
// string, the ok value returned will be false.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-an-item.
func ParseItem(s string, f func(bareItem, param string)) (ok bool) {
	_, rest, ok := consumeItem(s, f)
	return rest == "" && ok
}

// ParseDictionary parses a dictionary from a given HTTP Structured Field
// Values.
//
// Given an HTTP SFV string that represents a dictionary, it will call the
// given function using each of the keys, values, and parameters contained in
// the dictionary. This allows the caller to extract information out of the
// dictionary.
//
// This function will return once it encounters the end of the string, or
// something that is not a dictionary. If it cannot consume the entire given
// string, the ok value returned will be false.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-dictionary.
func ParseDictionary(s string, f func(key, val, param string)) (ok bool) {
	for len(s) != 0 {
		var key, val, param string
		val = "?1" // Default value for empty val is boolean true.
		if key, s, ok = consumeKey(s); !ok {
			return ok
		}
		if len(s) != 0 && s[0] == '=' {
			s = s[1:]
			if len(s) != 0 && s[0] == '(' {
				if val, s, ok = consumeBareInnerList(s, nil); !ok {
					return ok
				}
			} else {
				if val, s, ok = consumeBareItem(s); !ok {
					return ok
				}
			}
		}
		if param, s, ok = consumeParameter(s, nil); !ok {
			return ok
		}
		if f != nil {
			f(key, val, param)
		}
		s = s[countLeftWhitespace(s):]
		if len(s) == 0 {
			break
		}
		if s[0] == ',' {
			s = s[1:]
		}
		s = s[countLeftWhitespace(s):]
		if len(s) == 0 {
			return false
		}
	}
	return true
}

// https://www.rfc-editor.org/rfc/rfc9651.html#parse-param.
func consumeParameter(s string, f func(key, val string)) (consumed, rest string, ok bool) {
	rest = s
	for len(rest) != 0 {
		var key, val string
		val = "?1" // Default value for empty val is boolean true.
		if rest[0] != ';' {
			break
		}
		rest = rest[1:]
		rest = rest[countLeftWhitespace(rest):]
		key, rest, ok = consumeKey(rest)
		if !ok {
			return "", s, ok
		}
		if len(rest) != 0 && rest[0] == '=' {
			rest = rest[1:]
			val, rest, ok = consumeBareItem(rest)
			if !ok {
				return "", s, ok
			}
		}
		if f != nil {
			f(key, val)
		}
	}
	return s[:len(s)-len(rest)], rest, true
}

// ParseParameter parses a parameter from a given HTTP Structured Field Values.
//
// Given an HTTP SFV string that represents a parameter, it will call the given
// function using each of the keys and values contained in the parameter. This
// allows the caller to extract information out of the parameter.
//
// This function will return once it encounters the end of the string, or
// something that is not a parameter. If it cannot consume the entire given
// string, the ok value returned will be false.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#parse-param.
func ParseParameter(s string, f func(key, val string)) (ok bool) {
	_, rest, ok := consumeParameter(s, f)
	return rest == "" && ok
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-key.
func consumeKey(s string) (consumed, rest string, ok bool) {
	if len(s) == 0 || (!isLCAlpha(s[0]) && s[0] != '*') {
		return "", s, false
	}
	i := 0
	for _, ch := range []byte(s) {
		if !isLCAlpha(ch) && !isDigit(ch) && !slices.Contains([]byte("_-.*"), ch) {
			break
		}
		i++
	}
	return s[:i], s[i:], true
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-an-integer-or-decim.
func consumeIntegerOrDecimal(s string) (consumed, rest string, ok bool) {
	var i, signOffset, periodIndex int
	var isDecimal bool
	if i < len(s) && s[i] == '-' {
		i++
		signOffset++
	}
	if i >= len(s) {
		return "", s, false
	}
	if !isDigit(s[i]) {
		return "", s, false
	}
	for i < len(s) {
		ch := s[i]
		if isDigit(ch) {
			i++
			continue
		}
		if !isDecimal && ch == '.' {
			if i-signOffset > 12 {
				return "", s, false
			}
			periodIndex = i
			isDecimal = true
			i++
			continue
		}
		break
	}
	if !isDecimal && i-signOffset > 15 {
		return "", s, false
	}
	if isDecimal {
		if i-signOffset > 16 {
			return "", s, false
		}
		if s[i-1] == '.' {
			return "", s, false
		}
		if i-periodIndex-1 > 3 {
			return "", s, false
		}
	}
	return s[:i], s[i:], true
}

// ParseInteger parses an integer from a given HTTP Structured Field Values.
//
// The entire HTTP SFV string must consist of a valid integer. It returns the
// parsed integer and an ok boolean value, indicating success or not.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-an-integer-or-decim.
func ParseInteger(s string) (parsed int64, ok bool) {
	if _, rest, ok := consumeIntegerOrDecimal(s); !ok || rest != "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}
	return 0, false
}

// ParseDecimal parses a decimal from a given HTTP Structured Field Values.
//
// The entire HTTP SFV string must consist of a valid decimal. It returns the
// parsed decimal and an ok boolean value, indicating success or not.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-an-integer-or-decim.
func ParseDecimal(s string) (parsed float64, ok bool) {
	if _, rest, ok := consumeIntegerOrDecimal(s); !ok || rest != "" {
		return 0, false
	}
	if !strings.Contains(s, ".") {
		return 0, false
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n, true
	}
	return 0, false
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-string.
func consumeString(s string) (consumed, rest string, ok bool) {
	if len(s) == 0 || s[0] != '"' {
		return "", s, false
	}
	for i := 1; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '\\':
			if i+1 >= len(s) {
				return "", s, false
			}
			i++
			if ch = s[i]; ch != '"' && ch != '\\' {
				return "", s, false
			}
		case '"':
			return s[:i+1], s[i+1:], true
		default:
			if !isVChar(ch) && !isSP(ch) {
				return "", s, false
			}
		}
	}
	return "", s, false
}

// ParseString parses a Go string from a given HTTP Structured Field Values.
//
// The entire HTTP SFV string must consist of a valid string. It returns the
// parsed string and an ok boolean value, indicating success or not.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-string.
func ParseString(s string) (parsed string, ok bool) {
	if _, rest, ok := consumeString(s); !ok || rest != "" {
		return "", false
	}
	return s[1 : len(s)-1], true
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-token
func consumeToken(s string) (consumed, rest string, ok bool) {
	if len(s) == 0 || (!isAlpha(s[0]) && s[0] != '*') {
		return "", s, false
	}
	i := 0
	for _, ch := range []byte(s) {
		if !isTChar(ch) && !slices.Contains([]byte(":/"), ch) {
			break
		}
		i++
	}
	return s[:i], s[i:], true
}

// ParseToken parses a token from a given HTTP Structured Field Values.
//
// The entire HTTP SFV string must consist of a valid token. It returns the
// parsed token and an ok boolean value, indicating success or not.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-token
func ParseToken(s string) (parsed string, ok bool) {
	if _, rest, ok := consumeToken(s); !ok || rest != "" {
		return "", false
	}
	return s, true
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-byte-sequence.
func consumeByteSequence(s string) (consumed, rest string, ok bool) {
	if len(s) == 0 || s[0] != ':' {
		return "", s, false
	}
	for i := 1; i < len(s); i++ {
		if ch := s[i]; ch == ':' {
			return s[:i+1], s[i+1:], true
		}
		if ch := s[i]; !isAlpha(ch) && !isDigit(ch) && !slices.Contains([]byte("+/="), ch) {
			return "", s, false
		}
	}
	return "", s, false
}

// ParseByteSequence parses a byte sequence from a given HTTP Structured Field
// Values.
//
// The entire HTTP SFV string must consist of a valid byte sequence. It returns
// the parsed byte sequence and an ok boolean value, indicating success or not.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-byte-sequence.
func ParseByteSequence(s string) (parsed []byte, ok bool) {
	if _, rest, ok := consumeByteSequence(s); !ok || rest != "" {
		return nil, false
	}
	return []byte(s[1 : len(s)-1]), true
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-boolean.
func consumeBoolean(s string) (consumed, rest string, ok bool) {
	if len(s) >= 2 && (s[:2] == "?0" || s[:2] == "?1") {
		return s[:2], s[2:], true
	}
	return "", s, false
}

// ParseBoolean parses a boolean from a given HTTP Structured Field Values.
//
// The entire HTTP SFV string must consist of a valid boolean. It returns the
// parsed boolean and an ok boolean value, indicating success or not.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-boolean.
func ParseBoolean(s string) (parsed bool, ok bool) {
	if _, rest, ok := consumeBoolean(s); !ok || rest != "" {
		return false, false
	}
	return s == "?1", true
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-date.
func consumeDate(s string) (consumed, rest string, ok bool) {
	if len(s) == 0 || s[0] != '@' {
		return "", s, false
	}
	if _, rest, ok = consumeIntegerOrDecimal(s[1:]); !ok {
		return "", s, ok
	}
	consumed = s[:len(s)-len(rest)]
	if slices.Contains([]byte(consumed), '.') {
		return "", s, false
	}
	return consumed, rest, ok
}

// ParseDate parses a date from a given HTTP Structured Field Values.
//
// The entire HTTP SFV string must consist of a valid date. It returns the
// parsed date and an ok boolean value, indicating success or not.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-date.
func ParseDate(s string) (parsed time.Time, ok bool) {
	if _, rest, ok := consumeDate(s); !ok || rest != "" {
		return time.Time{}, false
	}
	if n, ok := ParseInteger(s[1:]); !ok {
		return time.Time{}, false
	} else {
		return time.Unix(n, 0), true
	}
}

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-display-string.
func consumeDisplayString(s string) (consumed, rest string, ok bool) {
	// To prevent excessive allocation, especially when input is large, we
	// maintain a buffer of 4 bytes to keep track of the last rune we
	// encounter. This way, we can validate that the display string conforms to
	// UTF-8 without actually building the whole string.
	var lastRune [4]byte
	var runeLen int
	isPartOfValidRune := func(ch byte) bool {
		lastRune[runeLen] = ch
		runeLen++
		if utf8.FullRune(lastRune[:runeLen]) {
			r, s := utf8.DecodeRune(lastRune[:runeLen])
			if r == utf8.RuneError {
				return false
			}
			copy(lastRune[:], lastRune[s:runeLen])
			runeLen -= s
			return true
		}
		return runeLen <= 4
	}

	if len(s) <= 1 || s[:2] != `%"` {
		return "", s, false
	}
	i := 2
	for i < len(s) {
		ch := s[i]
		if !isVChar(ch) && !isSP(ch) {
			return "", s, false
		}
		switch ch {
		case '"':
			if runeLen > 0 {
				return "", s, false
			}
			return s[:i+1], s[i+1:], true
		case '%':
			if i+2 >= len(s) {
				return "", s, false
			}
			if ch, ok = decOctetHex(s[i+1], s[i+2]); !ok {
				return "", s, ok
			}
			if ok = isPartOfValidRune(ch); !ok {
				return "", s, ok
			}
			i += 3
		default:
			if ok = isPartOfValidRune(ch); !ok {
				return "", s, ok
			}
			i++
		}
	}
	return "", s, false
}

// ParseDisplayString parses a display string from a given HTTP Structured
// Field Values.
//
// The entire HTTP SFV string must consist of a valid display string. It
// returns the parsed display string and an ok boolean value, indicating
// success or not.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-display-string.
func ParseDisplayString(s string) (parsed string, ok bool) {
	if _, rest, ok := consumeDisplayString(s); !ok || rest != "" {
		return "", false
	}
	// consumeDisplayString() already validates that we have a valid display
	// string. Therefore, we can just construct the display string, without
	// validating it again.
	s = s[2 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '%' {
			decoded, _ := decOctetHex(s[i+1], s[i+2])
			b.WriteByte(decoded)
			i += 3
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), true
}

// https://www.rfc-editor.org/rfc/rfc9651.html#parse-bare-item.
func consumeBareItem(s string) (consumed, rest string, ok bool) {
	if len(s) == 0 {
		return "", s, false
	}
	ch := s[0]
	switch {
	case ch == '-' || isDigit(ch):
		return consumeIntegerOrDecimal(s)
	case ch == '"':
		return consumeString(s)
	case ch == '*' || isAlpha(ch):
		return consumeToken(s)
	case ch == ':':
		return consumeByteSequence(s)
	case ch == '?':
		return consumeBoolean(s)
	case ch == '@':
		return consumeDate(s)
	case ch == '%':
		return consumeDisplayString(s)
	default:
		return "", s, false
	}
}
