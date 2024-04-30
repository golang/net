// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	ixml "golang.org/x/net/webdav/internal/xml"
)

// createLockBody comes from the example in Section 9.10.7.
const createLockBody = `<?xml version="1.0" encoding="utf-8" ?>
	<D:lockinfo xmlns:D='DAV:'>
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner>
			<D:href>http://example.org/~ejw/contact.html</D:href>
		</D:owner>
	</D:lockinfo>
`

// TODO: add tests to check XML responses with the expected prefix path
func TestPrefix(t *testing.T) {
	const dst, blah = "Destination", "blah blah blah"

	do := func(method, urlStr string, body string, wantStatusCode int, headers ...string) (http.Header, error) {
		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, urlStr, bodyReader)
		if err != nil {
			return nil, err
		}
		for len(headers) >= 2 {
			req.Header.Add(headers[0], headers[1])
			headers = headers[2:]
		}
		res, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != wantStatusCode {
			return nil, fmt.Errorf("%s got status code %d, want %d", urlStr, res.StatusCode, wantStatusCode)
		}
		return res.Header, nil
	}

	prefixes := []string{
		"/",
		"/a/",
		"/a/b/",
		"/a/b/c/",
	}
	ctx := context.Background()
	for _, prefix := range prefixes {
		fs := NewMemFS()
		h := &Handler{
			FileSystem: fs,
			LockSystem: NewMemLS(),
		}
		mux := http.NewServeMux()
		if prefix != "/" {
			h.Prefix = prefix
		}
		mux.Handle(prefix, h)
		srv := httptest.NewServer(mux)
		defer srv.Close()

		// The script is:
		//	MKCOL /a
		//	MKCOL /a/b
		//	PUT   /a/b/c
		//	COPY  /a/b/c /a/b/d
		//	MKCOL /a/b/e
		//	MOVE  /a/b/d /a/b/e/f
		//	LOCK  /a/b/e/g
		//	PUT   /a/b/e/g
		// which should yield the (possibly stripped) filenames /a/b/c,
		// /a/b/e/f and /a/b/e/g, plus their parent directories.

		wantA := map[string]int{
			"/":       http.StatusCreated,
			"/a/":     http.StatusMovedPermanently,
			"/a/b/":   http.StatusNotFound,
			"/a/b/c/": http.StatusNotFound,
		}[prefix]
		if _, err := do("MKCOL", srv.URL+"/a", "", wantA); err != nil {
			t.Errorf("prefix=%-9q MKCOL /a: %v", prefix, err)
			continue
		}

		wantB := map[string]int{
			"/":       http.StatusCreated,
			"/a/":     http.StatusCreated,
			"/a/b/":   http.StatusMovedPermanently,
			"/a/b/c/": http.StatusNotFound,
		}[prefix]
		if _, err := do("MKCOL", srv.URL+"/a/b", "", wantB); err != nil {
			t.Errorf("prefix=%-9q MKCOL /a/b: %v", prefix, err)
			continue
		}

		wantC := map[string]int{
			"/":       http.StatusCreated,
			"/a/":     http.StatusCreated,
			"/a/b/":   http.StatusCreated,
			"/a/b/c/": http.StatusMovedPermanently,
		}[prefix]
		if _, err := do("PUT", srv.URL+"/a/b/c", blah, wantC); err != nil {
			t.Errorf("prefix=%-9q PUT /a/b/c: %v", prefix, err)
			continue
		}

		wantD := map[string]int{
			"/":       http.StatusCreated,
			"/a/":     http.StatusCreated,
			"/a/b/":   http.StatusCreated,
			"/a/b/c/": http.StatusMovedPermanently,
		}[prefix]
		if _, err := do("COPY", srv.URL+"/a/b/c", "", wantD, dst, srv.URL+"/a/b/d"); err != nil {
			t.Errorf("prefix=%-9q COPY /a/b/c /a/b/d: %v", prefix, err)
			continue
		}

		wantE := map[string]int{
			"/":       http.StatusCreated,
			"/a/":     http.StatusCreated,
			"/a/b/":   http.StatusCreated,
			"/a/b/c/": http.StatusNotFound,
		}[prefix]
		if _, err := do("MKCOL", srv.URL+"/a/b/e", "", wantE); err != nil {
			t.Errorf("prefix=%-9q MKCOL /a/b/e: %v", prefix, err)
			continue
		}

		wantF := map[string]int{
			"/":       http.StatusCreated,
			"/a/":     http.StatusCreated,
			"/a/b/":   http.StatusCreated,
			"/a/b/c/": http.StatusNotFound,
		}[prefix]
		if _, err := do("MOVE", srv.URL+"/a/b/d", "", wantF, dst, srv.URL+"/a/b/e/f"); err != nil {
			t.Errorf("prefix=%-9q MOVE /a/b/d /a/b/e/f: %v", prefix, err)
			continue
		}

		var lockToken string
		wantG := map[string]int{
			"/":       http.StatusCreated,
			"/a/":     http.StatusCreated,
			"/a/b/":   http.StatusCreated,
			"/a/b/c/": http.StatusNotFound,
		}[prefix]
		if h, err := do("LOCK", srv.URL+"/a/b/e/g", createLockBody, wantG); err != nil {
			t.Errorf("prefix=%-9q LOCK /a/b/e/g: %v", prefix, err)
			continue
		} else {
			lockToken = h.Get("Lock-Token")
		}

		ifHeader := fmt.Sprintf("<%s/a/b/e/g> (%s)", srv.URL, lockToken)
		wantH := map[string]int{
			"/":       http.StatusCreated,
			"/a/":     http.StatusCreated,
			"/a/b/":   http.StatusCreated,
			"/a/b/c/": http.StatusNotFound,
		}[prefix]
		if _, err := do("PUT", srv.URL+"/a/b/e/g", blah, wantH, "If", ifHeader); err != nil {
			t.Errorf("prefix=%-9q PUT /a/b/e/g: %v", prefix, err)
			continue
		}

		wantI := map[string]int{
			"/":       http.StatusLocked,
			"/a/":     http.StatusLocked,
			"/a/b/":   http.StatusLocked,
			"/a/b/c/": http.StatusNotFound,
		}[prefix]
		if _, err := do("PUT", srv.URL+"/a/b/e/g", blah, wantI); err != nil {
			t.Errorf("prefix=%-9q PUT /a/b/e/g: %v", prefix, err)
			continue
		}

		badIfHeader := fmt.Sprintf("<%s/a/b/e/g> (foobar)", srv.URL)
		wantJ := map[string]int{
			"/":       http.StatusPreconditionFailed,
			"/a/":     http.StatusPreconditionFailed,
			"/a/b/":   http.StatusPreconditionFailed,
			"/a/b/c/": http.StatusNotFound,
		}[prefix]
		if _, err := do("PUT", srv.URL+"/a/b/e/g", blah, wantJ, "If", badIfHeader); err != nil {
			t.Errorf("prefix=%-9q PUT /a/b/e/g: %v", prefix, err)
			continue
		}

		got, err := find(ctx, nil, fs, "/")
		if err != nil {
			t.Errorf("prefix=%-9q find: %v", prefix, err)
			continue
		}
		sort.Strings(got)
		want := map[string][]string{
			"/":       {"/", "/a", "/a/b", "/a/b/c", "/a/b/e", "/a/b/e/f", "/a/b/e/g"},
			"/a/":     {"/", "/b", "/b/c", "/b/e", "/b/e/f", "/b/e/g"},
			"/a/b/":   {"/", "/c", "/e", "/e/f", "/e/g"},
			"/a/b/c/": {"/"},
		}[prefix]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("prefix=%-9q find:\ngot  %v\nwant %v", prefix, got, want)
			continue
		}
	}
}

func TestEscapeXML(t *testing.T) {
	// These test cases aren't exhaustive, and there is more than one way to
	// escape e.g. a quot (as "&#34;" or "&quot;") or an apos. We presume that
	// the encoding/xml package tests xml.EscapeText more thoroughly. This test
	// here is just a sanity check for this package's escapeXML function, and
	// its attempt to provide a fast path (and avoid a bytes.Buffer allocation)
	// when escaping filenames is obviously a no-op.
	testCases := map[string]string{
		"":              "",
		" ":             " ",
		"&":             "&amp;",
		"*":             "*",
		"+":             "+",
		",":             ",",
		"-":             "-",
		".":             ".",
		"/":             "/",
		"0":             "0",
		"9":             "9",
		":":             ":",
		"<":             "&lt;",
		">":             "&gt;",
		"A":             "A",
		"_":             "_",
		"a":             "a",
		"~":             "~",
		"\u0201":        "\u0201",
		"&amp;":         "&amp;amp;",
		"foo&<b/ar>baz": "foo&amp;&lt;b/ar&gt;baz",
	}

	for in, want := range testCases {
		if got := escapeXML(in); got != want {
			t.Errorf("in=%q: got %q, want %q", in, got, want)
		}
	}
}

func TestFilenameEscape(t *testing.T) {
	hrefRe := regexp.MustCompile(`<D:href>([^<]*)</D:href>`)
	displayNameRe := regexp.MustCompile(`<D:displayname>([^<]*)</D:displayname>`)
	do := func(method, urlStr string) (string, string, error) {
		req, err := http.NewRequest(method, urlStr, nil)
		if err != nil {
			return "", "", err
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", "", err
		}
		defer res.Body.Close()

		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return "", "", err
		}
		hrefMatch := hrefRe.FindStringSubmatch(string(b))
		if len(hrefMatch) != 2 {
			return "", "", errors.New("D:href not found")
		}
		displayNameMatch := displayNameRe.FindStringSubmatch(string(b))
		if len(displayNameMatch) != 2 {
			return "", "", errors.New("D:displayname not found")
		}

		return hrefMatch[1], displayNameMatch[1], nil
	}

	testCases := []struct {
		name, wantHref, wantDisplayName string
	}{{
		name:            `/foo%bar`,
		wantHref:        `/foo%25bar`,
		wantDisplayName: `foo%bar`,
	}, {
		name:            `/こんにちわ世界`,
		wantHref:        `/%E3%81%93%E3%82%93%E3%81%AB%E3%81%A1%E3%82%8F%E4%B8%96%E7%95%8C`,
		wantDisplayName: `こんにちわ世界`,
	}, {
		name:            `/Program Files/`,
		wantHref:        `/Program%20Files/`,
		wantDisplayName: `Program Files`,
	}, {
		name:            `/go+lang`,
		wantHref:        `/go+lang`,
		wantDisplayName: `go+lang`,
	}, {
		name:            `/go&lang`,
		wantHref:        `/go&amp;lang`,
		wantDisplayName: `go&amp;lang`,
	}, {
		name:            `/go<lang`,
		wantHref:        `/go%3Clang`,
		wantDisplayName: `go&lt;lang`,
	}, {
		name:            `/`,
		wantHref:        `/`,
		wantDisplayName: ``,
	}}
	ctx := context.Background()
	fs := NewMemFS()
	for _, tc := range testCases {
		if tc.name != "/" {
			if strings.HasSuffix(tc.name, "/") {
				if err := fs.Mkdir(ctx, tc.name, 0755); err != nil {
					t.Fatalf("name=%q: Mkdir: %v", tc.name, err)
				}
			} else {
				f, err := fs.OpenFile(ctx, tc.name, os.O_CREATE, 0644)
				if err != nil {
					t.Fatalf("name=%q: OpenFile: %v", tc.name, err)
				}
				f.Close()
			}
		}
	}

	srv := httptest.NewServer(&Handler{
		FileSystem: fs,
		LockSystem: NewMemLS(),
	})
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		u.Path = tc.name
		gotHref, gotDisplayName, err := do("PROPFIND", u.String())
		if err != nil {
			t.Errorf("name=%q: PROPFIND: %v", tc.name, err)
			continue
		}
		if gotHref != tc.wantHref {
			t.Errorf("name=%q: got href %q, want %q", tc.name, gotHref, tc.wantHref)
		}
		if gotDisplayName != tc.wantDisplayName {
			t.Errorf("name=%q: got dispayname %q, want %q", tc.name, gotDisplayName, tc.wantDisplayName)
		}
	}
}

func TestDelete(t *testing.T) {
	do := func(method, urlStr, body string, expectedCode int, headers ...string) (error, http.Header) {
		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, urlStr, bodyReader)
		if err != nil {
			return err, nil
		}

		for len(headers) >= 2 {
			req.Header.Add(headers[0], headers[1])
			headers = headers[2:]
		}

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err, nil
		}
		defer res.Body.Close()

		_, err = ioutil.ReadAll(res.Body)
		if err != nil {
			return err, res.Header
		}
		if res.StatusCode != expectedCode {
			return fmt.Errorf("%q path=%q: got status %d, want %d", method, urlStr, res.StatusCode, expectedCode), nil

		}
		return nil, res.Header
	}

	testCases := []struct {
		path           string
		lock, recreate bool
	}{{
		path: `/file`,
	}, {
		path:     `/file`,
		recreate: true,
	}, {
		// This reproduces https://github.com/golang/go/issues/42839
		path:     `/something_else`,
		lock:     true,
		recreate: true,
	}}

	srv := httptest.NewServer(&Handler{
		FileSystem: NewMemFS(),
		LockSystem: NewMemLS(),
	})
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Runs through the following logic:
	// Create File
	// If locking, lock file
	// Delete file
	// Check that file is gone
	// If recreating, recreate file
	for _, tc := range testCases {
		u.Path = tc.path
		err, _ := do("PUT", u.String(), "content", http.StatusCreated)
		if err != nil {
			t.Errorf("Initial create: %v", err)
			continue
		}

		headers := []string{}
		if tc.lock {
			err, hdrs := do("LOCK", u.String(), createLockBody, http.StatusOK)
			if err != nil {
				t.Errorf("Lock: %v", err)
				continue
			}

			lockToken := hdrs.Get("Lock-Token")
			if lockToken != "" {
				ifHeader := fmt.Sprintf("<%s%s> (%s)", srv.URL, tc.path, lockToken)
				headers = append(headers, "If", ifHeader)
			}
		}

		err, _ = do("DELETE", u.String(), "", http.StatusNoContent, headers...)
		if err != nil {
			t.Errorf("Delete: %v", err)
			continue
		}

		err, _ = do("GET", u.String(), "", http.StatusNotFound)
		if err != nil {
			t.Errorf("Get: %v", err)
			continue
		}

		if tc.recreate {
			err, _ := do("PUT", u.String(), "content", http.StatusCreated)
			if err != nil {
				t.Errorf("Second create: %v", err)
				continue
			}
		}
	}
}

func TestMoveLockedSrcUnlockedDst(t *testing.T) {
	// This test reproduces https://github.com/golang/go/issues/43556
	do := func(method, urlStr string, body string, wantStatusCode int, headers ...string) (http.Header, error) {
		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, urlStr, bodyReader)
		if err != nil {
			return nil, err
		}
		for len(headers) >= 2 {
			req.Header.Add(headers[0], headers[1])
			headers = headers[2:]
		}
		res, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != wantStatusCode {
			return nil, fmt.Errorf("got status code %d, want %d", res.StatusCode, wantStatusCode)
		}
		return res.Header, nil
	}

	srv := httptest.NewServer(&Handler{
		FileSystem: NewMemFS(),
		LockSystem: NewMemLS(),
	})
	defer srv.Close()

	src, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	src.Path = "/locked_src"
	hdrs, err := do("LOCK", src.String(), createLockBody, http.StatusCreated)
	if err != nil {
		t.Fatal(err)
	}

	lockToken := hdrs.Get("Lock-Token")
	if lockToken == "" {
		t.Errorf("Expected lock token")
	}

	if err != nil {
		t.Fatal(err)
	}
	ifHeader := fmt.Sprintf("<%s%s> (%s)", srv.URL, src.Path, lockToken)
	headers := []string{"If", ifHeader, "Destination", "/unlocked_path"}

	_, err = do("MOVE", src.String(), "", http.StatusCreated, headers...)

	if err != nil {
		t.Fatal(err)
	}
}
func TestLockRootEscape(t *testing.T) {
	lockrootRe := regexp.MustCompile(`<D:lockroot><D:href>([^<]*)</D:href></D:lockroot>`)
	do := func(urlStr string) (string, error) {
		bodyReader := strings.NewReader(createLockBody)
		req, err := http.NewRequest("LOCK", urlStr, bodyReader)
		if err != nil {
			return "", err
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()

		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return "", err
		}
		lockrootMatch := lockrootRe.FindStringSubmatch(string(b))
		if len(lockrootMatch) != 2 {
			return "", errors.New("D:lockroot not found")
		}

		return lockrootMatch[1], nil
	}

	testCases := []struct {
		name, wantLockroot string
	}{{
		name:         `/foo%bar`,
		wantLockroot: `/foo%25bar`,
	}, {
		name:         `/こんにちわ世界`,
		wantLockroot: `/%E3%81%93%E3%82%93%E3%81%AB%E3%81%A1%E3%82%8F%E4%B8%96%E7%95%8C`,
	}, {
		name:         `/Program Files/`,
		wantLockroot: `/Program%20Files/`,
	}, {
		name:         `/go+lang`,
		wantLockroot: `/go+lang`,
	}, {
		name:         `/go&lang`,
		wantLockroot: `/go&amp;lang`,
	}, {
		name:         `/go<lang`,
		wantLockroot: `/go%3Clang`,
	}}
	ctx := context.Background()
	fs := NewMemFS()
	for _, tc := range testCases {
		if tc.name != "/" {
			if strings.HasSuffix(tc.name, "/") {
				if err := fs.Mkdir(ctx, tc.name, 0755); err != nil {
					t.Fatalf("name=%q: Mkdir: %v", tc.name, err)
				}
			} else {
				f, err := fs.OpenFile(ctx, tc.name, os.O_CREATE, 0644)
				if err != nil {
					t.Fatalf("name=%q: OpenFile: %v", tc.name, err)
				}
				f.Close()
			}
		}
	}

	srv := httptest.NewServer(&Handler{
		FileSystem: fs,
		LockSystem: NewMemLS(),
	})
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		u.Path = tc.name
		gotLockroot, err := do(u.String())
		if err != nil {
			t.Errorf("name=%q: LOCK: %v", tc.name, err)
			continue
		}
		if gotLockroot != tc.wantLockroot {
			t.Errorf("name=%q: got lockroot %q, want %q", tc.name, gotLockroot, tc.wantLockroot)
		}
	}
}

type fakeFS struct {
	FileSystem
	errors map[string]error
}

func (f *fakeFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if err := f.errors[name]; err != nil {
		return nil, err
	}

	return f.FileSystem.Stat(ctx, name)
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		FileSystem: NewMemFS(),
		errors:     make(map[string]error),
	}
}

type multistatusResponse struct {
	XMLName  ixml.Name     `xml:"DAV: multistatus"`
	Response []davResponse `xml:"response"`
}

type davResponse struct {
	XMLName  ixml.Name     `xml:"response"`
	Href     []string      `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop []davProperty `xml:"prop"`
}

type davProperty struct {
	XMLName  ixml.Name
	Lang     string `xml:"xml:lang,attr,omitempty"`
	InnerXML []byte `xml:",innerxml"`
}

func TestPropfindErrorHandling(t *testing.T) {
	// This tests propfind error handling by setting up a hierarchy of files, then
	// injecting errors into specific ones. A table-driven test probes a few of them
	// to assert on how the system behaves.
	// The test is quite complicated because of how complicated parsing the propfind response
	// if, sadly.
	// A given file is a 'collection' (directory) if it ends in a '/'.
	// The general structure of each test is:
	// If the propfind shoudl succeed, parse it, then assert that the set of entries returned
	// matches what we expect. Further, if a given entry is a collection, assert that the
	// collection property exists.
	fs := newFakeFS()

	ctx := context.Background()
	for _, file := range []string{
		"/a/",
		"/a/b/",
		"/a/b/1",
		"/a/b/2",
		"/a/b/d/",
		"/a/b/d/1",
		"/a/b/d/2",
		"/a/c/",
		"/a/c/1",
		"/a/c/2",
	} {
		if file[len(file)-1] == '/' {
			err := fs.Mkdir(ctx, file, 0777)
			if err != nil {
				t.Fatalf("failed to mkdir %s: %v", file, err)
			}
			continue
		}

		f, err := fs.OpenFile(ctx, file, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			t.Fatalf("failed to create %s: %v", file, err)
		}
		f.Close()
	}

	fs.errors["/a/c/"] = os.ErrPermission
	fs.errors["/a/b/2"] = os.ErrPermission
	fs.errors["/a/d/2"] = os.ErrPermission

	srv := httptest.NewServer(&Handler{
		FileSystem: fs,
		LockSystem: NewMemLS(),
	})
	defer srv.Close()

	client := srv.Client()
	for name, test := range map[string]struct {
		path          string
		expectedFiles []string
		responseCode  int
	}{
		"rootDirIncludeBadSubdir": {
			path:         "/a/",
			responseCode: StatusMulti,
			expectedFiles: []string{
				"/a/",
				"/a/b/",
				"/a/c/",
			},
		},
		"testSubdirHasBadFile": {
			path:         "/a/b/",
			responseCode: StatusMulti,
			expectedFiles: []string{
				"/a/b/",
				"/a/b/1",
				"/a/b/2",
				"/a/b/d/",
			},
		},
		"testSubdirIsBad": {
			path:          "/a/c/",
			responseCode:  http.StatusForbidden,
			expectedFiles: []string{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Build and run a propfind of depth 1
			url := srv.URL + test.path
			req, err := http.NewRequestWithContext(ctx, "PROPFIND", url, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			// Set a depth of 1 so we only probe one directory at a time
			req.Header.Set("depth", "1")
			resp, err := client.Do(req)

			// Check that the propfind ran as expected
			if err != nil {
				t.Fatalf("Do: %v", err)
			}

			defer resp.Body.Close()

			if resp.StatusCode != test.responseCode {
				t.Fatalf("StatusCode: %v != %v", test.responseCode, resp.StatusCode)
			}

			// We're done in this case
			if resp.StatusCode != StatusMulti {
				return
			}

			// Now parse the propfind response, map it to a per-file list, then assert
			// on the set of files and any of their properties we care about
			var propResponse multistatusResponse
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			fmt.Printf("%s", string(body))
			dec := ixml.NewDecoder(bytes.NewBuffer(body))
			dec.DefaultSpace = "DAV"
			err = dec.Decode(&propResponse)
			if err != nil {
				t.Fatalf("decoding propfind response: %v", err)
			}

			pathToResponse := map[string]davResponse{}
			for _, responseFile := range propResponse.Response {
				pathToResponse[responseFile.Href[0]] = responseFile
			}

			for _, file := range test.expectedFiles {
				fileResp, ok := pathToResponse[file]
				if !ok {
					t.Fatalf("Could not find %s in response", file)
				}
				props := string(fileResp.Propstat[0].Prop[0].InnerXML)

				// Assert that it's a directory if we expect it to be.
				if file[len(file)-1] == '/' && !strings.Contains(props, `<D:resourcetype><D:collection xmlns:D="DAV:"/></D:resourcetype>`) {
					t.Fatalf("Expected %s to be a collection", file)
				}
			}

			if len(test.expectedFiles) != len(pathToResponse) {
				t.Errorf("expected %d files but got %d", len(test.expectedFiles), len(pathToResponse))
			}
		})
	}
}
