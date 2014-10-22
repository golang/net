// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type serverTester struct {
	cc net.Conn // client conn
	t  *testing.T
	ts *httptest.Server
	fr *Framer
}

func newServerTester(t *testing.T, handler http.HandlerFunc) *serverTester {
	ts := httptest.NewUnstartedServer(handler)
	ConfigureServer(ts.Config, &Server{})
	ts.TLS = ts.Config.TLSConfig // the httptest.Server has its own copy of this TLS config
	ts.StartTLS()

	t.Logf("Running test server at: %s", ts.URL)
	cc, err := tls.Dial("tcp", ts.Listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{npnProto},
	})
	if err != nil {
		t.Fatal(err)
	}
	log.SetOutput(twriter{t})
	return &serverTester{
		t:  t,
		ts: ts,
		cc: cc,
		fr: NewFramer(cc, cc),
	}
}

func (st *serverTester) Close() {
	st.ts.Close()
	st.cc.Close()
	log.SetOutput(os.Stderr)
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

func (st *serverTester) wantSettings() *SettingsFrame {
	f, err := st.fr.ReadFrame()
	if err != nil {
		st.t.Fatal(err)
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *SettingsFrame", f)
	}
	return sf
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
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Foo", "Bar")
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

	// TODO: send a request
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
