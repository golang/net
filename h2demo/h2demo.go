// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"camlistore.org/pkg/googlestorage"
	"github.com/bradfitz/http2"
)

var (
	openFirefox = flag.Bool("openff", false, "Open Firefox")
	prod        = flag.Bool("prod", false, "Whether to configure itself to be the production http2.golang.org server.")
)

func oldHTTPHandler(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, `<html>
<body>
<h1>Go + HTTP/2</h1>
<p>Welcome to <a href="https://golang.org/">the Go language</a>'s <a href="https://http2.github.io/">HTTP/2</a> demo & interop server.</p>
<p>Unfortunately, <b>you're not using HTTP/2 right now</b>.</p>
<p>See code & instructions for connecting at <a href="https://github.com/bradfitz/http2">https://github.com/bradfitz/http2</a>.</p>

</body></html>`)
}

func registerHandlers() {
	mux := http.NewServeMux()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			http.Redirect(w, r, "https://http2.golang.org/", http.StatusFound)
			return
		}
		if r.ProtoMajor == 1 {
			oldHTTPHandler(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// TODO: more
		io.WriteString(w, "<h1>Hello, world!</h1>Greetings from Go's HTTP/2 server. You're speaking HTTP/2.")
	})
}

func serveProdTLS() error {
	c, err := googlestorage.NewServiceClient()
	if err != nil {
		return err
	}
	slurp := func(key string) ([]byte, error) {
		const bucket = "http2-demo-server-tls"
		rc, _, err := c.GetObject(&googlestorage.Object{
			Bucket: bucket,
			Key:    key,
		})
		if err != nil {
			return nil, fmt.Errorf("Error fetching GCS object %q in bucket %q: %v", key, bucket, err)
		}
		defer rc.Close()
		return ioutil.ReadAll(rc)
	}
	certPem, err := slurp("http2.golang.org.chained.pem")
	if err != nil {
		return err
	}
	keyPem, err := slurp("http2.golang.org.key")
	if err != nil {
		return err
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		return err
	}
	srv := &http.Server{
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}
	http2.ConfigureServer(srv, &http2.Server{})
	ln, err := net.Listen("tcp", ":443")
	if err != nil {
		return err
	}
	return srv.Serve(tls.NewListener(tcpKeepAliveListener{ln.(*net.TCPListener)}, srv.TLSConfig))
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}

func serveProd() error {
	errc := make(chan error, 2)
	go func() { errc <- http.ListenAndServe(":80", nil) }()
	go func() { errc <- serveProdTLS() }()
	return <-errc
}

func main() {
	var srv http.Server
	flag.BoolVar(&http2.VerboseLogs, "verbose", false, "Verbose HTTP/2 debugging.")
	flag.StringVar(&srv.Addr, "addr", "localhost:4430", "host:port to listen on ")
	flag.Parse()

	registerHandlers()

	if *prod {
		log.Fatal(serveProd())
	}

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
