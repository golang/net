// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

var stderrVerbose = flag.Bool("stderr_verbose", false, "Mirror verbosity to stderr, unbuffered")

func stderrv() io.Writer {
	if *stderrVerbose {
		return os.Stderr
	}

	return io.Discard
}

type safeBuffer struct {
	b bytes.Buffer
	m sync.Mutex
}

func (sb *safeBuffer) Write(d []byte) (int, error) {
	sb.m.Lock()
	defer sb.m.Unlock()
	return sb.b.Write(d)
}

func (sb *safeBuffer) Bytes() []byte {
	sb.m.Lock()
	defer sb.m.Unlock()
	return sb.b.Bytes()
}

func (sb *safeBuffer) Len() int {
	sb.m.Lock()
	defer sb.m.Unlock()
	return sb.b.Len()
}

type serverTester struct {
	cc           net.Conn // client conn
	t            testing.TB
	group        *synctestGroup
	h1server     *http.Server
	h2server     *Server
	serverLogBuf safeBuffer // logger for httptest.Server
	logFilter    []string   // substrings to filter out
	scMu         sync.Mutex // guards sc
	sc           *serverConn
	testConnFramer

	// If http2debug!=2, then we capture Frame debug logs that will be written
	// to t.Log after a test fails. The read and write logs use separate locks
	// and buffers so we don't accidentally introduce synchronization between
	// the read and write goroutines, which may hide data races.
	frameReadLogMu   sync.Mutex
	frameReadLogBuf  bytes.Buffer
	frameWriteLogMu  sync.Mutex
	frameWriteLogBuf bytes.Buffer

	// writing headers:
	headerBuf bytes.Buffer
	hpackEnc  *hpack.Encoder
}

func init() {
	testHookOnPanicMu = new(sync.Mutex)
	goAwayTimeout = 25 * time.Millisecond
}

func resetHooks() {
	testHookOnPanicMu.Lock()
	testHookOnPanic = nil
	testHookOnPanicMu.Unlock()
}

func newTestServer(t testing.TB, handler http.HandlerFunc, opts ...interface{}) *httptest.Server {
	ts := httptest.NewUnstartedServer(handler)
	ts.EnableHTTP2 = true
	ts.Config.ErrorLog = log.New(twriter{t: t}, "", log.LstdFlags)
	h2server := new(Server)
	for _, opt := range opts {
		switch v := opt.(type) {
		case func(*httptest.Server):
			v(ts)
		case func(*http.Server):
			v(ts.Config)
		case func(*Server):
			v(h2server)
		default:
			t.Fatalf("unknown newTestServer option type %T", v)
		}
	}
	ConfigureServer(ts.Config, h2server)

	// ConfigureServer populates ts.Config.TLSConfig.
	// Copy it to ts.TLS as well.
	ts.TLS = ts.Config.TLSConfig

	// Go 1.22 changes the default minimum TLS version to TLS 1.2,
	// in order to properly test cases where we want to reject low
	// TLS versions, we need to explicitly configure the minimum
	// version here.
	ts.Config.TLSConfig.MinVersion = tls.VersionTLS10

	ts.StartTLS()
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	return ts
}

type serverTesterOpt string

var optFramerReuseFrames = serverTesterOpt("frame_reuse_frames")

var optQuiet = func(server *http.Server) {
	server.ErrorLog = log.New(io.Discard, "", 0)
}

func newServerTester(t testing.TB, handler http.HandlerFunc, opts ...interface{}) *serverTester {
	t.Helper()
	g := newSynctest(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	t.Cleanup(func() {
		g.Close(t)
	})

	h1server := &http.Server{}
	h2server := &Server{
		group: g,
	}
	tlsState := tls.ConnectionState{
		Version:     tls.VersionTLS13,
		ServerName:  "go.dev",
		CipherSuite: tls.TLS_AES_128_GCM_SHA256,
	}
	for _, opt := range opts {
		switch v := opt.(type) {
		case func(*Server):
			v(h2server)
		case func(*http.Server):
			v(h1server)
		case func(*tls.ConnectionState):
			v(&tlsState)
		default:
			t.Fatalf("unknown newServerTester option type %T", v)
		}
	}
	ConfigureServer(h1server, h2server)

	cli, srv := synctestNetPipe(g)
	cli.SetReadDeadline(g.Now())
	cli.autoWait = true

	st := &serverTester{
		t:        t,
		cc:       cli,
		group:    g,
		h1server: h1server,
		h2server: h2server,
	}
	st.hpackEnc = hpack.NewEncoder(&st.headerBuf)
	if h1server.ErrorLog == nil {
		h1server.ErrorLog = log.New(io.MultiWriter(stderrv(), twriter{t: t, st: st}, &st.serverLogBuf), "", log.LstdFlags)
	}

	t.Cleanup(func() {
		st.Close()
		g.AdvanceTime(goAwayTimeout) // give server time to shut down
	})

	connc := make(chan *serverConn)
	go func() {
		g.Join()
		h2server.serveConn(&netConnWithConnectionState{
			Conn:  srv,
			state: tlsState,
		}, &ServeConnOpts{
			Handler:    handler,
			BaseConfig: h1server,
		}, func(sc *serverConn) {
			connc <- sc
		})
	}()
	st.sc = <-connc

	st.fr = NewFramer(st.cc, st.cc)
	st.testConnFramer = testConnFramer{
		t:   t,
		fr:  NewFramer(st.cc, st.cc),
		dec: hpack.NewDecoder(initialHeaderTableSize, nil),
	}
	g.Wait()
	return st
}

type netConnWithConnectionState struct {
	net.Conn
	state tls.ConnectionState
}

func (c *netConnWithConnectionState) ConnectionState() tls.ConnectionState {
	return c.state
}

// newServerTesterWithRealConn creates a test server listening on a localhost port.
// Mostly superseded by newServerTester, which creates a test server using a fake
// net.Conn and synthetic time. This function is still around because some benchmarks
// rely on it; new tests should use newServerTester.
func newServerTesterWithRealConn(t testing.TB, handler http.HandlerFunc, opts ...interface{}) *serverTester {
	resetHooks()

	ts := httptest.NewUnstartedServer(handler)
	t.Cleanup(ts.Close)

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{NextProtoTLS},
	}

	var framerReuseFrames bool
	h2server := new(Server)
	for _, opt := range opts {
		switch v := opt.(type) {
		case func(*tls.Config):
			v(tlsConfig)
		case func(*httptest.Server):
			v(ts)
		case func(*http.Server):
			v(ts.Config)
		case func(*Server):
			v(h2server)
		case serverTesterOpt:
			switch v {
			case optFramerReuseFrames:
				framerReuseFrames = true
			}
		case func(net.Conn, http.ConnState):
			ts.Config.ConnState = v
		default:
			t.Fatalf("unknown newServerTester option type %T", v)
		}
	}

	ConfigureServer(ts.Config, h2server)

	// Go 1.22 changes the default minimum TLS version to TLS 1.2,
	// in order to properly test cases where we want to reject low
	// TLS versions, we need to explicitly configure the minimum
	// version here.
	ts.Config.TLSConfig.MinVersion = tls.VersionTLS10

	st := &serverTester{
		t: t,
	}
	st.hpackEnc = hpack.NewEncoder(&st.headerBuf)

	ts.TLS = ts.Config.TLSConfig // the httptest.Server has its own copy of this TLS config
	if ts.Config.ErrorLog == nil {
		ts.Config.ErrorLog = log.New(io.MultiWriter(stderrv(), twriter{t: t, st: st}, &st.serverLogBuf), "", log.LstdFlags)
	}
	ts.StartTLS()

	if VerboseLogs {
		t.Logf("Running test server at: %s", ts.URL)
	}
	testHookGetServerConn = func(v *serverConn) {
		st.scMu.Lock()
		defer st.scMu.Unlock()
		st.sc = v
	}
	log.SetOutput(io.MultiWriter(stderrv(), twriter{t: t, st: st}))
	cc, err := tls.Dial("tcp", ts.Listener.Addr().String(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	st.cc = cc
	st.testConnFramer = testConnFramer{
		t:   t,
		fr:  NewFramer(st.cc, st.cc),
		dec: hpack.NewDecoder(initialHeaderTableSize, nil),
	}
	if framerReuseFrames {
		st.fr.SetReuseFrames()
	}
	if !logFrameReads && !logFrameWrites {
		st.fr.debugReadLoggerf = func(m string, v ...interface{}) {
			m = time.Now().Format("2006-01-02 15:04:05.999999999 ") + strings.TrimPrefix(m, "http2: ") + "\n"
			st.frameReadLogMu.Lock()
			fmt.Fprintf(&st.frameReadLogBuf, m, v...)
			st.frameReadLogMu.Unlock()
		}
		st.fr.debugWriteLoggerf = func(m string, v ...interface{}) {
			m = time.Now().Format("2006-01-02 15:04:05.999999999 ") + strings.TrimPrefix(m, "http2: ") + "\n"
			st.frameWriteLogMu.Lock()
			fmt.Fprintf(&st.frameWriteLogBuf, m, v...)
			st.frameWriteLogMu.Unlock()
		}
		st.fr.logReads = true
		st.fr.logWrites = true
	}
	return st
}

// sync waits for all goroutines to idle.
func (st *serverTester) sync() {
	if st.group != nil {
		st.group.Wait()
	}
}

// advance advances synthetic time by a duration.
func (st *serverTester) advance(d time.Duration) {
	st.group.AdvanceTime(d)
}

func (st *serverTester) authority() string {
	return "dummy.tld"
}

func (st *serverTester) closeConn() {
	st.scMu.Lock()
	defer st.scMu.Unlock()
	st.sc.conn.Close()
}

func (st *serverTester) addLogFilter(phrase string) {
	st.logFilter = append(st.logFilter, phrase)
}

func (st *serverTester) stream(id uint32) *stream {
	ch := make(chan *stream, 1)
	st.sc.serveMsgCh <- func(int) {
		ch <- st.sc.streams[id]
	}
	return <-ch
}

func (st *serverTester) streamState(id uint32) streamState {
	ch := make(chan streamState, 1)
	st.sc.serveMsgCh <- func(int) {
		state, _ := st.sc.state(id)
		ch <- state
	}
	return <-ch
}

// loopNum reports how many times this conn's select loop has gone around.
func (st *serverTester) loopNum() int {
	lastc := make(chan int, 1)
	st.sc.serveMsgCh <- func(loopNum int) {
		lastc <- loopNum
	}
	return <-lastc
}

// awaitIdle heuristically awaits for the server conn's select loop to be idle.
// The heuristic is that the server connection's serve loop must schedule
// 50 times in a row without any channel sends or receives occurring.
func (st *serverTester) awaitIdle() {
	remain := 50
	last := st.loopNum()
	for remain > 0 {
		n := st.loopNum()
		if n == last+1 {
			remain--
		} else {
			remain = 50
		}
		last = n
	}
}

func (st *serverTester) Close() {
	if st.t.Failed() {
		st.frameReadLogMu.Lock()
		if st.frameReadLogBuf.Len() > 0 {
			st.t.Logf("Framer read log:\n%s", st.frameReadLogBuf.String())
		}
		st.frameReadLogMu.Unlock()

		st.frameWriteLogMu.Lock()
		if st.frameWriteLogBuf.Len() > 0 {
			st.t.Logf("Framer write log:\n%s", st.frameWriteLogBuf.String())
		}
		st.frameWriteLogMu.Unlock()

		// If we failed already (and are likely in a Fatal,
		// unwindowing), force close the connection, so the
		// httptest.Server doesn't wait forever for the conn
		// to close.
		if st.cc != nil {
			st.cc.Close()
		}
	}
	if st.cc != nil {
		st.cc.Close()
	}
	log.SetOutput(os.Stderr)
}

// greet initiates the client's HTTP/2 connection into a state where
// frames may be sent.
func (st *serverTester) greet() {
	st.t.Helper()
	st.greetAndCheckSettings(func(Setting) error { return nil })
}

func (st *serverTester) greetAndCheckSettings(checkSetting func(s Setting) error) {
	st.t.Helper()
	st.writePreface()
	st.writeSettings()
	st.sync()
	readFrame[*SettingsFrame](st.t, st).ForeachSetting(checkSetting)
	st.writeSettingsAck()

	// The initial WINDOW_UPDATE and SETTINGS ACK can come in any order.
	var gotSettingsAck bool
	var gotWindowUpdate bool

	for i := 0; i < 2; i++ {
		f := st.readFrame()
		if f == nil {
			st.t.Fatal("wanted a settings ACK and window update, got none")
		}
		switch f := f.(type) {
		case *SettingsFrame:
			if !f.Header().Flags.Has(FlagSettingsAck) {
				st.t.Fatal("Settings Frame didn't have ACK set")
			}
			gotSettingsAck = true

		case *WindowUpdateFrame:
			if f.FrameHeader.StreamID != 0 {
				st.t.Fatalf("WindowUpdate StreamID = %d; want 0", f.FrameHeader.StreamID)
			}
			conf := configFromServer(st.sc.hs, st.sc.srv)
			incr := uint32(conf.MaxUploadBufferPerConnection - initialWindowSize)
			if f.Increment != incr {
				st.t.Fatalf("WindowUpdate increment = %d; want %d", f.Increment, incr)
			}
			gotWindowUpdate = true

		default:
			st.t.Fatalf("Wanting a settings ACK or window update, received a %T", f)
		}
	}

	if !gotSettingsAck {
		st.t.Fatalf("Didn't get a settings ACK")
	}
	if !gotWindowUpdate {
		st.t.Fatalf("Didn't get a window update")
	}
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

func (st *serverTester) encodeHeaderField(k, v string) {
	err := st.hpackEnc.WriteField(hpack.HeaderField{Name: k, Value: v})
	if err != nil {
		st.t.Fatalf("HPACK encoding error for %q/%q: %v", k, v, err)
	}
}

// encodeHeaderRaw is the magic-free version of encodeHeader.
// It takes 0 or more (k, v) pairs and encodes them.
func (st *serverTester) encodeHeaderRaw(headers ...string) []byte {
	if len(headers)%2 == 1 {
		panic("odd number of kv args")
	}
	st.headerBuf.Reset()
	for len(headers) > 0 {
		k, v := headers[0], headers[1]
		st.encodeHeaderField(k, v)
		headers = headers[2:]
	}
	return st.headerBuf.Bytes()
}

// encodeHeader encodes headers and returns their HPACK bytes. headers
// must contain an even number of key/value pairs. There may be
// multiple pairs for keys (e.g. "cookie").  The :method, :path, and
// :scheme headers default to GET, / and https. The :authority header
// defaults to st.ts.Listener.Addr().
func (st *serverTester) encodeHeader(headers ...string) []byte {
	if len(headers)%2 == 1 {
		panic("odd number of kv args")
	}

	st.headerBuf.Reset()
	defaultAuthority := st.authority()

	if len(headers) == 0 {
		// Fast path, mostly for benchmarks, so test code doesn't pollute
		// profiles when we're looking to improve server allocations.
		st.encodeHeaderField(":method", "GET")
		st.encodeHeaderField(":scheme", "https")
		st.encodeHeaderField(":authority", defaultAuthority)
		st.encodeHeaderField(":path", "/")
		return st.headerBuf.Bytes()
	}

	if len(headers) == 2 && headers[0] == ":method" {
		// Another fast path for benchmarks.
		st.encodeHeaderField(":method", headers[1])
		st.encodeHeaderField(":scheme", "https")
		st.encodeHeaderField(":authority", defaultAuthority)
		st.encodeHeaderField(":path", "/")
		return st.headerBuf.Bytes()
	}

	pseudoCount := map[string]int{}
	keys := []string{":method", ":scheme", ":authority", ":path"}
	vals := map[string][]string{
		":method":    {"GET"},
		":scheme":    {"https"},
		":authority": {defaultAuthority},
		":path":      {"/"},
	}
	for len(headers) > 0 {
		k, v := headers[0], headers[1]
		headers = headers[2:]
		if _, ok := vals[k]; !ok {
			keys = append(keys, k)
		}
		if strings.HasPrefix(k, ":") {
			pseudoCount[k]++
			if pseudoCount[k] == 1 {
				vals[k] = []string{v}
			} else {
				// Allows testing of invalid headers w/ dup pseudo fields.
				vals[k] = append(vals[k], v)
			}
		} else {
			vals[k] = append(vals[k], v)
		}
	}
	for _, k := range keys {
		for _, v := range vals[k] {
			st.encodeHeaderField(k, v)
		}
	}
	return st.headerBuf.Bytes()
}

// bodylessReq1 writes a HEADERS frames with StreamID 1 and EndStream and EndHeaders set.
func (st *serverTester) bodylessReq1(headers ...string) {
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(headers...),
		EndStream:     true,
		EndHeaders:    true,
	})
}

func (st *serverTester) wantFlowControlConsumed(streamID, consumed int32) {
	conf := configFromServer(st.sc.hs, st.sc.srv)
	var initial int32
	if streamID == 0 {
		initial = conf.MaxUploadBufferPerConnection
	} else {
		initial = conf.MaxUploadBufferPerStream
	}
	donec := make(chan struct{})
	st.sc.sendServeMsg(func(sc *serverConn) {
		defer close(donec)
		var avail int32
		if streamID == 0 {
			avail = sc.inflow.avail + sc.inflow.unsent
		} else {
		}
		if got, want := initial-avail, consumed; got != want {
			st.t.Errorf("stream %v flow control consumed: %v, want %v", streamID, got, want)
		}
	})
	<-donec
}

func TestServer(t *testing.T) {
	gotReq := make(chan bool, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Foo", "Bar")
		gotReq <- true
	})
	defer st.Close()

	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(),
		EndStream:     true, // no DATA frames
		EndHeaders:    true,
	})

	<-gotReq
}

func TestServer_Request_Get(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader("foo-bar", "some-value"),
			EndStream:     true, // no DATA frames
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("Method = %q; want GET", r.Method)
		}
		if r.URL.Path != "/" {
			t.Errorf("URL.Path = %q; want /", r.URL.Path)
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

func TestServer_Request_Get_PathSlashes(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":path", "/%2f/"),
			EndStream:     true, // no DATA frames
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.RequestURI != "/%2f/" {
			t.Errorf("RequestURI = %q; want /%%2f/", r.RequestURI)
		}
		if r.URL.Path != "///" {
			t.Errorf("URL.Path = %q; want ///", r.URL.Path)
		}
	})
}

// TODO: add a test with EndStream=true on the HEADERS but setting a
// Content-Length anyway. Should we just omit it and force it to
// zero?

func TestServer_Request_Post_NoContentLength_EndStream(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST"),
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

func TestServer_Request_Post_Body_ImmediateEOF(t *testing.T) {
	testBodyContents(t, -1, "", func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     false, // to say DATA frames are coming
			EndHeaders:    true,
		})
		st.writeData(1, true, nil) // just kidding. empty body.
	})
}

func TestServer_Request_Post_Body_OneData(t *testing.T) {
	const content = "Some content"
	testBodyContents(t, -1, content, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     false, // to say DATA frames are coming
			EndHeaders:    true,
		})
		st.writeData(1, true, []byte(content))
	})
}

func TestServer_Request_Post_Body_TwoData(t *testing.T) {
	const content = "Some content"
	testBodyContents(t, -1, content, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     false, // to say DATA frames are coming
			EndHeaders:    true,
		})
		st.writeData(1, false, []byte(content[:5]))
		st.writeData(1, true, []byte(content[5:]))
	})
}

func TestServer_Request_Post_Body_ContentLength_Correct(t *testing.T) {
	const content = "Some content"
	testBodyContents(t, int64(len(content)), content, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1, // clients send odd numbers
			BlockFragment: st.encodeHeader(
				":method", "POST",
				"content-length", strconv.Itoa(len(content)),
			),
			EndStream:  false, // to say DATA frames are coming
			EndHeaders: true,
		})
		st.writeData(1, true, []byte(content))
	})
}

func TestServer_Request_Post_Body_ContentLength_TooLarge(t *testing.T) {
	testBodyContentsFail(t, 3, "request declared a Content-Length of 3 but only wrote 2 bytes",
		func(st *serverTester) {
			st.writeHeaders(HeadersFrameParam{
				StreamID: 1, // clients send odd numbers
				BlockFragment: st.encodeHeader(
					":method", "POST",
					"content-length", "3",
				),
				EndStream:  false, // to say DATA frames are coming
				EndHeaders: true,
			})
			st.writeData(1, true, []byte("12"))
		})
}

func TestServer_Request_Post_Body_ContentLength_TooSmall(t *testing.T) {
	testBodyContentsFail(t, 4, "sender tried to send more than declared Content-Length of 4 bytes",
		func(st *serverTester) {
			st.writeHeaders(HeadersFrameParam{
				StreamID: 1, // clients send odd numbers
				BlockFragment: st.encodeHeader(
					":method", "POST",
					"content-length", "4",
				),
				EndStream:  false, // to say DATA frames are coming
				EndHeaders: true,
			})
			st.writeData(1, true, []byte("12345"))
			// Return flow control bytes back, since the data handler closed
			// the stream.
			st.wantRSTStream(1, ErrCodeProtocol)
			st.wantFlowControlConsumed(0, 0)
		})
}

func testBodyContents(t *testing.T, wantContentLength int64, wantBody string, write func(st *serverTester)) {
	testServerRequest(t, write, func(r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Method = %q; want POST", r.Method)
		}
		if r.ContentLength != wantContentLength {
			t.Errorf("ContentLength = %v; want %d", r.ContentLength, wantContentLength)
		}
		all, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(all) != wantBody {
			t.Errorf("Read = %q; want %q", all, wantBody)
		}
		if err := r.Body.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func testBodyContentsFail(t *testing.T, wantContentLength int64, wantReadError string, write func(st *serverTester)) {
	testServerRequest(t, write, func(r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Method = %q; want POST", r.Method)
		}
		if r.ContentLength != wantContentLength {
			t.Errorf("ContentLength = %v; want %d", r.ContentLength, wantContentLength)
		}
		all, err := io.ReadAll(r.Body)
		if err == nil {
			t.Fatalf("expected an error (%q) reading from the body. Successfully read %q instead.",
				wantReadError, all)
		}
		if !strings.Contains(err.Error(), wantReadError) {
			t.Fatalf("Body.Read = %v; want substring %q", err, wantReadError)
		}
		if err := r.Body.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// Using a Host header, instead of :authority
func TestServer_Request_Get_Host(t *testing.T) {
	const host = "example.com"
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":authority", "", "host", host),
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
			BlockFragment: st.encodeHeader(":authority", host),
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
		fullHeaders := st.encodeHeader(
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

func TestServer_Request_Reject_HeaderFieldNameColon(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("has:colon", "v") })
}

func TestServer_Request_Reject_HeaderFieldNameNULL(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("has\x00null", "v") })
}

func TestServer_Request_Reject_HeaderFieldNameEmpty(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("", "v") })
}

func TestServer_Request_Reject_HeaderFieldValueNewline(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("foo", "has\nnewline") })
}

func TestServer_Request_Reject_HeaderFieldValueCR(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("foo", "has\rcarriage") })
}

func TestServer_Request_Reject_HeaderFieldValueDEL(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("foo", "has\x7fdel") })
}

func TestServer_Request_Reject_Pseudo_Missing_method(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":method", "") })
}

func TestServer_Request_Reject_Pseudo_ExactlyOne(t *testing.T) {
	// 8.1.2.3 Request Pseudo-Header Fields
	// "All HTTP/2 requests MUST include exactly one valid value" ...
	testRejectRequest(t, func(st *serverTester) {
		st.addLogFilter("duplicate pseudo-header")
		st.bodylessReq1(":method", "GET", ":method", "POST")
	})
}

func TestServer_Request_Reject_Pseudo_AfterRegular(t *testing.T) {
	// 8.1.2.3 Request Pseudo-Header Fields
	// "All pseudo-header fields MUST appear in the header block
	// before regular header fields. Any request or response that
	// contains a pseudo-header field that appears in a header
	// block after a regular header field MUST be treated as
	// malformed (Section 8.1.2.6)."
	testRejectRequest(t, func(st *serverTester) {
		st.addLogFilter("pseudo-header after regular header")
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		enc.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"})
		enc.WriteField(hpack.HeaderField{Name: "regular", Value: "foobar"})
		enc.WriteField(hpack.HeaderField{Name: ":path", Value: "/"})
		enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: buf.Bytes(),
			EndStream:     true,
			EndHeaders:    true,
		})
	})
}

func TestServer_Request_Reject_Pseudo_Missing_path(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":path", "") })
}

func TestServer_Request_Reject_Pseudo_Missing_scheme(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":scheme", "") })
}

func TestServer_Request_Reject_Pseudo_scheme_invalid(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":scheme", "bogus") })
}

func TestServer_Request_Reject_Pseudo_Unknown(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) {
		st.addLogFilter(`invalid pseudo-header ":unknown_thing"`)
		st.bodylessReq1(":unknown_thing", "")
	})
}

func TestServer_Request_Reject_Authority_Userinfo(t *testing.T) {
	// "':authority' MUST NOT include the deprecated userinfo subcomponent
	// for "http" or "https" schemed URIs."
	// https://www.rfc-editor.org/rfc/rfc9113.html#section-8.3.1-2.3.8
	testRejectRequest(t, func(st *serverTester) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		enc.WriteField(hpack.HeaderField{Name: ":authority", Value: "userinfo@example.tld"})
		enc.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"})
		enc.WriteField(hpack.HeaderField{Name: ":path", Value: "/"})
		enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: buf.Bytes(),
			EndStream:     true,
			EndHeaders:    true,
		})
	})
}

func testRejectRequest(t *testing.T, send func(*serverTester)) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server request made it to handler; should've been rejected")
	})
	defer st.Close()

	st.greet()
	send(st)
	st.wantRSTStream(1, ErrCodeProtocol)
}

func newServerTesterForError(t *testing.T) *serverTester {
	t.Helper()
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server request made it to handler; should've been rejected")
	}, optQuiet)
	st.greet()
	return st
}

// Section 5.1, on idle connections: "Receiving any frame other than
// HEADERS or PRIORITY on a stream in this state MUST be treated as a
// connection error (Section 5.4.1) of type PROTOCOL_ERROR."
func TestRejectFrameOnIdle_WindowUpdate(t *testing.T) {
	st := newServerTesterForError(t)
	st.fr.WriteWindowUpdate(123, 456)
	st.wantGoAway(123, ErrCodeProtocol)
}
func TestRejectFrameOnIdle_Data(t *testing.T) {
	st := newServerTesterForError(t)
	st.fr.WriteData(123, true, nil)
	st.wantGoAway(123, ErrCodeProtocol)
}
func TestRejectFrameOnIdle_RSTStream(t *testing.T) {
	st := newServerTesterForError(t)
	st.fr.WriteRSTStream(123, ErrCodeCancel)
	st.wantGoAway(123, ErrCodeProtocol)
}

func TestServer_Request_Connect(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1,
			BlockFragment: st.encodeHeaderRaw(
				":method", "CONNECT",
				":authority", "example.com:123",
			),
			EndStream:  true,
			EndHeaders: true,
		})
	}, func(r *http.Request) {
		if g, w := r.Method, "CONNECT"; g != w {
			t.Errorf("Method = %q; want %q", g, w)
		}
		if g, w := r.RequestURI, "example.com:123"; g != w {
			t.Errorf("RequestURI = %q; want %q", g, w)
		}
		if g, w := r.URL.Host, "example.com:123"; g != w {
			t.Errorf("URL.Host = %q; want %q", g, w)
		}
	})
}

func TestServer_Request_Connect_InvalidPath(t *testing.T) {
	testServerRejectsStream(t, ErrCodeProtocol, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1,
			BlockFragment: st.encodeHeaderRaw(
				":method", "CONNECT",
				":authority", "example.com:123",
				":path", "/bogus",
			),
			EndStream:  true,
			EndHeaders: true,
		})
	})
}

func TestServer_Request_Connect_InvalidScheme(t *testing.T) {
	testServerRejectsStream(t, ErrCodeProtocol, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1,
			BlockFragment: st.encodeHeaderRaw(
				":method", "CONNECT",
				":authority", "example.com:123",
				":scheme", "https",
			),
			EndStream:  true,
			EndHeaders: true,
		})
	})
}

func TestServer_Ping(t *testing.T) {
	st := newServerTester(t, nil)
	defer st.Close()
	st.greet()

	// Server should ignore this one, since it has ACK set.
	ackPingData := [8]byte{1, 2, 4, 8, 16, 32, 64, 128}
	if err := st.fr.WritePing(true, ackPingData); err != nil {
		t.Fatal(err)
	}

	// But the server should reply to this one, since ACK is false.
	pingData := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := st.fr.WritePing(false, pingData); err != nil {
		t.Fatal(err)
	}

	pf := readFrame[*PingFrame](t, st)
	if !pf.Flags.Has(FlagPingAck) {
		t.Error("response ping doesn't have ACK set")
	}
	if pf.Data != pingData {
		t.Errorf("response ping has data %q; want %q", pf.Data, pingData)
	}
}

type filterListener struct {
	net.Listener
	accept func(conn net.Conn) (net.Conn, error)
}

func (l *filterListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.accept(c)
}

func TestServer_MaxQueuedControlFrames(t *testing.T) {
	// Goroutine debugging makes this test very slow.
	disableGoroutineTracking(t)

	st := newServerTester(t, nil)
	st.greet()

	st.cc.(*synctestNetConn).SetReadBufferSize(0) // all writes block
	st.cc.(*synctestNetConn).autoWait = false     // don't sync after every write

	// Send maxQueuedControlFrames pings, plus a few extra
	// to account for ones that enter the server's write buffer.
	const extraPings = 2
	for i := 0; i < maxQueuedControlFrames+extraPings; i++ {
		pingData := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
		st.fr.WritePing(false, pingData)
	}
	st.group.Wait()

	// Unblock the server.
	// It should have closed the connection after exceeding the control frame limit.
	st.cc.(*synctestNetConn).SetReadBufferSize(math.MaxInt)

	st.advance(goAwayTimeout)
	// Some frames may have persisted in the server's buffers.
	for i := 0; i < 10; i++ {
		if st.readFrame() == nil {
			break
		}
	}
	st.wantClosed()
}

func TestServer_RejectsLargeFrames(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" || runtime.GOOS == "zos" {
		t.Skip("see golang.org/issue/13434, golang.org/issue/37321")
	}
	st := newServerTester(t, nil)
	defer st.Close()
	st.greet()

	// Write too large of a frame (too large by one byte)
	// We ignore the return value because it's expected that the server
	// will only read the first 9 bytes (the headre) and then disconnect.
	st.fr.WriteRawFrame(0xff, 0, 0, make([]byte, defaultMaxReadFrameSize+1))

	st.wantGoAway(0, ErrCodeFrameSize)
	st.advance(goAwayTimeout)
	st.wantClosed()
}

func TestServer_Handler_Sends_WindowUpdate(t *testing.T) {
	// Need to set this to at least twice the initial window size,
	// or st.greet gets stuck waiting for a WINDOW_UPDATE.
	//
	// This also needs to be less than MAX_FRAME_SIZE.
	const windowSize = 65535 * 2
	puppet := newHandlerPuppet()
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		puppet.act(w, r)
	}, func(s *Server) {
		s.MaxUploadBufferPerConnection = windowSize
		s.MaxUploadBufferPerStream = windowSize
	})
	defer st.Close()
	defer puppet.done()

	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false, // data coming
		EndHeaders:    true,
	})

	// Write less than half the max window of data and consume it.
	// The server doesn't return flow control yet, buffering the 1024 bytes to
	// combine with a future update.
	data := make([]byte, windowSize)
	st.writeData(1, false, data[:1024])
	puppet.do(readBodyHandler(t, string(data[:1024])))

	// Write up to the window limit.
	// The server returns the buffered credit.
	st.writeData(1, false, data[1024:])
	st.wantWindowUpdate(0, 1024)
	st.wantWindowUpdate(1, 1024)

	// The handler consumes the data and the server returns credit.
	puppet.do(readBodyHandler(t, string(data[1024:])))
	st.wantWindowUpdate(0, windowSize-1024)
	st.wantWindowUpdate(1, windowSize-1024)
}

// the version of the TestServer_Handler_Sends_WindowUpdate with padding.
// See golang.org/issue/16556
func TestServer_Handler_Sends_WindowUpdate_Padding(t *testing.T) {
	const windowSize = 65535 * 2
	puppet := newHandlerPuppet()
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		puppet.act(w, r)
	}, func(s *Server) {
		s.MaxUploadBufferPerConnection = windowSize
		s.MaxUploadBufferPerStream = windowSize
	})
	defer st.Close()
	defer puppet.done()

	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false,
		EndHeaders:    true,
	})

	// Write half a window of data, with some padding.
	// The server doesn't return the padding yet, buffering the 5 bytes to combine
	// with a future update.
	data := make([]byte, windowSize/2)
	pad := make([]byte, 4)
	st.writeDataPadded(1, false, data, pad)

	// The handler consumes the body.
	// The server returns flow control for the body and padding
	// (4 bytes of padding + 1 byte of length).
	puppet.do(readBodyHandler(t, string(data)))
	st.wantWindowUpdate(0, uint32(len(data)+1+len(pad)))
	st.wantWindowUpdate(1, uint32(len(data)+1+len(pad)))
}

func TestServer_Send_GoAway_After_Bogus_WindowUpdate(t *testing.T) {
	st := newServerTester(t, nil)
	defer st.Close()
	st.greet()
	if err := st.fr.WriteWindowUpdate(0, 1<<31-1); err != nil {
		t.Fatal(err)
	}
	st.wantGoAway(0, ErrCodeFlowControl)
}

func TestServer_Send_RstStream_After_Bogus_WindowUpdate(t *testing.T) {
	inHandler := make(chan bool)
	blockHandler := make(chan bool)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		inHandler <- true
		<-blockHandler
	})
	defer st.Close()
	defer close(blockHandler)
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-inHandler
	// Send a bogus window update:
	if err := st.fr.WriteWindowUpdate(1, 1<<31-1); err != nil {
		t.Fatal(err)
	}
	st.wantRSTStream(1, ErrCodeFlowControl)
}

// testServerPostUnblock sends a hanging POST with unsent data to handler,
// then runs fn once in the handler, and verifies that the error returned from
// handler is acceptable. It fails if takes over 5 seconds for handler to exit.
func testServerPostUnblock(t *testing.T,
	handler func(http.ResponseWriter, *http.Request) error,
	fn func(*serverTester),
	checkErr func(error),
	otherHeaders ...string) {
	inHandler := make(chan bool)
	errc := make(chan error, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		inHandler <- true
		errc <- handler(w, r)
	})
	defer st.Close()
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(append([]string{":method", "POST"}, otherHeaders...)...),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-inHandler
	fn(st)
	err := <-errc
	if checkErr != nil {
		checkErr(err)
	}
}

func TestServer_RSTStream_Unblocks_Read(t *testing.T) {
	testServerPostUnblock(t,
		func(w http.ResponseWriter, r *http.Request) (err error) {
			_, err = r.Body.Read(make([]byte, 1))
			return
		},
		func(st *serverTester) {
			if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
				t.Fatal(err)
			}
		},
		func(err error) {
			want := StreamError{StreamID: 0x1, Code: 0x8}
			if !reflect.DeepEqual(err, want) {
				t.Errorf("Read error = %v; want %v", err, want)
			}
		},
	)
}

func TestServer_RSTStream_Unblocks_Header_Write(t *testing.T) {
	// Run this test a bunch, because it doesn't always
	// deadlock. But with a bunch, it did.
	n := 50
	if testing.Short() {
		n = 5
	}
	for i := 0; i < n; i++ {
		testServer_RSTStream_Unblocks_Header_Write(t)
	}
}

func testServer_RSTStream_Unblocks_Header_Write(t *testing.T) {
	inHandler := make(chan bool, 1)
	unblockHandler := make(chan bool, 1)
	headerWritten := make(chan bool, 1)
	wroteRST := make(chan bool, 1)

	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		inHandler <- true
		<-wroteRST
		w.Header().Set("foo", "bar")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		headerWritten <- true
		<-unblockHandler
	})
	defer st.Close()

	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-inHandler
	if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	wroteRST <- true
	st.awaitIdle()
	<-headerWritten
	unblockHandler <- true
}

func TestServer_DeadConn_Unblocks_Read(t *testing.T) {
	testServerPostUnblock(t,
		func(w http.ResponseWriter, r *http.Request) (err error) {
			_, err = r.Body.Read(make([]byte, 1))
			return
		},
		func(st *serverTester) { st.cc.Close() },
		func(err error) {
			if err == nil {
				t.Error("unexpected nil error from Request.Body.Read")
			}
		},
	)
}

var blockUntilClosed = func(w http.ResponseWriter, r *http.Request) error {
	<-w.(http.CloseNotifier).CloseNotify()
	return nil
}

func TestServer_CloseNotify_After_RSTStream(t *testing.T) {
	testServerPostUnblock(t, blockUntilClosed, func(st *serverTester) {
		if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	}, nil)
}

func TestServer_CloseNotify_After_ConnClose(t *testing.T) {
	testServerPostUnblock(t, blockUntilClosed, func(st *serverTester) { st.cc.Close() }, nil)
}

// that CloseNotify unblocks after a stream error due to the client's
// problem that's unrelated to them explicitly canceling it (which is
// TestServer_CloseNotify_After_RSTStream above)
func TestServer_CloseNotify_After_StreamError(t *testing.T) {
	testServerPostUnblock(t, blockUntilClosed, func(st *serverTester) {
		// data longer than declared Content-Length => stream error
		st.writeData(1, true, []byte("1234"))
	}, nil, "content-length", "3")
}

func TestServer_StateTransitions(t *testing.T) {
	var st *serverTester
	inHandler := make(chan bool)
	writeData := make(chan bool)
	leaveHandler := make(chan bool)
	st = newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		inHandler <- true
		if st.stream(1) == nil {
			t.Errorf("nil stream 1 in handler")
		}
		if got, want := st.streamState(1), stateOpen; got != want {
			t.Errorf("in handler, state is %v; want %v", got, want)
		}
		writeData <- true
		if n, err := r.Body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Errorf("body read = %d, %v; want 0, EOF", n, err)
		}
		if got, want := st.streamState(1), stateHalfClosedRemote; got != want {
			t.Errorf("in handler, state is %v; want %v", got, want)
		}

		<-leaveHandler
	})
	st.greet()
	if st.stream(1) != nil {
		t.Fatal("stream 1 should be empty")
	}
	if got := st.streamState(1); got != stateIdle {
		t.Fatalf("stream 1 should be idle; got %v", got)
	}

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-inHandler
	<-writeData
	st.writeData(1, true, nil)

	leaveHandler <- true
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})

	if got, want := st.streamState(1), stateClosed; got != want {
		t.Errorf("at end, state is %v; want %v", got, want)
	}
	if st.stream(1) != nil {
		t.Fatal("at end, stream 1 should be gone")
	}
}

// test HEADERS w/o EndHeaders + another HEADERS (should get rejected)
func TestServer_Rejects_HeadersNoEnd_Then_Headers(t *testing.T) {
	st := newServerTesterForError(t)
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    false,
	})
	st.writeHeaders(HeadersFrameParam{ // Not a continuation.
		StreamID:      3, // different stream.
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantGoAway(0, ErrCodeProtocol)
}

// test HEADERS w/o EndHeaders + PING (should get rejected)
func TestServer_Rejects_HeadersNoEnd_Then_Ping(t *testing.T) {
	st := newServerTesterForError(t)
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    false,
	})
	if err := st.fr.WritePing(false, [8]byte{}); err != nil {
		t.Fatal(err)
	}
	st.wantGoAway(0, ErrCodeProtocol)
}

// test HEADERS w/ EndHeaders + a continuation HEADERS (should get rejected)
func TestServer_Rejects_HeadersEnd_Then_Continuation(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {}, optQuiet)
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	if err := st.fr.WriteContinuation(1, true, encodeHeaderNoImplicit(t, "foo", "bar")); err != nil {
		t.Fatal(err)
	}
	st.wantGoAway(1, ErrCodeProtocol)
}

// test HEADERS w/o EndHeaders + a continuation HEADERS on wrong stream ID
func TestServer_Rejects_HeadersNoEnd_Then_ContinuationWrongStream(t *testing.T) {
	st := newServerTesterForError(t)
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    false,
	})
	if err := st.fr.WriteContinuation(3, true, encodeHeaderNoImplicit(t, "foo", "bar")); err != nil {
		t.Fatal(err)
	}
	st.wantGoAway(0, ErrCodeProtocol)
}

// No HEADERS on stream 0.
func TestServer_Rejects_Headers0(t *testing.T) {
	st := newServerTesterForError(t)
	st.fr.AllowIllegalWrites = true
	st.writeHeaders(HeadersFrameParam{
		StreamID:      0,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantGoAway(0, ErrCodeProtocol)
}

// No CONTINUATION on stream 0.
func TestServer_Rejects_Continuation0(t *testing.T) {
	st := newServerTesterForError(t)
	st.fr.AllowIllegalWrites = true
	if err := st.fr.WriteContinuation(0, true, st.encodeHeader()); err != nil {
		t.Fatal(err)
	}
	st.wantGoAway(0, ErrCodeProtocol)
}

// No PRIORITY on stream 0.
func TestServer_Rejects_Priority0(t *testing.T) {
	st := newServerTesterForError(t)
	st.fr.AllowIllegalWrites = true
	st.writePriority(0, PriorityParam{StreamDep: 1})
	st.wantGoAway(0, ErrCodeProtocol)
}

// No HEADERS frame with a self-dependence.
func TestServer_Rejects_HeadersSelfDependence(t *testing.T) {
	testServerRejectsStream(t, ErrCodeProtocol, func(st *serverTester) {
		st.fr.AllowIllegalWrites = true
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
			Priority:      PriorityParam{StreamDep: 1},
		})
	})
}

// No PRIORITY frame with a self-dependence.
func TestServer_Rejects_PrioritySelfDependence(t *testing.T) {
	testServerRejectsStream(t, ErrCodeProtocol, func(st *serverTester) {
		st.fr.AllowIllegalWrites = true
		st.writePriority(1, PriorityParam{StreamDep: 1})
	})
}

func TestServer_Rejects_PushPromise(t *testing.T) {
	st := newServerTesterForError(t)
	pp := PushPromiseParam{
		StreamID:  1,
		PromiseID: 3,
	}
	if err := st.fr.WritePushPromise(pp); err != nil {
		t.Fatal(err)
	}
	st.wantGoAway(1, ErrCodeProtocol)
}

// testServerRejectsStream tests that the server sends a RST_STREAM with the provided
// error code after a client sends a bogus request.
func testServerRejectsStream(t *testing.T, code ErrCode, writeReq func(*serverTester)) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {})
	defer st.Close()
	st.greet()
	writeReq(st)
	st.wantRSTStream(1, code)
}

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
	<-gotReq
}

func getSlash(st *serverTester) { st.bodylessReq1() }

func TestServer_Response_NoData(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		// Nothing.
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: true,
		})
	})
}

func TestServer_Response_NoData_Header_FooBar(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Foo-Bar", "some-value")
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: true,
			header: http.Header{
				":status":        []string{"200"},
				"foo-bar":        []string{"some-value"},
				"content-length": []string{"0"},
			},
		})
	})
}

// Reject content-length headers containing a sign.
// See https://golang.org/issue/39017
func TestServerIgnoresContentLengthSignWhenWritingChunks(t *testing.T) {
	tests := []struct {
		name   string
		cl     string
		wantCL string
	}{
		{
			name:   "proper content-length",
			cl:     "3",
			wantCL: "3",
		},
		{
			name:   "ignore cl with plus sign",
			cl:     "+3",
			wantCL: "0",
		},
		{
			name:   "ignore cl with minus sign",
			cl:     "-3",
			wantCL: "0",
		},
		{
			name:   "max int64, for safe uint64->int64 conversion",
			cl:     "9223372036854775807",
			wantCL: "9223372036854775807",
		},
		{
			name:   "overflows int64, so ignored",
			cl:     "9223372036854775808",
			wantCL: "0",
		},
	}

	for _, tt := range tests {
		testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("content-length", tt.cl)
			return nil
		}, func(st *serverTester) {
			getSlash(st)
			st.wantHeaders(wantHeader{
				streamID:  1,
				endStream: true,
				header: http.Header{
					":status":        []string{"200"},
					"content-length": []string{tt.wantCL},
				},
			})
		})
	}
}

// Reject content-length headers containing a sign.
// See https://golang.org/issue/39017
func TestServerRejectsContentLengthWithSignNewRequests(t *testing.T) {
	tests := []struct {
		name   string
		cl     string
		wantCL int64
	}{
		{
			name:   "proper content-length",
			cl:     "3",
			wantCL: 3,
		},
		{
			name:   "ignore cl with plus sign",
			cl:     "+3",
			wantCL: 0,
		},
		{
			name:   "ignore cl with minus sign",
			cl:     "-3",
			wantCL: 0,
		},
		{
			name:   "max int64, for safe uint64->int64 conversion",
			cl:     "9223372036854775807",
			wantCL: 9223372036854775807,
		},
		{
			name:   "overflows int64, so ignored",
			cl:     "9223372036854775808",
			wantCL: 0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			writeReq := func(st *serverTester) {
				st.writeHeaders(HeadersFrameParam{
					StreamID:      1, // clients send odd numbers
					BlockFragment: st.encodeHeader("content-length", tt.cl),
					EndStream:     false,
					EndHeaders:    true,
				})
				st.writeData(1, false, []byte(""))
			}
			checkReq := func(r *http.Request) {
				if r.ContentLength != tt.wantCL {
					t.Fatalf("Got: %q\nWant: %q", r.ContentLength, tt.wantCL)
				}
			}
			testServerRequest(t, writeReq, checkReq)
		})
	}
}

func TestServer_Response_Data_Sniff_DoesntOverride(t *testing.T) {
	const msg = "<html>this is HTML."
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "foo/bar")
		io.WriteString(w, msg)
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":        []string{"200"},
				"content-type":   []string{"foo/bar"},
				"content-length": []string{strconv.Itoa(len(msg))},
			},
		})
		st.wantData(wantData{
			streamID:  1,
			endStream: true,
			data:      []byte(msg),
		})
	})
}

func TestServer_Response_TransferEncoding_chunked(t *testing.T) {
	const msg = "hi"
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Transfer-Encoding", "chunked") // should be stripped
		io.WriteString(w, msg)
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":        []string{"200"},
				"content-type":   []string{"text/plain; charset=utf-8"},
				"content-length": []string{strconv.Itoa(len(msg))},
			},
		})
	})
}

// Header accessed only after the initial write.
func TestServer_Response_Data_IgnoreHeaderAfterWrite_After(t *testing.T) {
	const msg = "<html>this is HTML."
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		io.WriteString(w, msg)
		w.Header().Set("foo", "should be ignored")
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":        []string{"200"},
				"content-type":   []string{"text/html; charset=utf-8"},
				"content-length": []string{strconv.Itoa(len(msg))},
			},
		})
	})
}

// Header accessed before the initial write and later mutated.
func TestServer_Response_Data_IgnoreHeaderAfterWrite_Overwrite(t *testing.T) {
	const msg = "<html>this is HTML."
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("foo", "proper value")
		io.WriteString(w, msg)
		w.Header().Set("foo", "should be ignored")
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":        []string{"200"},
				"foo":            []string{"proper value"},
				"content-type":   []string{"text/html; charset=utf-8"},
				"content-length": []string{strconv.Itoa(len(msg))},
			},
		})
	})
}

func TestServer_Response_Data_SniffLenType(t *testing.T) {
	const msg = "<html>this is HTML."
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		io.WriteString(w, msg)
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":        []string{"200"},
				"content-type":   []string{"text/html; charset=utf-8"},
				"content-length": []string{strconv.Itoa(len(msg))},
			},
		})
		st.wantData(wantData{
			streamID:  1,
			endStream: true,
			data:      []byte(msg),
		})
	})
}

func TestServer_Response_Header_Flush_MidWrite(t *testing.T) {
	const msg = "<html>this is HTML"
	const msg2 = ", and this is the next chunk"
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		io.WriteString(w, msg)
		w.(http.Flusher).Flush()
		io.WriteString(w, msg2)
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":      []string{"200"},
				"content-type": []string{"text/html; charset=utf-8"}, // sniffed
				// and no content-length
			},
		})
		st.wantData(wantData{
			streamID:  1,
			endStream: false,
			data:      []byte(msg),
		})
		st.wantData(wantData{
			streamID:  1,
			endStream: true,
			data:      []byte(msg2),
		})
	})
}

func TestServer_Response_LargeWrite(t *testing.T) {
	const size = 1 << 20
	const maxFrameSize = 16 << 10
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		n, err := w.Write(bytes.Repeat([]byte("a"), size))
		if err != nil {
			return fmt.Errorf("Write error: %v", err)
		}
		if n != size {
			return fmt.Errorf("wrong size %d from Write", n)
		}
		return nil
	}, func(st *serverTester) {
		if err := st.fr.WriteSettings(
			Setting{SettingInitialWindowSize, 0},
			Setting{SettingMaxFrameSize, maxFrameSize},
		); err != nil {
			t.Fatal(err)
		}
		st.wantSettingsAck()

		getSlash(st) // make the single request

		// Give the handler quota to write:
		if err := st.fr.WriteWindowUpdate(1, size); err != nil {
			t.Fatal(err)
		}
		// Give the handler quota to write to connection-level
		// window as well
		if err := st.fr.WriteWindowUpdate(0, size); err != nil {
			t.Fatal(err)
		}
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":      []string{"200"},
				"content-type": []string{"text/plain; charset=utf-8"}, // sniffed
				// and no content-length
			},
		})
		var bytes, frames int
		for {
			df := readFrame[*DataFrame](t, st)
			bytes += len(df.Data())
			frames++
			for _, b := range df.Data() {
				if b != 'a' {
					t.Fatal("non-'a' byte seen in DATA")
				}
			}
			if df.StreamEnded() {
				break
			}
		}
		if bytes != size {
			t.Errorf("Got %d bytes; want %d", bytes, size)
		}
		if want := int(size / maxFrameSize); frames < want || frames > want*2 {
			t.Errorf("Got %d frames; want %d", frames, size)
		}
	})
}

// Test that the handler can't write more than the client allows
func TestServer_Response_LargeWrite_FlowControlled(t *testing.T) {
	// Make these reads. Before each read, the client adds exactly enough
	// flow-control to satisfy the read. Numbers chosen arbitrarily.
	reads := []int{123, 1, 13, 127}
	size := 0
	for _, n := range reads {
		size += n
	}

	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.(http.Flusher).Flush()
		n, err := w.Write(bytes.Repeat([]byte("a"), size))
		if err != nil {
			return fmt.Errorf("Write error: %v", err)
		}
		if n != size {
			return fmt.Errorf("wrong size %d from Write", n)
		}
		return nil
	}, func(st *serverTester) {
		// Set the window size to something explicit for this test.
		// It's also how much initial data we expect.
		if err := st.fr.WriteSettings(Setting{SettingInitialWindowSize, uint32(reads[0])}); err != nil {
			t.Fatal(err)
		}
		st.wantSettingsAck()

		getSlash(st) // make the single request

		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
		})

		st.wantData(wantData{
			streamID:  1,
			endStream: false,
			size:      reads[0],
		})

		for i, quota := range reads[1:] {
			if err := st.fr.WriteWindowUpdate(1, uint32(quota)); err != nil {
				t.Fatal(err)
			}
			st.wantData(wantData{
				streamID:  1,
				endStream: i == len(reads[1:])-1,
				size:      quota,
			})
		}
	})
}

// Test that the handler blocked in a Write is unblocked if the server sends a RST_STREAM.
func TestServer_Response_RST_Unblocks_LargeWrite(t *testing.T) {
	const size = 1 << 20
	const maxFrameSize = 16 << 10
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.(http.Flusher).Flush()
		_, err := w.Write(bytes.Repeat([]byte("a"), size))
		if err == nil {
			return errors.New("unexpected nil error from Write in handler")
		}
		return nil
	}, func(st *serverTester) {
		if err := st.fr.WriteSettings(
			Setting{SettingInitialWindowSize, 0},
			Setting{SettingMaxFrameSize, maxFrameSize},
		); err != nil {
			t.Fatal(err)
		}
		st.wantSettingsAck()

		getSlash(st) // make the single request

		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
		})

		if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	})
}

func TestServer_Response_Empty_Data_Not_FlowControlled(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.(http.Flusher).Flush()
		// Nothing; send empty DATA
		return nil
	}, func(st *serverTester) {
		// Handler gets no data quota:
		if err := st.fr.WriteSettings(Setting{SettingInitialWindowSize, 0}); err != nil {
			t.Fatal(err)
		}
		st.wantSettingsAck()

		getSlash(st) // make the single request

		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
		})

		st.wantData(wantData{
			streamID:  1,
			endStream: true,
			size:      0,
		})
	})
}

func TestServer_Response_Automatic100Continue(t *testing.T) {
	const msg = "foo"
	const reply = "bar"
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		if v := r.Header.Get("Expect"); v != "" {
			t.Errorf("Expect header = %q; want empty", v)
		}
		buf := make([]byte, len(msg))
		// This read should trigger the 100-continue being sent.
		if n, err := io.ReadFull(r.Body, buf); err != nil || n != len(msg) || string(buf) != msg {
			return fmt.Errorf("ReadFull = %q, %v; want %q, nil", buf[:n], err, msg)
		}
		_, err := io.WriteString(w, reply)
		return err
	}, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST", "expect", "100-Continue"),
			EndStream:     false,
			EndHeaders:    true,
		})
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status": []string{"100"},
			},
		})

		// Okay, they sent status 100, so we can send our
		// gigantic and/or sensitive "foo" payload now.
		st.writeData(1, true, []byte(msg))

		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":        []string{"200"},
				"content-type":   []string{"text/plain; charset=utf-8"},
				"content-length": []string{strconv.Itoa(len(reply))},
			},
		})

		st.wantData(wantData{
			streamID:  1,
			endStream: true,
			data:      []byte(reply),
		})
	})
}

func TestServer_HandlerWriteErrorOnDisconnect(t *testing.T) {
	errc := make(chan error, 1)
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		p := []byte("some data.\n")
		for {
			_, err := w.Write(p)
			if err != nil {
				errc <- err
				return nil
			}
		}
	}, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     false,
			EndHeaders:    true,
		})
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
		})
		// Close the connection and wait for the handler to (hopefully) notice.
		st.cc.Close()
		_ = <-errc
	})
}

func TestServer_Rejects_Too_Many_Streams(t *testing.T) {
	const testPath = "/some/path"

	inHandler := make(chan uint32)
	leaveHandler := make(chan bool)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		id := w.(*responseWriter).rws.stream.id
		inHandler <- id
		if id == 1+(defaultMaxStreams+1)*2 && r.URL.Path != testPath {
			t.Errorf("decoded final path as %q; want %q", r.URL.Path, testPath)
		}
		<-leaveHandler
	})
	defer st.Close()

	// Automatically syncing after every write / before every read
	// slows this test down substantially.
	st.cc.(*synctestNetConn).autoWait = false

	st.greet()
	nextStreamID := uint32(1)
	streamID := func() uint32 {
		defer func() { nextStreamID += 2 }()
		return nextStreamID
	}
	sendReq := func(id uint32, headers ...string) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      id,
			BlockFragment: st.encodeHeader(headers...),
			EndStream:     true,
			EndHeaders:    true,
		})
	}
	for i := 0; i < defaultMaxStreams; i++ {
		sendReq(streamID())
		<-inHandler
	}
	defer func() {
		for i := 0; i < defaultMaxStreams; i++ {
			leaveHandler <- true
		}
	}()

	// And this one should cross the limit:
	// (It's also sent as a CONTINUATION, to verify we still track the decoder context,
	// even if we're rejecting it)
	rejectID := streamID()
	headerBlock := st.encodeHeader(":path", testPath)
	frag1, frag2 := headerBlock[:3], headerBlock[3:]
	st.writeHeaders(HeadersFrameParam{
		StreamID:      rejectID,
		BlockFragment: frag1,
		EndStream:     true,
		EndHeaders:    false, // CONTINUATION coming
	})
	if err := st.fr.WriteContinuation(rejectID, true, frag2); err != nil {
		t.Fatal(err)
	}
	st.sync()
	st.wantRSTStream(rejectID, ErrCodeProtocol)

	// But let a handler finish:
	leaveHandler <- true
	st.sync()
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})

	// And now another stream should be able to start:
	goodID := streamID()
	sendReq(goodID, ":path", testPath)
	if got := <-inHandler; got != goodID {
		t.Errorf("Got stream %d; want %d", got, goodID)
	}
}

// So many response headers that the server needs to use CONTINUATION frames:
func TestServer_Response_ManyHeaders_With_Continuation(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		h := w.Header()
		for i := 0; i < 5000; i++ {
			h.Set(fmt.Sprintf("x-header-%d", i), fmt.Sprintf("x-value-%d", i))
		}
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := readFrame[*HeadersFrame](t, st)
		if hf.HeadersEnded() {
			t.Fatal("got unwanted END_HEADERS flag")
		}
		n := 0
		for {
			n++
			cf := readFrame[*ContinuationFrame](t, st)
			if cf.HeadersEnded() {
				break
			}
		}
		if n < 5 {
			t.Errorf("Only got %d CONTINUATION frames; expected 5+ (currently 6)", n)
		}
	})
}

// This previously crashed (reported by Mathieu Lonjaret as observed
// while using Camlistore) because we got a DATA frame from the client
// after the handler exited and our logic at the time was wrong,
// keeping a stream in the map in stateClosed, which tickled an
// invariant check later when we tried to remove that stream (via
// defer sc.closeAllStreamsOnConnClose) when the serverConn serve loop
// ended.
func TestServer_NoCrash_HandlerClose_Then_ClientClose(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		// nothing
		return nil
	}, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     false, // DATA is coming
			EndHeaders:    true,
		})
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: true,
		})

		// Sent when the a Handler closes while a client has
		// indicated it's still sending DATA:
		st.wantRSTStream(1, ErrCodeNo)

		// Now the handler has ended, so it's ended its
		// stream, but the client hasn't closed its side
		// (stateClosedLocal).  So send more data and verify
		// it doesn't crash with an internal invariant panic, like
		// it did before.
		st.writeData(1, true, []byte("foo"))

		// Sent after a peer sends data anyway (admittedly the
		// previous RST_STREAM might've still been in-flight),
		// but they'll get the more friendly 'cancel' code
		// first.
		st.wantRSTStream(1, ErrCodeStreamClosed)

		// We should have our flow control bytes back,
		// since the handler didn't get them.
		st.wantFlowControlConsumed(0, 0)

		// Set up a bunch of machinery to record the panic we saw
		// previously.
		var (
			panMu    sync.Mutex
			panicVal interface{}
		)

		testHookOnPanicMu.Lock()
		testHookOnPanic = func(sc *serverConn, pv interface{}) bool {
			panMu.Lock()
			panicVal = pv
			panMu.Unlock()
			return true
		}
		testHookOnPanicMu.Unlock()

		// Now force the serve loop to end, via closing the connection.
		st.cc.Close()
		<-st.sc.doneServing

		panMu.Lock()
		got := panicVal
		panMu.Unlock()
		if got != nil {
			t.Errorf("Got panic: %v", got)
		}
	})
}

func TestServer_Rejects_TLS10(t *testing.T) { testRejectTLS(t, tls.VersionTLS10) }
func TestServer_Rejects_TLS11(t *testing.T) { testRejectTLS(t, tls.VersionTLS11) }

func testRejectTLS(t *testing.T, version uint16) {
	st := newServerTester(t, nil, func(state *tls.ConnectionState) {
		// As of 1.18 the default minimum Go TLS version is
		// 1.2. In order to test rejection of lower versions,
		// manually set the version to 1.0
		state.Version = version
	})
	defer st.Close()
	st.wantGoAway(0, ErrCodeInadequateSecurity)
}

func TestServer_Rejects_TLSBadCipher(t *testing.T) {
	st := newServerTester(t, nil, func(state *tls.ConnectionState) {
		state.Version = tls.VersionTLS12
		state.CipherSuite = tls.TLS_RSA_WITH_RC4_128_SHA
	})
	defer st.Close()
	st.wantGoAway(0, ErrCodeInadequateSecurity)
}

func TestServer_Advertises_Common_Cipher(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
	}, func(srv *http.Server) {
		// Have the server configured with no specific cipher suites.
		// This tests that Go's defaults include the required one.
		srv.TLSConfig = nil
	})

	// Have the client only support the one required by the spec.
	const requiredSuite = tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
	tlsConfig := tlsConfigInsecure.Clone()
	tlsConfig.MaxVersion = tls.VersionTLS12
	tlsConfig.CipherSuites = []uint16{requiredSuite}
	tr := &Transport{TLSClientConfig: tlsConfig}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}

// testServerResponse sets up an idle HTTP/2 connection. The client function should
// write a single request that must be handled by the handler.
func testServerResponse(t testing.TB,
	handler func(http.ResponseWriter, *http.Request) error,
	client func(*serverTester),
) {
	errc := make(chan error, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			t.Fatal("nil Body")
		}
		err := handler(w, r)
		select {
		case errc <- err:
		default:
			t.Errorf("unexpected duplicate request")
		}
	})
	defer st.Close()

	st.greet()
	client(st)

	if err := <-errc; err != nil {
		t.Fatalf("Error in handler: %v", err)
	}
}

// readBodyHandler returns an http Handler func that reads len(want)
// bytes from r.Body and fails t if the contents read were not
// the value of want.
func readBodyHandler(t *testing.T, want string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, len(want))
		_, err := io.ReadFull(r.Body, buf)
		if err != nil {
			t.Error(err)
			return
		}
		if string(buf) != want {
			t.Errorf("read %q; want %q", buf, want)
		}
	}
}

func TestServer_MaxDecoderHeaderTableSize(t *testing.T) {
	wantHeaderTableSize := uint32(initialHeaderTableSize * 2)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {}, func(s *Server) {
		s.MaxDecoderHeaderTableSize = wantHeaderTableSize
	})
	defer st.Close()

	var advHeaderTableSize *uint32
	st.greetAndCheckSettings(func(s Setting) error {
		switch s.ID {
		case SettingHeaderTableSize:
			advHeaderTableSize = &s.Val
		}
		return nil
	})

	if advHeaderTableSize == nil {
		t.Errorf("server didn't advertise a header table size")
	} else if got, want := *advHeaderTableSize, wantHeaderTableSize; got != want {
		t.Errorf("server advertised a header table size of %d, want %d", got, want)
	}
}

func TestServer_MaxEncoderHeaderTableSize(t *testing.T) {
	wantHeaderTableSize := uint32(initialHeaderTableSize / 2)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {}, func(s *Server) {
		s.MaxEncoderHeaderTableSize = wantHeaderTableSize
	})
	defer st.Close()

	st.greet()

	if got, want := st.sc.hpackEncoder.MaxDynamicTableSize(), wantHeaderTableSize; got != want {
		t.Errorf("server encoder is using a header table size of %d, want %d", got, want)
	}
}

// Issue 12843
func TestServerDoS_MaxHeaderListSize(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {})
	defer st.Close()

	// shake hands
	frameSize := defaultMaxReadFrameSize
	var advHeaderListSize *uint32
	st.greetAndCheckSettings(func(s Setting) error {
		switch s.ID {
		case SettingMaxFrameSize:
			if s.Val < minMaxFrameSize {
				frameSize = minMaxFrameSize
			} else if s.Val > maxFrameSize {
				frameSize = maxFrameSize
			} else {
				frameSize = int(s.Val)
			}
		case SettingMaxHeaderListSize:
			advHeaderListSize = &s.Val
		}
		return nil
	})

	if advHeaderListSize == nil {
		t.Errorf("server didn't advertise a max header list size")
	} else if *advHeaderListSize == 0 {
		t.Errorf("server advertised a max header list size of 0")
	}

	st.encodeHeaderField(":method", "GET")
	st.encodeHeaderField(":path", "/")
	st.encodeHeaderField(":scheme", "https")
	cookie := strings.Repeat("*", 4058)
	st.encodeHeaderField("cookie", cookie)
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.headerBuf.Bytes(),
		EndStream:     true,
		EndHeaders:    false,
	})

	// Capture the short encoding of a duplicate ~4K cookie, now
	// that we've already sent it once.
	st.headerBuf.Reset()
	st.encodeHeaderField("cookie", cookie)

	// Now send 1MB of it.
	const size = 1 << 20
	b := bytes.Repeat(st.headerBuf.Bytes(), size/st.headerBuf.Len())
	for len(b) > 0 {
		chunk := b
		if len(chunk) > frameSize {
			chunk = chunk[:frameSize]
		}
		b = b[len(chunk):]
		st.fr.WriteContinuation(1, len(b) == 0, chunk)
	}

	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: false,
		header: http.Header{
			":status":        []string{"431"},
			"content-type":   []string{"text/html; charset=utf-8"},
			"content-length": []string{"63"},
		},
	})
}

func TestServer_Response_Stream_With_Missing_Trailer(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Trailer", "test-trailer")
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
		})
		st.wantData(wantData{
			streamID:  1,
			endStream: true,
			size:      0,
		})
	})
}

func TestCompressionErrorOnWrite(t *testing.T) {
	const maxStrLen = 8 << 10
	var serverConfig *http.Server
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		// No response body.
	}, func(s *http.Server) {
		serverConfig = s
		serverConfig.MaxHeaderBytes = maxStrLen
	})
	st.addLogFilter("connection error: COMPRESSION_ERROR")
	defer st.Close()
	st.greet()

	maxAllowed := st.sc.framer.maxHeaderStringLen()

	// Crank this up, now that we have a conn connected with the
	// hpack.Decoder's max string length set has been initialized
	// from the earlier low ~8K value. We want this higher so don't
	// hit the max header list size. We only want to test hitting
	// the max string size.
	serverConfig.MaxHeaderBytes = 1 << 20

	// First a request with a header that's exactly the max allowed size
	// for the hpack compression. It's still too long for the header list
	// size, so we'll get the 431 error, but that keeps the compression
	// context still valid.
	hbf := st.encodeHeader("foo", strings.Repeat("a", maxAllowed))

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: hbf,
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: false,
		header: http.Header{
			":status":        []string{"431"},
			"content-type":   []string{"text/html; charset=utf-8"},
			"content-length": []string{"63"},
		},
	})
	df := readFrame[*DataFrame](t, st)
	if !strings.Contains(string(df.Data()), "HTTP Error 431") {
		t.Errorf("Unexpected data body: %q", df.Data())
	}
	if !df.StreamEnded() {
		t.Fatalf("expect data stream end")
	}

	// And now send one that's just one byte too big.
	hbf = st.encodeHeader("bar", strings.Repeat("b", maxAllowed+1))
	st.writeHeaders(HeadersFrameParam{
		StreamID:      3,
		BlockFragment: hbf,
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantGoAway(3, ErrCodeCompression)
}

func TestCompressionErrorOnClose(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		// No response body.
	})
	st.addLogFilter("connection error: COMPRESSION_ERROR")
	defer st.Close()
	st.greet()

	hbf := st.encodeHeader("foo", "bar")
	hbf = hbf[:len(hbf)-1] // truncate one byte from the end, so hpack.Decoder.Close fails.
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: hbf,
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantGoAway(1, ErrCodeCompression)
}

// test that a server handler can read trailers from a client
func TestServerReadsTrailers(t *testing.T) {
	const testBody = "some test body"
	writeReq := func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader("trailer", "Foo, Bar", "trailer", "Baz"),
			EndStream:     false,
			EndHeaders:    true,
		})
		st.writeData(1, false, []byte(testBody))
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1, // clients send odd numbers
			BlockFragment: st.encodeHeaderRaw(
				"foo", "foov",
				"bar", "barv",
				"baz", "bazv",
				"surprise", "wasn't declared; shouldn't show up",
			),
			EndStream:  true,
			EndHeaders: true,
		})
	}
	checkReq := func(r *http.Request) {
		wantTrailer := http.Header{
			"Foo": nil,
			"Bar": nil,
			"Baz": nil,
		}
		if !reflect.DeepEqual(r.Trailer, wantTrailer) {
			t.Errorf("initial Trailer = %v; want %v", r.Trailer, wantTrailer)
		}
		slurp, err := io.ReadAll(r.Body)
		if string(slurp) != testBody {
			t.Errorf("read body %q; want %q", slurp, testBody)
		}
		if err != nil {
			t.Fatalf("Body slurp: %v", err)
		}
		wantTrailerAfter := http.Header{
			"Foo": {"foov"},
			"Bar": {"barv"},
			"Baz": {"bazv"},
		}
		if !reflect.DeepEqual(r.Trailer, wantTrailerAfter) {
			t.Errorf("final Trailer = %v; want %v", r.Trailer, wantTrailerAfter)
		}
	}
	testServerRequest(t, writeReq, checkReq)
}

// test that a server handler can send trailers
func TestServerWritesTrailers_WithFlush(t *testing.T)    { testServerWritesTrailers(t, true) }
func TestServerWritesTrailers_WithoutFlush(t *testing.T) { testServerWritesTrailers(t, false) }

func testServerWritesTrailers(t *testing.T, withFlush bool) {
	// See https://httpwg.github.io/specs/rfc7540.html#rfc.section.8.1.3
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Trailer", "Server-Trailer-A, Server-Trailer-B")
		w.Header().Add("Trailer", "Server-Trailer-C")
		w.Header().Add("Trailer", "Transfer-Encoding, Content-Length, Trailer") // filtered

		// Regular headers:
		w.Header().Set("Foo", "Bar")
		w.Header().Set("Content-Length", "5") // len("Hello")

		io.WriteString(w, "Hello")
		if withFlush {
			w.(http.Flusher).Flush()
		}
		w.Header().Set("Server-Trailer-A", "valuea")
		w.Header().Set("Server-Trailer-C", "valuec") // skipping B
		// After a flush, random keys like Server-Surprise shouldn't show up:
		w.Header().Set("Server-Surpise", "surprise! this isn't predeclared!")
		// But we do permit promoting keys to trailers after a
		// flush if they start with the magic
		// otherwise-invalid "Trailer:" prefix:
		w.Header().Set("Trailer:Post-Header-Trailer", "hi1")
		w.Header().Set("Trailer:post-header-trailer2", "hi2")
		w.Header().Set("Trailer:Range", "invalid")
		w.Header().Set("Trailer:Foo\x01Bogus", "invalid")
		w.Header().Set("Transfer-Encoding", "should not be included; Forbidden by RFC 7230 4.1.2")
		w.Header().Set("Content-Length", "should not be included; Forbidden by RFC 7230 4.1.2")
		w.Header().Set("Trailer", "should not be included; Forbidden by RFC 7230 4.1.2")
		return nil
	}, func(st *serverTester) {
		// Ignore errors from writing invalid trailers.
		st.h1server.ErrorLog = log.New(io.Discard, "", 0)
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status": []string{"200"},
				"foo":     []string{"Bar"},
				"trailer": []string{
					"Server-Trailer-A, Server-Trailer-B",
					"Server-Trailer-C",
					"Transfer-Encoding, Content-Length, Trailer",
				},
				"content-type":   []string{"text/plain; charset=utf-8"},
				"content-length": []string{"5"},
			},
		})
		st.wantData(wantData{
			streamID:  1,
			endStream: false,
			data:      []byte("Hello"),
		})
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: true,
			header: http.Header{
				"post-header-trailer":  []string{"hi1"},
				"post-header-trailer2": []string{"hi2"},
				"server-trailer-a":     []string{"valuea"},
				"server-trailer-c":     []string{"valuec"},
			},
		})
	})
}

func TestServerWritesUndeclaredTrailers(t *testing.T) {
	const trailer = "Trailer-Header"
	const value = "hi1"
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(http.TrailerPrefix+trailer, value)
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	cl := &http.Client{Transport: tr}
	resp, err := cl.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got, want := resp.Trailer.Get(trailer), value; got != want {
		t.Errorf("trailer %v = %q, want %q", trailer, got, want)
	}
}

// validate transmitted header field names & values
// golang.org/issue/14048
func TestServerDoesntWriteInvalidHeaders(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Add("OK1", "x")
		w.Header().Add("Bad:Colon", "x") // colon (non-token byte) in key
		w.Header().Add("Bad1\x00", "x")  // null in key
		w.Header().Add("Bad2", "x\x00y") // null in value
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: true,
			header: http.Header{
				":status":        []string{"200"},
				"ok1":            []string{"x"},
				"content-length": []string{"0"},
			},
		})
	})
}

func BenchmarkServerGets(b *testing.B) {
	disableGoroutineTracking(b)
	b.ReportAllocs()

	const msg = "Hello, world"
	st := newServerTesterWithRealConn(b, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, msg)
	})
	defer st.Close()
	st.greet()

	// Give the server quota to reply. (plus it has the 64KB)
	if err := st.fr.WriteWindowUpdate(0, uint32(b.N*len(msg))); err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		id := 1 + uint32(i)*2
		st.writeHeaders(HeadersFrameParam{
			StreamID:      id,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
		st.wantFrameType(FrameHeaders)
		if df := readFrame[*DataFrame](b, st); !df.StreamEnded() {
			b.Fatalf("DATA didn't have END_STREAM; got %v", df)
		}
	}
}

func BenchmarkServerPosts(b *testing.B) {
	disableGoroutineTracking(b)
	b.ReportAllocs()

	const msg = "Hello, world"
	st := newServerTesterWithRealConn(b, func(w http.ResponseWriter, r *http.Request) {
		// Consume the (empty) body from th peer before replying, otherwise
		// the server will sometimes (depending on scheduling) send the peer a
		// a RST_STREAM with the CANCEL error code.
		if n, err := io.Copy(io.Discard, r.Body); n != 0 || err != nil {
			b.Errorf("Copy error; got %v, %v; want 0, nil", n, err)
		}
		io.WriteString(w, msg)
	})
	defer st.Close()
	st.greet()

	// Give the server quota to reply. (plus it has the 64KB)
	if err := st.fr.WriteWindowUpdate(0, uint32(b.N*len(msg))); err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		id := 1 + uint32(i)*2
		st.writeHeaders(HeadersFrameParam{
			StreamID:      id,
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     false,
			EndHeaders:    true,
		})
		st.writeData(id, true, nil)
		st.wantFrameType(FrameHeaders)
		if df := readFrame[*DataFrame](b, st); !df.StreamEnded() {
			b.Fatalf("DATA didn't have END_STREAM; got %v", df)
		}
	}
}

// Send a stream of messages from server to client in separate data frames.
// Brings up performance issues seen in long streams.
// Created to show problem in go issue #18502
func BenchmarkServerToClientStreamDefaultOptions(b *testing.B) {
	benchmarkServerToClientStream(b)
}

// Justification for Change-Id: Iad93420ef6c3918f54249d867098f1dadfa324d8
// Expect to see memory/alloc reduction by opting in to Frame reuse with the Framer.
func BenchmarkServerToClientStreamReuseFrames(b *testing.B) {
	benchmarkServerToClientStream(b, optFramerReuseFrames)
}

func benchmarkServerToClientStream(b *testing.B, newServerOpts ...interface{}) {
	disableGoroutineTracking(b)
	b.ReportAllocs()
	const msgLen = 1
	// default window size
	const windowSize = 1<<16 - 1

	// next message to send from the server and for the client to expect
	nextMsg := func(i int) []byte {
		msg := make([]byte, msgLen)
		msg[0] = byte(i)
		if len(msg) != msgLen {
			panic("invalid test setup msg length")
		}
		return msg
	}

	st := newServerTesterWithRealConn(b, func(w http.ResponseWriter, r *http.Request) {
		// Consume the (empty) body from th peer before replying, otherwise
		// the server will sometimes (depending on scheduling) send the peer a
		// a RST_STREAM with the CANCEL error code.
		if n, err := io.Copy(io.Discard, r.Body); n != 0 || err != nil {
			b.Errorf("Copy error; got %v, %v; want 0, nil", n, err)
		}
		for i := 0; i < b.N; i += 1 {
			w.Write(nextMsg(i))
			w.(http.Flusher).Flush()
		}
	}, newServerOpts...)
	defer st.Close()
	st.greet()

	const id = uint32(1)

	st.writeHeaders(HeadersFrameParam{
		StreamID:      id,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false,
		EndHeaders:    true,
	})

	st.writeData(id, true, nil)
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: false,
	})

	var pendingWindowUpdate = uint32(0)

	for i := 0; i < b.N; i += 1 {
		expected := nextMsg(i)
		st.wantData(wantData{
			streamID:  1,
			endStream: false,
			data:      expected,
		})
		// try to send infrequent but large window updates so they don't overwhelm the test
		pendingWindowUpdate += uint32(len(expected))
		if pendingWindowUpdate >= windowSize/2 {
			if err := st.fr.WriteWindowUpdate(0, pendingWindowUpdate); err != nil {
				b.Fatal(err)
			}
			if err := st.fr.WriteWindowUpdate(id, pendingWindowUpdate); err != nil {
				b.Fatal(err)
			}
			pendingWindowUpdate = 0
		}
	}
	st.wantData(wantData{
		streamID:  1,
		endStream: true,
	})
}

// go-fuzz bug, originally reported at https://github.com/bradfitz/http2/issues/53
// Verify we don't hang.
func TestIssue53(t *testing.T) {
	const data = "PRI * HTTP/2.0\r\n\r\nSM" +
		"\r\n\r\n\x00\x00\x00\x01\ainfinfin\ad"
	s := &http.Server{
		ErrorLog: log.New(io.MultiWriter(stderrv(), twriter{t: t}), "", log.LstdFlags),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Write([]byte("hello"))
		}),
	}
	s2 := &Server{
		MaxReadFrameSize:             1 << 16,
		PermitProhibitedCipherSuites: true,
	}
	c := &issue53Conn{[]byte(data), false, false}
	s2.ServeConn(c, &ServeConnOpts{BaseConfig: s})
	if !c.closed {
		t.Fatal("connection is not closed")
	}
}

type issue53Conn struct {
	data    []byte
	closed  bool
	written bool
}

func (c *issue53Conn) Read(b []byte) (n int, err error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n = copy(b, c.data)
	c.data = c.data[n:]
	return
}

func (c *issue53Conn) Write(b []byte) (n int, err error) {
	c.written = true
	return len(b), nil
}

func (c *issue53Conn) Close() error {
	c.closed = true
	return nil
}

func (c *issue53Conn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49706}
}
func (c *issue53Conn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49706}
}
func (c *issue53Conn) SetDeadline(t time.Time) error      { return nil }
func (c *issue53Conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *issue53Conn) SetWriteDeadline(t time.Time) error { return nil }

// golang.org/issue/33839
func TestServeConnOptsNilReceiverBehavior(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("got a panic that should not happen: %v", r)
		}
	}()

	var o *ServeConnOpts
	if o.context() == nil {
		t.Error("o.context should not return nil")
	}
	if o.baseConfig() == nil {
		t.Error("o.baseConfig should not return nil")
	}
	if o.handler() == nil {
		t.Error("o.handler should not return nil")
	}
}

// golang.org/issue/12895
func TestConfigureServer(t *testing.T) {
	tests := []struct {
		name      string
		tlsConfig *tls.Config
		wantErr   string
	}{
		{
			name: "empty server",
		},
		{
			name:      "empty CipherSuites",
			tlsConfig: &tls.Config{},
		},
		{
			name: "bad CipherSuites but MinVersion TLS 1.3",
			tlsConfig: &tls.Config{
				MinVersion:   tls.VersionTLS13,
				CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384},
			},
		},
		{
			name: "just the required cipher suite",
			tlsConfig: &tls.Config{
				CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			},
		},
		{
			name: "just the alternative required cipher suite",
			tlsConfig: &tls.Config{
				CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			},
		},
		{
			name: "missing required cipher suite",
			tlsConfig: &tls.Config{
				CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384},
			},
			wantErr: "is missing an HTTP/2-required",
		},
		{
			name: "required after bad",
			tlsConfig: &tls.Config{
				CipherSuites: []uint16{tls.TLS_RSA_WITH_RC4_128_SHA, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			},
		},
		{
			name: "bad after required",
			tlsConfig: &tls.Config{
				CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_RSA_WITH_RC4_128_SHA},
			},
		},
	}
	for _, tt := range tests {
		srv := &http.Server{TLSConfig: tt.tlsConfig}
		err := ConfigureServer(srv, nil)
		if (err != nil) != (tt.wantErr != "") {
			if tt.wantErr != "" {
				t.Errorf("%s: success, but want error", tt.name)
			} else {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
		}
		if err != nil && tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v; want substring %q", tt.name, err, tt.wantErr)
		}
		if err == nil && !srv.TLSConfig.PreferServerCipherSuites {
			t.Errorf("%s: PreferServerCipherSuite is false; want true", tt.name)
		}
	}
}

func TestServerNoAutoContentLengthOnHead(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		// No response body. (or smaller than one frame)
	})
	defer st.Close()
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(":method", "HEAD"),
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
		header: http.Header{
			":status": []string{"200"},
		},
	})
}

// golang.org/issue/13495
func TestServerNoDuplicateContentType(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = []string{""}
		fmt.Fprintf(w, "<html><head></head><body>hi</body></html>")
	})
	defer st.Close()
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: false,
		header: http.Header{
			":status":        []string{"200"},
			"content-type":   []string{""},
			"content-length": []string{"41"},
		},
	})
}

func TestServerContentLengthCanBeDisabled(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Length"] = nil
		fmt.Fprintf(w, "OK")
	})
	defer st.Close()
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: false,
		header: http.Header{
			":status":      []string{"200"},
			"content-type": []string{"text/plain; charset=utf-8"},
		},
	})
}

func disableGoroutineTracking(t testing.TB) {
	old := DebugGoroutines
	DebugGoroutines = false
	t.Cleanup(func() { DebugGoroutines = old })
}

func BenchmarkServer_GetRequest(b *testing.B) {
	disableGoroutineTracking(b)
	b.ReportAllocs()
	const msg = "Hello, world."
	st := newServerTesterWithRealConn(b, func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n > 0 {
			b.Errorf("Read %d bytes, error %v; want 0 bytes.", n, err)
		}
		io.WriteString(w, msg)
	})
	defer st.Close()

	st.greet()
	// Give the server quota to reply. (plus it has the 64KB)
	if err := st.fr.WriteWindowUpdate(0, uint32(b.N*len(msg))); err != nil {
		b.Fatal(err)
	}
	hbf := st.encodeHeader(":method", "GET")
	for i := 0; i < b.N; i++ {
		streamID := uint32(1 + 2*i)
		st.writeHeaders(HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: hbf,
			EndStream:     true,
			EndHeaders:    true,
		})
		st.wantFrameType(FrameHeaders)
		st.wantFrameType(FrameData)
	}
}

func BenchmarkServer_PostRequest(b *testing.B) {
	disableGoroutineTracking(b)
	b.ReportAllocs()
	const msg = "Hello, world."
	st := newServerTesterWithRealConn(b, func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n > 0 {
			b.Errorf("Read %d bytes, error %v; want 0 bytes.", n, err)
		}
		io.WriteString(w, msg)
	})
	defer st.Close()
	st.greet()
	// Give the server quota to reply. (plus it has the 64KB)
	if err := st.fr.WriteWindowUpdate(0, uint32(b.N*len(msg))); err != nil {
		b.Fatal(err)
	}
	hbf := st.encodeHeader(":method", "POST")
	for i := 0; i < b.N; i++ {
		streamID := uint32(1 + 2*i)
		st.writeHeaders(HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: hbf,
			EndStream:     false,
			EndHeaders:    true,
		})
		st.writeData(streamID, true, nil)
		st.wantFrameType(FrameHeaders)
		st.wantFrameType(FrameData)
	}
}

type connStateConn struct {
	net.Conn
	cs tls.ConnectionState
}

func (c connStateConn) ConnectionState() tls.ConnectionState { return c.cs }

// golang.org/issue/12737 -- handle any net.Conn, not just
// *tls.Conn.
func TestServerHandleCustomConn(t *testing.T) {
	var s Server
	c1, c2 := net.Pipe()
	clientDone := make(chan struct{})
	handlerDone := make(chan struct{})
	var req *http.Request
	go func() {
		defer close(clientDone)
		defer c2.Close()
		fr := NewFramer(c2, c2)
		io.WriteString(c2, ClientPreface)
		fr.WriteSettings()
		fr.WriteSettingsAck()
		f, err := fr.ReadFrame()
		if err != nil {
			t.Error(err)
			return
		}
		if sf, ok := f.(*SettingsFrame); !ok || sf.IsAck() {
			t.Errorf("Got %v; want non-ACK SettingsFrame", summarizeFrame(f))
			return
		}
		f, err = fr.ReadFrame()
		if err != nil {
			t.Error(err)
			return
		}
		if sf, ok := f.(*SettingsFrame); !ok || !sf.IsAck() {
			t.Errorf("Got %v; want ACK SettingsFrame", summarizeFrame(f))
			return
		}
		var henc hpackEncoder
		fr.WriteHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: henc.encodeHeaderRaw(t, ":method", "GET", ":path", "/", ":scheme", "https", ":authority", "foo.com"),
			EndStream:     true,
			EndHeaders:    true,
		})
		go io.Copy(io.Discard, c2)
		<-handlerDone
	}()
	const testString = "my custom ConnectionState"
	fakeConnState := tls.ConnectionState{
		ServerName:  testString,
		Version:     tls.VersionTLS12,
		CipherSuite: cipher_TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	}
	go s.ServeConn(connStateConn{c1, fakeConnState}, &ServeConnOpts{
		BaseConfig: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				req = r
			}),
		}})
	<-clientDone

	if req.TLS == nil {
		t.Fatalf("Request.TLS is nil. Got: %#v", req)
	}
	if req.TLS.ServerName != testString {
		t.Fatalf("Request.TLS = %+v; want ServerName of %q", req.TLS, testString)
	}
}

// golang.org/issue/14214
func TestServer_Rejects_ConnHeaders(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not get to Handler")
	})
	defer st.Close()
	st.greet()
	st.bodylessReq1("connection", "foo")
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: false,
		header: http.Header{
			":status":                []string{"400"},
			"content-type":           []string{"text/plain; charset=utf-8"},
			"x-content-type-options": []string{"nosniff"},
			"content-length":         []string{"51"},
		},
	})
}

type hpackEncoder struct {
	enc *hpack.Encoder
	buf bytes.Buffer
}

func (he *hpackEncoder) encodeHeaderRaw(t *testing.T, headers ...string) []byte {
	if len(headers)%2 == 1 {
		panic("odd number of kv args")
	}
	he.buf.Reset()
	if he.enc == nil {
		he.enc = hpack.NewEncoder(&he.buf)
	}
	for len(headers) > 0 {
		k, v := headers[0], headers[1]
		err := he.enc.WriteField(hpack.HeaderField{Name: k, Value: v})
		if err != nil {
			t.Fatalf("HPACK encoding error for %q/%q: %v", k, v, err)
		}
		headers = headers[2:]
	}
	return he.buf.Bytes()
}

func TestCheckValidHTTP2Request(t *testing.T) {
	tests := []struct {
		h    http.Header
		want error
	}{
		{
			h:    http.Header{"Te": {"trailers"}},
			want: nil,
		},
		{
			h:    http.Header{"Te": {"trailers", "bogus"}},
			want: errors.New(`request header "TE" may only be "trailers" in HTTP/2`),
		},
		{
			h:    http.Header{"Foo": {""}},
			want: nil,
		},
		{
			h:    http.Header{"Connection": {""}},
			want: errors.New(`request header "Connection" is not valid in HTTP/2`),
		},
		{
			h:    http.Header{"Proxy-Connection": {""}},
			want: errors.New(`request header "Proxy-Connection" is not valid in HTTP/2`),
		},
		{
			h:    http.Header{"Keep-Alive": {""}},
			want: errors.New(`request header "Keep-Alive" is not valid in HTTP/2`),
		},
		{
			h:    http.Header{"Upgrade": {""}},
			want: errors.New(`request header "Upgrade" is not valid in HTTP/2`),
		},
	}
	for i, tt := range tests {
		got := checkValidHTTP2RequestHeaders(tt.h)
		if !equalError(got, tt.want) {
			t.Errorf("%d. checkValidHTTP2Request = %v; want %v", i, got, tt.want)
		}
	}
}

// golang.org/issue/14030
func TestExpect100ContinueAfterHandlerWrites(t *testing.T) {
	const msg = "Hello"
	const msg2 = "World"

	doRead := make(chan bool, 1)
	defer close(doRead) // fallback cleanup

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, msg)
		w.(http.Flusher).Flush()

		// Do a read, which might force a 100-continue status to be sent.
		<-doRead
		r.Body.Read(make([]byte, 10))

		io.WriteString(w, msg2)
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	req, _ := http.NewRequest("POST", ts.URL, io.LimitReader(neverEnding('A'), 2<<20))
	req.Header.Set("Expect", "100-continue")

	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(res.Body, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("msg = %q; want %q", buf, msg)
	}

	doRead <- true

	if _, err := io.ReadFull(res.Body, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg2 {
		t.Fatalf("second msg = %q; want %q", buf, msg2)
	}
}

type funcReader func([]byte) (n int, err error)

func (f funcReader) Read(p []byte) (n int, err error) { return f(p) }

// golang.org/issue/16481 -- return flow control when streams close with unread data.
// (The Server version of the bug. See also TestUnreadFlowControlReturned_Transport)
func TestUnreadFlowControlReturned_Server(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reqFn func(r *http.Request)
	}{
		{
			"body-open",
			func(r *http.Request) {},
		},
		{
			"body-closed",
			func(r *http.Request) {
				r.Body.Close()
			},
		},
		{
			"read-1-byte-and-close",
			func(r *http.Request) {
				b := make([]byte, 1)
				r.Body.Read(b)
				r.Body.Close()
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unblock := make(chan bool, 1)
			defer close(unblock)

			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				// Don't read the 16KB request body. Wait until the client's
				// done sending it and then return. This should cause the Server
				// to then return those 16KB of flow control to the client.
				tt.reqFn(r)
				<-unblock
			})

			tr := &Transport{TLSClientConfig: tlsConfigInsecure}
			defer tr.CloseIdleConnections()

			// This previously hung on the 4th iteration.
			iters := 100
			if testing.Short() {
				iters = 20
			}
			for i := 0; i < iters; i++ {
				body := io.MultiReader(
					io.LimitReader(neverEnding('A'), 16<<10),
					funcReader(func([]byte) (n int, err error) {
						unblock <- true
						return 0, io.EOF
					}),
				)
				req, _ := http.NewRequest("POST", ts.URL, body)
				res, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatal(tt.name, err)
				}
				res.Body.Close()
			}
		})
	}
}

func TestServerReturnsStreamAndConnFlowControlOnBodyClose(t *testing.T) {
	unblockHandler := make(chan struct{})
	defer close(unblockHandler)

	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		r.Body.Close()
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-unblockHandler
	})
	defer st.Close()

	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndHeaders:    true,
	})
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: false,
	})
	const size = inflowMinRefresh // enough to trigger flow control return
	st.writeData(1, false, make([]byte, size))
	st.wantWindowUpdate(0, size) // conn-level flow control is returned
	unblockHandler <- struct{}{}
	st.wantData(wantData{
		streamID:  1,
		endStream: true,
	})
}

func TestServerIdleTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
	}, func(h2s *Server) {
		h2s.IdleTimeout = 500 * time.Millisecond
	})
	defer st.Close()

	st.greet()
	st.advance(500 * time.Millisecond)
	st.wantGoAway(0, ErrCodeNo)
}

func TestServerIdleTimeout_AfterRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	const (
		requestTimeout = 2 * time.Second
		idleTimeout    = 1 * time.Second
	)

	var st *serverTester
	st = newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		st.group.Sleep(requestTimeout)
	}, func(h2s *Server) {
		h2s.IdleTimeout = idleTimeout
	})
	defer st.Close()

	st.greet()

	// Send a request which takes twice the timeout. Verifies the
	// idle timeout doesn't fire while we're in a request:
	st.bodylessReq1()
	st.advance(requestTimeout)
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})

	// But the idle timeout should be rearmed after the request
	// is done:
	st.advance(idleTimeout)
	st.wantGoAway(1, ErrCodeNo)
}

// grpc-go closes the Request.Body currently with a Read.
// Verify that it doesn't race.
// See https://github.com/grpc/grpc-go/pull/938
func TestRequestBodyReadCloseRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		body := &requestBody{
			pipe: &pipe{
				b: new(bytes.Buffer),
			},
		}
		body.pipe.CloseWithError(io.EOF)

		done := make(chan bool, 1)
		buf := make([]byte, 10)
		go func() {
			time.Sleep(1 * time.Millisecond)
			body.Close()
			done <- true
		}()
		body.Read(buf)
		<-done
	}
}

func TestIssue20704Race(t *testing.T) {
	if testing.Short() && os.Getenv("GO_BUILDER_NAME") == "" {
		t.Skip("skipping in short mode")
	}
	const (
		itemSize  = 1 << 10
		itemCount = 100
	)

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < itemCount; i++ {
			_, err := w.Write(make([]byte, itemSize))
			if err != nil {
				return
			}
		}
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr}

	for i := 0; i < 1000; i++ {
		resp, err := cl.Get(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		// Force a RST stream to the server by closing without
		// reading the body:
		resp.Body.Close()
	}
}

func TestServer_Rejects_TooSmall(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		io.ReadAll(r.Body)
		return nil
	}, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1, // clients send odd numbers
			BlockFragment: st.encodeHeader(
				":method", "POST",
				"content-length", "4",
			),
			EndStream:  false, // to say DATA frames are coming
			EndHeaders: true,
		})
		st.writeData(1, true, []byte("12345"))
		st.wantRSTStream(1, ErrCodeProtocol)
		st.wantFlowControlConsumed(0, 0)
	})
}

// Tests that a handler setting "Connection: close" results in a GOAWAY being sent,
// and the connection still completing.
func TestServerHandlerConnectionClose(t *testing.T) {
	unblockHandler := make(chan bool, 1)
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Connection", "close")
		w.Header().Set("Foo", "bar")
		w.(http.Flusher).Flush()
		<-unblockHandler
		return nil
	}, func(st *serverTester) {
		defer close(unblockHandler) // backup; in case of errors
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
		var sawGoAway bool
		var sawRes bool
		var sawWindowUpdate bool
		for {
			f := st.readFrame()
			if f == nil {
				break
			}
			switch f := f.(type) {
			case *GoAwayFrame:
				sawGoAway = true
				if f.LastStreamID != 1 || f.ErrCode != ErrCodeNo {
					t.Errorf("unexpected GOAWAY frame: %v", summarizeFrame(f))
				}
				// Create a stream and reset it.
				// The server should ignore the stream.
				st.writeHeaders(HeadersFrameParam{
					StreamID:      3,
					BlockFragment: st.encodeHeader(),
					EndStream:     false,
					EndHeaders:    true,
				})
				st.fr.WriteRSTStream(3, ErrCodeCancel)
				// Create a stream and send data to it.
				// The server should return flow control, even though it
				// does not process the stream.
				st.writeHeaders(HeadersFrameParam{
					StreamID:      5,
					BlockFragment: st.encodeHeader(),
					EndStream:     false,
					EndHeaders:    true,
				})
				// Write enough data to trigger a window update.
				st.writeData(5, true, make([]byte, 1<<19))
			case *HeadersFrame:
				goth := st.decodeHeader(f.HeaderBlockFragment())
				wanth := [][2]string{
					{":status", "200"},
					{"foo", "bar"},
				}
				if !reflect.DeepEqual(goth, wanth) {
					t.Errorf("got headers %v; want %v", goth, wanth)
				}
				sawRes = true
			case *DataFrame:
				if f.StreamID != 1 || !f.StreamEnded() || len(f.Data()) != 0 {
					t.Errorf("unexpected DATA frame: %v", summarizeFrame(f))
				}
			case *WindowUpdateFrame:
				if !sawGoAway {
					t.Errorf("unexpected WINDOW_UPDATE frame: %v", summarizeFrame(f))
					return
				}
				if f.StreamID != 0 {
					st.t.Fatalf("WindowUpdate StreamID = %d; want 5", f.FrameHeader.StreamID)
					return
				}
				sawWindowUpdate = true
				unblockHandler <- true
				st.sync()
				st.advance(goAwayTimeout)
			default:
				t.Logf("unexpected frame: %v", summarizeFrame(f))
			}
		}
		if !sawGoAway {
			t.Errorf("didn't see GOAWAY")
		}
		if !sawRes {
			t.Errorf("didn't see response")
		}
		if !sawWindowUpdate {
			t.Errorf("didn't see WINDOW_UPDATE")
		}
	})
}

func TestServer_Headers_HalfCloseRemote(t *testing.T) {
	var st *serverTester
	writeData := make(chan bool)
	writeHeaders := make(chan bool)
	leaveHandler := make(chan bool)
	st = newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		if st.stream(1) == nil {
			t.Errorf("nil stream 1 in handler")
		}
		if got, want := st.streamState(1), stateOpen; got != want {
			t.Errorf("in handler, state is %v; want %v", got, want)
		}
		writeData <- true
		if n, err := r.Body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Errorf("body read = %d, %v; want 0, EOF", n, err)
		}
		if got, want := st.streamState(1), stateHalfClosedRemote; got != want {
			t.Errorf("in handler, state is %v; want %v", got, want)
		}
		writeHeaders <- true

		<-leaveHandler
	})
	st.greet()

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-writeData
	st.writeData(1, true, nil)

	<-writeHeaders

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})

	defer close(leaveHandler)

	st.wantRSTStream(1, ErrCodeStreamClosed)
}

func TestServerGracefulShutdown(t *testing.T) {
	handlerDone := make(chan struct{})
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		<-handlerDone
		w.Header().Set("x-foo", "bar")
	})
	defer st.Close()

	st.greet()
	st.bodylessReq1()

	st.sync()
	st.h1server.Shutdown(context.Background())

	st.wantGoAway(1, ErrCodeNo)

	close(handlerDone)
	st.sync()

	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
		header: http.Header{
			":status":        []string{"200"},
			"x-foo":          []string{"bar"},
			"content-length": []string{"0"},
		},
	})

	n, err := st.cc.Read([]byte{0})
	if n != 0 || err == nil {
		t.Errorf("Read = %v, %v; want 0, non-nil", n, err)
	}
}

// Issue 31753: don't sniff when Content-Encoding is set
func TestContentEncodingNoSniffing(t *testing.T) {
	type resp struct {
		name string
		body []byte
		// setting Content-Encoding as an interface instead of a string
		// directly, so as to differentiate between 3 states:
		//    unset, empty string "" and set string "foo/bar".
		contentEncoding interface{}
		wantContentType string
	}

	resps := []*resp{
		{
			name:            "gzip content-encoding, gzipped", // don't sniff.
			contentEncoding: "application/gzip",
			wantContentType: "",
			body: func() []byte {
				buf := new(bytes.Buffer)
				gzw := gzip.NewWriter(buf)
				gzw.Write([]byte("doctype html><p>Hello</p>"))
				gzw.Close()
				return buf.Bytes()
			}(),
		},
		{
			name:            "zlib content-encoding, zlibbed", // don't sniff.
			contentEncoding: "application/zlib",
			wantContentType: "",
			body: func() []byte {
				buf := new(bytes.Buffer)
				zw := zlib.NewWriter(buf)
				zw.Write([]byte("doctype html><p>Hello</p>"))
				zw.Close()
				return buf.Bytes()
			}(),
		},
		{
			name:            "no content-encoding", // must sniff.
			wantContentType: "application/x-gzip",
			body: func() []byte {
				buf := new(bytes.Buffer)
				gzw := gzip.NewWriter(buf)
				gzw.Write([]byte("doctype html><p>Hello</p>"))
				gzw.Close()
				return buf.Bytes()
			}(),
		},
		{
			name:            "phony content-encoding", // don't sniff.
			contentEncoding: "foo/bar",
			body:            []byte("doctype html><p>Hello</p>"),
		},
		{
			name:            "empty but set content-encoding",
			contentEncoding: "",
			wantContentType: "audio/mpeg",
			body:            []byte("ID3"),
		},
	}

	for _, tt := range resps {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.contentEncoding != nil {
					w.Header().Set("Content-Encoding", tt.contentEncoding.(string))
				}
				w.Write(tt.body)
			})

			tr := &Transport{TLSClientConfig: tlsConfigInsecure}
			defer tr.CloseIdleConnections()

			req, _ := http.NewRequest("GET", ts.URL, nil)
			res, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("GET %s: %v", ts.URL, err)
			}
			defer res.Body.Close()

			g := res.Header.Get("Content-Encoding")
			t.Logf("%s: Content-Encoding: %s", ts.URL, g)

			if w := tt.contentEncoding; g != w {
				if w != nil { // The case where contentEncoding was set explicitly.
					t.Errorf("Content-Encoding mismatch\n\tgot:  %q\n\twant: %q", g, w)
				} else if g != "" { // "" should be the equivalent when the contentEncoding is unset.
					t.Errorf("Unexpected Content-Encoding %q", g)
				}
			}

			g = res.Header.Get("Content-Type")
			if w := tt.wantContentType; g != w {
				t.Errorf("Content-Type mismatch\n\tgot:  %q\n\twant: %q", g, w)
			}
			t.Logf("%s: Content-Type: %s", ts.URL, g)
		})
	}
}

func TestServerWindowUpdateOnBodyClose(t *testing.T) {
	const windowSize = 65535 * 2
	content := make([]byte, windowSize)
	blockCh := make(chan bool)
	errc := make(chan error, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4)
		n, err := io.ReadFull(r.Body, buf)
		if err != nil {
			errc <- err
			return
		}
		if n != len(buf) {
			errc <- fmt.Errorf("too few bytes read: %d", n)
			return
		}
		blockCh <- true
		<-blockCh
		errc <- nil
	}, func(s *Server) {
		s.MaxUploadBufferPerConnection = windowSize
		s.MaxUploadBufferPerStream = windowSize
	})
	defer st.Close()

	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID: 1, // clients send odd numbers
		BlockFragment: st.encodeHeader(
			":method", "POST",
			"content-length", strconv.Itoa(len(content)),
		),
		EndStream:  false, // to say DATA frames are coming
		EndHeaders: true,
	})
	st.writeData(1, false, content[:windowSize/2])
	<-blockCh
	st.stream(1).body.CloseWithError(io.EOF)
	blockCh <- true

	// Wait for flow control credit for the portion of the request written so far.
	increments := windowSize / 2
	for {
		f := st.readFrame()
		if f == nil {
			break
		}
		if wu, ok := f.(*WindowUpdateFrame); ok && wu.StreamID == 0 {
			increments -= int(wu.Increment)
			if increments == 0 {
				break
			}
		}
	}

	// Writing data after the stream is reset immediately returns flow control credit.
	st.writeData(1, false, content[windowSize/2:])
	st.wantWindowUpdate(0, windowSize/2)

	if err := <-errc; err != nil {
		t.Error(err)
	}
}

func TestNoErrorLoggedOnPostAfterGOAWAY(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {})
	defer st.Close()

	st.greet()

	content := "some content"
	st.writeHeaders(HeadersFrameParam{
		StreamID: 1,
		BlockFragment: st.encodeHeader(
			":method", "POST",
			"content-length", strconv.Itoa(len(content)),
		),
		EndStream:  false,
		EndHeaders: true,
	})
	st.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})

	st.sc.startGracefulShutdown()
	st.wantRSTStream(1, ErrCodeNo)
	st.wantGoAway(1, ErrCodeNo)

	st.writeData(1, true, []byte(content))
	st.Close()

	if bytes.Contains(st.serverLogBuf.Bytes(), []byte("PROTOCOL_ERROR")) {
		t.Error("got protocol error")
	}
}

func TestServerSendsProcessing(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusProcessing)
		w.Write([]byte("stuff"))

		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status": []string{"102"},
			},
		})
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status":        []string{"200"},
				"content-type":   []string{"text/plain; charset=utf-8"},
				"content-length": []string{"5"},
			},
		})
	})
}

func TestServerSendsEarlyHints(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		h := w.Header()
		h.Add("Content-Length", "123")
		h.Add("Link", "</style.css>; rel=preload; as=style")
		h.Add("Link", "</script.js>; rel=preload; as=script")
		w.WriteHeader(http.StatusEarlyHints)

		h.Add("Link", "</foo.js>; rel=preload; as=script")
		w.WriteHeader(http.StatusEarlyHints)

		w.Write([]byte("stuff"))

		return nil
	}, func(st *serverTester) {
		getSlash(st)
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status": []string{"103"},
				"link": []string{
					"</style.css>; rel=preload; as=style",
					"</script.js>; rel=preload; as=script",
				},
			},
		})
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status": []string{"103"},
				"link": []string{
					"</style.css>; rel=preload; as=style",
					"</script.js>; rel=preload; as=script",
					"</foo.js>; rel=preload; as=script",
				},
			},
		})
		st.wantHeaders(wantHeader{
			streamID:  1,
			endStream: false,
			header: http.Header{
				":status": []string{"200"},
				"link": []string{
					"</style.css>; rel=preload; as=style",
					"</script.js>; rel=preload; as=script",
					"</foo.js>; rel=preload; as=script",
				},
				"content-type":   []string{"text/plain; charset=utf-8"},
				"content-length": []string{"123"},
			},
		})
	})
}

func TestProtocolErrorAfterGoAway(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	})
	defer st.Close()

	st.greet()
	content := "some content"
	st.writeHeaders(HeadersFrameParam{
		StreamID: 1,
		BlockFragment: st.encodeHeader(
			":method", "POST",
			"content-length", strconv.Itoa(len(content)),
		),
		EndStream:  false,
		EndHeaders: true,
	})
	st.writeData(1, false, []byte(content[:5]))

	// Send a GOAWAY with ErrCodeNo, followed by a bogus window update.
	// The server should close the connection.
	if err := st.fr.WriteGoAway(1, ErrCodeNo, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.fr.WriteWindowUpdate(0, 1<<31-1); err != nil {
		t.Fatal(err)
	}

	st.advance(goAwayTimeout)
	st.wantGoAway(1, ErrCodeNo)
	st.wantClosed()
}

func TestServerInitialFlowControlWindow(t *testing.T) {
	for _, want := range []int32{
		65535,
		1 << 19,
		1 << 21,
		// For MaxUploadBufferPerConnection values in the range
		// (65535, 65535*2), we don't send an initial WINDOW_UPDATE
		// because we only send flow control when the window drops
		// below half of the maximum. Perhaps it would be nice to
		// test this case, but we currently do not.
		65535 * 2,
	} {
		t.Run(fmt.Sprint(want), func(t *testing.T) {

			st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
			}, func(s *Server) {
				s.MaxUploadBufferPerConnection = want
			})
			st.writePreface()
			st.writeSettings()
			_ = readFrame[*SettingsFrame](t, st)
			st.writeSettingsAck()
			st.writeHeaders(HeadersFrameParam{
				StreamID:      1,
				BlockFragment: st.encodeHeader(),
				EndStream:     true,
				EndHeaders:    true,
			})
			window := 65535
		Frames:
			for {
				f := st.readFrame()
				switch f := f.(type) {
				case *WindowUpdateFrame:
					if f.FrameHeader.StreamID != 0 {
						t.Errorf("WindowUpdate StreamID = %d; want 0", f.FrameHeader.StreamID)
						return
					}
					window += int(f.Increment)
				case *HeadersFrame:
					break Frames
				case nil:
					break Frames
				default:
				}
			}
			if window != int(want) {
				t.Errorf("got initial flow control window = %v, want %v", window, want)
			}
		})
	}
}

// TestCanonicalHeaderCacheGrowth verifies that the canonical header cache
// size is capped to a reasonable level.
func TestCanonicalHeaderCacheGrowth(t *testing.T) {
	for _, size := range []int{1, (1 << 20) - 10} {
		base := strings.Repeat("X", size)
		sc := &serverConn{
			serveG: newGoroutineLock(),
		}
		count := 0
		added := 0
		for added < 10*maxCachedCanonicalHeadersKeysSize {
			h := fmt.Sprintf("%v-%v", base, count)
			c := sc.canonicalHeader(h)
			if len(h) != len(c) {
				t.Errorf("sc.canonicalHeader(%q) = %q, want same length", h, c)
			}
			count++
			added += len(h)
		}
		total := 0
		for k, v := range sc.canonHeader {
			total += len(k) + len(v) + 100
		}
		if total > maxCachedCanonicalHeadersKeysSize {
			t.Errorf("after adding %v ~%v-byte headers, canonHeader cache is ~%v bytes, want <%v", count, size, total, maxCachedCanonicalHeadersKeysSize)
		}
	}
}

// TestServerWriteDoesNotRetainBufferAfterReturn checks for access to
// the slice passed to ResponseWriter.Write after Write returns.
//
// Terminating the request stream on the client causes Write to return.
// We should not access the slice after this point.
func TestServerWriteDoesNotRetainBufferAfterReturn(t *testing.T) {
	donec := make(chan struct{})
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(donec)
		buf := make([]byte, 1<<20)
		var i byte
		for {
			i++
			_, err := w.Write(buf)
			for j := range buf {
				buf[j] = byte(i) // trigger race detector
			}
			if err != nil {
				return
			}
		}
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	req, _ := http.NewRequest("GET", ts.URL, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	<-donec
}

// TestServerWriteDoesNotRetainBufferAfterServerClose checks for access to
// the slice passed to ResponseWriter.Write after Write returns.
//
// Shutting down the Server causes Write to return.
// We should not access the slice after this point.
func TestServerWriteDoesNotRetainBufferAfterServerClose(t *testing.T) {
	donec := make(chan struct{}, 1)
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		donec <- struct{}{}
		defer close(donec)
		buf := make([]byte, 1<<20)
		var i byte
		for {
			i++
			_, err := w.Write(buf)
			for j := range buf {
				buf[j] = byte(i)
			}
			if err != nil {
				return
			}
		}
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	req, _ := http.NewRequest("GET", ts.URL, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	<-donec
	ts.Config.Close()
	<-donec
}

func TestServerMaxHandlerGoroutines(t *testing.T) {
	const maxHandlers = 10
	handlerc := make(chan chan bool)
	donec := make(chan struct{})
	defer close(donec)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		stopc := make(chan bool, 1)
		select {
		case handlerc <- stopc:
		case <-donec:
		}
		select {
		case shouldPanic := <-stopc:
			if shouldPanic {
				panic(http.ErrAbortHandler)
			}
		case <-donec:
		}
	}, func(s *Server) {
		s.MaxConcurrentStreams = maxHandlers
	})
	defer st.Close()

	st.greet()

	// Make maxHandlers concurrent requests.
	// Reset them all, but only after the handler goroutines have started.
	var stops []chan bool
	streamID := uint32(1)
	for i := 0; i < maxHandlers; i++ {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
		stops = append(stops, <-handlerc)
		st.fr.WriteRSTStream(streamID, ErrCodeCancel)
		streamID += 2
	}

	// Start another request, and immediately reset it.
	st.writeHeaders(HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	st.fr.WriteRSTStream(streamID, ErrCodeCancel)
	streamID += 2

	// Start another two requests. Don't reset these.
	for i := 0; i < 2; i++ {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
		streamID += 2
	}

	// The initial maxHandlers handlers are still executing,
	// so the last two requests don't start any new handlers.
	select {
	case <-handlerc:
		t.Errorf("handler unexpectedly started while maxHandlers are already running")
	case <-time.After(1 * time.Millisecond):
	}

	// Tell two handlers to exit.
	// The pending requests which weren't reset start handlers.
	stops[0] <- false // normal exit
	stops[1] <- true  // panic
	stops = stops[2:]
	stops = append(stops, <-handlerc)
	stops = append(stops, <-handlerc)

	// Make a bunch more requests.
	// Eventually, the server tells us to go away.
	for i := 0; i < 5*maxHandlers; i++ {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
		st.fr.WriteRSTStream(streamID, ErrCodeCancel)
		streamID += 2
	}
	fr := readFrame[*GoAwayFrame](t, st)
	if fr.ErrCode != ErrCodeEnhanceYourCalm {
		t.Errorf("err code = %v; want %v", fr.ErrCode, ErrCodeEnhanceYourCalm)
	}

	for _, s := range stops {
		close(s)
	}
}

func TestServerContinuationFlood(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Header)
	}, func(s *http.Server) {
		s.MaxHeaderBytes = 4096
	})
	defer st.Close()

	st.greet()

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
	})
	for i := 0; i < 1000; i++ {
		st.fr.WriteContinuation(1, false, st.encodeHeaderRaw(
			fmt.Sprintf("x-%v", i), "1234567890",
		))
	}
	st.fr.WriteContinuation(1, true, st.encodeHeaderRaw(
		"x-last-header", "1",
	))

	for {
		f := st.readFrame()
		if f == nil {
			break
		}
		switch f := f.(type) {
		case *HeadersFrame:
			t.Fatalf("received HEADERS frame; want GOAWAY and a closed connection")
		case *GoAwayFrame:
			// We might not see the GOAWAY (see below), but if we do it should
			// indicate that the server processed this request so the client doesn't
			// attempt to retry it.
			if got, want := f.LastStreamID, uint32(1); got != want {
				t.Errorf("received GOAWAY with LastStreamId %v, want %v", got, want)
			}

		}
	}
	// We expect to have seen a GOAWAY before the connection closes,
	// but the server will close the connection after one second
	// whether or not it has finished sending the GOAWAY. On windows-amd64-race
	// builders, this fairly consistently results in the connection closing without
	// the GOAWAY being sent.
	//
	// Since the server's behavior is inherently racy here and the important thing
	// is that the connection is closed, don't check for the GOAWAY having been sent.
}

func TestServerContinuationAfterInvalidHeader(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Header)
	})
	defer st.Close()

	st.greet()

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
	})
	st.fr.WriteContinuation(1, false, st.encodeHeaderRaw(
		"x-invalid-header", "\x00",
	))
	st.fr.WriteContinuation(1, true, st.encodeHeaderRaw(
		"x-valid-header", "1",
	))

	var sawGoAway bool
	for {
		f := st.readFrame()
		if f == nil {
			break
		}
		switch f.(type) {
		case *GoAwayFrame:
			sawGoAway = true
		case *HeadersFrame:
			t.Fatalf("received HEADERS frame; want GOAWAY")
		}
	}
	if !sawGoAway {
		t.Errorf("connection closed with no GOAWAY frame; want one")
	}
}

func TestServerUpgradeRequestPrefaceFailure(t *testing.T) {
	// An h2c upgrade request fails when the client preface is not as expected.
	s2 := &Server{
		// Setting IdleTimeout triggers #67168.
		IdleTimeout: 60 * time.Minute,
	}
	c1, c2 := net.Pipe()
	donec := make(chan struct{})
	go func() {
		defer close(donec)
		s2.ServeConn(c1, &ServeConnOpts{
			UpgradeRequest: httptest.NewRequest("GET", "/", nil),
		})
	}()
	// The server expects to see the HTTP/2 preface,
	// but we close the connection instead.
	c2.Close()
	<-donec
}

// Issue 67036: A stream error should result in the handler's request context being canceled.
func TestServerRequestCancelOnError(t *testing.T) {
	recvc := make(chan struct{}) // handler has started
	donec := make(chan struct{}) // handler has finished
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		close(recvc)
		<-r.Context().Done()
		close(donec)
	})
	defer st.Close()

	st.greet()

	// Client sends request headers, handler starts.
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	<-recvc

	// Client sends an invalid second set of request headers.
	// The stream is reset.
	// The handler's context is canceled, and the handler exits.
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	<-donec
}

func TestServerSetReadWriteDeadlineRace(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		ctl := http.NewResponseController(w)
		ctl.SetReadDeadline(time.Now().Add(3600 * time.Second))
		ctl.SetWriteDeadline(time.Now().Add(3600 * time.Second))
	})
	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestServerWriteByteTimeout(t *testing.T) {
	const timeout = 1 * time.Second
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 100))
	}, func(s *Server) {
		s.WriteByteTimeout = timeout
	})
	st.greet()

	st.cc.(*synctestNetConn).SetReadBufferSize(1) // write one byte at a time
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})

	// Read a few bytes, staying just under WriteByteTimeout.
	for i := 0; i < 10; i++ {
		st.advance(timeout - 1)
		if n, err := st.cc.Read(make([]byte, 1)); n != 1 || err != nil {
			t.Fatalf("read %v: %v, %v; want 1, nil", i, n, err)
		}
	}

	// Wait for WriteByteTimeout.
	// The connection should close.
	st.advance(1 * time.Second) // timeout after writing one byte
	st.advance(1 * time.Second) // timeout after failing to write any more bytes
	st.wantClosed()
}

func TestServerPingSent(t *testing.T) {
	const readIdleTimeout = 15 * time.Second
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
	}, func(s *Server) {
		s.ReadIdleTimeout = readIdleTimeout
	})
	st.greet()

	st.wantIdle()

	st.advance(readIdleTimeout)
	_ = readFrame[*PingFrame](t, st)
	st.wantIdle()

	st.advance(14 * time.Second)
	st.wantIdle()
	st.advance(1 * time.Second)
	st.wantClosed()
}

func TestServerPingResponded(t *testing.T) {
	const readIdleTimeout = 15 * time.Second
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
	}, func(s *Server) {
		s.ReadIdleTimeout = readIdleTimeout
	})
	st.greet()

	st.wantIdle()

	st.advance(readIdleTimeout)
	pf := readFrame[*PingFrame](t, st)
	st.wantIdle()

	st.advance(14 * time.Second)
	st.wantIdle()

	st.writePing(true, pf.Data)

	st.advance(2 * time.Second)
	st.wantIdle()
}
