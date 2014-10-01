// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// Package http2 implements the HTTP/2 protocol.
//
// This is a work in progress. This package is low-level and intended
// to be used directly by very few people. Most users will use it
// indirectly through integration with the net/http package. See
// ConfigureServer. That ConfigureServer call will likely be automatic
// or available via an empty import in the future.
//
// This package currently targets draft-14. See http://http2.github.io/
package http2

import (
	"bytes"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"strings"

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
		hs:             hs,
		conn:           c,
		handler:        h,
		framer:         NewFramer(c, c),
		streams:        make(map[uint32]*stream),
		canonHeader:    make(map[string]string),
		readFrameCh:    make(chan frameAndProcessed),
		readFrameErrCh: make(chan error, 1),
		doneServing:    make(chan struct{}),
	}
	sc.hpackDecoder = hpack.NewDecoder(initialHeaderTableSize, sc.onNewHeaderField)
	sc.serve()
}

// frameAndProcessed coordinates the readFrames and serve goroutines, since
// the Framer interface only permits the most recently-read Frame from being
// accessed. The serve goroutine sends on processed to signal to the readFrames
// goroutine that another frame may be read.
type frameAndProcessed struct {
	f         Frame
	processed chan struct{}
}

type serverConn struct {
	hs             *http.Server
	conn           *tls.Conn
	handler        http.Handler
	framer         *Framer
	doneServing    chan struct{}          // closed when serverConn.serve ends
	readFrameCh    chan frameAndProcessed // written by serverConn.readFrames
	readFrameErrCh chan error

	maxStreamID uint32 // max ever seen
	streams     map[uint32]*stream

	// State related to parsing current headers:
	hpackDecoder *hpack.Decoder
	header       http.Header
	canonHeader  map[string]string // http2-lower-case -> Go-Canonical-Case

	method, path, scheme, authority string

	// curHeaderStreamID is non-zero if we're in the middle
	// of parsing headers that span multiple frames.
	curHeaderStreamID uint32
}

type streamState int

const (
	stateIdle streamState = iota
	stateOpen
	stateHalfClosedLocal
	stateHalfClosedRemote
	stateResvLocal
	stateResvRemote
	stateClosed
)

type stream struct {
	id    uint32
	state streamState // owned by serverConn's processing loop
}

func (sc *serverConn) state(streamID uint32) streamState {
	// http://http2.github.io/http2-spec/#rfc.section.5.1
	if st, ok := sc.streams[streamID]; ok {
		return st.state
	}
	// "The first use of a new stream identifier implicitly closes all
	// streams in the "idle" state that might have been initiated by
	// that peer with a lower-valued stream identifier. For example, if
	// a client sends a HEADERS frame on stream 7 without ever sending a
	// frame on stream 5, then stream 5 transitions to the "closed"
	// state when the first frame for stream 7 is sent or received."
	if streamID <= sc.maxStreamID {
		return stateClosed
	}
	return stateIdle
}

func (sc *serverConn) logf(format string, args ...interface{}) {
	if lg := sc.hs.ErrorLog; lg != nil {
		lg.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (sc *serverConn) onNewHeaderField(f hpack.HeaderField) {
	log.Printf("Header field: +%v", f)
	if strings.HasPrefix(f.Name, ":") {
		switch f.Name {
		case ":method":
			sc.method = f.Value
		case ":path":
			sc.path = f.Value
		case ":scheme":
			sc.scheme = f.Value
		case ":authority":
			sc.authority = f.Value
		default:
			log.Printf("Ignoring unknown pseudo-header %q", f.Name)
		}
		return
	}
	sc.header.Add(sc.canonicalHeader(f.Name), f.Value)
}

func (sc *serverConn) canonicalHeader(v string) string {
	// TODO: use a sync.Pool instead of putting the cache on *serverConn?
	cv, ok := sc.canonHeader[v]
	if !ok {
		cv = http.CanonicalHeaderKey(v)
		sc.canonHeader[v] = cv
	}
	return cv
}

func (sc *serverConn) readFrames() {
	processed := make(chan struct{}, 1)
	for {
		f, err := sc.framer.ReadFrame()
		if err != nil {
			close(sc.readFrameCh)
			sc.readFrameErrCh <- err
			return
		}
		sc.readFrameCh <- frameAndProcessed{f, processed}
		<-processed
	}
}

func (sc *serverConn) serve() {
	defer sc.conn.Close()
	defer close(sc.doneServing)

	log.Printf("HTTP/2 connection from %v on %p", sc.conn.RemoteAddr(), sc.hs)

	// Read the client preface
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

	go sc.readFrames()

	for {
		select {
		case fp, ok := <-sc.readFrameCh:
			if !ok {
				err := <-sc.readFrameErrCh
				if err != io.EOF {
					sc.logf("client %s stopped sending frames: %v", sc.conn.RemoteAddr(), err)
				}
				return
			}
			f := fp.f
			log.Printf("got %v: %#v", f.Header(), f)
			err := sc.processFrame(f)
			fp.processed <- struct{}{} // let readFrames proceed
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
		}
	}
}

func (sc *serverConn) processFrame(f Frame) error {
	if s := sc.curHeaderStreamID; s != 0 {
		if cf, ok := f.(*ContinuationFrame); !ok {
			return ConnectionError(ErrCodeProtocol)
		} else if cf.Header().StreamID != s {
			return ConnectionError(ErrCodeProtocol)
		}
	}

	switch f := f.(type) {
	case *SettingsFrame:
		return sc.processSettings(f)
	case *HeadersFrame:
		return sc.processHeaders(f)
	case *ContinuationFrame:
		return sc.processContinuation(f)
	default:
		log.Printf("Ignoring unknown %v", f.Header)
		return nil
	}
}

func (sc *serverConn) processSettings(f *SettingsFrame) error {
	f.ForeachSetting(func(s Setting) {
		log.Printf("  setting %s = %v", s.ID, s.Val)
	})
	return nil
}

func (sc *serverConn) processHeaders(f *HeadersFrame) error {
	id := f.Header().StreamID

	// http://http2.github.io/http2-spec/#rfc.section.5.1.1
	if id%2 != 1 || id <= sc.maxStreamID {
		// Streams initiated by a client MUST use odd-numbered
		// stream identifiers. [...] The identifier of a newly
		// established stream MUST be numerically greater than all
		// streams that the initiating endpoint has opened or
		// reserved. [...]  An endpoint that receives an unexpected
		// stream identifier MUST respond with a connection error
		// (Section 5.4.1) of type PROTOCOL_ERROR.
		return ConnectionError(ErrCodeProtocol)
	}
	if id > sc.maxStreamID {
		sc.maxStreamID = id
	}

	sc.header = make(http.Header)
	sc.curHeaderStreamID = id
	return sc.processHeaderBlockFragment(f.HeaderBlockFragment(), f.HeadersEnded())
}

func (sc *serverConn) processHeaderBlockFragment(frag []byte, end bool) error {
	if _, err := sc.hpackDecoder.Write(frag); err != nil {
		// TODO: convert to stream error I assume?
	}
	if end {
		if err := sc.hpackDecoder.Close(); err != nil {
			// TODO: convert to stream error I assume?
			return err
		}
		sc.curHeaderStreamID = 0
		// TODO: transition state
	}
	return nil
}

func (sc *serverConn) processContinuation(f *ContinuationFrame) error {
	return sc.processHeaderBlockFragment(f.HeaderBlockFragment(), f.HeadersEnded())
}

// ConfigureServer adds HTTP/2 support to a net/http Server.
//
// The configuration conf may be nil.
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
