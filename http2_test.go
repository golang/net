// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bradfitz/http2/hpack"
)

func init() {
	DebugGoroutines = true
	flag.BoolVar(&VerboseLogs, "verboseh2", false, "Verbose HTTP/2 debug logging")
}

type serverTester struct {
	cc     net.Conn // client conn
	t      *testing.T
	ts     *httptest.Server
	fr     *Framer
	logBuf *bytes.Buffer
}

func newServerTester(t *testing.T, handler http.HandlerFunc) *serverTester {
	logBuf := new(bytes.Buffer)
	ts := httptest.NewUnstartedServer(handler)
	ConfigureServer(ts.Config, &Server{})
	ts.TLS = ts.Config.TLSConfig // the httptest.Server has its own copy of this TLS config
	ts.Config.ErrorLog = log.New(io.MultiWriter(twriter{t: t}, logBuf), "", log.LstdFlags)
	ts.StartTLS()

	if VerboseLogs {
		t.Logf("Running test server at: %s", ts.URL)
	}
	cc, err := tls.Dial("tcp", ts.Listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{npnProto},
	})
	if err != nil {
		t.Fatal(err)
	}
	log.SetOutput(twriter{t})
	return &serverTester{
		t:      t,
		ts:     ts,
		cc:     cc,
		fr:     NewFramer(cc, cc),
		logBuf: logBuf,
	}
}

func (st *serverTester) Close() {
	st.ts.Close()
	st.cc.Close()
	log.SetOutput(os.Stderr)
}

// greet initiates the client's HTTP/2 connection into a state where
// frames may be sent.
func (st *serverTester) greet() {
	st.writePreface()
	st.writeInitialSettings()
	st.wantSettings()
	st.writeSettingsAck()
	st.wantSettingsAck()
}

func (st *serverTester) writePreface() {
	n, err := st.cc.Write(clientPreface)
	if err != nil {
		st.t.Fatalf("Error writing client preface: %v", err)
	}
	if n != len(clientPreface) {
		st.t.Fatalf("Writing client preface, wrote %d bytes; want %d", n, len(clientPreface))
	}
}

func (st *serverTester) writeInitialSettings() {
	if err := st.fr.WriteSettings(); err != nil {
		st.t.Fatalf("Error writing initial SETTINGS frame from client to server: %v", err)
	}
}

func (st *serverTester) writeSettingsAck() {
	if err := st.fr.WriteSettingsAck(); err != nil {
		st.t.Fatalf("Error writing ACK of server's SETTINGS: %v", err)
	}
}

func (st *serverTester) writeHeaders(p HeadersFrameParam) {
	if err := st.fr.WriteHeaders(p); err != nil {
		st.t.Fatalf("Error writing HEADERS: %v", err)
	}
}

// bodylessReq1 writes a HEADERS frames with StreamID 1 and EndStream and EndHeaders set.
func (st *serverTester) bodylessReq1(headers ...string) {
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: encodeHeader(st.t, headers...),
		EndStream:     true,
		EndHeaders:    true,
	})
}

func (st *serverTester) writeData(streamID uint32, endStream bool, data []byte) {
	if err := st.fr.WriteData(streamID, endStream, data); err != nil {
		st.t.Fatalf("Error writing DATA: %v", err)
	}
}

func (st *serverTester) wantSettings() *SettingsFrame {
	f, err := st.fr.ReadFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting a SETTINGS frame: %v", err)
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *SettingsFrame", f)
	}
	return sf
}

func (st *serverTester) wantRSTStream(streamID uint32, errCode ErrCode) {
	f, err := st.fr.ReadFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting an RSTStream frame: %v", err)
	}
	rs, ok := f.(*RSTStreamFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *RSTStream", f)
	}
	if rs.FrameHeader.StreamID != streamID {
		st.t.Fatalf("RSTStream StreamID = %d; want %d", rs.FrameHeader.StreamID, streamID)
	}
	if rs.ErrCode != uint32(errCode) {
		st.t.Fatalf("RSTStream ErrCode = %d (%s); want %d (%s)", rs.ErrCode, rs.ErrCode, errCode, errCode)
	}
}

func (st *serverTester) wantSettingsAck() {
	f, err := st.fr.ReadFrame()
	if err != nil {
		st.t.Fatal(err)
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		st.t.Fatalf("Wanting a settings ACK, received a %T", f)
	}
	if !sf.Header().Flags.Has(FlagSettingsAck) {
		st.t.Fatal("Settings Frame didn't have ACK set")
	}

}

func TestServer(t *testing.T) {
	gotReq := make(chan bool, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Foo", "Bar")
		gotReq <- true
	})
	defer st.Close()

	covers("3.5", `
		The server connection preface consists of a potentially empty
		SETTINGS frame ([SETTINGS]) that MUST be the first frame the
		server sends in the HTTP/2 connection.
	`)

	st.writePreface()
	st.writeInitialSettings()
	st.wantSettings().ForeachSetting(func(s Setting) {
		t.Logf("Server sent setting %v = %v", s.ID, s.Val)
	})
	st.writeSettingsAck()
	st.wantSettingsAck()

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: encodeHeader(t),
		EndStream:     true, // no DATA frames
		EndHeaders:    true,
	})

	select {
	case <-gotReq:
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for request")
	}
}

func TestServer_Request_Get(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: encodeHeader(t, "foo-bar", "some-value"),
			EndStream:     true, // no DATA frames
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("Method = %q; want GET", r.Method)
		}
		if r.ContentLength != 0 {
			t.Errorf("ContentLength = %v; want 0", r.ContentLength)
		}
		if r.Close {
			t.Error("Close = true; want false")
		}
		if !strings.Contains(r.RemoteAddr, ":") {
			t.Errorf("RemoteAddr = %q; want something with a colon", r.RemoteAddr)
		}
		if r.Proto != "HTTP/2.0" || r.ProtoMajor != 2 || r.ProtoMinor != 0 {
			t.Errorf("Proto = %q Major=%v,Minor=%v; want HTTP/2.0", r.Proto, r.ProtoMajor, r.ProtoMinor)
		}
		wantHeader := http.Header{
			"Foo-Bar": []string{"some-value"},
		}
		if !reflect.DeepEqual(r.Header, wantHeader) {
			t.Errorf("Header = %#v; want %#v", r.Header, wantHeader)
		}
		if n, err := r.Body.Read([]byte(" ")); err != io.EOF || n != 0 {
			t.Errorf("Read = %d, %v; want 0, EOF", n, err)
		}
	})
}

// TODO: add a test with EndStream=true on the HEADERS but setting a
// Content-Length anyway.  Should we just omit it and force it to
// zero?

func TestServer_Request_Post_NoContentLength_EndStream(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: encodeHeader(t, ":method", "POST"),
			EndStream:     true,
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Method = %q; want POST", r.Method)
		}
		if r.ContentLength != 0 {
			t.Errorf("ContentLength = %v; want 0", r.ContentLength)
		}
		if n, err := r.Body.Read([]byte(" ")); err != io.EOF || n != 0 {
			t.Errorf("Read = %d, %v; want 0, EOF", n, err)
		}
	})
}

func TestServer_Request_Post_Body(t *testing.T) {
	t.Skip("TODO: post bodies not yet implemented")
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: encodeHeader(t, ":method", "POST"),
			EndStream:     false, // migth be DATA frames
			EndHeaders:    true,
		})
		st.writeData(1, true, nil)
	}, func(r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Method = %q; want POST", r.Method)
		}
		if r.ContentLength != -1 {
			t.Errorf("ContentLength = %v; want -1", r.ContentLength)
		}
		if n, err := r.Body.Read([]byte(" ")); err != io.EOF || n != 0 {
			t.Errorf("Read = %d, %v; want 0, EOF", n, err)
		}
	})
}

// Using a Host header, instead of :authority
func TestServer_Request_Get_Host(t *testing.T) {
	const host = "example.com"
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: encodeHeader(t, "host", host),
			EndStream:     true,
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Host != host {
			t.Errorf("Host = %q; want %q", r.Host, host)
		}
	})
}

// Using an :authority pseudo-header, instead of Host
func TestServer_Request_Get_Authority(t *testing.T) {
	const host = "example.com"
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: encodeHeader(t, ":authority", host),
			EndStream:     true,
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Host != host {
			t.Errorf("Host = %q; want %q", r.Host, host)
		}
	})
}

func TestServer_Request_WithContinuation(t *testing.T) {
	wantHeader := http.Header{
		"Foo-One":   []string{"value-one"},
		"Foo-Two":   []string{"value-two"},
		"Foo-Three": []string{"value-three"},
	}
	testServerRequest(t, func(st *serverTester) {
		fullHeaders := encodeHeader(t,
			"foo-one", "value-one",
			"foo-two", "value-two",
			"foo-three", "value-three",
		)
		remain := fullHeaders
		chunks := 0
		for len(remain) > 0 {
			const maxChunkSize = 5
			chunk := remain
			if len(chunk) > maxChunkSize {
				chunk = chunk[:maxChunkSize]
			}
			remain = remain[len(chunk):]

			if chunks == 0 {
				st.writeHeaders(HeadersFrameParam{
					StreamID:      1, // clients send odd numbers
					BlockFragment: chunk,
					EndStream:     true,  // no DATA frames
					EndHeaders:    false, // we'll have continuation frames
				})
			} else {
				err := st.fr.WriteContinuation(1, len(remain) == 0, chunk)
				if err != nil {
					t.Fatal(err)
				}
			}
			chunks++
		}
		if chunks < 2 {
			t.Fatal("too few chunks")
		}
	}, func(r *http.Request) {
		if !reflect.DeepEqual(r.Header, wantHeader) {
			t.Errorf("Header = %#v; want %#v", r.Header, wantHeader)
		}
	})
}

// Concatenated cookie headers. ("8.1.2.5 Compressing the Cookie Header Field")
func TestServer_Request_CookieConcat(t *testing.T) {
	const host = "example.com"
	testServerRequest(t, func(st *serverTester) {
		st.bodylessReq1(
			":authority", host,
			"cookie", "a=b",
			"cookie", "c=d",
			"cookie", "e=f",
		)
	}, func(r *http.Request) {
		const want = "a=b; c=d; e=f"
		if got := r.Header.Get("Cookie"); got != want {
			t.Errorf("Cookie = %q; want %q", got, want)
		}
	})
}

func TestServer_Request_Reject_CapitalHeader(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("UPPER", "v") })
}

func TestServer_Request_Reject_Pseudo_Missing_method(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":method", "") })
}

func TestServer_Request_Reject_Pseudo_Missing_path(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":path", "") })
}

func TestServer_Request_Reject_Pseudo_Missing_scheme(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":scheme", "") })
}

func TestServer_Request_Reject_Pseudo_Unknown(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":unknown_thing", "") })
}

func testRejectRequest(t *testing.T, send func(*serverTester)) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server request made it to handler; should've been rejected")
	})
	defer st.Close()

	st.greet()
	send(st)
	st.wantRSTStream(1, ErrCodeProtocol)
}

// TODO: test HEADERS w/o EndHeaders + another HEADERS (should get rejected)
// TODO: test HEADERS w/ EndHeaders + a continuation HEADERS (should get rejected)

// testServerRequest sets up an idle HTTP/2 connection and lets you
// write a single request with writeReq, and then verify that the
// *http.Request is built correctly in checkReq.
func testServerRequest(t *testing.T, writeReq func(*serverTester), checkReq func(*http.Request)) {
	gotReq := make(chan bool, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			t.Fatal("nil Body")
		}
		checkReq(r)
		gotReq <- true
	})
	defer st.Close()

	st.greet()
	writeReq(st)

	select {
	case <-gotReq:
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for request")
	}

}

func TestServerWithCurl(t *testing.T) {
	requireCurl(t)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO: add a bunch of different tests with different
		// behavior, as a function of r or a table.
		// -- with request body, without.
		// -- no interaction with w.
		// -- panic
		// -- modify Header only, but no writes or writeheader (this test)
		// -- WriteHeader only
		// -- Write only
		// -- WriteString
		// -- both
		// -- huge headers over a frame size so we get continuation headers.
		// Look at net/http's Server tests for inspiration.
		w.Header().Set("Foo", "Bar")
	}))
	ConfigureServer(ts.Config, &Server{})
	ts.TLS = ts.Config.TLSConfig // the httptest.Server has its own copy of this TLS config
	ts.StartTLS()
	defer ts.Close()

	var gotConn int32
	testHookOnConn = func() { atomic.StoreInt32(&gotConn, 1) }

	t.Logf("Running test server for curl to hit at: %s", ts.URL)
	container := curl(t, "--silent", "--http2", "--insecure", "-v", ts.URL)
	defer kill(container)
	resc := make(chan interface{}, 1)
	go func() {
		res, err := dockerLogs(container)
		if err != nil {
			resc <- err
		} else {
			resc <- res
		}
	}()
	select {
	case res := <-resc:
		if err, ok := res.(error); ok {
			t.Fatal(err)
		}
		if !strings.Contains(string(res.([]byte)), "< foo:Bar") {
			t.Errorf("didn't see foo:Bar header")
			t.Logf("Got: %s", res)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("timeout waiting for curl")
	}

	if atomic.LoadInt32(&gotConn) == 0 {
		t.Error("never saw an http2 connection")
	}
}

func dockerLogs(container string) ([]byte, error) {
	out, err := exec.Command("docker", "wait", container).CombinedOutput()
	if err != nil {
		return out, err
	}
	exitStatus, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return out, errors.New("unexpected exit status from docker wait")
	}
	out, err = exec.Command("docker", "logs", container).CombinedOutput()
	exec.Command("docker", "rm", container).Run()
	if err == nil && exitStatus != 0 {
		err = fmt.Errorf("exit status %d", exitStatus)
	}
	return out, err
}

func kill(container string) {
	exec.Command("docker", "kill", container).Run()
	exec.Command("docker", "rm", container).Run()
}

// Verify that curl has http2.
func requireCurl(t *testing.T) {
	out, err := dockerLogs(curl(t, "--version"))
	if err != nil {
		t.Skipf("failed to determine curl features; skipping test")
	}
	if !strings.Contains(string(out), "HTTP2") {
		t.Skip("curl doesn't support HTTP2; skipping test")
	}
}

func curl(t *testing.T, args ...string) (container string) {
	out, err := exec.Command("docker", append([]string{"run", "-d", "--net=host", "gohttp2/curl"}, args...)...).CombinedOutput()
	if err != nil {
		t.Skipf("Failed to run curl in docker: %v, %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

type twriter struct {
	t testing.TB
}

func (w twriter) Write(p []byte) (n int, err error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// encodeHeader encodes headers and returns their HPACK bytes. headers
// must contain an even number of key/value pairs.  There may be
// multiple pairs for keys (e.g. "cookie").  The :method, :path, and
// :scheme headers default to GET, / and https.
func encodeHeader(t *testing.T, headers ...string) []byte {
	if len(headers)%2 == 1 {
		panic("odd number of kv args")
	}
	keys := []string{":method", ":path", ":scheme"}
	vals := map[string][]string{
		":method": {"GET"},
		":path":   {"/"},
		":scheme": {"https"},
	}
	for len(headers) > 0 {
		k, v := headers[0], headers[1]
		headers = headers[2:]
		if _, ok := vals[k]; !ok {
			keys = append(keys, k)
		}
		if strings.HasPrefix(k, ":") {
			vals[k] = []string{v}
		} else {
			vals[k] = append(vals[k], v)
		}
	}
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	for _, k := range keys {
		for _, v := range vals[k] {
			if err := enc.WriteField(hpack.HeaderField{Name: k, Value: v}); err != nil {
				t.Fatalf("HPACK encoding error for %q/%q: %v", k, v, err)
			}
		}
	}
	return buf.Bytes()
}
