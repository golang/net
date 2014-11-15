// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/bradfitz/http2"
)

var openFirefox = flag.Bool("openff", false, "Open Firefox")

func main() {
	var srv http.Server
	flag.BoolVar(&http2.VerboseLogs, "verbose", false, "Verbose HTTP/2 debugging.")
	flag.StringVar(&srv.Addr, "addr", "localhost:4430", "host:port to listen on ")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<h1>Hello, world!</h1>Greetings from Go's HTTP/2 server.")
	})

	url := "https://" + srv.Addr + "/"
	log.Printf("Listening on " + url)
	http2.ConfigureServer(&srv, &http2.Server{})

	go func() {
		log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
	}()
	if *openFirefox && runtime.GOOS == "darwin" {
		time.Sleep(250 * time.Millisecond)
		exec.Command("open", "-b", "org.mozilla.nightly", "https://localhost:4430/").Run()
	}
	select {}
}
