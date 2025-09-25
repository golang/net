// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package httpsfv provide functionality for dealing with HTTP Structured Field
// Values.
package httpsfv

import (
	"slices"
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

// TODO(nsh): Implement other consume functions that will be needed to fully
// deal with all possible HTTP SFV, specifically:
// - consumeDictionary(s string, f func(key, val, param string)) (consumed, rest string, ok bool)
//   For example, given `a=123,b;a="a", i`, ConsumeDictionary will call f() 3 times
//   with the following args:
//   - key: `a`, val: `123`, param: ``
//   - key: `b`, val: ``, param:`;a="a"`
//   - key: `i`, val: ``, param: ``
//
// - consumeList(s string, f func(member, param string)) (consumed, rest string, ok bool)
//   For example, given `123.456;i, ("foo" "bar"; lvl=2); lvl=1`, ConsumeList will
//   call f() 2 times with the following args:
//   - member: `123.456`, param: `i`
//   - member: `("foo" "bar"; lvl=2)`, param: `; lvl=1`
//
// - consumeItem(s string, f func(bareItem, param string)) (consumed, rest string, ok bool)
//   For example, given `"foo"; bar=baz;foo=bar`, ConsumeItem will call f() with
//   the following args:
//   - bareItem: `"foo"`, param: `; bar=baz;foo=bar`
//
// - consumeInnerList(s string f func(bareItem, param, listParam string)) (consumed, rest string, ok bool)
//   For example, given `("foo"; a=1;b=2 "bar";baz;lvl=2);lvl=1`, ConsumeInnerList
//   will call f() 2 times with the following args:
//   - bareItem: `"foo"`, param: `; a=1;b=2`, listParam: `;lvl=1`
//   - bareItem: `"bar"`, param: `;baz;lvl=2`, listParam: `;lvl=1`

// TODO(nsh): Implement corresponding parse functions for all consume functions
// that exists.

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
		if i := countLeftWhitespace(rest); i > 0 {
			rest = rest[i:]
		}
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

// ParseParameter is used to parse a string that represents a parameter in an
// HTTP Structured Field Values.
//
// Given a string that represents a parameter, it will call the given function
// using each of the keys and values contained in the parameter. This allows
// the caller to extract information out of the parameter.
//
// This function will return once it encounters the end of the string, or
// something that is not a parameter. If it cannot consume the entire given
// string, the ok value returned will be false.
//
// https://www.rfc-editor.org/rfc/rfc9651.html#parse-param.
func ParseParameter(s string, f func(key, val string)) (ok bool) {
	_, rest, ok := consumeParameter(s, f)
	if rest != "" {
		return false
	}
	return ok
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

// https://www.rfc-editor.org/rfc/rfc9651.html#name-parsing-a-boolean.
func consumeBoolean(s string) (consumed, rest string, ok bool) {
	if len(s) >= 2 && (s[:2] == "?0" || s[:2] == "?1") {
		return s[:2], s[2:], true
	}
	return "", s, false
}

// https://www.rfc-editor.org/rfc/rfc9651.html#parse-bare-item.
func consumeBareItem(s string) (consumed, rest string, ok bool) {
	if len(s) == 0 {
		return "", s, false
	}

	// TODO(nsh): This is currently only up to date with RFC 8941. Implement
	// Date and Display string for full feature parity with RFC 9651.
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
	default:
		return "", s, false
	}
}
