package main

import (
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"time"

	"github.com/bradfitz/http2"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello world")
	})
	var srv http.Server

	log.Printf("Listening on https://localhost:4430/")
	srv.Addr = "localhost:4430"
	http2.ConfigureServer(&srv, &http2.Server{})

	closeFirefox()
	go func() {
		log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
	}()
	time.Sleep(500 * time.Millisecond)
	exec.Command("open", "-b", "org.mozilla.nightly", "https://localhost:4430/").Run()
	select {}
}

func closeFirefox() {
	exec.Command("open", "/Applications/CloseFirefoxNightly.app").Run()
}
