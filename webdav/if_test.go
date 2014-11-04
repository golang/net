// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseIfHeader(t *testing.T) {
	// The "section x.y.z" test cases come from section x.y.z of the spec at
	// http://www.webdav.org/specs/rfc4918.html
	testCases := []struct {
		desc  string
		input string
		want  ifHeader
	}{{
		"bad: empty",
		``,
		ifHeader{},
	}, {
		"bad: no parens",
		`foobar`,
		ifHeader{},
	}, {
		"bad: empty list #1",
		`()`,
		ifHeader{},
	}, {
		"bad: empty list #2",
		`(a) (b c) () (d)`,
		ifHeader{},
	}, {
		"bad: no list after resource #1",
		`<foo>`,
		ifHeader{},
	}, {
		"bad: no list after resource #1",
		`<foo> <bar> (a)`,
		ifHeader{},
	}, {
		"bad: no list after resource #3",
		`<foo> (a) (b) <bar>`,
		ifHeader{},
	}, {
		"bad: no-tag-list followed by tagged-list",
		`(a) (b) <foo> (c)`,
		ifHeader{},
	}, {
		"bad: unfinished list",
		`(a`,
		ifHeader{},
	}, {
		"bad: unfinished ETag",
		`([b`,
		ifHeader{},
	}, {
		"bad: unfinished Notted list",
		`(Not a`,
		ifHeader{},
	}, {
		"bad: double Not",
		`(Not Not a)`,
		ifHeader{},
	}, {
		"good: one list with a stateToken",
		`(a)`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					stateToken: `a`,
				}},
			}},
		},
	}, {
		"good: one list with an ETag",
		`([a])`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					entityTag: `a`,
				}},
			}},
		},
	}, {
		"good: one list with three Nots",
		`(Not a Not b Not [d])`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					not:        true,
					stateToken: `a`,
				}, {
					not:        true,
					stateToken: `b`,
				}, {
					not:       true,
					entityTag: `d`,
				}},
			}},
		},
	}, {
		"good: two lists",
		`(a) (b)`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					stateToken: `a`,
				}},
			}, {
				conditions: []ifCondition{{
					stateToken: `b`,
				}},
			}},
		},
	}, {
		"good: two Notted lists",
		`(Not a) (Not b)`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					not:        true,
					stateToken: `a`,
				}},
			}, {
				conditions: []ifCondition{{
					not:        true,
					stateToken: `b`,
				}},
			}},
		},
	}, {
		"section 7.5.1",
		`<http://www.example.com/users/f/fielding/index.html> 
			(<urn:uuid:f81d4fae-7dec-11d0-a765-00a0c91e6bf6>)`,
		ifHeader{
			lists: []ifList{{
				resourceTag: `http://www.example.com/users/f/fielding/index.html`,
				conditions: []ifCondition{{
					stateToken: `urn:uuid:f81d4fae-7dec-11d0-a765-00a0c91e6bf6`,
				}},
			}},
		},
	}, {
		"section 7.5.2 #1",
		`(<urn:uuid:150852e2-3847-42d5-8cbe-0f4f296f26cf>)`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					stateToken: `urn:uuid:150852e2-3847-42d5-8cbe-0f4f296f26cf`,
				}},
			}},
		},
	}, {
		"section 7.5.2 #2",
		`<http://example.com/locked/>
			(<urn:uuid:150852e2-3847-42d5-8cbe-0f4f296f26cf>)`,
		ifHeader{
			lists: []ifList{{
				resourceTag: `http://example.com/locked/`,
				conditions: []ifCondition{{
					stateToken: `urn:uuid:150852e2-3847-42d5-8cbe-0f4f296f26cf`,
				}},
			}},
		},
	}, {
		"section 7.5.2 #3",
		`<http://example.com/locked/member>
			(<urn:uuid:150852e2-3847-42d5-8cbe-0f4f296f26cf>)`,
		ifHeader{
			lists: []ifList{{
				resourceTag: `http://example.com/locked/member`,
				conditions: []ifCondition{{
					stateToken: `urn:uuid:150852e2-3847-42d5-8cbe-0f4f296f26cf`,
				}},
			}},
		},
	}, {
		"section 9.9.6",
		`(<urn:uuid:fe184f2e-6eec-41d0-c765-01adc56e6bb4>) 
			(<urn:uuid:e454f3f3-acdc-452a-56c7-00a5c91e4b77>)`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					stateToken: `urn:uuid:fe184f2e-6eec-41d0-c765-01adc56e6bb4`,
				}},
			}, {
				conditions: []ifCondition{{
					stateToken: `urn:uuid:e454f3f3-acdc-452a-56c7-00a5c91e4b77`,
				}},
			}},
		},
	}, {
		"section 9.10.8",
		`(<urn:uuid:e71d4fae-5dec-22d6-fea5-00a0c91e6be4>)`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					stateToken: `urn:uuid:e71d4fae-5dec-22d6-fea5-00a0c91e6be4`,
				}},
			}},
		},
	}, {
		"section 10.4.6",
		`(<urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2> 
			["I am an ETag"])
			(["I am another ETag"])`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					stateToken: `urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2`,
				}, {
					entityTag: `"I am an ETag"`,
				}},
			}, {
				conditions: []ifCondition{{
					entityTag: `"I am another ETag"`,
				}},
			}},
		},
	}, {
		"section 10.4.7",
		`(Not <urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2> 
			<urn:uuid:58f202ac-22cf-11d1-b12d-002035b29092>)`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					not:        true,
					stateToken: `urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2`,
				}, {
					stateToken: `urn:uuid:58f202ac-22cf-11d1-b12d-002035b29092`,
				}},
			}},
		},
	}, {
		"section 10.4.8",
		`(<urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2>) 
			(Not <DAV:no-lock>)`,
		ifHeader{
			lists: []ifList{{
				conditions: []ifCondition{{
					stateToken: `urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2`,
				}},
			}, {
				conditions: []ifCondition{{
					not:        true,
					stateToken: `DAV:no-lock`,
				}},
			}},
		},
	}, {
		"section 10.4.9",
		`</resource1> 
			(<urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2> 
			[W/"A weak ETag"]) (["strong ETag"])`,
		ifHeader{
			lists: []ifList{{
				resourceTag: `/resource1`,
				conditions: []ifCondition{{
					stateToken: `urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2`,
				}, {
					entityTag: `W/"A weak ETag"`,
				}},
			}, {
				resourceTag: `/resource1`,
				conditions: []ifCondition{{
					entityTag: `"strong ETag"`,
				}},
			}},
		},
	}, {
		"section 10.4.10",
		`<http://www.example.com/specs/> 
			(<urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2>)`,
		ifHeader{
			lists: []ifList{{
				resourceTag: `http://www.example.com/specs/`,
				conditions: []ifCondition{{
					stateToken: `urn:uuid:181d4fae-7d8c-11d0-a765-00a0c91e6bf2`,
				}},
			}},
		},
	}, {
		"section 10.4.11 #1",
		`</specs/rfc2518.doc> (["4217"])`,
		ifHeader{
			lists: []ifList{{
				resourceTag: `/specs/rfc2518.doc`,
				conditions: []ifCondition{{
					entityTag: `"4217"`,
				}},
			}},
		},
	}, {
		"section 10.4.11 #2",
		`</specs/rfc2518.doc> (Not ["4217"])`,
		ifHeader{
			lists: []ifList{{
				resourceTag: `/specs/rfc2518.doc`,
				conditions: []ifCondition{{
					not:       true,
					entityTag: `"4217"`,
				}},
			}},
		},
	}}

	for _, tc := range testCases {
		got, ok := parseIfHeader(strings.Replace(tc.input, "\n", "", -1))
		if gotEmpty := reflect.DeepEqual(got, ifHeader{}); gotEmpty == ok {
			t.Errorf("%s: should be different: empty header == %t, ok == %t", tc.desc, gotEmpty, ok)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\ngot  %v\nwant %v", tc.desc, got, tc.want)
			continue
		}
	}
}
