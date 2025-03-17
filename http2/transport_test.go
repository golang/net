// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

var (
	extNet        = flag.Bool("extnet", false, "do external network tests")
	transportHost = flag.String("transporthost", "http2.golang.org", "hostname to use for TestTransport")
	insecure      = flag.Bool("insecure", false, "insecure TLS dials") // TODO: dead code. remove?
)

var tlsConfigInsecure = &tls.Config{InsecureSkipVerify: true}

var canceledCtx context.Context

func init() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledCtx = ctx
}

func TestTransportExternal(t *testing.T) {
	if !*extNet {
		t.Skip("skipping external network test")
	}
	req, _ := http.NewRequest("GET", "https://"+*transportHost+"/", nil)
	rt := &Transport{TLSClientConfig: tlsConfigInsecure}
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("%v", err)
	}
	res.Write(os.Stdout)
}

type fakeTLSConn struct {
	net.Conn
}

func (c *fakeTLSConn) ConnectionState() tls.ConnectionState {
	return tls.ConnectionState{
		Version:     tls.VersionTLS12,
		CipherSuite: cipher_TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	}
}

func startH2cServer(t *testing.T) net.Listener {
	h2Server := &Server{}
	l := newLocalListener(t)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		h2Server.ServeConn(&fakeTLSConn{conn}, &ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "Hello, %v, http: %v", r.URL.Path, r.TLS == nil)
		})})
	}()
	return l
}

func TestIdleConnTimeout(t *testing.T) {
	for _, test := range []struct {
		name            string
		idleConnTimeout time.Duration
		wait            time.Duration
		baseTransport   *http.Transport
		wantNewConn     bool
	}{{
		name:            "NoExpiry",
		idleConnTimeout: 2 * time.Second,
		wait:            1 * time.Second,
		baseTransport:   nil,
		wantNewConn:     false,
	}, {
		name:            "H2TransportTimeoutExpires",
		idleConnTimeout: 1 * time.Second,
		wait:            2 * time.Second,
		baseTransport:   nil,
		wantNewConn:     true,
	}, {
		name:            "H1TransportTimeoutExpires",
		idleConnTimeout: 0 * time.Second,
		wait:            1 * time.Second,
		baseTransport: &http.Transport{
			IdleConnTimeout: 2 * time.Second,
		},
		wantNewConn: false,
	}} {
		t.Run(test.name, func(t *testing.T) {
			tt := newTestTransport(t, func(tr *Transport) {
				tr.IdleConnTimeout = test.idleConnTimeout
			})
			var tc *testClientConn
			for i := 0; i < 3; i++ {
				req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
				rt := tt.roundTrip(req)

				// This request happens on a new conn if it's the first request
				// (and there is no cached conn), or if the test timeout is long
				// enough that old conns are being closed.
				wantConn := i == 0 || test.wantNewConn
				if has := tt.hasConn(); has != wantConn {
					t.Fatalf("request %v: hasConn=%v, want %v", i, has, wantConn)
				}
				if wantConn {
					tc = tt.getConn()
					// Read client's SETTINGS and first WINDOW_UPDATE,
					// send our SETTINGS.
					tc.wantFrameType(FrameSettings)
					tc.wantFrameType(FrameWindowUpdate)
					tc.writeSettings()
				}
				if tt.hasConn() {
					t.Fatalf("request %v: Transport has more than one conn", i)
				}

				// Respond to the client's request.
				hf := readFrame[*HeadersFrame](t, tc)
				tc.writeHeaders(HeadersFrameParam{
					StreamID:   hf.StreamID,
					EndHeaders: true,
					EndStream:  true,
					BlockFragment: tc.makeHeaderBlockFragment(
						":status", "200",
					),
				})
				rt.wantStatus(200)

				// If this was a newly-accepted conn, read the SETTINGS ACK.
				if wantConn {
					tc.wantFrameType(FrameSettings) // ACK to our settings
				}

				tt.advance(test.wait)
				if got, want := tc.isClosed(), test.wantNewConn; got != want {
					t.Fatalf("after waiting %v, conn closed=%v; want %v", test.wait, got, want)
				}
			}
		})
	}
}

func TestTransportH2c(t *testing.T) {
	l := startH2cServer(t)
	defer l.Close()
	req, err := http.NewRequest("GET", "http://"+l.Addr().String()+"/foobar", nil)
	if err != nil {
		t.Fatal(err)
	}
	var gotConnCnt int32
	trace := &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			if !connInfo.Reused {
				atomic.AddInt32(&gotConnCnt, 1)
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	tr := &Transport{
		AllowHTTP: true,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProtoMajor != 2 {
		t.Fatal("proto not h2c")
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "Hello, /foobar, http: true"; got != want {
		t.Fatalf("response got %v, want %v", got, want)
	}
	if got, want := gotConnCnt, int32(1); got != want {
		t.Errorf("Too many got connections: %d", gotConnCnt)
	}
}

func TestTransport(t *testing.T) {
	const body = "sup"
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range []string{"GET", ""} {
		req := &http.Request{
			Method: m,
			URL:    u,
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("%d: %s", i, err)
		}

		t.Logf("%d: Got res: %+v", i, res)
		if g, w := res.StatusCode, 200; g != w {
			t.Errorf("%d: StatusCode = %v; want %v", i, g, w)
		}
		if g, w := res.Status, "200 OK"; g != w {
			t.Errorf("%d: Status = %q; want %q", i, g, w)
		}
		wantHeader := http.Header{
			"Content-Length": []string{"3"},
			"Content-Type":   []string{"text/plain; charset=utf-8"},
			"Date":           []string{"XXX"}, // see cleanDate
		}
		cleanDate(res)
		if !reflect.DeepEqual(res.Header, wantHeader) {
			t.Errorf("%d: res Header = %v; want %v", i, res.Header, wantHeader)
		}
		if res.Request != req {
			t.Errorf("%d: Response.Request = %p; want %p", i, res.Request, req)
		}
		if res.TLS == nil {
			t.Errorf("%d: Response.TLS = nil; want non-nil", i)
		}
		slurp, err := io.ReadAll(res.Body)
		if err != nil {
			t.Errorf("%d: Body read: %v", i, err)
		} else if string(slurp) != body {
			t.Errorf("%d: Body = %q; want %q", i, slurp, body)
		}
		res.Body.Close()
	}
}

func TestTransportFailureErrorForHTTP1Response(t *testing.T) {
	const expectedHTTP1PayloadHint = "frame header looked like an HTTP/1.1 header"

	ts := httptest.NewServer(http.NewServeMux())
	t.Cleanup(ts.Close)

	for _, tc := range []struct {
		name            string
		maxFrameSize    uint32
		expectedErrorIs error
	}{
		{
			name:         "with default max frame size",
			maxFrameSize: 0,
		},
		{
			name:         "with enough frame size to start reading",
			maxFrameSize: invalidHTTP1LookingFrameHeader().Length + 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Transport{
				DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
					return net.Dial(network, addr)
				},
				MaxReadFrameSize: tc.maxFrameSize,
				AllowHTTP:        true,
			}

			req, err := http.NewRequest("GET", ts.URL, nil)
			if err != nil {
				t.Fatal(err)
			}

			_, err = tr.RoundTrip(req)
			if !strings.Contains(err.Error(), expectedHTTP1PayloadHint) {
				t.Errorf("expected error to contain %q, got %v", expectedHTTP1PayloadHint, err)
			}
		})
	}
}

func testTransportReusesConns(t *testing.T, useClient, wantSame bool, modReq func(*http.Request)) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.RemoteAddr)
	}, func(ts *httptest.Server) {
		ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
			t.Logf("conn %v is now state %v", c.RemoteAddr(), st)
		}
	})
	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	if useClient {
		tr.ConnPool = noDialClientConnPool{new(clientConnPool)}
	}
	defer tr.CloseIdleConnections()
	get := func() string {
		req, err := http.NewRequest("GET", ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		modReq(req)
		var res *http.Response
		if useClient {
			c := ts.Client()
			ConfigureTransports(c.Transport.(*http.Transport))
			res, err = c.Do(req)
		} else {
			res, err = tr.RoundTrip(req)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		slurp, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("Body read: %v", err)
		}
		addr := strings.TrimSpace(string(slurp))
		if addr == "" {
			t.Fatalf("didn't get an addr in response")
		}
		return addr
	}
	first := get()
	second := get()
	if got := first == second; got != wantSame {
		t.Errorf("first and second responses on same connection: %v; want %v", got, wantSame)
	}
}

func TestTransportReusesConns(t *testing.T) {
	for _, test := range []struct {
		name     string
		modReq   func(*http.Request)
		wantSame bool
	}{{
		name:     "ReuseConn",
		modReq:   func(*http.Request) {},
		wantSame: true,
	}, {
		name:     "RequestClose",
		modReq:   func(r *http.Request) { r.Close = true },
		wantSame: false,
	}, {
		name:     "ConnClose",
		modReq:   func(r *http.Request) { r.Header.Set("Connection", "close") },
		wantSame: false,
	}} {
		t.Run(test.name, func(t *testing.T) {
			t.Run("Transport", func(t *testing.T) {
				const useClient = false
				testTransportReusesConns(t, useClient, test.wantSame, test.modReq)
			})
			t.Run("Client", func(t *testing.T) {
				const useClient = true
				testTransportReusesConns(t, useClient, test.wantSame, test.modReq)
			})
		})
	}
}

func TestTransportGetGotConnHooks_HTTP2Transport(t *testing.T) {
	testTransportGetGotConnHooks(t, false)
}
func TestTransportGetGotConnHooks_Client(t *testing.T) { testTransportGetGotConnHooks(t, true) }

func testTransportGetGotConnHooks(t *testing.T, useClient bool) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.RemoteAddr)
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	client := ts.Client()
	ConfigureTransports(client.Transport.(*http.Transport))

	var (
		getConns int32
		gotConns int32
	)
	for i := 0; i < 2; i++ {
		trace := &httptrace.ClientTrace{
			GetConn: func(hostport string) {
				atomic.AddInt32(&getConns, 1)
			},
			GotConn: func(connInfo httptrace.GotConnInfo) {
				got := atomic.AddInt32(&gotConns, 1)
				wantReused, wantWasIdle := false, false
				if got > 1 {
					wantReused, wantWasIdle = true, true
				}
				if connInfo.Reused != wantReused || connInfo.WasIdle != wantWasIdle {
					t.Errorf("GotConn %v: Reused=%v (want %v), WasIdle=%v (want %v)", i, connInfo.Reused, wantReused, connInfo.WasIdle, wantWasIdle)
				}
			},
		}
		req, err := http.NewRequest("GET", ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

		var res *http.Response
		if useClient {
			res, err = client.Do(req)
		} else {
			res, err = tr.RoundTrip(req)
		}
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if get := atomic.LoadInt32(&getConns); get != int32(i+1) {
			t.Errorf("after request %v, %v calls to GetConns: want %v", i, get, i+1)
		}
		if got := atomic.LoadInt32(&gotConns); got != int32(i+1) {
			t.Errorf("after request %v, %v calls to GotConns: want %v", i, got, i+1)
		}
	}
}

type testNetConn struct {
	net.Conn
	closed  bool
	onClose func()
}

func (c *testNetConn) Close() error {
	if !c.closed {
		// We can call Close multiple times on the same net.Conn.
		c.onClose()
	}
	c.closed = true
	return c.Conn.Close()
}

// Tests that the Transport only keeps one pending dial open per destination address.
// https://golang.org/issue/13397
func TestTransportGroupsPendingDials(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
	})
	var (
		mu         sync.Mutex
		dialCount  int
		closeCount int
	)
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			mu.Lock()
			dialCount++
			mu.Unlock()
			c, err := tls.Dial(network, addr, cfg)
			return &testNetConn{
				Conn: c,
				onClose: func() {
					mu.Lock()
					closeCount++
					mu.Unlock()
				},
			}, err
		},
	}
	defer tr.CloseIdleConnections()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest("GET", ts.URL, nil)
			if err != nil {
				t.Error(err)
				return
			}
			res, err := tr.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
		}()
	}
	wg.Wait()
	tr.CloseIdleConnections()
	if dialCount != 1 {
		t.Errorf("saw %d dials; want 1", dialCount)
	}
	if closeCount != 1 {
		t.Errorf("saw %d closes; want 1", closeCount)
	}
}

func TestTransportAbortClosesPipes(t *testing.T) {
	shutdown := make(chan struct{})
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			<-shutdown
		},
	)
	defer close(shutdown) // we must shutdown before st.Close() to avoid hanging

	errCh := make(chan error)
	go func() {
		defer close(errCh)
		tr := &Transport{TLSClientConfig: tlsConfigInsecure}
		req, err := http.NewRequest("GET", ts.URL, nil)
		if err != nil {
			errCh <- err
			return
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			errCh <- err
			return
		}
		defer res.Body.Close()
		ts.CloseClientConnections()
		_, err = io.ReadAll(res.Body)
		if err == nil {
			errCh <- errors.New("expected error from res.Body.Read")
			return
		}
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	// deadlock? that's a bug.
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

// TODO: merge this with TestTransportBody to make TestTransportRequest? This
// could be a table-driven test with extra goodies.
func TestTransportPath(t *testing.T) {
	gotc := make(chan *url.URL, 1)
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			gotc <- r.URL
		},
	)

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	const (
		path  = "/testpath"
		query = "q=1"
	)
	surl := ts.URL + path + "?" + query
	req, err := http.NewRequest("POST", surl, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: tr}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got := <-gotc
	if got.Path != path {
		t.Errorf("Read Path = %q; want %q", got.Path, path)
	}
	if got.RawQuery != query {
		t.Errorf("Read RawQuery = %q; want %q", got.RawQuery, query)
	}
}

func randString(n int) string {
	rnd := rand.New(rand.NewSource(int64(n)))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rnd.Intn(256))
	}
	return string(b)
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("unexpected Read") }
func (panicReader) Close() error             { panic("unexpected Close") }

func TestActualContentLength(t *testing.T) {
	tests := []struct {
		req  *http.Request
		want int64
	}{
		// Verify we don't read from Body:
		0: {
			req:  &http.Request{Body: panicReader{}},
			want: -1,
		},
		// nil Body means 0, regardless of ContentLength:
		1: {
			req:  &http.Request{Body: nil, ContentLength: 5},
			want: 0,
		},
		// ContentLength is used if set.
		2: {
			req:  &http.Request{Body: panicReader{}, ContentLength: 5},
			want: 5,
		},
		// http.NoBody means 0, not -1.
		3: {
			req:  &http.Request{Body: http.NoBody},
			want: 0,
		},
	}
	for i, tt := range tests {
		got := actualContentLength(tt.req)
		if got != tt.want {
			t.Errorf("test[%d]: got %d; want %d", i, got, tt.want)
		}
	}
}

func TestTransportBody(t *testing.T) {
	bodyTests := []struct {
		body         string
		noContentLen bool
	}{
		{body: "some message"},
		{body: "some message", noContentLen: true},
		{body: strings.Repeat("a", 1<<20), noContentLen: true},
		{body: strings.Repeat("a", 1<<20)},
		{body: randString(16<<10 - 1)},
		{body: randString(16 << 10)},
		{body: randString(16<<10 + 1)},
		{body: randString(512<<10 - 1)},
		{body: randString(512 << 10)},
		{body: randString(512<<10 + 1)},
		{body: randString(1<<20 - 1)},
		{body: randString(1 << 20)},
		{body: randString(1<<20 + 2)},
	}

	type reqInfo struct {
		req   *http.Request
		slurp []byte
		err   error
	}
	gotc := make(chan reqInfo, 1)
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			slurp, err := io.ReadAll(r.Body)
			if err != nil {
				gotc <- reqInfo{err: err}
			} else {
				gotc <- reqInfo{req: r, slurp: slurp}
			}
		},
	)

	for i, tt := range bodyTests {
		tr := &Transport{TLSClientConfig: tlsConfigInsecure}
		defer tr.CloseIdleConnections()

		var body io.Reader = strings.NewReader(tt.body)
		if tt.noContentLen {
			body = struct{ io.Reader }{body} // just a Reader, hiding concrete type and other methods
		}
		req, err := http.NewRequest("POST", ts.URL, body)
		if err != nil {
			t.Fatalf("#%d: %v", i, err)
		}
		c := &http.Client{Transport: tr}
		res, err := c.Do(req)
		if err != nil {
			t.Fatalf("#%d: %v", i, err)
		}
		defer res.Body.Close()
		ri := <-gotc
		if ri.err != nil {
			t.Errorf("#%d: read error: %v", i, ri.err)
			continue
		}
		if got := string(ri.slurp); got != tt.body {
			t.Errorf("#%d: Read body mismatch.\n got: %q (len %d)\nwant: %q (len %d)", i, shortString(got), len(got), shortString(tt.body), len(tt.body))
		}
		wantLen := int64(len(tt.body))
		if tt.noContentLen && tt.body != "" {
			wantLen = -1
		}
		if ri.req.ContentLength != wantLen {
			t.Errorf("#%d. handler got ContentLength = %v; want %v", i, ri.req.ContentLength, wantLen)
		}
	}
}

func shortString(v string) string {
	const maxLen = 100
	if len(v) <= maxLen {
		return v
	}
	return fmt.Sprintf("%v[...%d bytes omitted...]%v", v[:maxLen/2], len(v)-maxLen, v[len(v)-maxLen/2:])
}

func TestTransportDialTLS(t *testing.T) {
	var mu sync.Mutex // guards following
	var gotReq, didDial bool

	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gotReq = true
			mu.Unlock()
		},
	)
	tr := &Transport{
		DialTLS: func(netw, addr string, cfg *tls.Config) (net.Conn, error) {
			mu.Lock()
			didDial = true
			mu.Unlock()
			cfg.InsecureSkipVerify = true
			c, err := tls.Dial(netw, addr, cfg)
			if err != nil {
				return nil, err
			}
			return c, c.Handshake()
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	res, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	mu.Lock()
	if !gotReq {
		t.Error("didn't get request")
	}
	if !didDial {
		t.Error("didn't use dial hook")
	}
}

func TestConfigureTransport(t *testing.T) {
	t1 := &http.Transport{}
	err := ConfigureTransport(t1)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%#v", t1); !strings.Contains(got, `"h2"`) {
		// Laziness, to avoid buildtags.
		t.Errorf("stringification of HTTP/1 transport didn't contain \"h2\": %v", got)
	}
	wantNextProtos := []string{"h2", "http/1.1"}
	if t1.TLSClientConfig == nil {
		t.Errorf("nil t1.TLSClientConfig")
	} else if !reflect.DeepEqual(t1.TLSClientConfig.NextProtos, wantNextProtos) {
		t.Errorf("TLSClientConfig.NextProtos = %q; want %q", t1.TLSClientConfig.NextProtos, wantNextProtos)
	}
	if err := ConfigureTransport(t1); err == nil {
		t.Error("unexpected success on second call to ConfigureTransport")
	}

	// And does it work?
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	})

	t1.TLSClientConfig.InsecureSkipVerify = true
	c := &http.Client{Transport: t1}
	res, err := c.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	slurp, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(slurp), "HTTP/2.0"; got != want {
		t.Errorf("body = %q; want %q", got, want)
	}
}

type capitalizeReader struct {
	r io.Reader
}

func (cr capitalizeReader) Read(p []byte) (n int, err error) {
	n, err = cr.r.Read(p)
	for i, b := range p[:n] {
		if b >= 'a' && b <= 'z' {
			p[i] = b - ('a' - 'A')
		}
	}
	return
}

type flushWriter struct {
	w io.Writer
}

func (fw flushWriter) Write(p []byte) (n int, err error) {
	n, err = fw.w.Write(p)
	if f, ok := fw.w.(http.Flusher); ok {
		f.Flush()
	}
	return
}

func newLocalListener(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err == nil {
		return ln
	}
	ln, err = net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestTransportReqBodyAfterResponse_200(t *testing.T) { testTransportReqBodyAfterResponse(t, 200) }
func TestTransportReqBodyAfterResponse_403(t *testing.T) { testTransportReqBodyAfterResponse(t, 403) }

func testTransportReqBodyAfterResponse(t *testing.T, status int) {
	const bodySize = 1 << 10

	tc := newTestClientConn(t)
	tc.greet()

	body := tc.newRequestBody()
	body.writeBytes(bodySize / 2)
	req, _ := http.NewRequest("PUT", "https://dummy.tld/", body)
	rt := tc.roundTrip(req)

	tc.wantHeaders(wantHeader{
		streamID:  rt.streamID(),
		endStream: false,
		header: http.Header{
			":authority": []string{"dummy.tld"},
			":method":    []string{"PUT"},
			":path":      []string{"/"},
		},
	})

	// Provide enough congestion window for the full request body.
	tc.writeWindowUpdate(0, bodySize)
	tc.writeWindowUpdate(rt.streamID(), bodySize)

	tc.wantData(wantData{
		streamID:  rt.streamID(),
		endStream: false,
		size:      bodySize / 2,
	})

	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", strconv.Itoa(status),
		),
	})

	res := rt.response()
	if res.StatusCode != status {
		t.Fatalf("status code = %v; want %v", res.StatusCode, status)
	}

	body.writeBytes(bodySize / 2)
	body.closeWithError(io.EOF)

	if status == 200 {
		// After a 200 response, client sends the remaining request body.
		tc.wantData(wantData{
			streamID:  rt.streamID(),
			endStream: true,
			size:      bodySize / 2,
			multiple:  true,
		})
	} else {
		// After a 403 response, client gives up and resets the stream.
		tc.wantFrameType(FrameRSTStream)
	}

	rt.wantBody(nil)
}

// See golang.org/issue/13444
func TestTransportFullDuplex(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200) // redundant but for clarity
		w.(http.Flusher).Flush()
		io.Copy(flushWriter{w}, capitalizeReader{r.Body})
		fmt.Fprintf(w, "bye.\n")
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}

	pr, pw := io.Pipe()
	req, err := http.NewRequest("PUT", ts.URL, io.NopCloser(pr))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("StatusCode = %v; want %v", res.StatusCode, 200)
	}
	bs := bufio.NewScanner(res.Body)
	want := func(v string) {
		if !bs.Scan() {
			t.Fatalf("wanted to read %q but Scan() = false, err = %v", v, bs.Err())
		}
	}
	write := func(v string) {
		_, err := io.WriteString(pw, v)
		if err != nil {
			t.Fatalf("pipe write: %v", err)
		}
	}
	write("foo\n")
	want("FOO")
	write("bar\n")
	want("BAR")
	pw.Close()
	want("bye.")
	if err := bs.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestTransportConnectRequest(t *testing.T) {
	gotc := make(chan *http.Request, 1)
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotc <- r
	})

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}

	tests := []struct {
		req  *http.Request
		want string
	}{
		{
			req: &http.Request{
				Method: "CONNECT",
				Header: http.Header{},
				URL:    u,
			},
			want: u.Host,
		},
		{
			req: &http.Request{
				Method: "CONNECT",
				Header: http.Header{},
				URL:    u,
				Host:   "example.com:123",
			},
			want: "example.com:123",
		},
	}

	for i, tt := range tests {
		res, err := c.Do(tt.req)
		if err != nil {
			t.Errorf("%d. RoundTrip = %v", i, err)
			continue
		}
		res.Body.Close()
		req := <-gotc
		if req.Method != "CONNECT" {
			t.Errorf("method = %q; want CONNECT", req.Method)
		}
		if req.Host != tt.want {
			t.Errorf("Host = %q; want %q", req.Host, tt.want)
		}
		if req.URL.Host != tt.want {
			t.Errorf("URL.Host = %q; want %q", req.URL.Host, tt.want)
		}
	}
}

type headerType int

const (
	noHeader headerType = iota // omitted
	oneHeader
	splitHeader // broken into continuation on purpose
)

const (
	f0 = noHeader
	f1 = oneHeader
	f2 = splitHeader
	d0 = false
	d1 = true
)

// Test all 36 combinations of response frame orders:
//
//	(3 ways of 100-continue) * (2 ways of headers) * (2 ways of data) * (3 ways of trailers):func TestTransportResponsePattern_00f0(t *testing.T) { testTransportResponsePattern(h0, h1, false, h0) }
//
// Generated by http://play.golang.org/p/SScqYKJYXd
func TestTransportResPattern_c0h1d0t0(t *testing.T) { testTransportResPattern(t, f0, f1, d0, f0) }
func TestTransportResPattern_c0h1d0t1(t *testing.T) { testTransportResPattern(t, f0, f1, d0, f1) }
func TestTransportResPattern_c0h1d0t2(t *testing.T) { testTransportResPattern(t, f0, f1, d0, f2) }
func TestTransportResPattern_c0h1d1t0(t *testing.T) { testTransportResPattern(t, f0, f1, d1, f0) }
func TestTransportResPattern_c0h1d1t1(t *testing.T) { testTransportResPattern(t, f0, f1, d1, f1) }
func TestTransportResPattern_c0h1d1t2(t *testing.T) { testTransportResPattern(t, f0, f1, d1, f2) }
func TestTransportResPattern_c0h2d0t0(t *testing.T) { testTransportResPattern(t, f0, f2, d0, f0) }
func TestTransportResPattern_c0h2d0t1(t *testing.T) { testTransportResPattern(t, f0, f2, d0, f1) }
func TestTransportResPattern_c0h2d0t2(t *testing.T) { testTransportResPattern(t, f0, f2, d0, f2) }
func TestTransportResPattern_c0h2d1t0(t *testing.T) { testTransportResPattern(t, f0, f2, d1, f0) }
func TestTransportResPattern_c0h2d1t1(t *testing.T) { testTransportResPattern(t, f0, f2, d1, f1) }
func TestTransportResPattern_c0h2d1t2(t *testing.T) { testTransportResPattern(t, f0, f2, d1, f2) }
func TestTransportResPattern_c1h1d0t0(t *testing.T) { testTransportResPattern(t, f1, f1, d0, f0) }
func TestTransportResPattern_c1h1d0t1(t *testing.T) { testTransportResPattern(t, f1, f1, d0, f1) }
func TestTransportResPattern_c1h1d0t2(t *testing.T) { testTransportResPattern(t, f1, f1, d0, f2) }
func TestTransportResPattern_c1h1d1t0(t *testing.T) { testTransportResPattern(t, f1, f1, d1, f0) }
func TestTransportResPattern_c1h1d1t1(t *testing.T) { testTransportResPattern(t, f1, f1, d1, f1) }
func TestTransportResPattern_c1h1d1t2(t *testing.T) { testTransportResPattern(t, f1, f1, d1, f2) }
func TestTransportResPattern_c1h2d0t0(t *testing.T) { testTransportResPattern(t, f1, f2, d0, f0) }
func TestTransportResPattern_c1h2d0t1(t *testing.T) { testTransportResPattern(t, f1, f2, d0, f1) }
func TestTransportResPattern_c1h2d0t2(t *testing.T) { testTransportResPattern(t, f1, f2, d0, f2) }
func TestTransportResPattern_c1h2d1t0(t *testing.T) { testTransportResPattern(t, f1, f2, d1, f0) }
func TestTransportResPattern_c1h2d1t1(t *testing.T) { testTransportResPattern(t, f1, f2, d1, f1) }
func TestTransportResPattern_c1h2d1t2(t *testing.T) { testTransportResPattern(t, f1, f2, d1, f2) }
func TestTransportResPattern_c2h1d0t0(t *testing.T) { testTransportResPattern(t, f2, f1, d0, f0) }
func TestTransportResPattern_c2h1d0t1(t *testing.T) { testTransportResPattern(t, f2, f1, d0, f1) }
func TestTransportResPattern_c2h1d0t2(t *testing.T) { testTransportResPattern(t, f2, f1, d0, f2) }
func TestTransportResPattern_c2h1d1t0(t *testing.T) { testTransportResPattern(t, f2, f1, d1, f0) }
func TestTransportResPattern_c2h1d1t1(t *testing.T) { testTransportResPattern(t, f2, f1, d1, f1) }
func TestTransportResPattern_c2h1d1t2(t *testing.T) { testTransportResPattern(t, f2, f1, d1, f2) }
func TestTransportResPattern_c2h2d0t0(t *testing.T) { testTransportResPattern(t, f2, f2, d0, f0) }
func TestTransportResPattern_c2h2d0t1(t *testing.T) { testTransportResPattern(t, f2, f2, d0, f1) }
func TestTransportResPattern_c2h2d0t2(t *testing.T) { testTransportResPattern(t, f2, f2, d0, f2) }
func TestTransportResPattern_c2h2d1t0(t *testing.T) { testTransportResPattern(t, f2, f2, d1, f0) }
func TestTransportResPattern_c2h2d1t1(t *testing.T) { testTransportResPattern(t, f2, f2, d1, f1) }
func TestTransportResPattern_c2h2d1t2(t *testing.T) { testTransportResPattern(t, f2, f2, d1, f2) }

func testTransportResPattern(t *testing.T, expect100Continue, resHeader headerType, withData bool, trailers headerType) {
	const reqBody = "some request body"
	const resBody = "some response body"

	if resHeader == noHeader {
		// TODO: test 100-continue followed by immediate
		// server stream reset, without headers in the middle?
		panic("invalid combination")
	}

	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("POST", "https://dummy.tld/", strings.NewReader(reqBody))
	if expect100Continue != noHeader {
		req.Header.Set("Expect", "100-continue")
	}
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)

	// Possibly 100-continue, or skip when noHeader.
	tc.writeHeadersMode(expect100Continue, HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "100",
		),
	})

	// Client sends request body.
	tc.wantData(wantData{
		streamID:  rt.streamID(),
		endStream: true,
		size:      len(reqBody),
	})

	hdr := []string{
		":status", "200",
		"x-foo", "blah",
		"x-bar", "more",
	}
	if trailers != noHeader {
		hdr = append(hdr, "trailer", "some-trailer")
	}
	tc.writeHeadersMode(resHeader, HeadersFrameParam{
		StreamID:      rt.streamID(),
		EndHeaders:    true,
		EndStream:     withData == false && trailers == noHeader,
		BlockFragment: tc.makeHeaderBlockFragment(hdr...),
	})
	if withData {
		endStream := trailers == noHeader
		tc.writeData(rt.streamID(), endStream, []byte(resBody))
	}
	tc.writeHeadersMode(trailers, HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			"some-trailer", "some-value",
		),
	})

	rt.wantStatus(200)
	if !withData {
		rt.wantBody(nil)
	} else {
		rt.wantBody([]byte(resBody))
	}
	if trailers == noHeader {
		rt.wantTrailers(nil)
	} else {
		rt.wantTrailers(http.Header{
			"Some-Trailer": {"some-value"},
		})
	}
}

// Issue 26189, Issue 17739: ignore unknown 1xx responses
func TestTransportUnknown1xx(t *testing.T) {
	var buf bytes.Buffer
	defer func() { got1xxFuncForTests = nil }()
	got1xxFuncForTests = func(code int, header textproto.MIMEHeader) error {
		fmt.Fprintf(&buf, "code=%d header=%v\n", code, header)
		return nil
	}

	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	for i := 110; i <= 114; i++ {
		tc.writeHeaders(HeadersFrameParam{
			StreamID:   rt.streamID(),
			EndHeaders: true,
			EndStream:  false,
			BlockFragment: tc.makeHeaderBlockFragment(
				":status", fmt.Sprint(i),
				"foo-bar", fmt.Sprint(i),
			),
		})
	}
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "204",
		),
	})

	res := rt.response()
	if res.StatusCode != 204 {
		t.Fatalf("status code = %v; want 204", res.StatusCode)
	}
	want := `code=110 header=map[Foo-Bar:[110]]
code=111 header=map[Foo-Bar:[111]]
code=112 header=map[Foo-Bar:[112]]
code=113 header=map[Foo-Bar:[113]]
code=114 header=map[Foo-Bar:[114]]
`
	if got := buf.String(); got != want {
		t.Errorf("Got trace:\n%s\nWant:\n%s", got, want)
	}
}

func TestTransportReceiveUndeclaredTrailer(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			"some-trailer", "I'm an undeclared Trailer!",
		),
	})

	rt.wantStatus(200)
	rt.wantBody(nil)
	rt.wantTrailers(http.Header{
		"Some-Trailer": []string{"I'm an undeclared Trailer!"},
	})
}

func TestTransportInvalidTrailer_Pseudo1(t *testing.T) {
	testTransportInvalidTrailer_Pseudo(t, oneHeader)
}
func TestTransportInvalidTrailer_Pseudo2(t *testing.T) {
	testTransportInvalidTrailer_Pseudo(t, splitHeader)
}
func testTransportInvalidTrailer_Pseudo(t *testing.T, trailers headerType) {
	testInvalidTrailer(t, trailers, pseudoHeaderError(":colon"),
		":colon", "foo",
		"foo", "bar",
	)
}

func TestTransportInvalidTrailer_Capital1(t *testing.T) {
	testTransportInvalidTrailer_Capital(t, oneHeader)
}
func TestTransportInvalidTrailer_Capital2(t *testing.T) {
	testTransportInvalidTrailer_Capital(t, splitHeader)
}
func testTransportInvalidTrailer_Capital(t *testing.T, trailers headerType) {
	testInvalidTrailer(t, trailers, headerFieldNameError("Capital"),
		"foo", "bar",
		"Capital", "bad",
	)
}
func TestTransportInvalidTrailer_EmptyFieldName(t *testing.T) {
	testInvalidTrailer(t, oneHeader, headerFieldNameError(""),
		"", "bad",
	)
}
func TestTransportInvalidTrailer_BinaryFieldValue(t *testing.T) {
	testInvalidTrailer(t, oneHeader, headerFieldValueError("x"),
		"x", "has\nnewline",
	)
}

func testInvalidTrailer(t *testing.T, mode headerType, wantErr error, trailers ...string) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
			"trailer", "declared",
		),
	})
	tc.writeHeadersMode(mode, HeadersFrameParam{
		StreamID:      rt.streamID(),
		EndHeaders:    true,
		EndStream:     true,
		BlockFragment: tc.makeHeaderBlockFragment(trailers...),
	})

	rt.wantStatus(200)
	body, err := rt.readBody()
	se, ok := err.(StreamError)
	if !ok || se.Cause != wantErr {
		t.Fatalf("res.Body ReadAll error = %q, %#v; want StreamError with cause %T, %#v", body, err, wantErr, wantErr)
	}
	if len(body) > 0 {
		t.Fatalf("body = %q; want nothing", body)
	}
}

// headerListSize returns the HTTP2 header list size of h.
//
//	http://httpwg.org/specs/rfc7540.html#SETTINGS_MAX_HEADER_LIST_SIZE
//	http://httpwg.org/specs/rfc7540.html#MaxHeaderBlock
func headerListSize(h http.Header) (size uint32) {
	for k, vv := range h {
		for _, v := range vv {
			hf := hpack.HeaderField{Name: k, Value: v}
			size += hf.Size()
		}
	}
	return size
}

// padHeaders adds data to an http.Header until headerListSize(h) ==
// limit. Due to the way header list sizes are calculated, padHeaders
// cannot add fewer than len("Pad-Headers") + 32 bytes to h, and will
// call t.Fatal if asked to do so. PadHeaders first reserves enough
// space for an empty "Pad-Headers" key, then adds as many copies of
// filler as possible. Any remaining bytes necessary to push the
// header list size up to limit are added to h["Pad-Headers"].
func padHeaders(t *testing.T, h http.Header, limit uint64, filler string) {
	if limit > 0xffffffff {
		t.Fatalf("padHeaders: refusing to pad to more than 2^32-1 bytes. limit = %v", limit)
	}
	hf := hpack.HeaderField{Name: "Pad-Headers", Value: ""}
	minPadding := uint64(hf.Size())
	size := uint64(headerListSize(h))

	minlimit := size + minPadding
	if limit < minlimit {
		t.Fatalf("padHeaders: limit %v < %v", limit, minlimit)
	}

	// Use a fixed-width format for name so that fieldSize
	// remains constant.
	nameFmt := "Pad-Headers-%06d"
	hf = hpack.HeaderField{Name: fmt.Sprintf(nameFmt, 1), Value: filler}
	fieldSize := uint64(hf.Size())

	// Add as many complete filler values as possible, leaving
	// room for at least one empty "Pad-Headers" key.
	limit = limit - minPadding
	for i := 0; size+fieldSize < limit; i++ {
		name := fmt.Sprintf(nameFmt, i)
		h.Add(name, filler)
		size += fieldSize
	}

	// Add enough bytes to reach limit.
	remain := limit - size
	lastValue := strings.Repeat("*", int(remain))
	h.Add("Pad-Headers", lastValue)
}

func TestPadHeaders(t *testing.T) {
	check := func(h http.Header, limit uint32, fillerLen int) {
		if h == nil {
			h = make(http.Header)
		}
		filler := strings.Repeat("f", fillerLen)
		padHeaders(t, h, uint64(limit), filler)
		gotSize := headerListSize(h)
		if gotSize != limit {
			t.Errorf("Got size = %v; want %v", gotSize, limit)
		}
	}
	// Try all possible combinations for small fillerLen and limit.
	hf := hpack.HeaderField{Name: "Pad-Headers", Value: ""}
	minLimit := hf.Size()
	for limit := minLimit; limit <= 128; limit++ {
		for fillerLen := 0; uint32(fillerLen) <= limit; fillerLen++ {
			check(nil, limit, fillerLen)
		}
	}

	// Try a few tests with larger limits, plus cumulative
	// tests. Since these tests are cumulative, tests[i+1].limit
	// must be >= tests[i].limit + minLimit. See the comment on
	// padHeaders for more info on why the limit arg has this
	// restriction.
	tests := []struct {
		fillerLen int
		limit     uint32
	}{
		{
			fillerLen: 64,
			limit:     1024,
		},
		{
			fillerLen: 1024,
			limit:     1286,
		},
		{
			fillerLen: 256,
			limit:     2048,
		},
		{
			fillerLen: 1024,
			limit:     10 * 1024,
		},
		{
			fillerLen: 1023,
			limit:     11 * 1024,
		},
	}
	h := make(http.Header)
	for _, tc := range tests {
		check(nil, tc.limit, tc.fillerLen)
		check(h, tc.limit, tc.fillerLen)
	}
}

func TestTransportChecksRequestHeaderListSize(t *testing.T) {
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			// Consume body & force client to send
			// trailers before writing response.
			// io.ReadAll returns non-nil err for
			// requests that attempt to send greater than
			// maxHeaderListSize bytes of trailers, since
			// those requests generate a stream reset.
			io.ReadAll(r.Body)
			r.Body.Close()
		},
		func(ts *httptest.Server) {
			ts.Config.MaxHeaderBytes = 16 << 10
		},
		optQuiet,
	)

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	checkRoundTrip := func(req *http.Request, wantErr error, desc string) {
		// Make an arbitrary request to ensure we get the server's
		// settings frame and initialize peerMaxHeaderListSize.
		req0, err := http.NewRequest("GET", ts.URL, nil)
		if err != nil {
			t.Fatalf("newRequest: NewRequest: %v", err)
		}
		res0, err := tr.RoundTrip(req0)
		if err != nil {
			t.Errorf("%v: Initial RoundTrip err = %v", desc, err)
		}
		res0.Body.Close()

		res, err := tr.RoundTrip(req)
		if !errors.Is(err, wantErr) {
			if res != nil {
				res.Body.Close()
			}
			t.Errorf("%v: RoundTrip err = %v; want %v", desc, err, wantErr)
			return
		}
		if err == nil {
			if res == nil {
				t.Errorf("%v: response nil; want non-nil.", desc)
				return
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Errorf("%v: response status = %v; want %v", desc, res.StatusCode, http.StatusOK)
			}
			return
		}
		if res != nil {
			t.Errorf("%v: RoundTrip err = %v but response non-nil", desc, err)
		}
	}
	headerListSizeForRequest := func(req *http.Request) (size uint64) {
		const addGzipHeader = true
		const peerMaxHeaderListSize = 0xffffffffffffffff
		_, err := encodeRequestHeaders(req, addGzipHeader, peerMaxHeaderListSize, func(name, value string) {
			hf := hpack.HeaderField{Name: name, Value: value}
			size += uint64(hf.Size())
		})
		if err != nil {
			t.Fatal(err)
		}
		return size
	}
	// Create a new Request for each test, rather than reusing the
	// same Request, to avoid a race when modifying req.Headers.
	// See https://github.com/golang/go/issues/21316
	newRequest := func() *http.Request {
		// Body must be non-nil to enable writing trailers.
		body := strings.NewReader("hello")
		req, err := http.NewRequest("POST", ts.URL, body)
		if err != nil {
			t.Fatalf("newRequest: NewRequest: %v", err)
		}
		return req
	}

	var (
		scMu sync.Mutex
		sc   *serverConn
	)
	testHookGetServerConn = func(v *serverConn) {
		scMu.Lock()
		defer scMu.Unlock()
		if sc != nil {
			panic("testHookGetServerConn called multiple times")
		}
		sc = v
	}
	defer func() {
		testHookGetServerConn = nil
	}()

	// Validate peerMaxHeaderListSize.
	req := newRequest()
	checkRoundTrip(req, nil, "Initial request")
	addr := authorityAddr(req.URL.Scheme, req.URL.Host)
	cc, err := tr.connPool().GetClientConn(req, addr)
	if err != nil {
		t.Fatalf("GetClientConn: %v", err)
	}
	cc.mu.Lock()
	peerSize := cc.peerMaxHeaderListSize
	cc.mu.Unlock()
	scMu.Lock()
	wantSize := uint64(sc.maxHeaderListSize())
	scMu.Unlock()
	if peerSize != wantSize {
		t.Errorf("peerMaxHeaderListSize = %v; want %v", peerSize, wantSize)
	}

	// Sanity check peerSize. (*serverConn) maxHeaderListSize adds
	// 320 bytes of padding.
	wantHeaderBytes := uint64(ts.Config.MaxHeaderBytes) + 320
	if peerSize != wantHeaderBytes {
		t.Errorf("peerMaxHeaderListSize = %v; want %v.", peerSize, wantHeaderBytes)
	}

	// Pad headers & trailers, but stay under peerSize.
	req = newRequest()
	req.Header = make(http.Header)
	req.Trailer = make(http.Header)
	filler := strings.Repeat("*", 1024)
	padHeaders(t, req.Trailer, peerSize, filler)
	// cc.encodeHeaders adds some default headers to the request,
	// so we need to leave room for those.
	defaultBytes := headerListSizeForRequest(req)
	padHeaders(t, req.Header, peerSize-defaultBytes, filler)
	checkRoundTrip(req, nil, "Headers & Trailers under limit")

	// Add enough header bytes to push us over peerSize.
	req = newRequest()
	req.Header = make(http.Header)
	padHeaders(t, req.Header, peerSize, filler)
	checkRoundTrip(req, errRequestHeaderListSize, "Headers over limit")

	// Push trailers over the limit.
	req = newRequest()
	req.Trailer = make(http.Header)
	padHeaders(t, req.Trailer, peerSize+1, filler)
	checkRoundTrip(req, errRequestHeaderListSize, "Trailers over limit")

	// Send headers with a single large value.
	req = newRequest()
	filler = strings.Repeat("*", int(peerSize))
	req.Header = make(http.Header)
	req.Header.Set("Big", filler)
	checkRoundTrip(req, errRequestHeaderListSize, "Single large header")

	// Send trailers with a single large value.
	req = newRequest()
	req.Trailer = make(http.Header)
	req.Trailer.Set("Big", filler)
	checkRoundTrip(req, errRequestHeaderListSize, "Single large trailer")
}

func TestTransportChecksResponseHeaderListSize(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)

	hdr := []string{":status", "200"}
	large := strings.Repeat("a", 1<<10)
	for i := 0; i < 5042; i++ {
		hdr = append(hdr, large, large)
	}
	hbf := tc.makeHeaderBlockFragment(hdr...)
	// Note: this number might change if our hpack implementation changes.
	// That's fine. This is just a sanity check that our response can fit in a single
	// header block fragment frame.
	if size, want := len(hbf), 6329; size != want {
		t.Fatalf("encoding over 10MB of duplicate keypairs took %d bytes; expected %d", size, want)
	}
	tc.writeHeaders(HeadersFrameParam{
		StreamID:      rt.streamID(),
		EndHeaders:    true,
		EndStream:     true,
		BlockFragment: hbf,
	})

	res, err := rt.result()
	if e, ok := err.(StreamError); ok {
		err = e.Cause
	}
	if err != errResponseHeaderListSize {
		size := int64(0)
		if res != nil {
			res.Body.Close()
			for k, vv := range res.Header {
				for _, v := range vv {
					size += int64(len(k)) + int64(len(v)) + 32
				}
			}
		}
		t.Fatalf("RoundTrip Error = %v (and %d bytes of response headers); want errResponseHeaderListSize", err, size)
	}
}

func TestTransportCookieHeaderSplit(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	req.Header.Add("Cookie", "a=b;c=d;  e=f;")
	req.Header.Add("Cookie", "e=f;g=h; ")
	req.Header.Add("Cookie", "i=j")
	rt := tc.roundTrip(req)

	tc.wantHeaders(wantHeader{
		streamID:  rt.streamID(),
		endStream: true,
		header: http.Header{
			"cookie": []string{"a=b", "c=d", "e=f", "e=f", "g=h", "i=j"},
		},
	})
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "204",
		),
	})

	if err := rt.err(); err != nil {
		t.Fatalf("RoundTrip = %v, want success", err)
	}
}

// Test that the Transport returns a typed error from Response.Body.Read calls
// when the server sends an error. (here we use a panic, since that should generate
// a stream error, but others like cancel should be similar)
func TestTransportBodyReadErrorType(t *testing.T) {
	doPanic := make(chan bool, 1)
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush() // force headers out
			<-doPanic
			panic("boom")
		},
		optQuiet,
	)

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}

	res, err := c.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	doPanic <- true
	buf := make([]byte, 100)
	n, err := res.Body.Read(buf)
	got, ok := err.(StreamError)
	want := StreamError{StreamID: 0x1, Code: 0x2}
	if !ok || got.StreamID != want.StreamID || got.Code != want.Code {
		t.Errorf("Read = %v, %#v; want error %#v", n, err, want)
	}
}

// golang.org/issue/13924
// This used to fail after many iterations, especially with -race:
// go test -v -run=TestTransportDoubleCloseOnWriteError -count=500 -race
func TestTransportDoubleCloseOnWriteError(t *testing.T) {
	var (
		mu   sync.Mutex
		conn net.Conn // to close if set
	)

	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if conn != nil {
				conn.Close()
			}
		},
	)

	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			tc, err := tls.Dial(network, addr, cfg)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			conn = tc
			return tc, nil
		},
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}
	c.Get(ts.URL)
}

// Test that the http1 Transport.DisableKeepAlives option is respected
// and connections are closed as soon as idle.
// See golang.org/issue/14008
func TestTransportDisableKeepAlives(t *testing.T) {
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "hi")
		},
	)

	connClosed := make(chan struct{}) // closed on tls.Conn.Close
	tr := &Transport{
		t1: &http.Transport{
			DisableKeepAlives: true,
		},
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			tc, err := tls.Dial(network, addr, cfg)
			if err != nil {
				return nil, err
			}
			return &noteCloseConn{Conn: tc, closefn: func() { close(connClosed) }}, nil
		},
	}
	c := &http.Client{Transport: tr}
	res, err := c.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	select {
	case <-connClosed:
	case <-time.After(1 * time.Second):
		t.Errorf("timeout")
	}

}

// Test concurrent requests with Transport.DisableKeepAlives. We can share connections,
// but when things are totally idle, it still needs to close.
func TestTransportDisableKeepAlives_Concurrency(t *testing.T) {
	const D = 25 * time.Millisecond
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(D)
			io.WriteString(w, "hi")
		},
	)

	var dials int32
	var conns sync.WaitGroup
	tr := &Transport{
		t1: &http.Transport{
			DisableKeepAlives: true,
		},
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			tc, err := tls.Dial(network, addr, cfg)
			if err != nil {
				return nil, err
			}
			atomic.AddInt32(&dials, 1)
			conns.Add(1)
			return &noteCloseConn{Conn: tc, closefn: func() { conns.Done() }}, nil
		},
	}
	c := &http.Client{Transport: tr}
	var reqs sync.WaitGroup
	const N = 20
	for i := 0; i < N; i++ {
		reqs.Add(1)
		if i == N-1 {
			// For the final request, try to make all the
			// others close. This isn't verified in the
			// count, other than the Log statement, since
			// it's so timing dependent. This test is
			// really to make sure we don't interrupt a
			// valid request.
			time.Sleep(D * 2)
		}
		go func() {
			defer reqs.Done()
			res, err := c.Get(ts.URL)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := io.ReadAll(res.Body); err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
		}()
	}
	reqs.Wait()
	conns.Wait()
	t.Logf("did %d dials, %d requests", atomic.LoadInt32(&dials), N)
}

type noteCloseConn struct {
	net.Conn
	onceClose sync.Once
	closefn   func()
}

func (c *noteCloseConn) Close() error {
	c.onceClose.Do(c.closefn)
	return c.Conn.Close()
}

func isTimeout(err error) bool {
	switch err := err.(type) {
	case nil:
		return false
	case *url.Error:
		return isTimeout(err.Err)
	case net.Error:
		return err.Timeout()
	}
	return false
}

// Test that the http1 Transport.ResponseHeaderTimeout option and cancel is sent.
func TestTransportResponseHeaderTimeout_NoBody(t *testing.T) {
	testTransportResponseHeaderTimeout(t, false)
}
func TestTransportResponseHeaderTimeout_Body(t *testing.T) {
	testTransportResponseHeaderTimeout(t, true)
}

func testTransportResponseHeaderTimeout(t *testing.T, body bool) {
	const bodySize = 4 << 20
	tc := newTestClientConn(t, func(tr *Transport) {
		tr.t1 = &http.Transport{
			ResponseHeaderTimeout: 5 * time.Millisecond,
		}
	})
	tc.greet()

	var req *http.Request
	var reqBody *testRequestBody
	if body {
		reqBody = tc.newRequestBody()
		reqBody.writeBytes(bodySize)
		reqBody.closeWithError(io.EOF)
		req, _ = http.NewRequest("POST", "https://dummy.tld/", reqBody)
		req.Header.Set("Content-Type", "text/foo")
	} else {
		req, _ = http.NewRequest("GET", "https://dummy.tld/", nil)
	}

	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)

	tc.writeWindowUpdate(0, bodySize)
	tc.writeWindowUpdate(rt.streamID(), bodySize)

	if body {
		tc.wantData(wantData{
			endStream: true,
			size:      bodySize,
			multiple:  true,
		})
	}

	tc.advance(4 * time.Millisecond)
	if rt.done() {
		t.Fatalf("RoundTrip is done after 4ms; want still waiting")
	}
	tc.advance(1 * time.Millisecond)

	if err := rt.err(); !isTimeout(err) {
		t.Fatalf("RoundTrip error: %v; want timeout error", err)
	}
}

func TestTransportDisableCompression(t *testing.T) {
	const body = "sup"
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		want := http.Header{
			"User-Agent": []string{"Go-http-client/2.0"},
		}
		if !reflect.DeepEqual(r.Header, want) {
			t.Errorf("request headers = %v; want %v", r.Header, want)
		}
	})

	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		t1: &http.Transport{
			DisableCompression: true,
		},
	}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
}

// RFC 7540 section 8.1.2.2
func TestTransportRejectsConnHeaders(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var got []string
		for k := range r.Header {
			got = append(got, k)
		}
		sort.Strings(got)
		w.Header().Set("Got-Header", strings.Join(got, ","))
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	tests := []struct {
		key   string
		value []string
		want  string
	}{
		{
			key:   "Upgrade",
			value: []string{"anything"},
			want:  "ERROR: http2: invalid Upgrade request header: [\"anything\"]",
		},
		{
			key:   "Connection",
			value: []string{"foo"},
			want:  "ERROR: http2: invalid Connection request header: [\"foo\"]",
		},
		{
			key:   "Connection",
			value: []string{"close"},
			want:  "Accept-Encoding,User-Agent",
		},
		{
			key:   "Connection",
			value: []string{"CLoSe"},
			want:  "Accept-Encoding,User-Agent",
		},
		{
			key:   "Connection",
			value: []string{"close", "something-else"},
			want:  "ERROR: http2: invalid Connection request header: [\"close\" \"something-else\"]",
		},
		{
			key:   "Connection",
			value: []string{"keep-alive"},
			want:  "Accept-Encoding,User-Agent",
		},
		{
			key:   "Connection",
			value: []string{"Keep-ALIVE"},
			want:  "Accept-Encoding,User-Agent",
		},
		{
			key:   "Proxy-Connection", // just deleted and ignored
			value: []string{"keep-alive"},
			want:  "Accept-Encoding,User-Agent",
		},
		{
			key:   "Transfer-Encoding",
			value: []string{""},
			want:  "Accept-Encoding,User-Agent",
		},
		{
			key:   "Transfer-Encoding",
			value: []string{"foo"},
			want:  "ERROR: http2: invalid Transfer-Encoding request header: [\"foo\"]",
		},
		{
			key:   "Transfer-Encoding",
			value: []string{"chunked"},
			want:  "Accept-Encoding,User-Agent",
		},
		{
			key:   "Transfer-Encoding",
			value: []string{"chunKed"}, // Kelvin sign
			want:  "ERROR: http2: invalid Transfer-Encoding request header: [\"chunKed\"]",
		},
		{
			key:   "Transfer-Encoding",
			value: []string{"chunked", "other"},
			want:  "ERROR: http2: invalid Transfer-Encoding request header: [\"chunked\" \"other\"]",
		},
		{
			key:   "Content-Length",
			value: []string{"123"},
			want:  "Accept-Encoding,User-Agent",
		},
		{
			key:   "Keep-Alive",
			value: []string{"doop"},
			want:  "Accept-Encoding,User-Agent",
		},
	}

	for _, tt := range tests {
		req, _ := http.NewRequest("GET", ts.URL, nil)
		req.Header[tt.key] = tt.value
		res, err := tr.RoundTrip(req)
		var got string
		if err != nil {
			got = fmt.Sprintf("ERROR: %v", err)
		} else {
			got = res.Header.Get("Got-Header")
			res.Body.Close()
		}
		if got != tt.want {
			t.Errorf("For key %q, value %q, got = %q; want %q", tt.key, tt.value, got, tt.want)
		}
	}
}

// Reject content-length headers containing a sign.
// See https://golang.org/issue/39017
func TestTransportRejectsContentLengthWithSign(t *testing.T) {
	tests := []struct {
		name   string
		cl     []string
		wantCL string
	}{
		{
			name:   "proper content-length",
			cl:     []string{"3"},
			wantCL: "3",
		},
		{
			name:   "ignore cl with plus sign",
			cl:     []string{"+3"},
			wantCL: "",
		},
		{
			name:   "ignore cl with minus sign",
			cl:     []string{"-3"},
			wantCL: "",
		},
		{
			name:   "max int64, for safe uint64->int64 conversion",
			cl:     []string{"9223372036854775807"},
			wantCL: "9223372036854775807",
		},
		{
			name:   "overflows int64, so ignored",
			cl:     []string{"9223372036854775808"},
			wantCL: "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", tt.cl[0])
			})
			tr := &Transport{TLSClientConfig: tlsConfigInsecure}
			defer tr.CloseIdleConnections()

			req, _ := http.NewRequest("HEAD", ts.URL, nil)
			res, err := tr.RoundTrip(req)

			var got string
			if err != nil {
				got = fmt.Sprintf("ERROR: %v", err)
			} else {
				got = res.Header.Get("Content-Length")
				res.Body.Close()
			}

			if got != tt.wantCL {
				t.Fatalf("Got: %q\nWant: %q", got, tt.wantCL)
			}
		})
	}
}

// golang.org/issue/14048
// golang.org/issue/64766
func TestTransportFailsOnInvalidHeadersAndTrailers(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var got []string
		for k := range r.Header {
			got = append(got, k)
		}
		sort.Strings(got)
		w.Header().Set("Got-Header", strings.Join(got, ","))
	})

	tests := [...]struct {
		h       http.Header
		t       http.Header
		wantErr string
	}{
		0: {
			h:       http.Header{"with space": {"foo"}},
			wantErr: `invalid HTTP header name "with space"`,
		},
		1: {
			h:       http.Header{"name": {"Брэд"}},
			wantErr: "", // okay
		},
		2: {
			h:       http.Header{"имя": {"Brad"}},
			wantErr: `invalid HTTP header name "имя"`,
		},
		3: {
			h:       http.Header{"foo": {"foo\x01bar"}},
			wantErr: `invalid HTTP header value for header "foo"`,
		},
		4: {
			t:       http.Header{"foo": {"foo\x01bar"}},
			wantErr: `invalid HTTP trailer value for header "foo"`,
		},
		5: {
			t:       http.Header{"x-\r\nda": {"foo\x01bar"}},
			wantErr: `invalid HTTP trailer name "x-\r\nda"`,
		},
	}

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	for i, tt := range tests {
		req, _ := http.NewRequest("GET", ts.URL, nil)
		req.Header = tt.h
		req.Trailer = tt.t
		res, err := tr.RoundTrip(req)
		var bad bool
		if tt.wantErr == "" {
			if err != nil {
				bad = true
				t.Errorf("case %d: error = %v; want no error", i, err)
			}
		} else {
			if !strings.Contains(fmt.Sprint(err), tt.wantErr) {
				bad = true
				t.Errorf("case %d: error = %v; want error %q", i, err, tt.wantErr)
			}
		}
		if err == nil {
			if bad {
				t.Logf("case %d: server got headers %q", i, res.Header.Get("Got-Header"))
			}
			res.Body.Close()
		}
	}
}

// Tests that gzipReader doesn't crash on a second Read call following
// the first Read call's gzip.NewReader returning an error.
func TestGzipReader_DoubleReadCrash(t *testing.T) {
	gz := &gzipReader{
		body: io.NopCloser(strings.NewReader("0123456789")),
	}
	var buf [1]byte
	n, err1 := gz.Read(buf[:])
	if n != 0 || !strings.Contains(fmt.Sprint(err1), "invalid header") {
		t.Fatalf("Read = %v, %v; want 0, invalid header", n, err1)
	}
	n, err2 := gz.Read(buf[:])
	if n != 0 || err2 != err1 {
		t.Fatalf("second Read = %v, %v; want 0, %v", n, err2, err1)
	}
}

func TestGzipReader_ReadAfterClose(t *testing.T) {
	body := bytes.Buffer{}
	w := gzip.NewWriter(&body)
	w.Write([]byte("012345679"))
	w.Close()
	gz := &gzipReader{
		body: io.NopCloser(&body),
	}
	var buf [1]byte
	n, err := gz.Read(buf[:])
	if n != 1 || err != nil {
		t.Fatalf("first Read = %v, %v; want 1, nil", n, err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz Close error: %v", err)
	}
	n, err = gz.Read(buf[:])
	if n != 0 || err != fs.ErrClosed {
		t.Fatalf("Read after close = %v, %v; want 0, fs.ErrClosed", n, err)
	}
}

func TestTransportNewTLSConfig(t *testing.T) {
	tests := [...]struct {
		conf *tls.Config
		host string
		want *tls.Config
	}{
		// Normal case.
		0: {
			conf: nil,
			host: "foo.com",
			want: &tls.Config{
				ServerName: "foo.com",
				NextProtos: []string{NextProtoTLS},
			},
		},

		// User-provided name (bar.com) takes precedence:
		1: {
			conf: &tls.Config{
				ServerName: "bar.com",
			},
			host: "foo.com",
			want: &tls.Config{
				ServerName: "bar.com",
				NextProtos: []string{NextProtoTLS},
			},
		},

		// NextProto is prepended:
		2: {
			conf: &tls.Config{
				NextProtos: []string{"foo", "bar"},
			},
			host: "example.com",
			want: &tls.Config{
				ServerName: "example.com",
				NextProtos: []string{NextProtoTLS, "foo", "bar"},
			},
		},

		// NextProto is not duplicated:
		3: {
			conf: &tls.Config{
				NextProtos: []string{"foo", "bar", NextProtoTLS},
			},
			host: "example.com",
			want: &tls.Config{
				ServerName: "example.com",
				NextProtos: []string{"foo", "bar", NextProtoTLS},
			},
		},
	}
	for i, tt := range tests {
		// Ignore the session ticket keys part, which ends up populating
		// unexported fields in the Config:
		if tt.conf != nil {
			tt.conf.SessionTicketsDisabled = true
		}

		tr := &Transport{TLSClientConfig: tt.conf}
		got := tr.newTLSConfig(tt.host)

		got.SessionTicketsDisabled = false

		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%d. got %#v; want %#v", i, got, tt.want)
		}
	}
}

// The Google GFE responds to HEAD requests with a HEADERS frame
// without END_STREAM, followed by a 0-length DATA frame with
// END_STREAM. Make sure we don't get confused by that. (We did.)
func TestTransportReadHeadResponse(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("HEAD", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false, // as the GFE does
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
			"content-length", "123",
		),
	})
	tc.writeData(rt.streamID(), true, nil)

	res := rt.response()
	if res.ContentLength != 123 {
		t.Fatalf("Content-Length = %d; want 123", res.ContentLength)
	}
	rt.wantBody(nil)
}

func TestTransportReadHeadResponseWithBody(t *testing.T) {
	// This test uses an invalid response format.
	// Discard logger output to not spam tests output.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	response := "redirecting to /elsewhere"
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("HEAD", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
			"content-length", strconv.Itoa(len(response)),
		),
	})
	tc.writeData(rt.streamID(), true, []byte(response))

	res := rt.response()
	if res.ContentLength != int64(len(response)) {
		t.Fatalf("Content-Length = %d; want %d", res.ContentLength, len(response))
	}
	rt.wantBody(nil)
}

type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// golang.org/issue/15425: test that a handler closing the request
// body doesn't terminate the stream to the peer. (It just stops
// readability from the handler's side, and eventually the client
// runs out of flow control tokens)
func TestTransportHandlerBodyClose(t *testing.T) {
	const bodySize = 10 << 20
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		r.Body.Close()
		io.Copy(w, io.LimitReader(neverEnding('A'), bodySize))
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	g0 := runtime.NumGoroutine()

	const numReq = 10
	for i := 0; i < numReq; i++ {
		req, err := http.NewRequest("POST", ts.URL, struct{ io.Reader }{io.LimitReader(neverEnding('A'), bodySize)})
		if err != nil {
			t.Fatal(err)
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if n != bodySize || err != nil {
			t.Fatalf("req#%d: Copy = %d, %v; want %d, nil", i, n, err, bodySize)
		}
	}
	tr.CloseIdleConnections()

	if !waitCondition(5*time.Second, 100*time.Millisecond, func() bool {
		gd := runtime.NumGoroutine() - g0
		return gd < numReq/2
	}) {
		t.Errorf("appeared to leak goroutines")
	}
}

// https://golang.org/issue/15930
func TestTransportFlowControl(t *testing.T) {
	const bufLen = 64 << 10
	var total int64 = 100 << 20 // 100MB
	if testing.Short() {
		total = 10 << 20
	}

	var wrote int64 // updated atomically
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, bufLen)
		for wrote < total {
			n, err := w.Write(b)
			atomic.AddInt64(&wrote, int64(n))
			if err != nil {
				t.Errorf("ResponseWriter.Write error: %v", err)
				break
			}
			w.(http.Flusher).Flush()
		}
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		t.Fatal("NewRequest error:", err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal("RoundTrip error:", err)
	}
	defer resp.Body.Close()

	var read int64
	b := make([]byte, bufLen)
	for {
		n, err := resp.Body.Read(b)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("Read error:", err)
		}
		read += int64(n)

		const max = transportDefaultStreamFlow
		if w := atomic.LoadInt64(&wrote); -max > read-w || read-w > max {
			t.Fatalf("Too much data inflight: server wrote %v bytes but client only received %v", w, read)
		}

		// Let the server get ahead of the client.
		time.Sleep(1 * time.Millisecond)
	}
}

// golang.org/issue/14627 -- if the server sends a GOAWAY frame, make
// the Transport remember it and return it back to users (via
// RoundTrip or request body reads) if needed (e.g. if the server
// proceeds to close the TCP connection before the client gets its
// response)
func TestTransportUsesGoAwayDebugError_RoundTrip(t *testing.T) {
	testTransportUsesGoAwayDebugError(t, false)
}

func TestTransportUsesGoAwayDebugError_Body(t *testing.T) {
	testTransportUsesGoAwayDebugError(t, true)
}

func testTransportUsesGoAwayDebugError(t *testing.T, failMidBody bool) {
	tc := newTestClientConn(t)
	tc.greet()

	const goAwayErrCode = ErrCodeHTTP11Required // arbitrary
	const goAwayDebugData = "some debug data"

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)

	if failMidBody {
		tc.writeHeaders(HeadersFrameParam{
			StreamID:   rt.streamID(),
			EndHeaders: true,
			EndStream:  false,
			BlockFragment: tc.makeHeaderBlockFragment(
				":status", "200",
				"content-length", "123",
			),
		})
	}

	// Write two GOAWAY frames, to test that the Transport takes
	// the interesting parts of both.
	tc.writeGoAway(5, ErrCodeNo, []byte(goAwayDebugData))
	tc.writeGoAway(5, goAwayErrCode, nil)
	tc.closeWrite()

	res, err := rt.result()
	whence := "RoundTrip"
	if failMidBody {
		whence = "Body.Read"
		if err != nil {
			t.Fatalf("RoundTrip error = %v, want success", err)
		}
		_, err = res.Body.Read(make([]byte, 1))
	}

	want := GoAwayError{
		LastStreamID: 5,
		ErrCode:      goAwayErrCode,
		DebugData:    goAwayDebugData,
	}
	if !reflect.DeepEqual(err, want) {
		t.Errorf("%v error = %T: %#v, want %T (%#v)", whence, err, err, want, want)
	}
}

func testTransportReturnsUnusedFlowControl(t *testing.T, oneDataFrame bool) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
			"content-length", "5000",
		),
	})
	initialInflow := tc.inflowWindow(0)

	// Two cases:
	// - Send one DATA frame with 5000 bytes.
	// - Send two DATA frames with 1 and 4999 bytes each.
	//
	// In both cases, the client should consume one byte of data,
	// refund that byte, then refund the following 4999 bytes.
	//
	// In the second case, the server waits for the client to reset the
	// stream before sending the second DATA frame. This tests the case
	// where the client receives a DATA frame after it has reset the stream.
	const streamNotEnded = false
	if oneDataFrame {
		tc.writeData(rt.streamID(), streamNotEnded, make([]byte, 5000))
	} else {
		tc.writeData(rt.streamID(), streamNotEnded, make([]byte, 1))
	}

	res := rt.response()
	if n, err := res.Body.Read(make([]byte, 1)); err != nil || n != 1 {
		t.Fatalf("body read = %v, %v; want 1, nil", n, err)
	}
	res.Body.Close() // leaving 4999 bytes unread
	tc.sync()

	sentAdditionalData := false
	tc.wantUnorderedFrames(
		func(f *RSTStreamFrame) bool {
			if f.ErrCode != ErrCodeCancel {
				t.Fatalf("Expected a RSTStreamFrame with code cancel; got %v", summarizeFrame(f))
			}
			if !oneDataFrame {
				// Send the remaining data now.
				tc.writeData(rt.streamID(), streamNotEnded, make([]byte, 4999))
				sentAdditionalData = true
			}
			return true
		},
		func(f *PingFrame) bool {
			return true
		},
		func(f *WindowUpdateFrame) bool {
			if !oneDataFrame && !sentAdditionalData {
				t.Fatalf("Got WindowUpdateFrame, don't expect one yet")
			}
			if f.Increment != 5000 {
				t.Fatalf("Expected WindowUpdateFrames for 5000 bytes; got %v", summarizeFrame(f))
			}
			return true
		},
	)

	if got, want := tc.inflowWindow(0), initialInflow; got != want {
		t.Fatalf("connection flow tokens = %v, want %v", got, want)
	}
}

// See golang.org/issue/16481
func TestTransportReturnsUnusedFlowControlSingleWrite(t *testing.T) {
	testTransportReturnsUnusedFlowControl(t, true)
}

// See golang.org/issue/20469
func TestTransportReturnsUnusedFlowControlMultipleWrites(t *testing.T) {
	testTransportReturnsUnusedFlowControl(t, false)
}

// Issue 16612: adjust flow control on open streams when transport
// receives SETTINGS with INITIAL_WINDOW_SIZE from server.
func TestTransportAdjustsFlowControl(t *testing.T) {
	const bodySize = 1 << 20

	tc := newTestClientConn(t)
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	// Don't write our SETTINGS yet.

	body := tc.newRequestBody()
	body.writeBytes(bodySize)
	body.closeWithError(io.EOF)

	req, _ := http.NewRequest("POST", "https://dummy.tld/", body)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)

	gotBytes := int64(0)
	for {
		f := readFrame[*DataFrame](t, tc)
		gotBytes += int64(len(f.Data()))
		// After we've got half the client's initial flow control window's worth
		// of request body data, give it just enough flow control to finish.
		if gotBytes >= initialWindowSize/2 {
			break
		}
	}

	tc.writeSettings(Setting{ID: SettingInitialWindowSize, Val: bodySize})
	tc.writeWindowUpdate(0, bodySize)
	tc.writeSettingsAck()

	tc.wantUnorderedFrames(
		func(f *SettingsFrame) bool { return true },
		func(f *DataFrame) bool {
			gotBytes += int64(len(f.Data()))
			return f.StreamEnded()
		},
	)

	if gotBytes != bodySize {
		t.Fatalf("server received %v bytes of body, want %v", gotBytes, bodySize)
	}

	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	rt.wantStatus(200)
}

// See golang.org/issue/16556
func TestTransportReturnsDataPaddingFlowControl(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
			"content-length", "5000",
		),
	})

	initialConnWindow := tc.inflowWindow(0)
	initialStreamWindow := tc.inflowWindow(rt.streamID())

	pad := make([]byte, 5)
	tc.writeDataPadded(rt.streamID(), false, make([]byte, 5000), pad)

	// Padding flow control should have been returned.
	if got, want := tc.inflowWindow(0), initialConnWindow-5000; got != want {
		t.Errorf("conn inflow window = %v, want %v", got, want)
	}
	if got, want := tc.inflowWindow(rt.streamID()), initialStreamWindow-5000; got != want {
		t.Errorf("stream inflow window = %v, want %v", got, want)
	}
}

// golang.org/issue/16572 -- RoundTrip shouldn't hang when it gets a
// StreamError as a result of the response HEADERS
func TestTransportReturnsErrorOnBadResponseHeaders(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
			"  content-type", "bogus",
		),
	})

	err := rt.err()
	want := StreamError{1, ErrCodeProtocol, headerFieldNameError("  content-type")}
	if !reflect.DeepEqual(err, want) {
		t.Fatalf("RoundTrip error = %#v; want %#v", err, want)
	}

	fr := readFrame[*RSTStreamFrame](t, tc)
	if fr.StreamID != 1 || fr.ErrCode != ErrCodeProtocol {
		t.Errorf("Frame = %v; want RST_STREAM for stream 1 with ErrCodeProtocol", summarizeFrame(fr))
	}
}

// byteAndEOFReader returns is in an io.Reader which reads one byte
// (the underlying byte) and io.EOF at once in its Read call.
type byteAndEOFReader byte

func (b byteAndEOFReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		panic("unexpected useless call")
	}
	p[0] = byte(b)
	return 1, io.EOF
}

// Issue 16788: the Transport had a regression where it started
// sending a spurious DATA frame with a duplicate END_STREAM bit after
// the request body writer goroutine had already read an EOF from the
// Request.Body and included the END_STREAM on a data-carrying DATA
// frame.
//
// Notably, to trigger this, the requests need to use a Request.Body
// which returns (non-0, io.EOF) and also needs to set the ContentLength
// explicitly.
func TestTransportBodyDoubleEndStream(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Nothing.
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", ts.URL, byteAndEOFReader('a'))
		req.ContentLength = 1
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("failure on req %d: %v", i+1, err)
		}
		defer res.Body.Close()
	}
}

// golang.org/issue/16847, golang.org/issue/19103
func TestTransportRequestPathPseudo(t *testing.T) {
	type result struct {
		path string
		err  string
	}
	tests := []struct {
		req  *http.Request
		want result
	}{
		0: {
			req: &http.Request{
				Method: "GET",
				URL: &url.URL{
					Host: "foo.com",
					Path: "/foo",
				},
			},
			want: result{path: "/foo"},
		},
		// In Go 1.7, we accepted paths of "//foo".
		// In Go 1.8, we rejected it (issue 16847).
		// In Go 1.9, we accepted it again (issue 19103).
		1: {
			req: &http.Request{
				Method: "GET",
				URL: &url.URL{
					Host: "foo.com",
					Path: "//foo",
				},
			},
			want: result{path: "//foo"},
		},

		// Opaque with //$Matching_Hostname/path
		2: {
			req: &http.Request{
				Method: "GET",
				URL: &url.URL{
					Scheme: "https",
					Opaque: "//foo.com/path",
					Host:   "foo.com",
					Path:   "/ignored",
				},
			},
			want: result{path: "/path"},
		},

		// Opaque with some other Request.Host instead:
		3: {
			req: &http.Request{
				Method: "GET",
				Host:   "bar.com",
				URL: &url.URL{
					Scheme: "https",
					Opaque: "//bar.com/path",
					Host:   "foo.com",
					Path:   "/ignored",
				},
			},
			want: result{path: "/path"},
		},

		// Opaque without the leading "//":
		4: {
			req: &http.Request{
				Method: "GET",
				URL: &url.URL{
					Opaque: "/path",
					Host:   "foo.com",
					Path:   "/ignored",
				},
			},
			want: result{path: "/path"},
		},

		// Opaque we can't handle:
		5: {
			req: &http.Request{
				Method: "GET",
				URL: &url.URL{
					Scheme: "https",
					Opaque: "//unknown_host/path",
					Host:   "foo.com",
					Path:   "/ignored",
				},
			},
			want: result{err: `invalid request :path "https://unknown_host/path" from URL.Opaque = "//unknown_host/path"`},
		},

		// A CONNECT request:
		6: {
			req: &http.Request{
				Method: "CONNECT",
				URL: &url.URL{
					Host: "foo.com",
				},
			},
			want: result{},
		},
	}
	for i, tt := range tests {
		hbuf := &bytes.Buffer{}
		henc := hpack.NewEncoder(hbuf)

		const addGzipHeader = false
		const peerMaxHeaderListSize = 0xffffffffffffffff
		_, err := encodeRequestHeaders(tt.req, addGzipHeader, peerMaxHeaderListSize, func(name, value string) {
			henc.WriteField(hpack.HeaderField{Name: name, Value: value})
		})
		hdrs := hbuf.Bytes()
		var got result
		hpackDec := hpack.NewDecoder(initialHeaderTableSize, func(f hpack.HeaderField) {
			if f.Name == ":path" {
				got.path = f.Value
			}
		})
		if err != nil {
			got.err = err.Error()
		} else if len(hdrs) > 0 {
			if _, err := hpackDec.Write(hdrs); err != nil {
				t.Errorf("%d. bogus hpack: %v", i, err)
				continue
			}
		}
		if got != tt.want {
			t.Errorf("%d. got %+v; want %+v", i, got, tt.want)
		}

	}

}

// golang.org/issue/17071 -- don't sniff the first byte of the request body
// before we've determined that the ClientConn is usable.
func TestRoundTripDoesntConsumeRequestBodyEarly(t *testing.T) {
	const body = "foo"
	req, _ := http.NewRequest("POST", "http://foo.com/", io.NopCloser(strings.NewReader(body)))
	cc := &ClientConn{
		closed:      true,
		reqHeaderMu: make(chan struct{}, 1),
		t:           &Transport{},
	}
	_, err := cc.RoundTrip(req)
	if err != errClientConnUnusable {
		t.Fatalf("RoundTrip = %v; want errClientConnUnusable", err)
	}
	slurp, err := io.ReadAll(req.Body)
	if err != nil {
		t.Errorf("ReadAll = %v", err)
	}
	if string(slurp) != body {
		t.Errorf("Body = %q; want %q", slurp, body)
	}
}

func TestClientConnPing(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	ctx := context.Background()
	cc, err := tr.dialClientConn(ctx, ts.Listener.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = cc.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Issue 16974: if the server sent a DATA frame after the user
// canceled the Transport's Request, the Transport previously wrote to a
// closed pipe, got an error, and ended up closing the whole TCP
// connection.
func TestTransportCancelDataResponseRace(t *testing.T) {
	cancel := make(chan struct{})
	clientGotResponse := make(chan bool, 1)

	const msg = "Hello."
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/hello") {
			time.Sleep(50 * time.Millisecond)
			io.WriteString(w, msg)
			return
		}
		for i := 0; i < 50; i++ {
			io.WriteString(w, "Some data.")
			w.(http.Flusher).Flush()
			if i == 2 {
				<-clientGotResponse
				close(cancel)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	c := &http.Client{Transport: tr}
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.Cancel = cancel
	res, err := c.Do(req)
	clientGotResponse <- true
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, res.Body); err == nil {
		t.Fatal("unexpected success")
	}

	res, err = c.Get(ts.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	slurp, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(slurp) != msg {
		t.Errorf("Got = %q; want %q", slurp, msg)
	}
}

// Issue 21316: It should be safe to reuse an http.Request after the
// request has completed.
func TestTransportNoRaceOnRequestObjectAfterRequestComplete(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, "body")
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	req, _ := http.NewRequest("GET", ts.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("error reading response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("error closing response body: %v", err)
	}

	// This access of req.Header should not race with code in the transport.
	req.Header = http.Header{}
}

func TestTransportCloseAfterLostPing(t *testing.T) {
	tc := newTestClientConn(t, func(tr *Transport) {
		tr.PingTimeout = 1 * time.Second
		tr.ReadIdleTimeout = 1 * time.Second
	})
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)
	tc.wantFrameType(FrameHeaders)

	tc.advance(1 * time.Second)
	tc.wantFrameType(FramePing)

	tc.advance(1 * time.Second)
	err := rt.err()
	if err == nil || !strings.Contains(err.Error(), "client connection lost") {
		t.Fatalf("expected to get error about \"connection lost\", got %v", err)
	}
}

func TestTransportPingWriteBlocks(t *testing.T) {
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {},
	)
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			s, c := net.Pipe() // unbuffered, unlike a TCP conn
			go func() {
				// Read initial handshake frames.
				// Without this, we block indefinitely in newClientConn,
				// and never get to the point of sending a PING.
				var buf [1024]byte
				s.Read(buf[:])
			}()
			return c, nil
		},
		PingTimeout:     1 * time.Millisecond,
		ReadIdleTimeout: 1 * time.Millisecond,
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}
	_, err := c.Get(ts.URL)
	if err == nil {
		t.Fatalf("Get = nil, want error")
	}
}

func TestTransportPingWhenReadingMultiplePings(t *testing.T) {
	tc := newTestClientConn(t, func(tr *Transport) {
		tr.ReadIdleTimeout = 1000 * time.Millisecond
	})
	tc.greet()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})

	for i := 0; i < 5; i++ {
		// No ping yet...
		tc.advance(999 * time.Millisecond)
		if f := tc.readFrame(); f != nil {
			t.Fatalf("unexpected frame: %v", f)
		}

		// ...ping now.
		tc.advance(1 * time.Millisecond)
		f := readFrame[*PingFrame](t, tc)
		tc.writePing(true, f.Data)
	}

	// Cancel the request, Transport resets it and returns an error from body reads.
	cancel()
	tc.sync()

	tc.wantFrameType(FrameRSTStream)
	_, err := rt.readBody()
	if err == nil {
		t.Fatalf("Response.Body.Read() = %v, want error", err)
	}
}

func TestTransportPingWhenReadingPingDisabled(t *testing.T) {
	tc := newTestClientConn(t, func(tr *Transport) {
		tr.ReadIdleTimeout = 0 // PINGs disabled
	})
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})

	// No PING is sent, even after a long delay.
	tc.advance(1 * time.Minute)
	if f := tc.readFrame(); f != nil {
		t.Fatalf("unexpected frame: %v", f)
	}
}

func TestTransportRetryAfterGOAWAYNoRetry(t *testing.T) {
	tt := newTestTransport(t)

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tt.roundTrip(req)

	// First attempt: Server sends a GOAWAY with an error and
	// a MaxStreamID less than the request ID.
	// This probably indicates that there was something wrong with our request,
	// so we don't retry it.
	tc := tt.getConn()
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	tc.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	tc.writeSettings()
	tc.writeGoAway(0 /*max id*/, ErrCodeInternal, nil)
	if rt.err() == nil {
		t.Fatalf("after GOAWAY, RoundTrip is not done, want error")
	}
}

func TestTransportRetryAfterGOAWAYRetry(t *testing.T) {
	tt := newTestTransport(t)

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tt.roundTrip(req)

	// First attempt: Server sends a GOAWAY with ErrCodeNo and
	// a MaxStreamID less than the request ID.
	// We take the server at its word that nothing has really gone wrong,
	// and retry the request.
	tc := tt.getConn()
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	tc.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	tc.writeSettings()
	tc.writeGoAway(0 /*max id*/, ErrCodeNo, nil)
	if rt.done() {
		t.Fatalf("after GOAWAY, RoundTrip is done; want it to be retrying")
	}

	// Second attempt succeeds on a new connection.
	tc = tt.getConn()
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	tc.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	tc.writeSettings()
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   1,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})

	rt.wantStatus(200)
}

func TestTransportRetryAfterGOAWAYSecondRequest(t *testing.T) {
	tt := newTestTransport(t)

	// First request succeeds.
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt1 := tt.roundTrip(req)
	tc := tt.getConn()
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	tc.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	tc.writeSettings()
	tc.wantFrameType(FrameSettings) // Settings ACK
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   1,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	rt1.wantStatus(200)

	// Second request: Server sends a GOAWAY with
	// a MaxStreamID less than the request ID.
	// The server says it didn't see this request,
	// so we retry it on a new connection.
	req, _ = http.NewRequest("GET", "https://dummy.tld/", nil)
	rt2 := tt.roundTrip(req)

	// Second request, first attempt.
	tc.wantHeaders(wantHeader{
		streamID:  3,
		endStream: true,
	})
	tc.writeSettings()
	tc.writeGoAway(1 /*max id*/, ErrCodeProtocol, nil)
	if rt2.done() {
		t.Fatalf("after GOAWAY, RoundTrip is done; want it to be retrying")
	}

	// Second request, second attempt.
	tc = tt.getConn()
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	tc.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	tc.writeSettings()
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   1,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	rt2.wantStatus(200)
}

func TestTransportRetryAfterRefusedStream(t *testing.T) {
	tt := newTestTransport(t)

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tt.roundTrip(req)

	// First attempt: Server sends a RST_STREAM.
	tc := tt.getConn()
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	tc.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	tc.writeSettings()
	tc.wantFrameType(FrameSettings) // settings ACK
	tc.writeRSTStream(1, ErrCodeRefusedStream)
	if rt.done() {
		t.Fatalf("after RST_STREAM, RoundTrip is done; want it to be retrying")
	}

	// Second attempt succeeds on the same connection.
	tc.wantHeaders(wantHeader{
		streamID:  3,
		endStream: true,
	})
	tc.writeSettings()
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   3,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "204",
		),
	})

	rt.wantStatus(204)
}

func TestTransportRetryHasLimit(t *testing.T) {
	tt := newTestTransport(t)

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tt.roundTrip(req)

	// First attempt: Server sends a GOAWAY.
	tc := tt.getConn()
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)

	var totalDelay time.Duration
	count := 0
	for streamID := uint32(1); ; streamID += 2 {
		count++
		tc.wantHeaders(wantHeader{
			streamID:  streamID,
			endStream: true,
		})
		if streamID == 1 {
			tc.writeSettings()
			tc.wantFrameType(FrameSettings) // settings ACK
		}
		tc.writeRSTStream(streamID, ErrCodeRefusedStream)

		d, scheduled := tt.group.TimeUntilEvent()
		if !scheduled {
			if streamID == 1 {
				continue
			}
			break
		}
		totalDelay += d
		if totalDelay > 5*time.Minute {
			t.Fatalf("RoundTrip still retrying after %v, should have given up", totalDelay)
		}
		tt.advance(d)
	}
	if got, want := count, 5; got < count {
		t.Errorf("RoundTrip made %v attempts, want at least %v", got, want)
	}
	if rt.err() == nil {
		t.Errorf("RoundTrip succeeded, want error")
	}
}

func TestTransportResponseDataBeforeHeaders(t *testing.T) {
	// Discard log output complaining about protocol error.
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) }) // after other cleanup is done

	tc := newTestClientConn(t)
	tc.greet()

	// First request is normal to ensure the check is per stream and not per connection.
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt1 := tc.roundTrip(req)
	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt1.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	rt1.wantStatus(200)

	// Second request returns a DATA frame with no HEADERS.
	rt2 := tc.roundTrip(req)
	tc.wantFrameType(FrameHeaders)
	tc.writeData(rt2.streamID(), true, []byte("payload"))
	if err, ok := rt2.err().(StreamError); !ok || err.Code != ErrCodeProtocol {
		t.Fatalf("expected stream PROTOCOL_ERROR, got: %v", err)
	}
}

func TestTransportMaxFrameReadSize(t *testing.T) {
	for _, test := range []struct {
		maxReadFrameSize uint32
		want             uint32
	}{{
		maxReadFrameSize: 64000,
		want:             64000,
	}, {
		maxReadFrameSize: 1024,
		want:             minMaxFrameSize,
	}} {
		t.Run(fmt.Sprint(test.maxReadFrameSize), func(t *testing.T) {
			tc := newTestClientConn(t, func(tr *Transport) {
				tr.MaxReadFrameSize = test.maxReadFrameSize
			})

			fr := readFrame[*SettingsFrame](t, tc)
			got, ok := fr.Value(SettingMaxFrameSize)
			if !ok {
				t.Errorf("Transport.MaxReadFrameSize = %v; server got no setting, want %v", test.maxReadFrameSize, test.want)
			} else if got != test.want {
				t.Errorf("Transport.MaxReadFrameSize = %v; server got %v, want %v", test.maxReadFrameSize, got, test.want)
			}
		})
	}
}

func TestTransportRequestsLowServerLimit(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
	}, func(s *Server) {
		s.MaxConcurrentStreams = 1
	})

	var (
		connCountMu sync.Mutex
		connCount   int
	)
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			connCountMu.Lock()
			defer connCountMu.Unlock()
			connCount++
			return tls.Dial(network, addr, cfg)
		},
	}
	defer tr.CloseIdleConnections()

	const reqCount = 3
	for i := 0; i < reqCount; i++ {
		req, err := http.NewRequest("GET", ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := res.StatusCode, 200; got != want {
			t.Errorf("StatusCode = %v; want %v", got, want)
		}
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
	}

	if connCount != 1 {
		t.Errorf("created %v connections for %v requests, want 1", connCount, reqCount)
	}
}

// tests Transport.StrictMaxConcurrentStreams
func TestTransportRequestsStallAtServerLimit(t *testing.T) {
	const maxConcurrent = 2

	tc := newTestClientConn(t, func(tr *Transport) {
		tr.StrictMaxConcurrentStreams = true
	})
	tc.greet(Setting{SettingMaxConcurrentStreams, maxConcurrent})

	cancelClientRequest := make(chan struct{})

	// Start maxConcurrent+2 requests.
	// The server does not respond to any of them yet.
	var rts []*testRoundTrip
	for k := 0; k < maxConcurrent+2; k++ {
		req, _ := http.NewRequest("GET", fmt.Sprintf("https://dummy.tld/%d", k), nil)
		if k == maxConcurrent {
			req.Cancel = cancelClientRequest
		}
		rt := tc.roundTrip(req)
		rts = append(rts, rt)

		if k < maxConcurrent {
			// We are under the stream limit, so the client sends the request.
			tc.wantHeaders(wantHeader{
				streamID:  rt.streamID(),
				endStream: true,
				header: http.Header{
					":authority": []string{"dummy.tld"},
					":method":    []string{"GET"},
					":path":      []string{fmt.Sprintf("/%d", k)},
				},
			})
		} else {
			// We have reached the stream limit,
			// so the client cannot send the request.
			if fr := tc.readFrame(); fr != nil {
				t.Fatalf("after making new request while at stream limit, got unexpected frame: %v", fr)
			}
		}

		if rt.done() {
			t.Fatalf("rt %v done", k)
		}
	}

	// Cancel the maxConcurrent'th request.
	// The request should fail.
	close(cancelClientRequest)
	tc.sync()
	if err := rts[maxConcurrent].err(); err == nil {
		t.Fatalf("RoundTrip(%d) should have failed due to cancel, did not", maxConcurrent)
	}

	// No requests should be complete, except for the canceled one.
	for i, rt := range rts {
		if i != maxConcurrent && rt.done() {
			t.Fatalf("RoundTrip(%d) is done, but should not be", i)
		}
	}

	// Server responds to a request, unblocking the last one.
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rts[0].streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	tc.wantHeaders(wantHeader{
		streamID:  rts[maxConcurrent+1].streamID(),
		endStream: true,
		header: http.Header{
			":authority": []string{"dummy.tld"},
			":method":    []string{"GET"},
			":path":      []string{fmt.Sprintf("/%d", maxConcurrent+1)},
		},
	})
	rts[0].wantStatus(200)
}

func TestTransportMaxDecoderHeaderTableSize(t *testing.T) {
	var reqSize, resSize uint32 = 8192, 16384
	tc := newTestClientConn(t, func(tr *Transport) {
		tr.MaxDecoderHeaderTableSize = reqSize
	})

	fr := readFrame[*SettingsFrame](t, tc)
	if v, ok := fr.Value(SettingHeaderTableSize); !ok {
		t.Fatalf("missing SETTINGS_HEADER_TABLE_SIZE setting")
	} else if v != reqSize {
		t.Fatalf("received SETTINGS_HEADER_TABLE_SIZE = %d, want %d", v, reqSize)
	}

	tc.writeSettings(Setting{SettingHeaderTableSize, resSize})
	tc.cc.mu.Lock()
	defer tc.cc.mu.Unlock()
	if got, want := tc.cc.peerMaxHeaderTableSize, resSize; got != want {
		t.Fatalf("peerHeaderTableSize = %d, want %d", got, want)
	}
}

func TestTransportMaxEncoderHeaderTableSize(t *testing.T) {
	var peerAdvertisedMaxHeaderTableSize uint32 = 16384
	tc := newTestClientConn(t, func(tr *Transport) {
		tr.MaxEncoderHeaderTableSize = 8192
	})
	tc.greet(Setting{SettingHeaderTableSize, peerAdvertisedMaxHeaderTableSize})

	if got, want := tc.cc.henc.MaxDynamicTableSize(), tc.tr.MaxEncoderHeaderTableSize; got != want {
		t.Fatalf("henc.MaxDynamicTableSize() = %d, want %d", got, want)
	}
}

func TestAuthorityAddr(t *testing.T) {
	tests := []struct {
		scheme, authority string
		want              string
	}{
		{"http", "foo.com", "foo.com:80"},
		{"https", "foo.com", "foo.com:443"},
		{"https", "foo.com:", "foo.com:443"},
		{"https", "foo.com:1234", "foo.com:1234"},
		{"https", "1.2.3.4:1234", "1.2.3.4:1234"},
		{"https", "1.2.3.4", "1.2.3.4:443"},
		{"https", "1.2.3.4:", "1.2.3.4:443"},
		{"https", "[::1]:1234", "[::1]:1234"},
		{"https", "[::1]", "[::1]:443"},
		{"https", "[::1]:", "[::1]:443"},
	}
	for _, tt := range tests {
		got := authorityAddr(tt.scheme, tt.authority)
		if got != tt.want {
			t.Errorf("authorityAddr(%q, %q) = %q; want %q", tt.scheme, tt.authority, got, tt.want)
		}
	}
}

// Issue 20448: stop allocating for DATA frames' payload after
// Response.Body.Close is called.
func TestTransportAllocationsAfterResponseBodyClose(t *testing.T) {
	megabyteZero := make([]byte, 1<<20)

	writeErr := make(chan error, 1)

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		var sum int64
		for i := 0; i < 100; i++ {
			n, err := w.Write(megabyteZero)
			sum += int64(n)
			if err != nil {
				writeErr <- err
				return
			}
		}
		t.Logf("wrote all %d bytes", sum)
		writeErr <- nil
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}
	res, err := c.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	var buf [1]byte
	if _, err := res.Body.Read(buf[:]); err != nil {
		t.Error(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Error(err)
	}

	trb, ok := res.Body.(transportResponseBody)
	if !ok {
		t.Fatalf("res.Body = %T; want transportResponseBody", res.Body)
	}
	if trb.cs.bufPipe.b != nil {
		t.Errorf("response body pipe is still open")
	}

	gotErr := <-writeErr
	if gotErr == nil {
		t.Errorf("Handler unexpectedly managed to write its entire response without getting an error")
	} else if gotErr != errStreamClosed {
		t.Errorf("Handler Write err = %v; want errStreamClosed", gotErr)
	}
}

// Issue 18891: make sure Request.Body == NoBody means no DATA frame
// is ever sent, even if empty.
func TestTransportNoBodyMeansNoDATA(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", http.NoBody)
	rt := tc.roundTrip(req)

	tc.wantHeaders(wantHeader{
		streamID:  rt.streamID(),
		endStream: true, // END_STREAM should be set when body is http.NoBody
		header: http.Header{
			":authority": []string{"dummy.tld"},
			":method":    []string{"GET"},
			":path":      []string{"/"},
		},
	})
	if fr := tc.readFrame(); fr != nil {
		t.Fatalf("unexpected frame after headers: %v", fr)
	}
}

func benchSimpleRoundTrip(b *testing.B, nReqHeaders, nResHeader int) {
	disableGoroutineTracking(b)
	b.ReportAllocs()
	ts := newTestServer(b,
		func(w http.ResponseWriter, r *http.Request) {
			for i := 0; i < nResHeader; i++ {
				name := fmt.Sprint("A-", i)
				w.Header().Set(name, "*")
			}
		},
		optQuiet,
	)

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		b.Fatal(err)
	}

	for i := 0; i < nReqHeaders; i++ {
		name := fmt.Sprint("A-", i)
		req.Header.Set(name, "*")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res, err := tr.RoundTrip(req)
		if err != nil {
			if res != nil {
				res.Body.Close()
			}
			b.Fatalf("RoundTrip err = %v; want nil", err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b.Fatalf("Response code = %v; want %v", res.StatusCode, http.StatusOK)
		}
	}
}

type infiniteReader struct{}

func (r infiniteReader) Read(b []byte) (int, error) {
	return len(b), nil
}

// Issue 20521: it is not an error to receive a response and end stream
// from the server without the body being consumed.
func TestTransportResponseAndResetWithoutConsumingBodyRace(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	// The request body needs to be big enough to trigger flow control.
	req, _ := http.NewRequest("PUT", ts.URL, infiniteReader{})
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Response code = %v; want %v", res.StatusCode, http.StatusOK)
	}
}

// Verify transport doesn't crash when receiving bogus response lacking a :status header.
// Issue 22880.
func TestTransportHandlesInvalidStatuslessResponse(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false, // we'll send some DATA to try to crash the transport
		BlockFragment: tc.makeHeaderBlockFragment(
			"content-type", "text/html", // no :status header
		),
	})
	tc.writeData(rt.streamID(), true, []byte("payload"))
}

func BenchmarkClientRequestHeaders(b *testing.B) {
	b.Run("   0 Headers", func(b *testing.B) { benchSimpleRoundTrip(b, 0, 0) })
	b.Run("  10 Headers", func(b *testing.B) { benchSimpleRoundTrip(b, 10, 0) })
	b.Run(" 100 Headers", func(b *testing.B) { benchSimpleRoundTrip(b, 100, 0) })
	b.Run("1000 Headers", func(b *testing.B) { benchSimpleRoundTrip(b, 1000, 0) })
}

func BenchmarkClientResponseHeaders(b *testing.B) {
	b.Run("   0 Headers", func(b *testing.B) { benchSimpleRoundTrip(b, 0, 0) })
	b.Run("  10 Headers", func(b *testing.B) { benchSimpleRoundTrip(b, 0, 10) })
	b.Run(" 100 Headers", func(b *testing.B) { benchSimpleRoundTrip(b, 0, 100) })
	b.Run("1000 Headers", func(b *testing.B) { benchSimpleRoundTrip(b, 0, 1000) })
}

func BenchmarkDownloadFrameSize(b *testing.B) {
	b.Run(" 16k Frame", func(b *testing.B) { benchLargeDownloadRoundTrip(b, 16*1024) })
	b.Run(" 64k Frame", func(b *testing.B) { benchLargeDownloadRoundTrip(b, 64*1024) })
	b.Run("128k Frame", func(b *testing.B) { benchLargeDownloadRoundTrip(b, 128*1024) })
	b.Run("256k Frame", func(b *testing.B) { benchLargeDownloadRoundTrip(b, 256*1024) })
	b.Run("512k Frame", func(b *testing.B) { benchLargeDownloadRoundTrip(b, 512*1024) })
}
func benchLargeDownloadRoundTrip(b *testing.B, frameSize uint32) {
	disableGoroutineTracking(b)
	const transferSize = 1024 * 1024 * 1024 // must be multiple of 1M
	b.ReportAllocs()
	ts := newTestServer(b,
		func(w http.ResponseWriter, r *http.Request) {
			// test 1GB transfer
			w.Header().Set("Content-Length", strconv.Itoa(transferSize))
			w.Header().Set("Content-Transfer-Encoding", "binary")
			var data [1024 * 1024]byte
			for i := 0; i < transferSize/(1024*1024); i++ {
				w.Write(data[:])
			}
		}, optQuiet,
	)

	tr := &Transport{TLSClientConfig: tlsConfigInsecure, MaxReadFrameSize: frameSize}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		b.Fatal(err)
	}

	b.N = 3
	b.SetBytes(transferSize)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res, err := tr.RoundTrip(req)
		if err != nil {
			if res != nil {
				res.Body.Close()
			}
			b.Fatalf("RoundTrip err = %v; want nil", err)
		}
		data, _ := io.ReadAll(res.Body)
		if len(data) != transferSize {
			b.Fatalf("Response length invalid")
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b.Fatalf("Response code = %v; want %v", res.StatusCode, http.StatusOK)
		}
	}
}

func activeStreams(cc *ClientConn) int {
	count := 0
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for _, cs := range cc.streams {
		select {
		case <-cs.abort:
		default:
			count++
		}
	}
	return count
}

type closeMode int

const (
	closeAtHeaders closeMode = iota
	closeAtBody
	shutdown
	shutdownCancel
)

// See golang.org/issue/17292
func testClientConnClose(t *testing.T, closeMode closeMode) {
	clientDone := make(chan struct{})
	defer close(clientDone)
	handlerDone := make(chan struct{})
	closeDone := make(chan struct{})
	beforeHeader := func() {}
	bodyWrite := func(w http.ResponseWriter) {}
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		beforeHeader()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		bodyWrite(w)
		select {
		case <-w.(http.CloseNotifier).CloseNotify():
			// client closed connection before completion
			if closeMode == shutdown || closeMode == shutdownCancel {
				t.Error("expected request to complete")
			}
		case <-clientDone:
			if closeMode == closeAtHeaders || closeMode == closeAtBody {
				t.Error("expected connection closed by client")
			}
		}
	})
	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	ctx := context.Background()
	cc, err := tr.dialClientConn(ctx, ts.Listener.Addr().String(), false)
	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if closeMode == closeAtHeaders {
		beforeHeader = func() {
			if err := cc.Close(); err != nil {
				t.Error(err)
			}
			close(closeDone)
		}
	}
	var sendBody chan struct{}
	if closeMode == closeAtBody {
		sendBody = make(chan struct{})
		bodyWrite = func(w http.ResponseWriter) {
			<-sendBody
			b := make([]byte, 32)
			w.Write(b)
			w.(http.Flusher).Flush()
			if err := cc.Close(); err != nil {
				t.Errorf("unexpected ClientConn close error: %v", err)
			}
			close(closeDone)
			w.Write(b)
			w.(http.Flusher).Flush()
		}
	}
	res, err := cc.RoundTrip(req)
	if res != nil {
		defer res.Body.Close()
	}
	if closeMode == closeAtHeaders {
		got := fmt.Sprint(err)
		want := "http2: client connection force closed via ClientConn.Close"
		if got != want {
			t.Fatalf("RoundTrip error = %v, want %v", got, want)
		}
	} else {
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if got, want := activeStreams(cc), 1; got != want {
			t.Errorf("got %d active streams, want %d", got, want)
		}
	}
	switch closeMode {
	case shutdownCancel:
		if err = cc.Shutdown(canceledCtx); err != context.Canceled {
			t.Errorf("got %v, want %v", err, context.Canceled)
		}
		if cc.closing == false {
			t.Error("expected closing to be true")
		}
		if cc.CanTakeNewRequest() == true {
			t.Error("CanTakeNewRequest to return false")
		}
		if v, want := len(cc.streams), 1; v != want {
			t.Errorf("expected %d active streams, got %d", want, v)
		}
		clientDone <- struct{}{}
		<-handlerDone
	case shutdown:
		wait := make(chan struct{})
		shutdownEnterWaitStateHook = func() {
			close(wait)
			shutdownEnterWaitStateHook = func() {}
		}
		defer func() { shutdownEnterWaitStateHook = func() {} }()
		shutdown := make(chan struct{}, 1)
		go func() {
			if err = cc.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
			close(shutdown)
		}()
		// Let the shutdown to enter wait state
		<-wait
		cc.mu.Lock()
		if cc.closing == false {
			t.Error("expected closing to be true")
		}
		cc.mu.Unlock()
		if cc.CanTakeNewRequest() == true {
			t.Error("CanTakeNewRequest to return false")
		}
		if got, want := activeStreams(cc), 1; got != want {
			t.Errorf("got %d active streams, want %d", got, want)
		}
		// Let the active request finish
		clientDone <- struct{}{}
		// Wait for the shutdown to end
		select {
		case <-shutdown:
		case <-time.After(2 * time.Second):
			t.Fatal("expected server connection to close")
		}
	case closeAtHeaders, closeAtBody:
		if closeMode == closeAtBody {
			go close(sendBody)
			if _, err := io.Copy(io.Discard, res.Body); err == nil {
				t.Error("expected a Copy error, got nil")
			}
		}
		<-closeDone
		if got, want := activeStreams(cc), 0; got != want {
			t.Errorf("got %d active streams, want %d", got, want)
		}
		// wait for server to get the connection close notice
		select {
		case <-handlerDone:
		case <-time.After(2 * time.Second):
			t.Fatal("expected server connection to close")
		}
	}
}

// The client closes the connection just after the server got the client's HEADERS
// frame, but before the server sends its HEADERS response back. The expected
// result is an error on RoundTrip explaining the client closed the connection.
func TestClientConnCloseAtHeaders(t *testing.T) {
	testClientConnClose(t, closeAtHeaders)
}

// The client closes the connection between two server's response DATA frames.
// The expected behavior is a response body io read error on the client.
func TestClientConnCloseAtBody(t *testing.T) {
	testClientConnClose(t, closeAtBody)
}

// The client sends a GOAWAY frame before the server finished processing a request.
// We expect the connection not to close until the request is completed.
func TestClientConnShutdown(t *testing.T) {
	testClientConnClose(t, shutdown)
}

// The client sends a GOAWAY frame before the server finishes processing a request,
// but cancels the passed context before the request is completed. The expected
// behavior is the client closing the connection after the context is canceled.
func TestClientConnShutdownCancel(t *testing.T) {
	testClientConnClose(t, shutdownCancel)
}

// Issue 25009: use Request.GetBody if present, even if it seems like
// we might not need it. Apparently something else can still read from
// the original request body. Data race? In any case, rewinding
// unconditionally on retry is a nicer model anyway and should
// simplify code in the future (after the Go 1.11 freeze)
func TestTransportUsesGetBodyWhenPresent(t *testing.T) {
	calls := 0
	someBody := func() io.ReadCloser {
		return struct{ io.ReadCloser }{io.NopCloser(bytes.NewReader(nil))}
	}
	req := &http.Request{
		Body: someBody(),
		GetBody: func() (io.ReadCloser, error) {
			calls++
			return someBody(), nil
		},
	}

	req2, err := shouldRetryRequest(req, errClientConnUnusable)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("Calls = %d; want 1", calls)
	}
	if req2 == req {
		t.Error("req2 changed")
	}
	if req2 == nil {
		t.Fatal("req2 is nil")
	}
	if req2.Body == nil {
		t.Fatal("req2.Body is nil")
	}
	if req2.GetBody == nil {
		t.Fatal("req2.GetBody is nil")
	}
	if req2.Body == req.Body {
		t.Error("req2.Body unchanged")
	}
}

// Issue 22891: verify that the "https" altproto we register with net/http
// is a certain type: a struct with one field with our *http2.Transport in it.
func TestNoDialH2RoundTripperType(t *testing.T) {
	t1 := new(http.Transport)
	t2 := new(Transport)
	rt := noDialH2RoundTripper{t2}
	if err := registerHTTPSProtocol(t1, rt); err != nil {
		t.Fatal(err)
	}
	rv := reflect.ValueOf(rt)
	if rv.Type().Kind() != reflect.Struct {
		t.Fatalf("kind = %v; net/http expects struct", rv.Type().Kind())
	}
	if n := rv.Type().NumField(); n != 1 {
		t.Fatalf("fields = %d; net/http expects 1", n)
	}
	v := rv.Field(0)
	if _, ok := v.Interface().(*Transport); !ok {
		t.Fatalf("wrong kind %T; want *Transport", v.Interface())
	}
}

type errReader struct {
	body []byte
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.body) > 0 {
		n := copy(p, r.body)
		r.body = r.body[n:]
		return n, nil
	}
	return 0, r.err
}

func testTransportBodyReadError(t *testing.T, body []byte) {
	tc := newTestClientConn(t)
	tc.greet()

	bodyReadError := errors.New("body read error")
	b := tc.newRequestBody()
	b.Write(body)
	b.closeWithError(bodyReadError)
	req, _ := http.NewRequest("PUT", "https://dummy.tld/", b)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	var receivedBody []byte
readFrames:
	for {
		switch f := tc.readFrame().(type) {
		case *DataFrame:
			receivedBody = append(receivedBody, f.Data()...)
		case *RSTStreamFrame:
			break readFrames
		default:
			t.Fatalf("unexpected frame: %v", f)
		case nil:
			t.Fatalf("transport is idle, want RST_STREAM")
		}
	}
	if !bytes.Equal(receivedBody, body) {
		t.Fatalf("body: %q; expected %q", receivedBody, body)
	}

	if err := rt.err(); err != bodyReadError {
		t.Fatalf("err = %v; want %v", err, bodyReadError)
	}

	if got := activeStreams(tc.cc); got != 0 {
		t.Fatalf("active streams count: %v; want 0", got)
	}
}

func TestTransportBodyReadError_Immediately(t *testing.T) { testTransportBodyReadError(t, nil) }
func TestTransportBodyReadError_Some(t *testing.T)        { testTransportBodyReadError(t, []byte("123")) }

// Issue 32254: verify that the client sends END_STREAM flag eagerly with the last
// (or in this test-case the only one) request body data frame, and does not send
// extra zero-len data frames.
func TestTransportBodyEagerEndStream(t *testing.T) {
	const reqBody = "some request body"
	const resBody = "some response body"

	tc := newTestClientConn(t)
	tc.greet()

	body := strings.NewReader(reqBody)
	req, _ := http.NewRequest("PUT", "https://dummy.tld/", body)
	tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	f := readFrame[*DataFrame](t, tc)
	if !f.StreamEnded() {
		t.Fatalf("data frame without END_STREAM %v", f)
	}
}

type chunkReader struct {
	chunks [][]byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) > 0 {
		n := copy(p, r.chunks[0])
		r.chunks = r.chunks[1:]
		return n, nil
	}
	panic("shouldn't read this many times")
}

// Issue 32254: if the request body is larger than the specified
// content length, the client should refuse to send the extra part
// and abort the stream.
//
// In _len3 case, the first Read() matches the expected content length
// but the second read returns more data.
//
// In _len2 case, the first Read() exceeds the expected content length.
func TestTransportBodyLargerThanSpecifiedContentLength_len3(t *testing.T) {
	body := &chunkReader{[][]byte{
		[]byte("123"),
		[]byte("456"),
	}}
	testTransportBodyLargerThanSpecifiedContentLength(t, body, 3)
}

func TestTransportBodyLargerThanSpecifiedContentLength_len2(t *testing.T) {
	body := &chunkReader{[][]byte{
		[]byte("123"),
	}}
	testTransportBodyLargerThanSpecifiedContentLength(t, body, 2)
}

func testTransportBodyLargerThanSpecifiedContentLength(t *testing.T, body *chunkReader, contentLen int64) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		r.Body.Read(make([]byte, 6))
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	req, _ := http.NewRequest("POST", ts.URL, body)
	req.ContentLength = contentLen
	_, err := tr.RoundTrip(req)
	if err != errReqBodyTooLong {
		t.Fatalf("expected %v, got %v", errReqBodyTooLong, err)
	}
}

func TestClientConnTooIdle(t *testing.T) {
	tests := []struct {
		cc   func() *ClientConn
		want bool
	}{
		{
			func() *ClientConn {
				return &ClientConn{idleTimeout: 5 * time.Second, lastIdle: time.Now().Add(-10 * time.Second)}
			},
			true,
		},
		{
			func() *ClientConn {
				return &ClientConn{idleTimeout: 5 * time.Second, lastIdle: time.Time{}}
			},
			false,
		},
		{
			func() *ClientConn {
				return &ClientConn{idleTimeout: 60 * time.Second, lastIdle: time.Now().Add(-10 * time.Second)}
			},
			false,
		},
		{
			func() *ClientConn {
				return &ClientConn{idleTimeout: 0, lastIdle: time.Now().Add(-10 * time.Second)}
			},
			false,
		},
	}
	for i, tt := range tests {
		got := tt.cc().tooIdleLocked()
		if got != tt.want {
			t.Errorf("%d. got %v; want %v", i, got, tt.want)
		}
	}
}

type fakeConnErr struct {
	net.Conn
	writeErr error
	closed   bool
}

func (fce *fakeConnErr) Write(b []byte) (n int, err error) {
	return 0, fce.writeErr
}

func (fce *fakeConnErr) Close() error {
	fce.closed = true
	return nil
}

// issue 39337: close the connection on a failed write
func TestTransportNewClientConnCloseOnWriteError(t *testing.T) {
	tr := &Transport{}
	writeErr := errors.New("write error")
	fakeConn := &fakeConnErr{writeErr: writeErr}
	_, err := tr.NewClientConn(fakeConn)
	if err != writeErr {
		t.Fatalf("expected %v, got %v", writeErr, err)
	}
	if !fakeConn.closed {
		t.Error("expected closed conn")
	}
}

func TestTransportRoundtripCloseOnWriteError(t *testing.T) {
	req, err := http.NewRequest("GET", "https://dummy.tld/", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	ctx := context.Background()
	cc, err := tr.dialClientConn(ctx, ts.Listener.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}

	writeErr := errors.New("write error")
	cc.wmu.Lock()
	cc.werr = writeErr
	cc.wmu.Unlock()

	_, err = cc.RoundTrip(req)
	if err != writeErr {
		t.Fatalf("expected %v, got %v", writeErr, err)
	}

	cc.mu.Lock()
	closed := cc.closed
	cc.mu.Unlock()
	if !closed {
		t.Fatal("expected closed")
	}
}

// Issue 31192: A failed request may be retried if the body has not been read
// already. If the request body has started to be sent, one must wait until it
// is completed.
func TestTransportBodyRewindRace(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		return
	})

	tr := &http.Transport{
		TLSClientConfig: tlsConfigInsecure,
		MaxConnsPerHost: 1,
	}
	err := ConfigureTransport(tr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: tr,
	}

	const clients = 50

	var wg sync.WaitGroup
	wg.Add(clients)
	for i := 0; i < clients; i++ {
		req, err := http.NewRequest("POST", ts.URL, bytes.NewBufferString("abcdef"))
		if err != nil {
			t.Fatalf("unexpect new request error: %v", err)
		}

		go func() {
			defer wg.Done()
			res, err := client.Do(req)
			if err == nil {
				res.Body.Close()
			}
		}()
	}

	wg.Wait()
}

// Issue 42498: A request with a body will never be sent if the stream is
// reset prior to sending any data.
func TestTransportServerResetStreamAtHeaders(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	})

	tr := &http.Transport{
		TLSClientConfig:       tlsConfigInsecure,
		MaxConnsPerHost:       1,
		ExpectContinueTimeout: 10 * time.Second,
	}

	err := ConfigureTransport(tr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: tr,
	}

	req, err := http.NewRequest("POST", ts.URL, errorReader{io.EOF})
	if err != nil {
		t.Fatalf("unexpect new request error: %v", err)
	}
	req.ContentLength = 0 // so transport is tempted to sniff it
	req.Header.Set("Expect", "100-continue")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}

type trackingReader struct {
	rdr     io.Reader
	wasRead uint32
}

func (tr *trackingReader) Read(p []byte) (int, error) {
	atomic.StoreUint32(&tr.wasRead, 1)
	return tr.rdr.Read(p)
}

func (tr *trackingReader) WasRead() bool {
	return atomic.LoadUint32(&tr.wasRead) != 0
}

func TestTransportExpectContinue(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reject":
			w.WriteHeader(403)
		default:
			io.Copy(io.Discard, r.Body)
		}
	})

	tr := &http.Transport{
		TLSClientConfig:       tlsConfigInsecure,
		MaxConnsPerHost:       1,
		ExpectContinueTimeout: 10 * time.Second,
	}

	err := ConfigureTransport(tr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: tr,
	}

	testCases := []struct {
		Name         string
		Path         string
		Body         *trackingReader
		ExpectedCode int
		ShouldRead   bool
	}{
		{
			Name:         "read-all",
			Path:         "/",
			Body:         &trackingReader{rdr: strings.NewReader("hello")},
			ExpectedCode: 200,
			ShouldRead:   true,
		},
		{
			Name:         "reject",
			Path:         "/reject",
			Body:         &trackingReader{rdr: strings.NewReader("hello")},
			ExpectedCode: 403,
			ShouldRead:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			startTime := time.Now()

			req, err := http.NewRequest("POST", ts.URL+tc.Path, tc.Body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Expect", "100-continue")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()

			if delta := time.Since(startTime); delta >= tr.ExpectContinueTimeout {
				t.Error("Request didn't finish before expect continue timeout")
			}
			if res.StatusCode != tc.ExpectedCode {
				t.Errorf("Unexpected status code, got %d, expected %d", res.StatusCode, tc.ExpectedCode)
			}
			if tc.Body.WasRead() != tc.ShouldRead {
				t.Errorf("Unexpected read status, got %v, expected %v", tc.Body.WasRead(), tc.ShouldRead)
			}
		})
	}
}

type closeChecker struct {
	io.ReadCloser
	closed chan struct{}
}

func newCloseChecker(r io.ReadCloser) *closeChecker {
	return &closeChecker{r, make(chan struct{})}
}

func newStaticCloseChecker(body string) *closeChecker {
	return newCloseChecker(io.NopCloser(strings.NewReader("body")))
}

func (rc *closeChecker) Read(b []byte) (n int, err error) {
	select {
	default:
	case <-rc.closed:
		// TODO(dneil): Consider restructuring the request write to avoid reading
		// from the request body after closing it, and check for read-after-close here.
		// Currently, abortRequestBodyWrite races with writeRequestBody.
		return 0, errors.New("read after Body.Close")
	}
	return rc.ReadCloser.Read(b)
}

func (rc *closeChecker) Close() error {
	close(rc.closed)
	return rc.ReadCloser.Close()
}

func (rc *closeChecker) isClosed() error {
	// The RoundTrip contract says that it will close the request body,
	// but that it may do so in a separate goroutine. Wait a reasonable
	// amount of time before concluding that the body isn't being closed.
	timeout := time.Duration(10 * time.Second)
	select {
	case <-rc.closed:
	case <-time.After(timeout):
		return fmt.Errorf("body not closed after %v", timeout)
	}
	return nil
}

// A blockingWriteConn is a net.Conn that blocks in Write after some number of bytes are written.
type blockingWriteConn struct {
	net.Conn
	writeOnce    sync.Once
	writec       chan struct{} // closed after the write limit is reached
	unblockc     chan struct{} // closed to unblock writes
	count, limit int
}

func newBlockingWriteConn(conn net.Conn, limit int) *blockingWriteConn {
	return &blockingWriteConn{
		Conn:     conn,
		limit:    limit,
		writec:   make(chan struct{}),
		unblockc: make(chan struct{}),
	}
}

// wait waits until the conn blocks writing the limit+1st byte.
func (c *blockingWriteConn) wait() {
	<-c.writec
}

// unblock unblocks writes to the conn.
func (c *blockingWriteConn) unblock() {
	close(c.unblockc)
}

func (c *blockingWriteConn) Write(b []byte) (n int, err error) {
	if c.count+len(b) > c.limit {
		c.writeOnce.Do(func() {
			close(c.writec)
		})
		<-c.unblockc
	}
	n, err = c.Conn.Write(b)
	c.count += n
	return n, err
}

// Write several requests to a ClientConn at the same time, looking for race conditions.
// See golang.org/issue/48340
func TestTransportFrameBufferReuse(t *testing.T) {
	filler := hex.EncodeToString([]byte(randString(2048)))

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Big"), filler; got != want {
			t.Errorf(`r.Header.Get("Big") = %q, want %q`, got, want)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("error reading request body: %v", err)
		}
		if got, want := string(b), filler; got != want {
			t.Errorf("request body = %q, want %q", got, want)
		}
		if got, want := r.Trailer.Get("Big"), filler; got != want {
			t.Errorf(`r.Trailer.Get("Big") = %q, want %q`, got, want)
		}
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	var wg sync.WaitGroup
	defer wg.Wait()
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest("POST", ts.URL, strings.NewReader(filler))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Big", filler)
			req.Trailer = make(http.Header)
			req.Trailer.Set("Big", filler)
			res, err := tr.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := res.StatusCode, 200; got != want {
				t.Errorf("StatusCode = %v; want %v", got, want)
			}
			if res != nil && res.Body != nil {
				res.Body.Close()
			}
		}()
	}

}

// Ensure that a request blocking while being written to the underlying net.Conn doesn't
// block access to the ClientConn pool. Test requests blocking while writing headers, the body,
// and trailers.
// See golang.org/issue/32388
func TestTransportBlockingRequestWrite(t *testing.T) {
	filler := hex.EncodeToString([]byte(randString(2048)))
	for _, test := range []struct {
		name string
		req  func(url string) (*http.Request, error)
	}{{
		name: "headers",
		req: func(url string) (*http.Request, error) {
			req, err := http.NewRequest("POST", url, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Big", filler)
			return req, err
		},
	}, {
		name: "body",
		req: func(url string) (*http.Request, error) {
			req, err := http.NewRequest("POST", url, strings.NewReader(filler))
			if err != nil {
				return nil, err
			}
			return req, err
		},
	}, {
		name: "trailer",
		req: func(url string) (*http.Request, error) {
			req, err := http.NewRequest("POST", url, strings.NewReader("body"))
			if err != nil {
				return nil, err
			}
			req.Trailer = make(http.Header)
			req.Trailer.Set("Big", filler)
			return req, err
		},
	}} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if v := r.Header.Get("Big"); v != "" && v != filler {
					t.Errorf("request header mismatch")
				}
				if v, _ := io.ReadAll(r.Body); len(v) != 0 && string(v) != "body" && string(v) != filler {
					t.Errorf("request body mismatch\ngot:  %q\nwant: %q", string(v), filler)
				}
				if v := r.Trailer.Get("Big"); v != "" && v != filler {
					t.Errorf("request trailer mismatch\ngot:  %q\nwant: %q", string(v), filler)
				}
			}, func(s *Server) {
				s.MaxConcurrentStreams = 1
			})

			// This Transport creates connections that block on writes after 1024 bytes.
			connc := make(chan *blockingWriteConn, 1)
			connCount := 0
			tr := &Transport{
				TLSClientConfig: tlsConfigInsecure,
				DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
					connCount++
					c, err := tls.Dial(network, addr, cfg)
					wc := newBlockingWriteConn(c, 1024)
					select {
					case connc <- wc:
					default:
					}
					return wc, err
				},
			}
			defer tr.CloseIdleConnections()

			// Request 1: A small request to ensure we read the server MaxConcurrentStreams.
			{
				req, err := http.NewRequest("POST", ts.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				res, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := res.StatusCode, 200; got != want {
					t.Errorf("StatusCode = %v; want %v", got, want)
				}
				if res != nil && res.Body != nil {
					res.Body.Close()
				}
			}

			// Request 2: A large request that blocks while being written.
			reqc := make(chan struct{})
			go func() {
				defer close(reqc)
				req, err := test.req(ts.URL)
				if err != nil {
					t.Error(err)
					return
				}
				res, _ := tr.RoundTrip(req)
				if res != nil && res.Body != nil {
					res.Body.Close()
				}
			}()
			conn := <-connc
			conn.wait() // wait for the request to block

			// Request 3: A small request that is sent on a new connection, since request 2
			// is hogging the only available stream on the previous connection.
			{
				req, err := http.NewRequest("POST", ts.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				res, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := res.StatusCode, 200; got != want {
					t.Errorf("StatusCode = %v; want %v", got, want)
				}
				if res != nil && res.Body != nil {
					res.Body.Close()
				}
			}

			// Request 2 should still be blocking at this point.
			select {
			case <-reqc:
				t.Errorf("request 2 unexpectedly completed")
			default:
			}

			conn.unblock()
			<-reqc

			if connCount != 2 {
				t.Errorf("created %v connections, want 1", connCount)
			}
		})
	}
}

func TestTransportCloseRequestBody(t *testing.T) {
	var statusCode int
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	ctx := context.Background()
	cc, err := tr.dialClientConn(ctx, ts.Listener.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range []int{200, 401} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			statusCode = status
			pr, pw := io.Pipe()
			body := newCloseChecker(pr)
			req, err := http.NewRequest("PUT", "https://dummy.tld/", body)
			if err != nil {
				t.Fatal(err)
			}
			res, err := cc.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			pw.Close()
			if err := body.isClosed(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTransportRetriesOnStreamProtocolError(t *testing.T) {
	// This test verifies that
	//   - receiving a protocol error on a connection does not interfere with
	//     other requests in flight on that connection;
	//   - the connection is not reused for further requests; and
	//   - the failed request is retried on a new connecection.
	tt := newTestTransport(t)

	// Start two requests. The first is a long request
	// that will finish after the second. The second one
	// will result in the protocol error.

	// Request #1: The long request.
	req1, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt1 := tt.roundTrip(req1)
	tc1 := tt.getConn()
	tc1.wantFrameType(FrameSettings)
	tc1.wantFrameType(FrameWindowUpdate)
	tc1.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	tc1.writeSettings()
	tc1.wantFrameType(FrameSettings) // settings ACK

	// Request #2(a): The short request.
	req2, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt2 := tt.roundTrip(req2)
	tc1.wantHeaders(wantHeader{
		streamID:  3,
		endStream: true,
	})

	// Request #2(a) fails with ErrCodeProtocol.
	tc1.writeRSTStream(3, ErrCodeProtocol)
	if rt1.done() {
		t.Fatalf("After protocol error on RoundTrip #2, RoundTrip #1 is done; want still in progress")
	}
	if rt2.done() {
		t.Fatalf("After protocol error on RoundTrip #2, RoundTrip #2 is done; want still in progress")
	}

	// Request #2(b): The short request is retried on a new connection.
	tc2 := tt.getConn()
	tc2.wantFrameType(FrameSettings)
	tc2.wantFrameType(FrameWindowUpdate)
	tc2.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
	})
	tc2.writeSettings()
	tc2.wantFrameType(FrameSettings) // settings ACK

	// Request #2(b) succeeds.
	tc2.writeHeaders(HeadersFrameParam{
		StreamID:   1,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc1.makeHeaderBlockFragment(
			":status", "201",
		),
	})
	rt2.wantStatus(201)

	// Request #1 succeeds.
	tc1.writeHeaders(HeadersFrameParam{
		StreamID:   1,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc1.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	rt1.wantStatus(200)
}

func TestClientConnReservations(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet(
		Setting{ID: SettingMaxConcurrentStreams, Val: initialMaxConcurrentStreams},
	)

	doRoundTrip := func() {
		req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
		rt := tc.roundTrip(req)
		tc.wantFrameType(FrameHeaders)
		tc.writeHeaders(HeadersFrameParam{
			StreamID:   rt.streamID(),
			EndHeaders: true,
			EndStream:  true,
			BlockFragment: tc.makeHeaderBlockFragment(
				":status", "200",
			),
		})
		rt.wantStatus(200)
	}

	n := 0
	for n <= initialMaxConcurrentStreams && tc.cc.ReserveNewRequest() {
		n++
	}
	if n != initialMaxConcurrentStreams {
		t.Errorf("did %v reservations; want %v", n, initialMaxConcurrentStreams)
	}
	doRoundTrip()
	n2 := 0
	for n2 <= 5 && tc.cc.ReserveNewRequest() {
		n2++
	}
	if n2 != 1 {
		t.Fatalf("after one RoundTrip, did %v reservations; want 1", n2)
	}

	// Use up all the reservations
	for i := 0; i < n; i++ {
		doRoundTrip()
	}

	n2 = 0
	for n2 <= initialMaxConcurrentStreams && tc.cc.ReserveNewRequest() {
		n2++
	}
	if n2 != n {
		t.Errorf("after reset, reservations = %v; want %v", n2, n)
	}
}

func TestTransportTimeoutServerHangs(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "PUT", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.advance(5 * time.Second)
	if f := tc.readFrame(); f != nil {
		t.Fatalf("unexpected frame: %v", f)
	}
	if rt.done() {
		t.Fatalf("after 5 seconds with no response, RoundTrip unexpectedly returned")
	}

	cancel()
	tc.sync()
	if rt.err() != context.Canceled {
		t.Fatalf("RoundTrip error: %v; want context.Canceled", rt.err())
	}
}

func TestTransportContentLengthWithoutBody(t *testing.T) {
	contentLength := ""
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", contentLength)
	})
	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	for _, test := range []struct {
		name              string
		contentLength     string
		wantBody          string
		wantErr           error
		wantContentLength int64
	}{
		{
			name:              "non-zero content length",
			contentLength:     "42",
			wantErr:           io.ErrUnexpectedEOF,
			wantContentLength: 42,
		},
		{
			name:              "zero content length",
			contentLength:     "0",
			wantErr:           nil,
			wantContentLength: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			contentLength = test.contentLength

			req, _ := http.NewRequest("GET", ts.URL, nil)
			res, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)

			if err != test.wantErr {
				t.Errorf("Expected error %v, got: %v", test.wantErr, err)
			}
			if len(body) > 0 {
				t.Errorf("Expected empty body, got: %v", body)
			}
			if res.ContentLength != test.wantContentLength {
				t.Errorf("Expected content length %d, got: %d", test.wantContentLength, res.ContentLength)
			}
		})
	}
}

func TestTransportCloseResponseBodyWhileRequestBodyHangs(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		io.Copy(io.Discard, r.Body)
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	pr, pw := net.Pipe()
	req, err := http.NewRequest("GET", ts.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	// Closing the Response's Body interrupts the blocked body read.
	res.Body.Close()
	pw.Close()
}

func TestTransport300ResponseBody(t *testing.T) {
	reqc := make(chan struct{})
	body := []byte("response body")
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(300)
		w.(http.Flusher).Flush()
		<-reqc
		w.Write(body)
	})

	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()

	pr, pw := net.Pipe()
	req, err := http.NewRequest("GET", ts.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	close(reqc)
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("error reading response body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("got response body %q, want %q", string(got), string(body))
	}
	res.Body.Close()
	pw.Close()
}

func TestTransportWriteByteTimeout(t *testing.T) {
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {},
	)
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			_, c := net.Pipe()
			return c, nil
		},
		WriteByteTimeout: 1 * time.Millisecond,
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}

	_, err := c.Get(ts.URL)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Get on unresponsive connection: got %q; want ErrDeadlineExceeded", err)
	}
}

type slowWriteConn struct {
	net.Conn
	hasWriteDeadline bool
}

func (c *slowWriteConn) SetWriteDeadline(t time.Time) error {
	c.hasWriteDeadline = !t.IsZero()
	return nil
}

func (c *slowWriteConn) Write(b []byte) (n int, err error) {
	if c.hasWriteDeadline && len(b) > 1 {
		n, err = c.Conn.Write(b[:1])
		if err != nil {
			return n, err
		}
		return n, fmt.Errorf("slow write: %w", os.ErrDeadlineExceeded)
	}
	return c.Conn.Write(b)
}

func TestTransportSlowWrites(t *testing.T) {
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {},
	)
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			cfg.InsecureSkipVerify = true
			c, err := tls.Dial(network, addr, cfg)
			return &slowWriteConn{Conn: c}, err
		},
		WriteByteTimeout: 1 * time.Millisecond,
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}

	const bodySize = 1 << 20
	resp, err := c.Post(ts.URL, "text/foo", io.LimitReader(neverEnding('A'), bodySize))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestTransportClosesConnAfterGoAwayNoStreams(t *testing.T) {
	testTransportClosesConnAfterGoAway(t, 0)
}
func TestTransportClosesConnAfterGoAwayLastStream(t *testing.T) {
	testTransportClosesConnAfterGoAway(t, 1)
}

// testTransportClosesConnAfterGoAway verifies that the transport
// closes a connection after reading a GOAWAY from it.
//
// lastStream is the last stream ID in the GOAWAY frame.
// When 0, the transport (unsuccessfully) retries the request (stream 1);
// when 1, the transport reads the response after receiving the GOAWAY.
func testTransportClosesConnAfterGoAway(t *testing.T, lastStream uint32) {
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeGoAway(lastStream, ErrCodeNo, nil)

	if lastStream > 0 {
		// Send a valid response to first request.
		tc.writeHeaders(HeadersFrameParam{
			StreamID:   rt.streamID(),
			EndHeaders: true,
			EndStream:  true,
			BlockFragment: tc.makeHeaderBlockFragment(
				":status", "200",
			),
		})
	}

	tc.closeWrite()
	err := rt.err()
	if gotErr, wantErr := err != nil, lastStream == 0; gotErr != wantErr {
		t.Errorf("RoundTrip got error %v (want error: %v)", err, wantErr)
	}
	if !tc.isClosed() {
		t.Errorf("ClientConn did not close its net.Conn, expected it to")
	}
}

type slowCloser struct {
	closing chan struct{}
	closed  chan struct{}
}

func (r *slowCloser) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (r *slowCloser) Close() error {
	close(r.closing)
	<-r.closed
	return nil
}

func TestTransportSlowClose(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
	})

	client := ts.Client()
	body := &slowCloser{
		closing: make(chan struct{}),
		closed:  make(chan struct{}),
	}

	reqc := make(chan struct{})
	go func() {
		defer close(reqc)
		res, err := client.Post(ts.URL, "text/plain", body)
		if err != nil {
			t.Error(err)
		}
		res.Body.Close()
	}()
	defer func() {
		close(body.closed)
		<-reqc // wait for POST request to finish
	}()

	<-body.closing // wait for POST request to call body.Close
	// This GET request should not be blocked by the in-progress POST.
	res, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}

func TestTransportDialTLSContext(t *testing.T) {
	blockCh := make(chan struct{})
	serverTLSConfigFunc := func(ts *httptest.Server) {
		ts.Config.TLSConfig = &tls.Config{
			// Triggers the server to request the clients certificate
			// during TLS handshake.
			ClientAuth: tls.RequestClientCert,
		}
	}
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {},
		serverTLSConfigFunc,
	)
	tr := &Transport{
		TLSClientConfig: &tls.Config{
			GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
				// Tests that the context provided to `req` is
				// passed into this function.
				close(blockCh)
				<-cri.Context().Done()
				return nil, cri.Context().Err()
			},
			InsecureSkipVerify: true,
		},
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		res, err := tr.RoundTrip(req)
		if err != nil {
			errCh <- err
			return
		}
		res.Body.Close()
	}()
	// Wait for GetClientCertificate handler to be called
	<-blockCh
	// Cancel the context
	cancel()
	// Expect the cancellation error here
	err = <-errCh
	if err == nil {
		t.Fatal("cancelling context during client certificate fetch did not error as expected")
		return
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error returned after cancellation: %v", err)
	}
}

// TestDialRaceResumesDial tests that, given two concurrent requests
// to the same address, when the first Dial is interrupted because
// the first request's context is cancelled, the second request
// resumes the dial automatically.
func TestDialRaceResumesDial(t *testing.T) {
	blockCh := make(chan struct{})
	serverTLSConfigFunc := func(ts *httptest.Server) {
		ts.Config.TLSConfig = &tls.Config{
			// Triggers the server to request the clients certificate
			// during TLS handshake.
			ClientAuth: tls.RequestClientCert,
		}
	}
	ts := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {},
		serverTLSConfigFunc,
	)
	tr := &Transport{
		TLSClientConfig: &tls.Config{
			GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
				select {
				case <-blockCh:
					// If we already errored, return without error.
					return &tls.Certificate{}, nil
				default:
				}
				close(blockCh)
				<-cri.Context().Done()
				return nil, cri.Context().Err()
			},
			InsecureSkipVerify: true,
		},
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Create two requests with independent cancellation.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	req1 := req.WithContext(ctx1)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	req2 := req.WithContext(ctx2)
	errCh := make(chan error)
	go func() {
		res, err := tr.RoundTrip(req1)
		if err != nil {
			errCh <- err
			return
		}
		res.Body.Close()
	}()
	successCh := make(chan struct{})
	go func() {
		// Don't start request until first request
		// has initiated the handshake.
		<-blockCh
		res, err := tr.RoundTrip(req2)
		if err != nil {
			errCh <- err
			return
		}
		res.Body.Close()
		// Close successCh to indicate that the second request
		// made it to the server successfully.
		close(successCh)
	}()
	// Wait for GetClientCertificate handler to be called
	<-blockCh
	// Cancel the context first
	cancel1()
	// Expect the cancellation error here
	err = <-errCh
	if err == nil {
		t.Fatal("cancelling context during client certificate fetch did not error as expected")
		return
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error returned after cancellation: %v", err)
	}
	select {
	case err := <-errCh:
		t.Fatalf("unexpected second error: %v", err)
	case <-successCh:
	}
}

func TestTransportDataAfter1xxHeader(t *testing.T) {
	// Discard logger output to avoid spamming stderr.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	// https://go.dev/issue/65927 - server sends a 1xx response, followed by a DATA frame.
	tc := newTestClientConn(t)
	tc.greet()

	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	rt := tc.roundTrip(req)

	tc.wantFrameType(FrameHeaders)
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  false,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "100",
		),
	})
	tc.writeData(rt.streamID(), true, []byte{0})
	err := rt.err()
	if err, ok := err.(StreamError); !ok || err.Code != ErrCodeProtocol {
		t.Errorf("RoundTrip error: %v; want ErrCodeProtocol", err)
	}
	tc.wantFrameType(FrameRSTStream)
}

func TestIssue66763Race(t *testing.T) {
	tr := &Transport{
		IdleConnTimeout: 1 * time.Nanosecond,
		AllowHTTP:       true, // issue 66763 only occurs when AllowHTTP is true
	}
	defer tr.CloseIdleConnections()

	cli, srv := net.Pipe()
	donec := make(chan struct{})
	go func() {
		// Creating the client conn may succeed or fail,
		// depending on when the idle timeout happens.
		// Either way, the idle timeout will close the net.Conn.
		tr.NewClientConn(cli)
		close(donec)
	}()

	// The client sends its preface and SETTINGS frame,
	// and then closes its conn after the idle timeout.
	io.ReadAll(srv)
	srv.Close()

	<-donec
}

// Issue 67671: Sending a Connection: close request on a Transport with AllowHTTP
// set caused a the transport to wedge.
func TestIssue67671(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		AllowHTTP:       true,
	}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.Close = true
	for i := 0; i < 2; i++ {
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
}

func TestTransport1xxLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		opt     any
		ctxfn   func(context.Context) context.Context
		hcount  int
		limited bool
	}{{
		name:    "default",
		hcount:  10,
		limited: false,
	}, {
		name: "MaxHeaderListSize",
		opt: func(tr *Transport) {
			tr.MaxHeaderListSize = 10000
		},
		hcount:  10,
		limited: true,
	}, {
		name: "MaxResponseHeaderBytes",
		opt: func(tr *http.Transport) {
			tr.MaxResponseHeaderBytes = 10000
		},
		hcount:  10,
		limited: true,
	}, {
		name: "limit by client trace",
		ctxfn: func(ctx context.Context) context.Context {
			count := 0
			return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
				Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
					count++
					if count >= 10 {
						return errors.New("too many 1xx")
					}
					return nil
				},
			})
		},
		hcount:  10,
		limited: true,
	}, {
		name: "limit disabled by client trace",
		opt: func(tr *Transport) {
			tr.MaxHeaderListSize = 10000
		},
		ctxfn: func(ctx context.Context) context.Context {
			return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
				Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
					return nil
				},
			})
		},
		hcount:  20,
		limited: false,
	}} {
		t.Run(test.name, func(t *testing.T) {
			tc := newTestClientConn(t, test.opt)
			tc.greet()

			ctx := context.Background()
			if test.ctxfn != nil {
				ctx = test.ctxfn(ctx)
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil)
			rt := tc.roundTrip(req)
			tc.wantFrameType(FrameHeaders)

			for i := 0; i < test.hcount; i++ {
				if fr, err := tc.fr.ReadFrame(); err != os.ErrDeadlineExceeded {
					t.Fatalf("after writing %v 1xx headers: read %v, %v; want idle", i, fr, err)
				}
				tc.writeHeaders(HeadersFrameParam{
					StreamID:   rt.streamID(),
					EndHeaders: true,
					EndStream:  false,
					BlockFragment: tc.makeHeaderBlockFragment(
						":status", "103",
						"x-field", strings.Repeat("a", 1000),
					),
				})
			}
			if test.limited {
				tc.wantFrameType(FrameRSTStream)
			} else {
				tc.wantIdle()
			}
		})
	}
}

func TestTransportSendPingWithReset(t *testing.T) {
	tc := newTestClientConn(t, func(tr *Transport) {
		tr.StrictMaxConcurrentStreams = true
	})

	const maxConcurrent = 3
	tc.greet(Setting{SettingMaxConcurrentStreams, maxConcurrent})

	// Start several requests.
	var rts []*testRoundTrip
	for i := 0; i < maxConcurrent+1; i++ {
		req := must(http.NewRequest("GET", "https://dummy.tld/", nil))
		rt := tc.roundTrip(req)
		if i >= maxConcurrent {
			tc.wantIdle()
			continue
		}
		tc.wantFrameType(FrameHeaders)
		tc.writeHeaders(HeadersFrameParam{
			StreamID:   rt.streamID(),
			EndHeaders: true,
			BlockFragment: tc.makeHeaderBlockFragment(
				":status", "200",
			),
		})
		rt.wantStatus(200)
		rts = append(rts, rt)
	}

	// Cancel one request. We send a PING frame along with the RST_STREAM.
	rts[0].response().Body.Close()
	tc.wantRSTStream(rts[0].streamID(), ErrCodeCancel)
	pf := readFrame[*PingFrame](t, tc)
	tc.wantIdle()

	// Cancel another request. No PING frame, since one is in flight.
	rts[1].response().Body.Close()
	tc.wantRSTStream(rts[1].streamID(), ErrCodeCancel)
	tc.wantIdle()

	// Respond to the PING.
	// This finalizes the previous resets, and allows the pending request to be sent.
	tc.writePing(true, pf.Data)
	tc.wantFrameType(FrameHeaders)
	tc.wantIdle()

	// Receive a byte of data for the remaining stream, which resets our ability
	// to send pings (see comment on ClientConn.rstStreamPingsBlocked).
	tc.writeData(rts[2].streamID(), false, []byte{0})

	// Cancel the last request. We send another PING, since none are in flight.
	rts[2].response().Body.Close()
	tc.wantRSTStream(rts[2].streamID(), ErrCodeCancel)
	tc.wantFrameType(FramePing)
	tc.wantIdle()
}

// Issue #70505: gRPC gets upset if we send more than 2 pings per HEADERS/DATA frame
// sent by the server.
func TestTransportSendNoMoreThanOnePingWithReset(t *testing.T) {
	tc := newTestClientConn(t)
	tc.greet()

	makeAndResetRequest := func() {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		req := must(http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil))
		rt := tc.roundTrip(req)
		tc.wantFrameType(FrameHeaders)
		cancel()
		tc.wantRSTStream(rt.streamID(), ErrCodeCancel) // client sends RST_STREAM
	}

	// Create a request and cancel it.
	// The client sends a PING frame along with the reset.
	makeAndResetRequest()
	pf1 := readFrame[*PingFrame](t, tc) // client sends PING

	// Create another request and cancel it.
	// We do not send a PING frame along with the reset,
	// because we haven't received a HEADERS or DATA frame from the server
	// since the last PING we sent.
	makeAndResetRequest()

	// Server belatedly responds to request 1.
	// The server has not responded to our first PING yet.
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   1,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})

	// Create yet another request and cancel it.
	// We still do not send a PING frame along with the reset.
	// We've received a HEADERS frame, but it came before the response to the PING.
	makeAndResetRequest()

	// The server responds to our PING.
	tc.writePing(true, pf1.Data)

	// Create yet another request and cancel it.
	// Still no PING frame; we got a response to the previous one,
	// but no HEADERS or DATA.
	makeAndResetRequest()

	// Server belatedly responds to the second request.
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   3,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})

	// One more request.
	// This time we send a PING frame.
	makeAndResetRequest()
	tc.wantFrameType(FramePing)
}

func TestTransportConnBecomesUnresponsive(t *testing.T) {
	// We send a number of requests in series to an unresponsive connection.
	// Each request is canceled or times out without a response.
	// Eventually, we open a new connection rather than trying to use the old one.
	tt := newTestTransport(t)

	const maxConcurrent = 3

	t.Logf("first request opens a new connection and succeeds")
	req1 := must(http.NewRequest("GET", "https://dummy.tld/", nil))
	rt1 := tt.roundTrip(req1)
	tc1 := tt.getConn()
	tc1.wantFrameType(FrameSettings)
	tc1.wantFrameType(FrameWindowUpdate)
	hf1 := readFrame[*HeadersFrame](t, tc1)
	tc1.writeSettings(Setting{SettingMaxConcurrentStreams, maxConcurrent})
	tc1.wantFrameType(FrameSettings) // ack
	tc1.writeHeaders(HeadersFrameParam{
		StreamID:   hf1.StreamID,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc1.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	rt1.wantStatus(200)
	rt1.response().Body.Close()

	// Send more requests.
	// None receive a response.
	// Each is canceled.
	for i := 0; i < maxConcurrent; i++ {
		t.Logf("request %v receives no response and is canceled", i)
		ctx, cancel := context.WithCancel(context.Background())
		req := must(http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil))
		tt.roundTrip(req)
		if tt.hasConn() {
			t.Fatalf("new connection created; expect existing conn to be reused")
		}
		tc1.wantFrameType(FrameHeaders)
		cancel()
		tc1.wantFrameType(FrameRSTStream)
		if i == 0 {
			tc1.wantFrameType(FramePing)
		}
		tc1.wantIdle()
	}

	// The conn has hit its concurrency limit.
	// The next request is sent on a new conn.
	req2 := must(http.NewRequest("GET", "https://dummy.tld/", nil))
	rt2 := tt.roundTrip(req2)
	tc2 := tt.getConn()
	tc2.wantFrameType(FrameSettings)
	tc2.wantFrameType(FrameWindowUpdate)
	hf := readFrame[*HeadersFrame](t, tc2)
	tc2.writeSettings(Setting{SettingMaxConcurrentStreams, maxConcurrent})
	tc2.wantFrameType(FrameSettings) // ack
	tc2.writeHeaders(HeadersFrameParam{
		StreamID:   hf.StreamID,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc2.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	rt2.wantStatus(200)
	rt2.response().Body.Close()
}

// Test that the Transport can use a conn provided to it by a TLSNextProto hook.
func TestTransportTLSNextProtoConnOK(t *testing.T) {
	t1 := &http.Transport{}
	t2, _ := ConfigureTransports(t1)
	tt := newTestTransport(t, t2)

	// Create a new, fake connection and pass it to the Transport via the TLSNextProto hook.
	cli, _ := synctestNetPipe(tt.group)
	cliTLS := tls.Client(cli, tlsConfigInsecure)
	go func() {
		tt.group.Join()
		t1.TLSNextProto["h2"]("dummy.tld", cliTLS)
	}()
	tt.sync()
	tc := tt.getConn()
	tc.greet()

	// Send a request on the Transport.
	// It uses the conn we provided.
	req := must(http.NewRequest("GET", "https://dummy.tld/", nil))
	rt := tt.roundTrip(req)
	tc.wantHeaders(wantHeader{
		streamID:  1,
		endStream: true,
		header: http.Header{
			":authority": []string{"dummy.tld"},
			":method":    []string{"GET"},
			":path":      []string{"/"},
		},
	})
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   1,
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})
	rt.wantStatus(200)
	rt.wantBody(nil)
}

// Test the case where a conn provided via a TLSNextProto hook immediately encounters an error.
func TestTransportTLSNextProtoConnImmediateFailureUsed(t *testing.T) {
	t1 := &http.Transport{}
	t2, _ := ConfigureTransports(t1)
	tt := newTestTransport(t, t2)

	// Create a new, fake connection and pass it to the Transport via the TLSNextProto hook.
	cli, _ := synctestNetPipe(tt.group)
	cliTLS := tls.Client(cli, tlsConfigInsecure)
	go func() {
		tt.group.Join()
		t1.TLSNextProto["h2"]("dummy.tld", cliTLS)
	}()
	tt.sync()
	tc := tt.getConn()

	// The connection encounters an error before we send a request that uses it.
	tc.closeWrite()

	// Send a request on the Transport.
	//
	// It should fail, because we have no usable connections, but not with ErrNoCachedConn.
	req := must(http.NewRequest("GET", "https://dummy.tld/", nil))
	rt := tt.roundTrip(req)
	if err := rt.err(); err == nil || errors.Is(err, ErrNoCachedConn) {
		t.Fatalf("RoundTrip with broken conn: got %v, want an error other than ErrNoCachedConn", err)
	}

	// Send the request again.
	// This time it should fail with ErrNoCachedConn,
	// because the dead conn has been removed from the pool.
	rt = tt.roundTrip(req)
	if err := rt.err(); !errors.Is(err, ErrNoCachedConn) {
		t.Fatalf("RoundTrip after broken conn is used: got %v, want ErrNoCachedConn", err)
	}
}

// Test the case where a conn provided via a TLSNextProto hook is closed for idleness
// before we use it.
func TestTransportTLSNextProtoConnIdleTimoutBeforeUse(t *testing.T) {
	t1 := &http.Transport{
		IdleConnTimeout: 1 * time.Second,
	}
	t2, _ := ConfigureTransports(t1)
	tt := newTestTransport(t, t2)

	// Create a new, fake connection and pass it to the Transport via the TLSNextProto hook.
	cli, _ := synctestNetPipe(tt.group)
	cliTLS := tls.Client(cli, tlsConfigInsecure)
	go func() {
		tt.group.Join()
		t1.TLSNextProto["h2"]("dummy.tld", cliTLS)
	}()
	tt.sync()
	tc := tt.getConn()

	// The connection encounters an error before we send a request that uses it.
	tc.advance(2 * time.Second)

	// Send a request on the Transport.
	//
	// It should fail with ErrNoCachedConn.
	req := must(http.NewRequest("GET", "https://dummy.tld/", nil))
	rt := tt.roundTrip(req)
	if err := rt.err(); !errors.Is(err, ErrNoCachedConn) {
		t.Fatalf("RoundTrip with conn closed for idleness: got %v, want ErrNoCachedConn", err)
	}
}

// Test the case where a conn provided via a TLSNextProto hook immediately encounters an error,
// but no requests are sent which would use the bad connection.
func TestTransportTLSNextProtoConnImmediateFailureUnused(t *testing.T) {
	t1 := &http.Transport{}
	t2, _ := ConfigureTransports(t1)
	tt := newTestTransport(t, t2)

	// Create a new, fake connection and pass it to the Transport via the TLSNextProto hook.
	cli, _ := synctestNetPipe(tt.group)
	cliTLS := tls.Client(cli, tlsConfigInsecure)
	go func() {
		tt.group.Join()
		t1.TLSNextProto["h2"]("dummy.tld", cliTLS)
	}()
	tt.sync()
	tc := tt.getConn()

	// The connection encounters an error before we send a request that uses it.
	tc.closeWrite()

	// Some time passes.
	// The dead connection is removed from the pool.
	tc.advance(10 * time.Second)

	// Send a request on the Transport.
	//
	// It should fail with ErrNoCachedConn, because the pool contains no conns.
	req := must(http.NewRequest("GET", "https://dummy.tld/", nil))
	rt := tt.roundTrip(req)
	if err := rt.err(); !errors.Is(err, ErrNoCachedConn) {
		t.Fatalf("RoundTrip after broken conn expires: got %v, want ErrNoCachedConn", err)
	}
}

func TestExtendedConnectClientWithServerSupport(t *testing.T) {
	setForTest(t, &disableExtendedConnectProtocol, false)
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(":protocol") != "extended-connect" {
			t.Fatalf("unexpected :protocol header received")
		}
		t.Log(io.Copy(w, r.Body))
	})
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		AllowHTTP:       true,
	}
	defer tr.CloseIdleConnections()
	pr, pw := io.Pipe()
	pwDone := make(chan struct{})
	req, _ := http.NewRequest("CONNECT", ts.URL, pr)
	req.Header.Set(":protocol", "extended-connect")
	req.Header.Set("X-A", "A")
	req.Header.Set("X-B", "B")
	req.Header.Set("X-C", "C")
	go func() {
		pw.Write([]byte("hello, extended connect"))
		pw.Close()
		close(pwDone)
	}()

	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, []byte("hello, extended connect")) {
		t.Fatal("unexpected body received")
	}
}

func TestExtendedConnectClientWithoutServerSupport(t *testing.T) {
	setForTest(t, &disableExtendedConnectProtocol, true)
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, r.Body)
	})
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		AllowHTTP:       true,
	}
	defer tr.CloseIdleConnections()
	pr, pw := io.Pipe()
	pwDone := make(chan struct{})
	req, _ := http.NewRequest("CONNECT", ts.URL, pr)
	req.Header.Set(":protocol", "extended-connect")
	req.Header.Set("X-A", "A")
	req.Header.Set("X-B", "B")
	req.Header.Set("X-C", "C")
	go func() {
		pw.Write([]byte("hello, extended connect"))
		pw.Close()
		close(pwDone)
	}()

	_, err := tr.RoundTrip(req)
	if !errors.Is(err, errExtendedConnectNotSupported) {
		t.Fatalf("expected error errExtendedConnectNotSupported, got: %v", err)
	}
}

// Issue #70658: Make sure extended CONNECT requests don't get stuck if a
// connection fails early in its lifetime.
func TestExtendedConnectReadFrameError(t *testing.T) {
	tc := newTestClientConn(t)
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)

	req, _ := http.NewRequest("CONNECT", "https://dummy.tld/", nil)
	req.Header.Set(":protocol", "extended-connect")
	rt := tc.roundTrip(req)
	tc.wantIdle() // waiting for SETTINGS response

	tc.closeWrite() // connection breaks without sending SETTINGS
	if !rt.done() {
		t.Fatalf("after connection closed: RoundTrip still running; want done")
	}
	if rt.err() == nil {
		t.Fatalf("after connection closed: RoundTrip succeeded; want error")
	}
}
