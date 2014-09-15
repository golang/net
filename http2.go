// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// Package http2 implements the HTTP/2 protocol.
//
// It currently targets draft-14. See http://http2.github.io/
package http2

import (
	"bytes"
	"crypto/tls"
	"io"
	"log"
	"net/http"

	"github.com/bradfitz/http2/hpack"
)

const (
	// ClientPreface is the string that must be sent by new
	// connections from clients.
	ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

var (
	clientPreface = []byte(ClientPreface)
)

const (
	npnProto = "h2-14"

	// http://http2.github.io/http2-spec/#SettingValues
	initialHeaderTableSize = 4096
)

// Server is an HTTP2 server.
type Server struct {
	// MaxStreams optionally ...
	MaxStreams int
}

func (srv *Server) handleConn(hs *http.Server, c *tls.Conn, h http.Handler) {
	sc := &serverConn{
		hs:      hs,
		conn:    c,
		handler: h,
		framer:  NewFramer(c, c),
		streams: make(map[uint32]*stream),
	}
	sc.hpackDecoder = hpack.NewDecoder(initialHeaderTableSize, sc.onNewHeaderField)
	sc.serve()
}

type serverConn struct {
	hs      *http.Server
	conn    *tls.Conn
	handler http.Handler
	framer  *Framer

	hpackDecoder *hpack.Decoder

	// midHeaderStreamID is non-zero if we're in the middle
	// of parsing headers that span multiple frames.
	midHeaderStreamID uint32

	streams map[uint32]*stream
}

type streamState int

const (
	stateOpen streamState = iota
	stateHalfClosedLocal
	stateHalfClosedRemote
	stateResvLocal
	stateResvRemote
)

type stream struct {
	id    uint32
	state streamState
}

func (sc *serverConn) logf(format string, args ...interface{}) {
	if lg := sc.hs.ErrorLog; lg != nil {
		lg.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (sc *serverConn) frameAcceptable(f Frame) error {
	if hf, ok := f.(*HeadersFrame); ok && hf.Header().StreamID%2 != 1 {
		// TODO: all of http://http2.github.io/http2-spec/#rfc.section.5.1.1

	}
	if s := sc.midHeaderStreamID; s != 0 {
		if cf, ok := f.(*ContinuationFrame); !ok {
			return ConnectionError(ErrCodeProtocol)
		} else if cf.Header().StreamID != s {
			return ConnectionError(ErrCodeProtocol)
		}
	}
	return nil
}

func (sc *serverConn) onNewHeaderField(f hpack.HeaderField) {
	log.Printf("Header field: +%v", f)
}

func (sc *serverConn) serve() {
	defer sc.conn.Close()
	log.Printf("HTTP/2 connection from %v on %p", sc.conn.RemoteAddr(), sc.hs)

	buf := make([]byte, len(ClientPreface))
	// TODO: timeout reading from the client
	if _, err := io.ReadFull(sc.conn, buf); err != nil {
		sc.logf("error reading client preface: %v", err)
		return
	}
	if !bytes.Equal(buf, clientPreface) {
		sc.logf("bogus greeting from client: %q", buf)
		return
	}
	log.Printf("client %v said hello", sc.conn.RemoteAddr())
	for {

		f, err := sc.framer.ReadFrame()
		if err == nil {
			err = sc.frameAcceptable(f)
		}
		if h2e, ok := err.(Error); ok {
			if h2e.IsConnectionError() {
				sc.logf("Disconnection; connection error: %v", err)
				return
			}
			// TODO: stream errors, etc
		}
		if err != nil {
			sc.logf("Disconnection due to other error: %v", err)
			return
		}

		log.Printf("got %v: %#v", f.Header(), f)
		switch f := f.(type) {
		case *SettingsFrame:
			f.ForeachSetting(func(s SettingID, v uint32) {
				log.Printf("  setting %s = %v", s, v)
			})
		case *HeadersFrame:
			sc.hpackDecoder.Write(f.HeaderBlockFragment())
			if f.HeadersEnded() {
				sc.midHeaderStreamID = 0
				// TODO: transition state
			}
		case *ContinuationFrame:
			sc.hpackDecoder.Write(f.HeaderBlockFragment())
			if f.HeadersEnded() {
				sc.midHeaderStreamID = 0
				// TODO: transition state
			}
		}
	}
}

// ConfigureServer adds HTTP2 support to s as configured by the HTTP/2
// server configuration in conf. The configuration may be nil.
//
// ConfigureServer must be called before s begins serving.
func ConfigureServer(s *http.Server, conf *Server) {
	if conf == nil {
		conf = new(Server)
	}
	if s.TLSConfig == nil {
		s.TLSConfig = new(tls.Config)
	}
	haveNPN := false
	for _, p := range s.TLSConfig.NextProtos {
		if p == npnProto {
			haveNPN = true
			break
		}
	}
	if !haveNPN {
		s.TLSConfig.NextProtos = append(s.TLSConfig.NextProtos, npnProto)
	}

	if s.TLSNextProto == nil {
		s.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}
	s.TLSNextProto[npnProto] = func(hs *http.Server, c *tls.Conn, h http.Handler) {
		if testHookOnConn != nil {
			testHookOnConn()
		}
		conf.handleConn(hs, c, h)
	}
}

var testHookOnConn func() // for testing
