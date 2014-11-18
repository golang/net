// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"camlistore.org/pkg/googlestorage"
	"camlistore.org/pkg/singleflight"
	"github.com/bradfitz/http2"
)

var (
	openFirefox = flag.Bool("openff", false, "Open Firefox")
	prod        = flag.Bool("prod", false, "Whether to configure itself to be the production http2.golang.org server.")
)

func homeOldHTTP(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, `<html>
<body>
<h1>Go + HTTP/2</h1>
<p>Welcome to <a href="https://golang.org/">the Go language</a>'s <a href="https://http2.github.io/">HTTP/2</a> demo & interop server.</p>
<p>Unfortunately, you're <b>not</b> using HTTP/2 right now.</p>
<p>See code & instructions for connecting at <a href="https://github.com/bradfitz/http2">https://github.com/bradfitz/http2</a>.</p>

</body></html>`)
}

func home(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, `<html>
<body>
<h1>Go + HTTP/2</h1>

<p>Welcome to <a href="https://golang.org/">the Go language</a>'s <a
href="https://http2.github.io/">HTTP/2</a> demo & interop server.</p>

<p>Congratulations, <b>you're using HTTP/2 right now</b>.</p>

<p>This server exists for others in the HTTP/2 community to test their HTTP/2 client implementations and point out flaws in our server.</p>

<p> The code is currently at <a
href="https://github.com/bradfitz/http2">github.com/bradfitz/http2</a>
but will move to the Go standard library at some point in the future
(enabled by default, without users needing to change their code).</p>

<p>Contact info: <i>bradfitz@golang.org</i>, or <a
href="https://github.com/bradfitz/http2/issues">file a bug</a>.</p>

<h2>Handlers for testing</h2>
<ul>
  <li>GET <a href="/reqinfo">/reqinfo</a> to dump the request + headers received</li>
  <li>GET <a href="/clockstream">/clockstream</a> streams the current time every second</li>
  <li>GET <a href="/file/gopher.png">/file/gopher.png</a> for a small file (does If-Modified-Since, Content-Range, etc)</li>
  <li>GET <a href="/file/go.src.tar.gz">/file/go.src.tar.gz</a> for a larger file (~10 MB)</li>
  <li>GET <a href="/redirect">/redirect</a> to redirect back to / (this page)</li>
  <li>PUT something to <a href="/crc32">/crc32</a> to get a count of number of bytes and its CRC-32</li>
</ul>

</body></html>`)
}

func reqInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "Method: %s\n", r.Method)
	fmt.Fprintf(w, "Protocol: %s\n", r.Proto)
	fmt.Fprintf(w, "Host: %s\n", r.Host)
	fmt.Fprintf(w, "RemoteAddr: %s\n", r.RemoteAddr)
	fmt.Fprintf(w, "RequestURI: %q\n", r.RequestURI)
	fmt.Fprintf(w, "URL: %#v\n", r.URL)
	fmt.Fprintf(w, "Body.ContentLength: %d (-1 means unknown)\n", r.ContentLength)
	fmt.Fprintf(w, "Close: %v (relevant for HTTP/1 only)\n", r.Close)
	fmt.Fprintf(w, "TLS: %#v\n", r.TLS)
	fmt.Fprintf(w, "\nHeaders:\n")
	r.Header.Write(w)
}

func crcHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PUT" {
		http.Error(w, "PUT required.", 400)
		return
	}
	crc := crc32.NewIEEE()
	n, err := io.Copy(crc, r.Body)
	if err == nil {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "bytes=%d, CRC32=%x", n, crc.Sum(nil))
	}
}

var (
	fsGrp   singleflight.Group
	fsMu    sync.Mutex // guards fsCache
	fsCache = map[string]http.Handler{}
)

// fileServer returns a file-serving handler that proxies URL.
// It lazily fetches URL on the first access and caches its contents forever.
func fileServer(url string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hi, err := fsGrp.Do(url, func() (interface{}, error) {
			fsMu.Lock()
			if h, ok := fsCache[url]; ok {
				fsMu.Unlock()
				return h, nil
			}
			fsMu.Unlock()

			res, err := http.Get(url)
			if err != nil {
				return nil, err
			}
			defer res.Body.Close()
			slurp, err := ioutil.ReadAll(res.Body)
			if err != nil {
				return nil, err
			}

			modTime := time.Now()
			var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.ServeContent(w, r, path.Base(url), modTime, bytes.NewReader(slurp))
			})
			fsMu.Lock()
			fsCache[url] = h
			fsMu.Unlock()
			return h, nil
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		hi.(http.Handler).ServeHTTP(w, r)
	})
}

func clockStreamHandler(w http.ResponseWriter, r *http.Request) {
	clientGone := w.(http.CloseNotifier).CloseNotify()
	w.Header().Set("Content-Type", "text/plain")
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	fmt.Fprintf(w, "# ~1KB of junk to force browsers to start rendering immediately: \n")
	io.WriteString(w, strings.Repeat("# xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", 13))

	for {
		fmt.Fprintf(w, "%v\n", time.Now())
		w.(http.Flusher).Flush()
		select {
		case <-ticker.C:
		case <-clientGone:
			log.Printf("Client %v disconnected from the clock", r.RemoteAddr)
			return
		}
	}
}

func registerHandlers() {
	mux2 := http.NewServeMux()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			http.Redirect(w, r, "https://http2.golang.org/", http.StatusFound)
			return
		}
		if r.ProtoMajor == 1 {
			if r.URL.Path == "/reqinfo" {
				reqInfoHandler(w, r)
			} else {
				homeOldHTTP(w, r)
			}
			return
		}
		mux2.ServeHTTP(w, r)
	})
	mux2.HandleFunc("/", home)
	mux2.Handle("/file/gopher.png", fileServer("https://golang.org/doc/gopher/frontpage.png"))
	mux2.Handle("/file/go.src.tar.gz", fileServer("https://storage.googleapis.com/golang/go1.4rc1.src.tar.gz"))
	mux2.HandleFunc("/reqinfo", reqInfoHandler)
	mux2.HandleFunc("/crc32", crcHandler)
	mux2.HandleFunc("/clockstream", clockStreamHandler)
	mux2.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
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
