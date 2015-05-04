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

func (app *h2i) Main() error {
	cfg := &tls.Config{
		ServerName:         app.host,
		NextProtos:         strings.Split(*flagNextProto, ","),
		InsecureSkipVerify: *flagInsecure,
	}

	hostAndPort := withPort(app.host)
	log.Printf("Connecting to %s ...", hostAndPort)
	tc, err := tls.Dial("tcp", hostAndPort, cfg)
	if err != nil {
		return fmt.Errorf("Error dialing %s: %v", withPort(app.host), err)
	}
	log.Printf("Connected to %v", tc.RemoteAddr())
	defer tc.Close()

	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("TLS handshake: %v", err)
	}
	if !*flagInsecure {
		if err := tc.VerifyHostname(app.host); err != nil {
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

	app.framer = http2.NewFramer(tc, tc)

	oldState, err := terminal.MakeRaw(0)
	if err != nil {
		return err
	}
	defer terminal.Restore(0, oldState)

	var screen = struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stdout}

	app.term = terminal.NewTerminal(screen, "h2i> ")
	app.term.AutoCompleteCallback = func(line string, pos int, key rune) (newLine string, newPos int, ok bool) {
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
	go func() { errc <- app.readFrames() }()
	go func() { errc <- app.readConsole() }()
	return <-errc
}

func (app *h2i) logf(format string, args ...interface{}) {
	fmt.Fprintf(app.term, format+"\n", args...)
}

func (app *h2i) readConsole() error {
	for {
		line, err := app.term.ReadLine()
		if err != nil {
			return fmt.Errorf("terminal.ReadLine: %v", err)
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		cmd, args := f[0], f[1:]
		if _, fn, ok := lookupCommand(cmd); ok {
			err = fn(app, args)
		} else {
			app.logf("Unknown command %q", line)
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

func (app *h2i) cmdQuit(args []string) error {
	if len(args) > 0 {
		app.logf("the QUIT command takes no argument")
		return nil
	}
	return errExitApp
}

func (app *h2i) cmdSettings(args []string) error {
	if len(args) == 1 && args[0] == "ack" {
		return app.framer.WriteSettingsAck()
	}
	app.logf("TODO: unhandled SETTINGS")
	return nil
}

func (app *h2i) cmdPing(args []string) error {
	if len(args) > 1 {
		app.logf("invalid PING usage: only accepts 0 or 1 args")
		return nil // nil means don't end the program
	}
	var data [8]byte
	if len(args) == 1 {
		copy(data[:], args[0])
	} else {
		copy(data[:], "h2i_ping")
	}
	return app.framer.WritePing(false, data)
}

func (app *h2i) cmdHeaders(args []string) error {
	if len(args) > 0 {
		app.logf("Error: HEADERS doesn't yet take arguments.")
		// TODO: flags for restricting window size, to force CONTINUATION
		// frames.
		return nil
	}
	var h1req bytes.Buffer
	app.term.SetPrompt("(as HTTP/1.1)> ")
	defer app.term.SetPrompt("h2i> ")
	for {
		line, err := app.term.ReadLine()
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
		app.logf("Invalid HTTP/1.1 request: %v", err)
		return nil
	}
	if app.streamID == 0 {
		app.streamID = 1
	} else {
		app.streamID += 2
	}
	app.logf("Opening Stream-ID %d:", app.streamID)
	hbf := app.encodeHeaders(req)
	if len(hbf) > 16<<10 {
		app.logf("TODO: h2i doesn't yet write CONTINUATION frames. Copy it from transport.go")
		return nil
	}
	return app.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      app.streamID,
		BlockFragment: hbf,
		EndStream:     req.Method == "GET" || req.Method == "HEAD", // good enough for now
		EndHeaders:    true,                                        // for now
	})
}

func (app *h2i) readFrames() error {
	for {
		f, err := app.framer.ReadFrame()
		if err != nil {
			return fmt.Errorf("ReadFrame: %v", err)
		}
		app.logf("%v", f)
		switch f := f.(type) {
		case *http2.PingFrame:
			app.logf("  Data = %q", f.Data)
		case *http2.SettingsFrame:
			f.ForeachSetting(func(s http2.Setting) error {
				app.logf("  %v", s)
				app.peerSetting[s.ID] = s.Val
				return nil
			})
		case *http2.WindowUpdateFrame:
			app.logf("  Window-Increment = %v\n", f.Increment)
		case *http2.GoAwayFrame:
			app.logf("  Last-Stream-ID = %d; Error-Code = %v (%d)\n", f.LastStreamID, f.ErrCode, f.ErrCode)
		case *http2.DataFrame:
			app.logf("  %q", f.Data())
		case *http2.HeadersFrame:
			if f.HasPriority() {
				app.logf("  PRIORITY = %v", f.Priority)
			}
			if app.hdec == nil {
				// TODO: if the user uses h2i to send a SETTINGS frame advertising
				// something larger, we'll need to respect SETTINGS_HEADER_TABLE_SIZE
				// and stuff here instead of using the 4k default. But for now:
				tableSize := uint32(4 << 10)
				app.hdec = hpack.NewDecoder(tableSize, app.onNewHeaderField)
			}
			app.hdec.Write(f.HeaderBlockFragment())
		}
	}
}

// called from readLoop
func (app *h2i) onNewHeaderField(f hpack.HeaderField) {
	if f.Sensitive {
		app.logf("  %s = %q (SENSITIVE)", f.Name, f.Value)
	}
	app.logf("  %s = %q", f.Name, f.Value)
}

func (app *h2i) encodeHeaders(req *http.Request) []byte {
	app.hbuf.Reset()

	// TODO(bradfitz): figure out :authority-vs-Host stuff between http2 and Go
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	path := req.URL.Path
	if path == "" {
		path = "/"
	}

	app.writeHeader(":authority", host) // probably not right for all sites
	app.writeHeader(":method", req.Method)
	app.writeHeader(":path", path)
	app.writeHeader(":scheme", "https")

	for k, vv := range req.Header {
		lowKey := strings.ToLower(k)
		if lowKey == "host" {
			continue
		}
		for _, v := range vv {
			app.writeHeader(lowKey, v)
		}
	}
	return app.hbuf.Bytes()
}

func (app *h2i) writeHeader(name, value string) {
	app.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
	app.logf(" %s = %s", name, value)
}
