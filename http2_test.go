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
	"testing"
)

func TestServer(t *testing.T) {
	requireCurl(t)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Hello, test.")
	}))
	ConfigureServer(ts.Config, &Server{})
	ts.StartTLS()
	defer ts.Close()
	out, err := curl(t, "--http2", "--insecure", "-v", ts.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("Error fetching with curl: %v, %s", err, out)
	}
	t.Logf("Got: %s", out)
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
