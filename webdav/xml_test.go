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

func TestReadLockInfo(t *testing.T) {
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

func TestReadPropfind(t *testing.T) {
	testCases := []struct {
		desc       string
		input      string
		wantPF     propfind
		wantStatus int
	}{{
		desc: "propfind: propname",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:propname/>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName:  xml.Name{"DAV:", "propfind"},
			Propname: new(struct{}),
		},
	}, {
		desc:  "propfind: empty body means allprop",
		input: "",
		wantPF: propfind{
			Allprop: new(struct{}),
		},
	}, {
		desc: "propfind: allprop",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"   <A:allprop/>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName: xml.Name{"DAV:", "propfind"},
			Allprop: new(struct{}),
		},
	}, {
		desc: "propfind: allprop followed by include",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:allprop/>\n" +
			"  <A:include><A:displayname/></A:include>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName: xml.Name{"DAV:", "propfind"},
			Allprop: new(struct{}),
			Include: propnames{xml.Name{"DAV:", "displayname"}},
		},
	}, {
		desc: "propfind: include followed by allprop",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:include><A:displayname/></A:include>\n" +
			"  <A:allprop/>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName: xml.Name{"DAV:", "propfind"},
			Allprop: new(struct{}),
			Include: propnames{xml.Name{"DAV:", "displayname"}},
		},
	}, {
		desc: "propfind: propfind",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop><A:displayname/></A:prop>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName: xml.Name{"DAV:", "propfind"},
			Prop:    propnames{xml.Name{"DAV:", "displayname"}},
		},
	}, {
		desc: "propfind: prop with ignored comments",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop>\n" +
			"    <!-- ignore -->\n" +
			"    <A:displayname><!-- ignore --></A:displayname>\n" +
			"  </A:prop>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName: xml.Name{"DAV:", "propfind"},
			Prop:    propnames{xml.Name{"DAV:", "displayname"}},
		},
	}, {
		desc: "propfind: propfind with ignored whitespace",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop>   <A:displayname/></A:prop>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName: xml.Name{"DAV:", "propfind"},
			Prop:    propnames{xml.Name{"DAV:", "displayname"}},
		},
	}, {
		desc: "propfind: propfind with ignored mixed-content",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop>foo<A:displayname/>bar</A:prop>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName: xml.Name{"DAV:", "propfind"},
			Prop:    propnames{xml.Name{"DAV:", "displayname"}},
		},
	}, {
		desc: "propfind: propname with ignored element (section A.4)",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:propname/>\n" +
			"  <E:leave-out xmlns:E='E:'>*boss*</E:leave-out>\n" +
			"</A:propfind>",
		wantPF: propfind{
			XMLName:  xml.Name{"DAV:", "propfind"},
			Propname: new(struct{}),
		},
	}, {
		desc:       "propfind: bad: junk",
		input:      "xxx",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: propname and allprop (section A.3)",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:propname/>" +
			"  <A:allprop/>" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: propname and prop",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop><A:displayname/></A:prop>\n" +
			"  <A:propname/>\n" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: allprop and prop",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:allprop/>\n" +
			"  <A:prop><A:foo/><A:/prop>\n" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: empty propfind with ignored element (section A.4)",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <E:expired-props/>\n" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: empty prop",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop/>\n" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: prop with just chardata",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop>foo</A:prop>\n" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "bad: interrupted prop",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop><A:foo></A:prop>\n",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "bad: malformed end element prop",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop><A:foo/></A:bar></A:prop>\n",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: property with chardata value",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop><A:foo>bar</A:foo></A:prop>\n" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: property with whitespace value",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:prop><A:foo> </A:foo></A:prop>\n" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}, {
		desc: "propfind: bad: include without allprop",
		input: "" +
			"<A:propfind xmlns:A='DAV:'>\n" +
			"  <A:include><A:foo/></A:include>\n" +
			"</A:propfind>",
		wantStatus: http.StatusBadRequest,
	}}

	for _, tc := range testCases {
		pf, status, err := readPropfind(strings.NewReader(tc.input))
		if tc.wantStatus != 0 {
			if err == nil {
				t.Errorf("%s: got nil error, want non-nil", tc.desc)
				continue
			}
		} else if err != nil {
			t.Errorf("%s: %v", tc.desc, err)
			continue
		}
		if !reflect.DeepEqual(pf, tc.wantPF) || status != tc.wantStatus {
			t.Errorf("%s:\ngot  propfind=%v, status=%v\nwant propfind=%v, status=%v",
				tc.desc, pf, status, tc.wantPF, tc.wantStatus)
			continue
		}
	}
}
