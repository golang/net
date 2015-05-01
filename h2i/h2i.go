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

  ping [data]
  settings ack
*/
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/bradfitz/http2"
	"github.com/bradfitz/http2/hpack"
	"golang.org/x/crypto/ssh/terminal"
)

// Flags
var (
	flagNextProto = flag.String("nextproto", "h2,h2-14", "Comma-separated list of NPN/ALPN protocol names to negotiate.")
	flagInsecure  = flag.Bool("insecure", false, "Whether to skip TLS cert validation")
)

type command func(*h2i, []string) error

var commands = map[string]command{
	"ping":     (*h2i).cmdPing,
	"settings": (*h2i).cmdSettings,
	"quit":     (*h2i).cmdQuit,
	"headers":  (*h2i).cmdHeaders,
}

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

	// owned by the command loop:
	streamID uint32
	hbuf     bytes.Buffer
	henc     *hpack.Encoder

	// owned by the readFrames loop:
	peerSetting map[http2.SettingID]uint32
	hdec        *hpack.Decoder
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
		host:        host,
		peerSetting: make(map[http2.SettingID]uint32),
	}
	app.henc = hpack.NewEncoder(&app.hbuf)

	if err := app.Main(); err != nil {
		if app.term != nil {
			app.logf("%v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
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

	a.term = terminal.NewTerminal(screen, "h2i> ")
	a.term.AutoCompleteCallback = func(line string, pos int, key rune) (newLine string, newPos int, ok bool) {
		if key != '\t' {
			return
		}
		name, _, ok := lookupCommand(line)
		if !ok {
			return
		}
		return name, len(name), true
	}

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
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		cmd, args := f[0], f[1:]
		if _, fn, ok := lookupCommand(cmd); ok {
			err = fn(a, args)
		} else {
			a.logf("Unknown command %q", line)
		}
		if err == errExitApp {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func lookupCommand(prefix string) (name string, c command, ok bool) {
	prefix = strings.ToLower(prefix)
	if c, ok = commands[prefix]; ok {
		return prefix, c, ok
	}

	for full, candidate := range commands {
		if strings.HasPrefix(full, prefix) {
			if c != nil {
				return "", nil, false // ambiguous
			}
			c = candidate
			name = full
		}
	}
	return name, c, c != nil
}

var errExitApp = errors.New("internal sentinel error value to quit the console reading loop")

func (a *h2i) cmdQuit(args []string) error {
	if len(args) > 0 {
		a.logf("the QUIT command takes no argument")
		return nil
	}
	return errExitApp
}

func (a *h2i) cmdSettings(args []string) error {
	if len(args) == 1 && args[0] == "ack" {
		return a.framer.WriteSettingsAck()
	}
	a.logf("TODO: unhandled SETTINGS")
	return nil
}

func (a *h2i) cmdPing(args []string) error {
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

func (a *h2i) cmdHeaders(args []string) error {
	if len(args) > 0 {
		a.logf("Error: HEADERS doesn't yet take arguments.")
		// TODO: flags for restricting window size, to force CONTINUATION
		// frames.
		return nil
	}
	var h1req bytes.Buffer
	a.term.SetPrompt("(as HTTP/1.1)> ")
	defer a.term.SetPrompt("h2i> ")
	for {
		line, err := a.term.ReadLine()
		if err != nil {
			return err
		}
		h1req.WriteString(line)
		h1req.WriteString("\r\n")
		if line == "" {
			break
		}
	}
	req, err := http.ReadRequest(bufio.NewReader(&h1req))
	if err != nil {
		a.logf("Invalid HTTP/1.1 request: %v", err)
		return nil
	}
	if a.streamID == 0 {
		a.streamID = 1
	} else {
		a.streamID += 2
	}
	a.logf("Opening Stream-ID %d:", a.streamID)
	hbf := a.encodeHeaders(req)
	if len(hbf) > 16<<10 {
		a.logf("TODO: h2i doesn't yet write CONTINUATION frames. Copy it from transport.go")
		return nil
	}
	return a.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      a.streamID,
		BlockFragment: hbf,
		EndStream:     req.Method == "GET" || req.Method == "HEAD", // good enough for now
		EndHeaders:    true,                                        // for now
	})
}

func (a *h2i) readFrames() error {
	for {
		f, err := a.framer.ReadFrame()
		if err != nil {
			return fmt.Errorf("ReadFrame: %v", err)
		}
		a.logf("%v", f)
		switch f := f.(type) {
		case *http2.PingFrame:
			a.logf("  Data = %q", f.Data)
		case *http2.SettingsFrame:
			f.ForeachSetting(func(s http2.Setting) error {
				a.logf("  %v", s)
				a.peerSetting[s.ID] = s.Val
				return nil
			})
		case *http2.WindowUpdateFrame:
			a.logf("  Window-Increment = %v\n", f.Increment)
		case *http2.GoAwayFrame:
			a.logf("  Last-Stream-ID = %d; Error-Code = %v (%d)\n", f.LastStreamID, f.ErrCode, f.ErrCode)
		case *http2.DataFrame:
			a.logf("  %q", f.Data())
		case *http2.HeadersFrame:
			if f.HasPriority() {
				a.logf("  PRIORITY = %v", f.Priority)
			}
			if a.hdec == nil {
				// TODO: if the user uses h2i to send a SETTINGS frame advertising
				// something larger, we'll need to respect SETTINGS_HEADER_TABLE_SIZE
				// and stuff here instead of using the 4k default. But for now:
				tableSize := uint32(4 << 10)
				a.hdec = hpack.NewDecoder(tableSize, a.onNewHeaderField)
			}
			a.hdec.Write(f.HeaderBlockFragment())
		}
	}
}

// called from readLoop
func (a *h2i) onNewHeaderField(f hpack.HeaderField) {
	if f.Sensitive {
		a.logf("  %s = %q (SENSITIVE)", f.Name, f.Value)
	}
	a.logf("  %s = %q", f.Name, f.Value)
}

func (a *h2i) encodeHeaders(req *http.Request) []byte {
	a.hbuf.Reset()

	// TODO(bradfitz): figure out :authority-vs-Host stuff between http2 and Go
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	path := req.URL.Path
	if path == "" {
		path = "/"
	}

	a.writeHeader(":authority", host) // probably not right for all sites
	a.writeHeader(":method", req.Method)
	a.writeHeader(":path", path)
	a.writeHeader(":scheme", "https")

	for k, vv := range req.Header {
		lowKey := strings.ToLower(k)
		if lowKey == "host" {
			continue
		}
		for _, v := range vv {
			a.writeHeader(lowKey, v)
		}
	}
	return a.hbuf.Bytes()
}

func (a *h2i) writeHeader(name, value string) {
	a.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
	a.logf(" %s = %s", name, value)
}
