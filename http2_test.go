// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServer(t *testing.T) {
	requireCurl(t)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Hello, test.")
	}))
	ConfigureServer(ts.Config, &Server{})
	ts.TLS = ts.Config.TLSConfig // the httptest.Server has its own copy of this TLS config
	ts.StartTLS()
	defer ts.Close()

	var gotConn int32
	testHookOnConn = func() { atomic.StoreInt32(&gotConn, 1) }

	t.Logf("Running curl on %s", ts.URL)
	out, err := curl(t, "--silent", "--http2", "--insecure", "-v", ts.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("Error fetching with curl: %v, %s", err, out)
	}
	t.Logf("Got: %s", out)

	if atomic.LoadInt32(&gotConn) == 0 {
		t.Error("never saw an http2 connection")
	}
}

// Verify that curl has http2.
func requireCurl(t *testing.T) {
	out, err := curl(t, "--version").CombinedOutput()
	if err != nil {
		t.Skipf("failed to determine curl features; skipping test")
	}
	if !strings.Contains(string(out), "HTTP2") {
		t.Skip("curl doesn't support HTTP2; skipping test")
	}
}

func curl(t *testing.T, args ...string) *exec.Cmd {
	return exec.Command("docker", append([]string{"run", "--net=host", "gohttp2/curl"}, args...)...)
}
