// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

func TestMemPS(t *testing.T) {
	ctx := context.Background()
	// calcProps calculates the getlastmodified and getetag DAV: property
	// values in pstats for resource name in file-system fs.
	calcProps := func(name string, fs FileSystem, ls LockSystem, pstats []Propstat) error {
		fi, err := fs.Stat(ctx, name)
		if err != nil {
			return err
		}
		for _, pst := range pstats {
			for i, p := range pst.Props {
				switch p.XMLName {
				case xml.Name{Space: "DAV:", Local: "getlastmodified"}:
					p.InnerXML = []byte(fi.ModTime().UTC().Format(http.TimeFormat))
					pst.Props[i] = p
				case xml.Name{Space: "DAV:", Local: "getetag"}:
					if fi.IsDir() {
						continue
					}
					etag, err := findETag(ctx, fs, ls, name, fi)
					if err != nil {
						return err
					}
					p.InnerXML = []byte(etag)
					pst.Props[i] = p
				}
			}
		}
		return nil
	}

	const (
		lockEntry = `` +
			`<D:lockentry xmlns:D="DAV:">` +
			`<D:lockscope><D:exclusive/></D:lockscope>` +
			`<D:locktype><D:write/></D:locktype>` +
			`</D:lockentry>`
		statForbiddenError = `<D:cannot-modify-protected-property xmlns:D="DAV:"/>`
	)

	type propOp struct {
		op            string
		name          string
		pnames        []xml.Name
		patches       []Proppatch
		wantPnames    []xml.Name
		wantPropstats []Propstat
	}

	testCases := []struct {
		desc        string
		noDeadProps bool
		buildfs     []string
		propOp      []propOp
	}{{
		desc:    "propname",
		buildfs: []string{"mkdir /dir", "touch /file"},
		propOp: []propOp{{
			op:   "propname",
			name: "/dir",
			wantPnames: []xml.Name{
				{Space: "DAV:", Local: "resourcetype"},
				{Space: "DAV:", Local: "displayname"},
				{Space: "DAV:", Local: "supportedlock"},
				{Space: "DAV:", Local: "getlastmodified"},
			},
		}, {
			op:   "propname",
			name: "/file",
			wantPnames: []xml.Name{
				{Space: "DAV:", Local: "resourcetype"},
				{Space: "DAV:", Local: "displayname"},
				{Space: "DAV:", Local: "getcontentlength"},
				{Space: "DAV:", Local: "getlastmodified"},
				{Space: "DAV:", Local: "getcontenttype"},
				{Space: "DAV:", Local: "getetag"},
				{Space: "DAV:", Local: "supportedlock"},
			},
		}},
	}, {
		desc:    "allprop dir and file",
		buildfs: []string{"mkdir /dir", "write /file foobarbaz"},
		propOp: []propOp{{
			op:   "allprop",
			name: "/dir",
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "DAV:", Local: "resourcetype"},
					InnerXML: []byte(`<D:collection xmlns:D="DAV:"/>`),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "displayname"},
					InnerXML: []byte("dir"),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getlastmodified"},
					InnerXML: nil, // Calculated during test.
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "supportedlock"},
					InnerXML: []byte(lockEntry),
				}},
			}},
		}, {
			op:   "allprop",
			name: "/file",
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "DAV:", Local: "resourcetype"},
					InnerXML: []byte(""),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "displayname"},
					InnerXML: []byte("file"),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getcontentlength"},
					InnerXML: []byte("9"),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getlastmodified"},
					InnerXML: nil, // Calculated during test.
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getcontenttype"},
					InnerXML: []byte("text/plain; charset=utf-8"),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getetag"},
					InnerXML: nil, // Calculated during test.
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "supportedlock"},
					InnerXML: []byte(lockEntry),
				}},
			}},
		}, {
			op:   "allprop",
			name: "/file",
			pnames: []xml.Name{
				{Space: "DAV:", Local: "resourcetype"},
				{Space: "foo", Local: "bar"},
			},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "DAV:", Local: "resourcetype"},
					InnerXML: []byte(""),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "displayname"},
					InnerXML: []byte("file"),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getcontentlength"},
					InnerXML: []byte("9"),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getlastmodified"},
					InnerXML: nil, // Calculated during test.
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getcontenttype"},
					InnerXML: []byte("text/plain; charset=utf-8"),
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "getetag"},
					InnerXML: nil, // Calculated during test.
				}, {
					XMLName:  xml.Name{Space: "DAV:", Local: "supportedlock"},
					InnerXML: []byte(lockEntry),
				}}}, {
				Status: http.StatusNotFound,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}}},
			},
		}},
	}, {
		desc:    "propfind DAV:resourcetype",
		buildfs: []string{"mkdir /dir", "touch /file"},
		propOp: []propOp{{
			op:     "propfind",
			name:   "/dir",
			pnames: []xml.Name{{Space: "DAV:", Local: "resourcetype"}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "DAV:", Local: "resourcetype"},
					InnerXML: []byte(`<D:collection xmlns:D="DAV:"/>`),
				}},
			}},
		}, {
			op:     "propfind",
			name:   "/file",
			pnames: []xml.Name{{Space: "DAV:", Local: "resourcetype"}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "DAV:", Local: "resourcetype"},
					InnerXML: []byte(""),
				}},
			}},
		}},
	}, {
		desc:    "propfind unsupported DAV properties",
		buildfs: []string{"mkdir /dir"},
		propOp: []propOp{{
			op:     "propfind",
			name:   "/dir",
			pnames: []xml.Name{{Space: "DAV:", Local: "getcontentlanguage"}},
			wantPropstats: []Propstat{{
				Status: http.StatusNotFound,
				Props: []Property{{
					XMLName: xml.Name{Space: "DAV:", Local: "getcontentlanguage"},
				}},
			}},
		}, {
			op:     "propfind",
			name:   "/dir",
			pnames: []xml.Name{{Space: "DAV:", Local: "creationdate"}},
			wantPropstats: []Propstat{{
				Status: http.StatusNotFound,
				Props: []Property{{
					XMLName: xml.Name{Space: "DAV:", Local: "creationdate"},
				}},
			}},
		}},
	}, {
		desc:    "propfind getetag for files but not for directories",
		buildfs: []string{"mkdir /dir", "touch /file"},
		propOp: []propOp{{
			op:     "propfind",
			name:   "/dir",
			pnames: []xml.Name{{Space: "DAV:", Local: "getetag"}},
			wantPropstats: []Propstat{{
				Status: http.StatusNotFound,
				Props: []Property{{
					XMLName: xml.Name{Space: "DAV:", Local: "getetag"},
				}},
			}},
		}, {
			op:     "propfind",
			name:   "/file",
			pnames: []xml.Name{{Space: "DAV:", Local: "getetag"}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "DAV:", Local: "getetag"},
					InnerXML: nil, // Calculated during test.
				}},
			}},
		}},
	}, {
		desc:        "proppatch property on no-dead-properties file system",
		buildfs:     []string{"mkdir /dir"},
		noDeadProps: true,
		propOp: []propOp{{
			op:   "proppatch",
			name: "/dir",
			patches: []Proppatch{{
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
			wantPropstats: []Propstat{{
				Status: http.StatusForbidden,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
		}, {
			op:   "proppatch",
			name: "/dir",
			patches: []Proppatch{{
				Props: []Property{{
					XMLName: xml.Name{Space: "DAV:", Local: "getetag"},
				}},
			}},
			wantPropstats: []Propstat{{
				Status:   http.StatusForbidden,
				XMLError: statForbiddenError,
				Props: []Property{{
					XMLName: xml.Name{Space: "DAV:", Local: "getetag"},
				}},
			}},
		}},
	}, {
		desc:    "proppatch dead property",
		buildfs: []string{"mkdir /dir"},
		propOp: []propOp{{
			op:   "proppatch",
			name: "/dir",
			patches: []Proppatch{{
				Props: []Property{{
					XMLName:  xml.Name{Space: "foo", Local: "bar"},
					InnerXML: []byte("baz"),
				}},
			}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
		}, {
			op:     "propfind",
			name:   "/dir",
			pnames: []xml.Name{{Space: "foo", Local: "bar"}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "foo", Local: "bar"},
					InnerXML: []byte("baz"),
				}},
			}},
		}},
	}, {
		desc:    "proppatch dead property with failed dependency",
		buildfs: []string{"mkdir /dir"},
		propOp: []propOp{{
			op:   "proppatch",
			name: "/dir",
			patches: []Proppatch{{
				Props: []Property{{
					XMLName:  xml.Name{Space: "foo", Local: "bar"},
					InnerXML: []byte("baz"),
				}},
			}, {
				Props: []Property{{
					XMLName:  xml.Name{Space: "DAV:", Local: "displayname"},
					InnerXML: []byte("xxx"),
				}},
			}},
			wantPropstats: []Propstat{{
				Status:   http.StatusForbidden,
				XMLError: statForbiddenError,
				Props: []Property{{
					XMLName: xml.Name{Space: "DAV:", Local: "displayname"},
				}},
			}, {
				Status: StatusFailedDependency,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
		}, {
			op:     "propfind",
			name:   "/dir",
			pnames: []xml.Name{{Space: "foo", Local: "bar"}},
			wantPropstats: []Propstat{{
				Status: http.StatusNotFound,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
		}},
	}, {
		desc:    "proppatch remove dead property",
		buildfs: []string{"mkdir /dir"},
		propOp: []propOp{{
			op:   "proppatch",
			name: "/dir",
			patches: []Proppatch{{
				Props: []Property{{
					XMLName:  xml.Name{Space: "foo", Local: "bar"},
					InnerXML: []byte("baz"),
				}, {
					XMLName:  xml.Name{Space: "spam", Local: "ham"},
					InnerXML: []byte("eggs"),
				}},
			}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}, {
					XMLName: xml.Name{Space: "spam", Local: "ham"},
				}},
			}},
		}, {
			op:   "propfind",
			name: "/dir",
			pnames: []xml.Name{
				{Space: "foo", Local: "bar"},
				{Space: "spam", Local: "ham"},
			},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "foo", Local: "bar"},
					InnerXML: []byte("baz"),
				}, {
					XMLName:  xml.Name{Space: "spam", Local: "ham"},
					InnerXML: []byte("eggs"),
				}},
			}},
		}, {
			op:   "proppatch",
			name: "/dir",
			patches: []Proppatch{{
				Remove: true,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
		}, {
			op:   "propfind",
			name: "/dir",
			pnames: []xml.Name{
				{Space: "foo", Local: "bar"},
				{Space: "spam", Local: "ham"},
			},
			wantPropstats: []Propstat{{
				Status: http.StatusNotFound,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}, {
				Status: http.StatusOK,
				Props: []Property{{
					XMLName:  xml.Name{Space: "spam", Local: "ham"},
					InnerXML: []byte("eggs"),
				}},
			}},
		}},
	}, {
		desc:    "propname with dead property",
		buildfs: []string{"touch /file"},
		propOp: []propOp{{
			op:   "proppatch",
			name: "/file",
			patches: []Proppatch{{
				Props: []Property{{
					XMLName:  xml.Name{Space: "foo", Local: "bar"},
					InnerXML: []byte("baz"),
				}},
			}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
		}, {
			op:   "propname",
			name: "/file",
			wantPnames: []xml.Name{
				{Space: "DAV:", Local: "resourcetype"},
				{Space: "DAV:", Local: "displayname"},
				{Space: "DAV:", Local: "getcontentlength"},
				{Space: "DAV:", Local: "getlastmodified"},
				{Space: "DAV:", Local: "getcontenttype"},
				{Space: "DAV:", Local: "getetag"},
				{Space: "DAV:", Local: "supportedlock"},
				{Space: "foo", Local: "bar"},
			},
		}},
	}, {
		desc:    "proppatch remove unknown dead property",
		buildfs: []string{"mkdir /dir"},
		propOp: []propOp{{
			op:   "proppatch",
			name: "/dir",
			patches: []Proppatch{{
				Remove: true,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
			wantPropstats: []Propstat{{
				Status: http.StatusOK,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo", Local: "bar"},
				}},
			}},
		}},
	}, {
		desc:    "bad: propfind unknown property",
		buildfs: []string{"mkdir /dir"},
		propOp: []propOp{{
			op:     "propfind",
			name:   "/dir",
			pnames: []xml.Name{{Space: "foo:", Local: "bar"}},
			wantPropstats: []Propstat{{
				Status: http.StatusNotFound,
				Props: []Property{{
					XMLName: xml.Name{Space: "foo:", Local: "bar"},
				}},
			}},
		}},
	}}

	for _, tc := range testCases {
		fs, err := buildTestFS(tc.buildfs)
		if err != nil {
			t.Fatalf("%s: cannot create test filesystem: %v", tc.desc, err)
		}
		if tc.noDeadProps {
			fs = noDeadPropsFS{fs}
		}
		ls := NewMemLS()
		for _, op := range tc.propOp {
			desc := fmt.Sprintf("%s: %s %s", tc.desc, op.op, op.name)
			if err = calcProps(op.name, fs, ls, op.wantPropstats); err != nil {
				t.Fatalf("%s: calcProps: %v", desc, err)
			}

			// Call property system.
			var propstats []Propstat
			switch op.op {
			case "propname":
				pnames, err := propnames(ctx, fs, ls, op.name)
				if err != nil {
					t.Errorf("%s: got error %v, want nil", desc, err)
					continue
				}
				sort.Sort(byXMLName(pnames))
				sort.Sort(byXMLName(op.wantPnames))
				if !reflect.DeepEqual(pnames, op.wantPnames) {
					t.Errorf("%s: pnames\ngot  %q\nwant %q", desc, pnames, op.wantPnames)
				}
				continue
			case "allprop":
				propstats, err = allprop(ctx, fs, ls, op.name, op.pnames)
			case "propfind":
				propstats, err = props(ctx, fs, ls, op.name, op.pnames)
			case "proppatch":
				propstats, err = patch(ctx, fs, ls, op.name, op.patches)
			default:
				t.Fatalf("%s: %s not implemented", desc, op.op)
			}
			if err != nil {
				t.Errorf("%s: got error %v, want nil", desc, err)
				continue
			}
			// Compare return values from allprop, propfind or proppatch.
			for _, pst := range propstats {
				sort.Sort(byPropname(pst.Props))
			}
			for _, pst := range op.wantPropstats {
				sort.Sort(byPropname(pst.Props))
			}
			sort.Sort(byStatus(propstats))
			sort.Sort(byStatus(op.wantPropstats))
			if !reflect.DeepEqual(propstats, op.wantPropstats) {
				t.Errorf("%s: propstat\ngot  %#v\nwant %#v", desc, propstats, op.wantPropstats)
			}
		}
	}
}

type testDeadPropsHolder struct {
	props map[xml.Name]Property
}

func (h *testDeadPropsHolder) DeadProps() (map[xml.Name]Property, error) {
	if len(h.props) == 0 {
		return nil, nil
	}
	ret := make(map[xml.Name]Property, len(h.props))
	for k, v := range h.props {
		ret[k] = v
	}
	return ret, nil
}

func (h *testDeadPropsHolder) Patch(patches []Proppatch) ([]Propstat, error) {
	pstat := Propstat{Status: http.StatusOK}
	for _, patch := range patches {
		for _, p := range patch.Props {
			pstat.Props = append(pstat.Props, Property{XMLName: p.XMLName})
			if patch.Remove {
				delete(h.props, p.XMLName)
				continue
			}
			if h.props == nil {
				h.props = map[xml.Name]Property{}
			}
			h.props[p.XMLName] = p
		}
	}
	return []Propstat{pstat}, nil
}

type testFileSystemProps struct {
	FileSystem
	err        error
	cacheProps map[string]map[xml.Name]Property
}

func (fs *testFileSystemProps) Props(ctx context.Context, name string) (DeadPropsHolder, error) {
	if fs.err != nil {
		return nil, fs.err
	}
	props, ok := fs.cacheProps[name]
	if !ok {
		return nil, nil
	}
	if props == nil {
		props = map[xml.Name]Property{}
		fs.cacheProps[name] = props
	}
	return &testDeadPropsHolder{props: props}, nil
}

func patchTestDeadProps(t *testing.T, fs FileSystem, name string, patches []Proppatch) {
	t.Helper()
	f, err := fs.OpenFile(context.Background(), name, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", name, err)
	}
	defer f.Close()
	dph, ok := f.(DeadPropsHolder)
	if !ok {
		t.Fatalf("OpenFile(%q) did not return a DeadPropsHolder", name)
	}
	if _, err := dph.Patch(patches); err != nil {
		t.Fatalf("Patch(%q): %v", name, err)
	}
}

func TestFileSystemProps(t *testing.T) {
	fsPropName := xml.Name{Space: "fs", Local: "prop"}
	fsPropName2 := xml.Name{Space: "fs", Local: "prop2"}
	filePropName := xml.Name{Space: "file", Local: "prop"}
	fileProp := Property{
		XMLName:  filePropName,
		InnerXML: []byte("from-file"),
	}
	fsProp := Property{
		XMLName:  fsPropName,
		InnerXML: []byte("from-fs"),
	}
	fsProp2 := Property{
		XMLName:  fsPropName2,
		InnerXML: []byte("from-fs-2"),
	}

	base, err := buildTestFS([]string{"touch /file"})
	if err != nil {
		t.Fatalf("cannot create test filesystem: %v", err)
	}
	patchTestDeadProps(t, base, "/file", []Proppatch{{Props: []Property{fileProp}}})
	fs := &testFileSystemProps{
		FileSystem: base,
		cacheProps: map[string]map[xml.Name]Property{"/file": nil},
	}

	type propOp struct {
		name    string
		patches []Proppatch
		want    map[xml.Name]Property
	}

	mls := NewMemLS()

	checkAllProps := func(opName string, want map[xml.Name]Property) {
		t.Helper()
		propstats, err := allprop(context.Background(), fs, mls, "/file", nil)
		if err != nil {
			t.Fatalf("%s allprop: %v", opName, err)
		}
		got := map[xml.Name]Property{}
		for _, pstat := range propstats {
			for _, p := range pstat.Props {
				got[p.XMLName] = p
			}
		}
		for name, prop := range want {
			gotProp, ok := got[name]
			if !ok {
				t.Fatalf("%s missing prop %v in allprop", opName, name)
			}
			if !reflect.DeepEqual(gotProp.InnerXML, prop.InnerXML) {
				t.Fatalf("%s prop %v innerXML = %q, want %q", opName, name, gotProp.InnerXML, prop.InnerXML)
			}
		}
		for _, name := range []xml.Name{fsPropName, fsPropName2} {
			if _, ok := want[name]; ok {
				continue
			}
			if _, ok := got[name]; ok {
				t.Fatalf("%s unexpectedly exposed prop %v", opName, name)
			}
		}
		if _, ok := got[filePropName]; ok {
			t.Fatalf("%s unexpectedly exposed prop %v", opName, filePropName)
		}
	}

	checkFileDeadProps := func(opName string) {
		t.Helper()
		f, err := base.OpenFile(context.Background(), "/file", os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("%s OpenFile: %v", opName, err)
		}
		defer f.Close()
		fileProps, err := f.(DeadPropsHolder).DeadProps()
		if err != nil {
			t.Fatalf("%s file DeadProps: %v", opName, err)
		}
		if _, ok := fileProps[fsPropName]; ok {
			t.Fatalf("%s file dead props unexpectedly contain %v: %v", opName, fsPropName, fileProps)
		}
		if _, ok := fileProps[fsPropName2]; ok {
			t.Fatalf("%s file dead props unexpectedly contain %v: %v", opName, fsPropName2, fileProps)
		}
		if _, ok := fileProps[filePropName]; !ok {
			t.Fatalf("%s file dead props missing original file prop: %v", opName, fileProps)
		}
	}

	ops := []propOp{{
		name: "add 1",
		patches: []Proppatch{{
			Props: []Property{fsProp},
		}},
	}, {
		name: "add 2",
		patches: []Proppatch{{
			Props: []Property{fsProp2},
		}},
	}, {
		name: "get all",
		want: map[xml.Name]Property{
			fsPropName:  fsProp,
			fsPropName2: fsProp2,
		},
	}, {
		name: "del",
		patches: []Proppatch{{
			Remove: true,
			Props: []Property{{
				XMLName: fsPropName,
			}},
		}},
	}, {
		name: "get all after del",
		want: map[xml.Name]Property{
			fsPropName2: fsProp2,
		},
	}}

	for _, op := range ops {
		if op.patches != nil {
			if _, err := patch(context.Background(), fs, NewMemLS(), "/file", op.patches); err != nil {
				t.Fatalf("%s patch: %v", op.name, err)
			}
		}
		if op.want != nil {
			checkAllProps(op.name, op.want)
			checkFileDeadProps(op.name)
		}
	}
}

func cmpXMLName(a, b xml.Name) bool {
	if a.Space != b.Space {
		return a.Space < b.Space
	}
	return a.Local < b.Local
}

type byXMLName []xml.Name

func (b byXMLName) Len() int           { return len(b) }
func (b byXMLName) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byXMLName) Less(i, j int) bool { return cmpXMLName(b[i], b[j]) }

type byPropname []Property

func (b byPropname) Len() int           { return len(b) }
func (b byPropname) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byPropname) Less(i, j int) bool { return cmpXMLName(b[i].XMLName, b[j].XMLName) }

type byStatus []Propstat

func (b byStatus) Len() int           { return len(b) }
func (b byStatus) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byStatus) Less(i, j int) bool { return b[i].Status < b[j].Status }

type noDeadPropsFS struct {
	FileSystem
}

func (fs noDeadPropsFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (File, error) {
	f, err := fs.FileSystem.OpenFile(ctx, name, flag, perm)
	if err != nil {
		return nil, err
	}
	return noDeadPropsFile{f}, nil
}

// noDeadPropsFile wraps a File but strips any optional DeadPropsHolder methods
// provided by the underlying File implementation.
type noDeadPropsFile struct {
	f File
}

func (f noDeadPropsFile) Close() error                              { return f.f.Close() }
func (f noDeadPropsFile) Read(p []byte) (int, error)                { return f.f.Read(p) }
func (f noDeadPropsFile) Readdir(count int) ([]os.FileInfo, error)  { return f.f.Readdir(count) }
func (f noDeadPropsFile) Seek(off int64, whence int) (int64, error) { return f.f.Seek(off, whence) }
func (f noDeadPropsFile) Stat() (os.FileInfo, error)                { return f.f.Stat() }
func (f noDeadPropsFile) Write(p []byte) (int, error)               { return f.f.Write(p) }

type overrideContentType struct {
	os.FileInfo
	contentType string
	err         error
}

func (o *overrideContentType) ContentType(ctx context.Context) (string, error) {
	return o.contentType, o.err
}

func TestFindContentTypeOverride(t *testing.T) {
	fs, err := buildTestFS([]string{"touch /file"})
	if err != nil {
		t.Fatalf("cannot create test filesystem: %v", err)
	}
	ctx := context.Background()
	fi, err := fs.Stat(ctx, "/file")
	if err != nil {
		t.Fatalf("cannot Stat /file: %v", err)
	}

	// Check non overridden case
	originalContentType, err := findContentType(ctx, fs, nil, "/file", fi)
	if err != nil {
		t.Fatalf("findContentType /file failed: %v", err)
	}
	if originalContentType != "text/plain; charset=utf-8" {
		t.Fatalf("ContentType wrong want %q got %q", "text/plain; charset=utf-8", originalContentType)
	}

	// Now try overriding the ContentType
	o := &overrideContentType{fi, "OverriddenContentType", nil}
	ContentType, err := findContentType(ctx, fs, nil, "/file", o)
	if err != nil {
		t.Fatalf("findContentType /file failed: %v", err)
	}
	if ContentType != o.contentType {
		t.Fatalf("ContentType wrong want %q got %q", o.contentType, ContentType)
	}

	// Now return ErrNotImplemented and check we get the original content type
	o = &overrideContentType{fi, "OverriddenContentType", ErrNotImplemented}
	ContentType, err = findContentType(ctx, fs, nil, "/file", o)
	if err != nil {
		t.Fatalf("findContentType /file failed: %v", err)
	}
	if ContentType != originalContentType {
		t.Fatalf("ContentType wrong want %q got %q", originalContentType, ContentType)
	}
}

type overrideETag struct {
	os.FileInfo
	eTag string
	err  error
}

func (o *overrideETag) ETag(ctx context.Context) (string, error) {
	return o.eTag, o.err
}

func TestFindETagOverride(t *testing.T) {
	fs, err := buildTestFS([]string{"touch /file"})
	if err != nil {
		t.Fatalf("cannot create test filesystem: %v", err)
	}
	ctx := context.Background()
	fi, err := fs.Stat(ctx, "/file")
	if err != nil {
		t.Fatalf("cannot Stat /file: %v", err)
	}

	// Check non overridden case
	originalETag, err := findETag(ctx, fs, nil, "/file", fi)
	if err != nil {
		t.Fatalf("findETag /file failed: %v", err)
	}
	matchETag := regexp.MustCompile(`^"-?[0-9a-f]{6,}"$`)
	if !matchETag.MatchString(originalETag) {
		t.Fatalf("ETag wrong, wanted something matching %v got %q", matchETag, originalETag)
	}

	// Now try overriding the ETag
	o := &overrideETag{fi, `"OverriddenETag"`, nil}
	ETag, err := findETag(ctx, fs, nil, "/file", o)
	if err != nil {
		t.Fatalf("findETag /file failed: %v", err)
	}
	if ETag != o.eTag {
		t.Fatalf("ETag wrong want %q got %q", o.eTag, ETag)
	}

	// Now return ErrNotImplemented and check we get the original Etag
	o = &overrideETag{fi, `"OverriddenETag"`, ErrNotImplemented}
	ETag, err = findETag(ctx, fs, nil, "/file", o)
	if err != nil {
		t.Fatalf("findETag /file failed: %v", err)
	}
	if ETag != originalETag {
		t.Fatalf("ETag wrong want %q got %q", originalETag, ETag)
	}
}
