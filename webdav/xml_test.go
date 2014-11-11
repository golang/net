// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"encoding/xml"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestParseLockInfo(t *testing.T) {
	// The "section x.y.z" test cases come from section x.y.z of the spec at
	// http://www.webdav.org/specs/rfc4918.html
	testCases := []struct {
		desc       string
		input      string
		wantLI     lockInfo
		wantStatus int
	}{{
		"bad: junk",
		"xxx",
		lockInfo{},
		http.StatusBadRequest,
	}, {
		"bad: invalid owner XML",
		"" +
			"<D:lockinfo xmlns:D='DAV:'>\n" +
			"  <D:lockscope><D:exclusive/></D:lockscope>\n" +
			"  <D:locktype><D:write/></D:locktype>\n" +
			"  <D:owner>\n" +
			"    <D:href>   no end tag   \n" +
			"  </D:owner>\n" +
			"</D:lockinfo>",
		lockInfo{},
		http.StatusBadRequest,
	}, {
		"bad: invalid UTF-8",
		"" +
			"<D:lockinfo xmlns:D='DAV:'>\n" +
			"  <D:lockscope><D:exclusive/></D:lockscope>\n" +
			"  <D:locktype><D:write/></D:locktype>\n" +
			"  <D:owner>\n" +
			"    <D:href>   \xff   </D:href>\n" +
			"  </D:owner>\n" +
			"</D:lockinfo>",
		lockInfo{},
		http.StatusBadRequest,
	}, {
		"bad: unfinished XML #1",
		"" +
			"<D:lockinfo xmlns:D='DAV:'>\n" +
			"  <D:lockscope><D:exclusive/></D:lockscope>\n" +
			"  <D:locktype><D:write/></D:locktype>\n",
		lockInfo{},
		http.StatusBadRequest,
	}, {
		"bad: unfinished XML #2",
		"" +
			"<D:lockinfo xmlns:D='DAV:'>\n" +
			"  <D:lockscope><D:exclusive/></D:lockscope>\n" +
			"  <D:locktype><D:write/></D:locktype>\n" +
			"  <D:owner>\n",
		lockInfo{},
		http.StatusBadRequest,
	}, {
		"good: empty",
		"",
		lockInfo{},
		0,
	}, {
		"good: plain-text owner",
		"" +
			"<D:lockinfo xmlns:D='DAV:'>\n" +
			"  <D:lockscope><D:exclusive/></D:lockscope>\n" +
			"  <D:locktype><D:write/></D:locktype>\n" +
			"  <D:owner>gopher</D:owner>\n" +
			"</D:lockinfo>",
		lockInfo{
			XMLName:   xml.Name{Space: "DAV:", Local: "lockinfo"},
			Exclusive: new(struct{}),
			Write:     new(struct{}),
			Owner: owner{
				InnerXML: "gopher",
			},
		},
		0,
	}, {
		"section 9.10.7",
		"" +
			"<D:lockinfo xmlns:D='DAV:'>\n" +
			"  <D:lockscope><D:exclusive/></D:lockscope>\n" +
			"  <D:locktype><D:write/></D:locktype>\n" +
			"  <D:owner>\n" +
			"    <D:href>http://example.org/~ejw/contact.html</D:href>\n" +
			"  </D:owner>\n" +
			"</D:lockinfo>",
		lockInfo{
			XMLName:   xml.Name{Space: "DAV:", Local: "lockinfo"},
			Exclusive: new(struct{}),
			Write:     new(struct{}),
			Owner: owner{
				InnerXML: "\n    <D:href>http://example.org/~ejw/contact.html</D:href>\n  ",
			},
		},
		0,
	}}

	for _, tc := range testCases {
		li, status, err := readLockInfo(strings.NewReader(tc.input))
		if tc.wantStatus != 0 {
			if err == nil {
				t.Errorf("%s: got nil error, want non-nil", tc.desc)
				continue
			}
		} else if err != nil {
			t.Errorf("%s: %v", tc.desc, err)
			continue
		}
		if !reflect.DeepEqual(li, tc.wantLI) || status != tc.wantStatus {
			t.Errorf("%s:\ngot  lockInfo=%v, status=%v\nwant lockInfo=%v, status=%v",
				tc.desc, li, status, tc.wantLI, tc.wantStatus)
			continue
		}
	}
}
