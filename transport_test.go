// Copyright 2015 The Go Authors.
// See https://go.googlesource.com/go/+/master/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://go.googlesource.com/go/+/master/LICENSE

package http2

import (
	"flag"
	"net/http"
	"os"
	"testing"
)

var extNet = flag.Bool("extnet", false, "do external network tests")

func TestTransport(t *testing.T) {
	if !*extNet {
		t.Skip("skipping external network test")
	}
	req, _ := http.NewRequest("GET", "https://http2.golang.org/", nil)
	var rt http.RoundTripper = &Transport{}
	//rt = http.DefaultTransport
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("%v", err)
	}
	res.Write(os.Stdout)
}
