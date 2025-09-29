// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package httpsfv

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestConsumeBareInnerList(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		wantBareItems []string
		wantParams    []string
		wantListParam string
		wantOk        bool
	}{
		{
			name:          "valid inner list without param",
			in:            `(a b c)`,
			wantBareItems: []string{"a", "b", "c"},
			wantParams:    []string{"", "", ""},
			wantOk:        true,
		},
		{
			name:          "valid inner list with param",
			in:            `(a;d b c;e)`,
			wantBareItems: []string{"a", "b", "c"},
			wantParams:    []string{";d", "", ";e"},
			wantOk:        true,
		},
		{
			name:          "valid inner list with fake ending parenthesis",
			in:            `(")";foo=")")`,
			wantBareItems: []string{`")"`},
			wantParams:    []string{`;foo=")"`},
			wantOk:        true,
		},
		{
			name:          "valid inner list with list parameter",
			in:            `(a b;c); d`,
			wantBareItems: []string{"a", "b"},
			wantParams:    []string{"", ";c"},
			wantOk:        true,
		},
		{
			name:          "valid inner list with more content after",
			in:            `(a b;c); d, a`,
			wantBareItems: []string{"a", "b"},
			wantParams:    []string{"", ";c"},
			wantOk:        true,
		},
		{
			name:          "invalid inner list",
			in:            `(a b;c `,
			wantBareItems: []string{"a", "b"},
			wantParams:    []string{"", ";c"},
		},
	}

	for _, tc := range tests {
		var gotBareItems, gotParams []string
		f := func(bareItem, param string) {
			gotBareItems = append(gotBareItems, bareItem)
			gotParams = append(gotParams, param)
		}
		gotConsumed, gotRest, ok := consumeBareInnerList(tc.in, f)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if !slices.Equal(tc.wantBareItems, gotBareItems) {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, gotBareItems, tc.wantBareItems)
		}
		if !slices.Equal(tc.wantParams, gotParams) {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, gotParams, tc.wantParams)
		}
		if gotConsumed+gotRest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, gotConsumed, gotRest, tc.in)
		}
	}
}

func TestParseBareInnerList(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		wantBareItems []string
		wantParams    []string
		wantOk        bool
	}{
		{
			name:          "valid inner list",
			in:            `(a b;c)`,
			wantBareItems: []string{"a", "b"},
			wantParams:    []string{"", ";c"},
			wantOk:        true,
		},
		{
			name:          "valid inner list with list parameter",
			in:            `(a b;c); d`,
			wantBareItems: []string{"a", "b"},
			wantParams:    []string{"", ";c"},
		},
		{
			name:          "invalid inner list",
			in:            `(a b;c `,
			wantBareItems: []string{"a", "b"},
			wantParams:    []string{"", ";c"},
		},
	}

	for _, tc := range tests {
		var gotBareItems, gotParams []string
		f := func(bareItem, param string) {
			gotBareItems = append(gotBareItems, bareItem)
			gotParams = append(gotParams, param)
		}
		ok := ParseBareInnerList(tc.in, f)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if !slices.Equal(tc.wantBareItems, gotBareItems) {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, gotBareItems, tc.wantBareItems)
		}
		if !slices.Equal(tc.wantParams, gotParams) {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, gotParams, tc.wantParams)
		}
	}
}

func TestConsumeItem(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantBareItem string
		wantParam    string
		wantOk       bool
	}{
		{
			name:         "valid bare item",
			in:           `fookey`,
			wantBareItem: `fookey`,
			wantOk:       true,
		},
		{
			name:         "valid bare item and param",
			in:           `fookey; a="123"`,
			wantBareItem: `fookey`,
			wantParam:    `; a="123"`,
			wantOk:       true,
		},
		{
			name:         "valid item with content after",
			in:           `fookey; a="123", otheritem; otherparam=1`,
			wantBareItem: `fookey`,
			wantParam:    `; a="123"`,
			wantOk:       true,
		},
		{
			name: "invalid just param",
			in:   `;a="123"`,
		},
	}

	for _, tc := range tests {
		var gotBareItem, gotParam string
		f := func(bareItem, param string) {
			gotBareItem = bareItem
			gotParam = param
		}
		gotConsumed, gotRest, ok := consumeItem(tc.in, f)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if tc.wantBareItem != gotBareItem {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, gotBareItem, tc.wantBareItem)
		}
		if tc.wantParam != gotParam {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, gotParam, tc.wantParam)
		}
		if gotConsumed+gotRest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, gotConsumed, gotRest, tc.in)
		}
	}
}

func TestParseItem(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantBareItem string
		wantParam    string
		wantOk       bool
	}{
		{
			name:         "valid bare item",
			in:           `fookey`,
			wantBareItem: `fookey`,
			wantOk:       true,
		},
		{
			name:         "valid bare item and param",
			in:           `fookey; a="123"`,
			wantBareItem: `fookey`,
			wantParam:    `; a="123"`,
			wantOk:       true,
		},
		{
			name:         "valid item with content after",
			in:           `fookey; a="123", otheritem; otherparam=1`,
			wantBareItem: `fookey`,
			wantParam:    `; a="123"`,
		},
		{
			name: "invalid just param",
			in:   `;a="123"`,
		},
	}

	for _, tc := range tests {
		var gotBareItem, gotParam string
		f := func(bareItem, param string) {
			gotBareItem = bareItem
			gotParam = param
		}
		ok := ParseItem(tc.in, f)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if tc.wantBareItem != gotBareItem {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, gotBareItem, tc.wantBareItem)
		}
		if tc.wantParam != gotParam {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, gotParam, tc.wantParam)
		}
	}
}

func TestConsumeParameter(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   any
		wantOk bool
	}{
		{
			name:   "valid string",
			in:     `;parameter;want="wantvalue"`,
			want:   "wantvalue",
			wantOk: true,
		},
		{
			name:   "valid integer",
			in:     `;parameter;want=123456;something`,
			want:   123456,
			wantOk: true,
		},
		{
			name:   "valid decimal",
			in:     `;parameter;want=3.14;something`,
			want:   3.14,
			wantOk: true,
		},
		{
			name:   "valid implicit bool",
			in:     `;parameter;want;something`,
			want:   true,
			wantOk: true,
		},
		{
			name:   "valid token",
			in:     `;want=*atoken;something`,
			want:   "*atoken",
			wantOk: true,
		},
		{
			name:   "valid byte sequence",
			in:     `;want=:eWF5Cg==:;something`,
			want:   "eWF5Cg==",
			wantOk: true,
		},
		{
			name:   "valid repeated key",
			in:     `;want=:eWF5Cg==:;now;want=1;is;repeated;want="overwritten!"`,
			want:   "overwritten!",
			wantOk: true,
		},
		{
			name:   "valid parameter with content after",
			in:     `;want=:eWF5Cg==:;now;want=1;is;repeated;want="overwritten!", some=stuff`,
			want:   "overwritten!",
			wantOk: true,
		},
		{
			name: "invalid parameter",
			in:   `;UPPERCASEKEY=NOT_ACCEPTED`,
		},
	}

	for _, tc := range tests[len(tests)-1:] {
		var got any
		f := func(key, val string) {
			if key != "want" {
				return
			}
			switch {
			case strings.HasPrefix(val, "?"): // Bool
				got = val == "?1"
			case strings.HasPrefix(val, `"`): // String
				got = val[1 : len(val)-1]
			case strings.HasPrefix(val, "*"): // Token
				got = val
			case strings.HasPrefix(val, ":"): // Byte sequence
				got = val[1 : len(val)-1]
			default:
				if valConv, err := strconv.Atoi(val); err == nil { // Integer
					got = valConv
					return
				}
				if valConv, err := strconv.ParseFloat(val, 64); err == nil { // Float
					got = valConv
					return
				}
			}
		}
		consumed, rest, ok := consumeParameter(tc.in, f)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if got != tc.want {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, got, tc.want)
		}
		if consumed+rest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, got, rest, tc.in)
		}
	}
}

func TestParseParameter(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   any
		wantOk bool
	}{
		{
			name:   "valid parameter",
			in:     `;parameter;want="wantvalue"`,
			want:   "wantvalue",
			wantOk: true,
		},
		{
			name: "valid parameter with content after",
			in:   `;want=:eWF5Cg==:;now;want=1;is;repeated;want="overwritten!", some=stuff`,
			want: "overwritten!",
		},
		{
			name: "invalid parameter",
			in:   `;UPPERCASEKEY=NOT_ACCEPTED`,
		},
	}

	for _, tc := range tests[len(tests)-1:] {
		var got any
		f := func(key, val string) {
			if key != "want" {
				return
			}
			switch {
			case strings.HasPrefix(val, "?"): // Bool
				got = val == "?1"
			case strings.HasPrefix(val, `"`): // String
				got = val[1 : len(val)-1]
			case strings.HasPrefix(val, "*"): // Token
				got = val
			case strings.HasPrefix(val, ":"): // Byte sequence
				got = val[1 : len(val)-1]
			default:
				if valConv, err := strconv.Atoi(val); err == nil { // Integer
					got = valConv
					return
				}
				if valConv, err := strconv.ParseFloat(val, 64); err == nil { // Float
					got = valConv
					return
				}
			}
		}
		ok := ParseParameter(tc.in, f)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if got != tc.want {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, got, tc.want)
		}
	}
}

func TestConsumeKey(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOk bool
	}{
		{
			name:   "valid basic key",
			in:     `fookey`,
			want:   `fookey`,
			wantOk: true,
		},
		{
			name:   "valid basic key with more content after",
			in:     `fookey,u=7`,
			want:   `fookey`,
			wantOk: true,
		},
		{
			name: "invalid key",
			in:   `1keycannotstartwithnum`,
		},
	}

	for _, tc := range tests {
		got, gotRest, ok := consumeKey(tc.in)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if tc.want != got {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, got, tc.want)
		}
		if got+gotRest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, got, gotRest, tc.in)
		}
	}
}

func TestConsumeIntegerOrDecimal(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOk bool
	}{
		{
			name:   "valid integer",
			in:     "123456",
			want:   "123456",
			wantOk: true,
		},
		{
			name:   "valid integer with more content after",
			in:     "123456,12345",
			want:   "123456",
			wantOk: true,
		},
		{
			name:   "valid max integer",
			in:     "999999999999999",
			want:   "999999999999999",
			wantOk: true,
		},
		{
			name:   "valid min integer",
			in:     "-999999999999999",
			want:   "-999999999999999",
			wantOk: true,
		},
		{
			name: "invalid integer too high",
			in:   "9999999999999999",
		},
		{
			name: "invalid integer too low",
			in:   "-9999999999999999",
		},
		{
			name:   "valid decimal",
			in:     "-123456789012.123",
			want:   "-123456789012.123",
			wantOk: true,
		},
		{
			name: "invalid decimal integer component too long",
			in:   "1234567890123.1",
		},
		{
			name: "invalid decimal fraction component too long",
			in:   "1.1234",
		},
		{
			name: "invalid decimal trailing dot",
			in:   "1.",
		},
	}

	for _, tc := range tests {
		got, gotRest, ok := consumeIntegerOrDecimal(tc.in)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if tc.want != got {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, got, tc.want)
		}
		if got+gotRest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, got, gotRest, tc.in)
		}
	}
}

func TestConsumeString(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOk bool
	}{
		{
			name:   "valid basic string",
			in:     `"foo bar"`,
			want:   `"foo bar"`,
			wantOk: true,
		},
		{
			name:   "valid basic string with more content after",
			in:     `"foo bar", a=3`,
			want:   `"foo bar"`,
			wantOk: true,
		},
		{
			name:   "valid string with escaped dquote",
			in:     `"foo bar \""`,
			want:   `"foo bar \""`,
			wantOk: true,
		},
		{
			name: "invalid string no starting dquote",
			in:   `foo bar"`,
		},
		{
			name: "invalid string no closing dquote",
			in:   `"foo bar`,
		},
		{
			name: "invalid string invalid character",
			in:   string([]byte{'"', 0x00, '"'}),
		},
	}

	for _, tc := range tests {
		got, gotRest, ok := consumeString(tc.in)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if tc.want != got {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, got, tc.want)
		}
		if got+gotRest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, got, gotRest, tc.in)
		}
	}
}

func TestConsumeToken(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOk bool
	}{
		{
			name:   "valid token",
			in:     "*atoken",
			want:   "*atoken",
			wantOk: true,
		},
		{
			name:   "valid token with more content after",
			in:     "*atoken something",
			want:   "*atoken",
			wantOk: true,
		},
		{
			name: "invalid token",
			in:   "0invalid",
		},
	}

	for _, tc := range tests {
		got, gotRest, ok := consumeToken(tc.in)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if tc.want != got {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, got, tc.want)
		}
		if got+gotRest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, got, gotRest, tc.in)
		}
	}
}

func TestConsumeByteSequence(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOk bool
	}{
		{
			name:   "valid byte sequence",
			in:     ":aGVsbG8gd29ybGQ=:",
			want:   ":aGVsbG8gd29ybGQ=:",
			wantOk: true,
		},
		{
			name:   "valid byte sequence with more content after",
			in:     ":aGVsbG8gd29ybGQ=::aGVsbG8gd29ybGQ=:",
			want:   ":aGVsbG8gd29ybGQ=:",
			wantOk: true,
		},
		{
			name: "invalid byte sequence character",
			in:   ":-:",
		},
		{
			name: "invalid byte sequence opening",
			in:   "aGVsbG8gd29ybGQ=:",
		},
		{
			name: "invalid byte sequence closing",
			in:   ":aGVsbG8gd29ybGQ=",
		},
	}

	for _, tc := range tests {
		got, gotRest, ok := consumeByteSequence(tc.in)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if tc.want != got {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, got, tc.want)
		}
		if got+gotRest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, got, gotRest, tc.in)
		}
	}
}

func TestConsumeBoolean(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOk bool
	}{
		{
			name:   "valid boolean",
			in:     "?0",
			want:   "?0",
			wantOk: true,
		},
		{
			name:   "valid boolean with more content after",
			in:     "?1, a=1",
			want:   "?1",
			wantOk: true,
		},
		{
			name: "invalid boolean",
			in:   "!2",
		},
	}

	for _, tc := range tests {
		got, gotRest, ok := consumeBoolean(tc.in)
		if ok != tc.wantOk {
			t.Fatalf("test %q: want ok to be %v, got: %v", tc.name, tc.wantOk, ok)
		}
		if tc.want != got {
			t.Fatalf("test %q: mismatch.\n got: %#v\nwant: %#v\n", tc.name, got, tc.want)
		}
		if got+gotRest != tc.in {
			t.Fatalf("test %q: %#v + %#v != %#v", tc.name, got, gotRest, tc.in)
		}
	}
}
