// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

/*
The h2i command is an interactive HTTP/2 console.

Usage:
  $ h2i [flags] <hostname>

Interactive commands in the console:

  ping [up-to-eight-bytes]

*/
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"

	"github.com/bradfitz/http2"
	"golang.org/x/crypto/ssh/terminal"
)

// Flags
var (
	flagNextProto = flag.String("nextproto", "h2,h2-14", "Comma-separated list of NPN/ALPN protocol names to negotiate.")
	flagInsecure  = flag.Bool("insecure", false, "Whether to skip TLS cert validation")
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: h2i <hostname>\n\n")
	flag.PrintDefaults()
	os.Exit(1)
}

// withPort adds ":443" if another port isn't already present.
func withPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err != nil {
		return net.JoinHostPort(host, "443")
	}
	return host
}

// h2i is the app's state.
type h2i struct {
	host   string
	tc     *tls.Conn
	framer *http2.Framer
	term   *terminal.Terminal
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 1 {
		usage()
	}
	log.SetFlags(0)

	host := flag.Arg(0)
	app := &h2i{
		host: host,
	}
	if err := app.Main(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func (a *h2i) Main() error {
	cfg := &tls.Config{
		ServerName:         a.host,
		NextProtos:         strings.Split(*flagNextProto, ","),
		InsecureSkipVerify: *flagInsecure,
	}

	hostAndPort := withPort(a.host)
	log.Printf("Connecting to %s ...", hostAndPort)
	tc, err := tls.Dial("tcp", hostAndPort, cfg)
	if err != nil {
		return fmt.Errorf("Error dialing %s: %v", withPort(a.host), err)
	}
	log.Printf("Connected to %v", tc.RemoteAddr())
	defer tc.Close()

	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("TLS handshake: %v", err)
	}
	if !*flagInsecure {
		if err := tc.VerifyHostname(a.host); err != nil {
			return fmt.Errorf("VerifyHostname: %v", err)
		}
	}
	state := tc.ConnectionState()
	log.Printf("Negotiated protocol %q", state.NegotiatedProtocol)
	if !state.NegotiatedProtocolIsMutual || state.NegotiatedProtocol == "" {
		return fmt.Errorf("Could not negotiate protocol mutually")
	}

	if _, err := io.WriteString(tc, http2.ClientPreface); err != nil {
		return err
	}

	a.framer = http2.NewFramer(tc, tc)

	oldState, err := terminal.MakeRaw(0)
	if err != nil {
		return err
	}
	defer terminal.Restore(0, oldState)

	var screen = struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stdout}

	a.term = terminal.NewTerminal(screen, "> ")

	errc := make(chan error, 2)
	go func() { errc <- a.readFrames() }()
	go func() { errc <- a.readConsole() }()
	return <-errc
}

func (a *h2i) logf(format string, args ...interface{}) {
	fmt.Fprintf(a.term, format+"\n", args...)
}

func (a *h2i) readConsole() error {
	for {
		line, err := a.term.ReadLine()
		if err != nil {
			return fmt.Errorf("terminal.ReadLine: %v", err)
		}
		if line == "q" || line == "quit" {
			return nil
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		cmd, args := f[0], f[1:]
		cmd = strings.ToLower(cmd)
		switch cmd {
		case "ping":
			err = a.sendPing(args)
		default:
			a.logf("Unknown command %q", line)
		}
		if err != nil {
			return err
		}
	}
}

func (a *h2i) sendPing(args []string) error {
	if len(args) > 1 {
		a.logf("invalid PING usage: only accepts 0 or 1 args")
		return nil // nil means don't end the program
	}
	var data [8]byte
	if len(args) == 1 {
		copy(data[:], args[0])
	} else {
		copy(data[:], "h2i_ping")
	}
	return a.framer.WritePing(false, data)
}

func (a *h2i) readFrames() error {
	for {
		f, err := a.framer.ReadFrame()
		if err != nil {
			return fmt.Errorf("ReadFrame: %v", err)
		}
		switch f := f.(type) {
		case *http2.PingFrame:
			fmt.Fprintf(a.term, "%v %q\n", f, f.Data)
		default:
			fmt.Fprintf(a.term, "%v\n", f)
		}
	}
}
