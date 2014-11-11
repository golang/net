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
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bradfitz/http2/hpack"
)

var VerboseLogs = false

const (
	// ClientPreface is the string that must be sent by new
	// connections from clients.
	ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	// SETTINGS_MAX_FRAME_SIZE default
	// http://http2.github.io/http2-spec/#rfc.section.6.5.2
	initialMaxFrameSize = 16384
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

func (srv *Server) handleConn(hs *http.Server, c net.Conn, h http.Handler) {
	sc := &serverConn{
		hs:                hs,
		conn:              c,
		handler:           h,
		framer:            NewFramer(c, c),
		streams:           make(map[uint32]*stream),
		canonHeader:       make(map[string]string),
		readFrameCh:       make(chan frameAndProcessed),
		readFrameErrCh:    make(chan error, 1),
		writeHeaderCh:     make(chan headerWriteReq), // must not be buffered
		doneServing:       make(chan struct{}),
		maxWriteFrameSize: initialMaxFrameSize,
		serveG:            newGoroutineLock(),
	}
	sc.hpackEncoder = hpack.NewEncoder(&sc.headerWriteBuf)
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
	// Immutable:
	hs             *http.Server
	conn           net.Conn
	handler        http.Handler
	framer         *Framer
	hpackDecoder   *hpack.Decoder
	hpackEncoder   *hpack.Encoder
	doneServing    chan struct{}          // closed when serverConn.serve ends
	readFrameCh    chan frameAndProcessed // written by serverConn.readFrames
	readFrameErrCh chan error
	writeHeaderCh  chan headerWriteReq // must not be buffered
	serveG         goroutineLock       // used to verify funcs are on serve()

	// Everything following is owned by the serve loop; use serveG.check()

	maxStreamID uint32 // max ever seen
	streams     map[uint32]*stream

	maxWriteFrameSize uint32 // TODO: update this when settings come in

	// State related to parsing current headers:
	header            http.Header
	canonHeader       map[string]string // http2-lower-case -> Go-Canonical-Case
	method, path      string
	scheme, authority string
	sawRegularHeader  bool // saw a non-pseudo header already
	invalidHeader     bool

	// curHeaderStreamID and curStream are non-zero if we're in
	// the middle of parsing headers that span multiple frames.
	curHeaderStreamID uint32
	curStream         *stream

	// State related to writing current headers:
	headerWriteBuf bytes.Buffer // written/accessed from serve goroutine
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
	sc.serveG.check()
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

func (sc *serverConn) vlogf(format string, args ...interface{}) {
	if VerboseLogs {
		sc.logf(format, args...)
	}
}

func (sc *serverConn) logf(format string, args ...interface{}) {
	if lg := sc.hs.ErrorLog; lg != nil {
		lg.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (sc *serverConn) condlogf(err error, format string, args ...interface{}) {
	if err == nil {
		return
	}
	str := err.Error()
	if strings.Contains(str, "use of closed network connection") {
		// Boring, expected errors.
		sc.vlogf(format, args...)
	} else {
		sc.logf(format, args...)
	}
}

func (sc *serverConn) onNewHeaderField(f hpack.HeaderField) {
	sc.serveG.check()
	switch {
	case !validHeader(f.Name):
		sc.invalidHeader = true
	case strings.HasPrefix(f.Name, ":"):
		if sc.sawRegularHeader {
			sc.logf("pseudo-header after regular header")
			sc.invalidHeader = true
			return
		}
		var dst *string
		switch f.Name {
		case ":method":
			dst = &sc.method
		case ":path":
			dst = &sc.path
		case ":scheme":
			dst = &sc.scheme
		case ":authority":
			dst = &sc.authority
		default:
			// 8.1.2.1 Pseudo-Header Fields
			// "Endpoints MUST treat a request or response
			// that contains undefined or invalid
			// pseudo-header fields as malformed (Section
			// 8.1.2.6)."
			sc.logf("invalid pseudo-header %q", f.Name)
			sc.invalidHeader = true
			return
		}
		if *dst != "" {
			sc.logf("duplicate pseudo-header %q sent", f.Name)
			sc.invalidHeader = true
			return
		}
		*dst = f.Value
	case f.Name == "cookie":
		sc.sawRegularHeader = true
		if s, ok := sc.header["Cookie"]; ok && len(s) == 1 {
			s[0] = s[0] + "; " + f.Value
		} else {
			sc.header.Add("Cookie", f.Value)
		}
	default:
		sc.sawRegularHeader = true
		sc.header.Add(sc.canonicalHeader(f.Name), f.Value)
	}
}

func (sc *serverConn) canonicalHeader(v string) string {
	sc.serveG.check()
	// TODO: use a sync.Pool instead of putting the cache on *serverConn?
	cv, ok := sc.canonHeader[v]
	if !ok {
		cv = http.CanonicalHeaderKey(v)
		sc.canonHeader[v] = cv
	}
	return cv
}

// readFrames is the loop that reads incoming frames.
// It's run on its own goroutine.
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
	sc.serveG.check()
	defer sc.conn.Close()
	defer close(sc.doneServing)

	sc.vlogf("HTTP/2 connection from %v on %p", sc.conn.RemoteAddr(), sc.hs)

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
	sc.vlogf("client %v said hello", sc.conn.RemoteAddr())

	f, err := sc.framer.ReadFrame()
	if err != nil {
		sc.logf("error reading initial frame from client: %v", err)
		return
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		sc.logf("invalid initial frame type %T received from client", f)
		return
	}
	sf.ForeachSetting(func(s Setting) {
		// TODO: process, record
	})

	// TODO: don't send two network packets for our SETTINGS + our
	// ACK of their settings.  But if we make framer write to a
	// *bufio.Writer, that increases the per-connection memory
	// overhead, and there could be many idle conns. So maybe some
	// liveswitchWriter-like thing where we only switch to a
	// *bufio Writer when we really need one temporarily, else go
	// back to an unbuffered writes by default.
	if err := sc.framer.WriteSettings( /* TODO: actual settings */ ); err != nil {
		sc.logf("error writing server's initial settings: %v", err)
		return
	}
	if err := sc.framer.WriteSettingsAck(); err != nil {
		sc.logf("error writing server's ack of client's settings: %v", err)
		return
	}

	go sc.readFrames()

	for {
		select {
		case hr := <-sc.writeHeaderCh:
			if err := sc.writeHeaderInLoop(hr); err != nil {
				sc.condlogf(err, "error writing response header: %v", err)
				return
			}
		case fp, ok := <-sc.readFrameCh:
			if !ok {
				err := <-sc.readFrameErrCh
				if err != io.EOF {
					errstr := err.Error()
					if !strings.Contains(errstr, "use of closed network connection") {
						sc.logf("client %s stopped sending frames: %v", sc.conn.RemoteAddr(), errstr)
					}
				}
				return
			}
			f := fp.f
			sc.vlogf("got %v: %#v", f.Header(), f)
			err := sc.processFrame(f)
			fp.processed <- struct{}{} // let readFrames proceed
			switch ev := err.(type) {
			case nil:
				// nothing.
			case StreamError:
				if err := sc.resetStreamInLoop(ev); err != nil {
					sc.logf("Error writing RSTSTream: %v", err)
					return
				}
			case ConnectionError:
				sc.logf("Disconnecting; %v", ev)
				return
			default:
				sc.logf("Disconnection due to other error: %v", err)
				return
			}
		}
	}
}

func (sc *serverConn) resetStreamInLoop(se StreamError) error {
	sc.serveG.check()
	if err := sc.framer.WriteRSTStream(se.streamID, uint32(se.code)); err != nil {
		return err
	}
	delete(sc.streams, se.streamID)
	return nil
}

func (sc *serverConn) processFrame(f Frame) error {
	sc.serveG.check()

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
	sc.serveG.check()
	f.ForeachSetting(func(s Setting) {
		log.Printf("  setting %s = %v", s.ID, s.Val)
	})
	return nil
}

func (sc *serverConn) processHeaders(f *HeadersFrame) error {
	sc.serveG.check()
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

	st := &stream{
		id:    id,
		state: stateOpen,
	}
	if f.Header().Flags.Has(FlagHeadersEndStream) {
		st.state = stateHalfClosedRemote
	}
	sc.streams[id] = st

	sc.header = make(http.Header)
	sc.method = ""
	sc.path = ""
	sc.scheme = ""
	sc.authority = ""
	sc.invalidHeader = false
	sc.sawRegularHeader = false
	sc.curHeaderStreamID = id
	sc.curStream = st
	return sc.processHeaderBlockFragment(id, f.HeaderBlockFragment(), f.HeadersEnded())
}

func (sc *serverConn) processContinuation(f *ContinuationFrame) error {
	sc.serveG.check()
	id := f.Header().StreamID
	if sc.curHeaderStreamID != id {
		return ConnectionError(ErrCodeProtocol)
	}
	return sc.processHeaderBlockFragment(id, f.HeaderBlockFragment(), f.HeadersEnded())
}

func (sc *serverConn) processHeaderBlockFragment(streamID uint32, frag []byte, end bool) error {
	sc.serveG.check()
	if _, err := sc.hpackDecoder.Write(frag); err != nil {
		// TODO: convert to stream error I assume?
		return err
	}
	if !end {
		return nil
	}
	if err := sc.hpackDecoder.Close(); err != nil {
		// TODO: convert to stream error I assume?
		return err
	}
	if sc.invalidHeader || sc.method == "" || sc.path == "" ||
		(sc.scheme != "https" && sc.scheme != "http") {
		// See 8.1.2.6 Malformed Requests and Responses:
		//
		// Malformed requests or responses that are detected
		// MUST be treated as a stream error (Section 5.4.2)
		// of type PROTOCOL_ERROR."
		//
		// 8.1.2.3 Request Pseudo-Header Fields
		// "All HTTP/2 requests MUST include exactly one valid
		// value for the :method, :scheme, and :path
		// pseudo-header fields"
		return StreamError{streamID, ErrCodeProtocol}
	}
	curStream := sc.curStream
	sc.curHeaderStreamID = 0
	sc.curStream = nil

	// TODO: transition streamID state
	go sc.startHandler(curStream.id, curStream.state == stateOpen, sc.method, sc.path, sc.scheme, sc.authority, sc.header)

	return nil
}

// Run on its own goroutine.
func (sc *serverConn) startHandler(streamID uint32, bodyOpen bool, method, path, scheme, authority string, reqHeader http.Header) {
	var tlsState *tls.ConnectionState // make this non-nil if https
	if scheme == "https" {
		// TODO: get from sc's ConnectionState
		tlsState = &tls.ConnectionState{}
	}
	if authority == "" {
		authority = reqHeader.Get("Host")
	}
	req := &http.Request{
		Method:     method,
		URL:        &url.URL{},
		RemoteAddr: sc.conn.RemoteAddr().String(),
		Header:     reqHeader,
		RequestURI: path,
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		ProtoMinor: 0,
		TLS:        tlsState,
		Host:       authority,
		Body: &requestBody{
			sc:       sc,
			streamID: streamID,
			hasBody:  bodyOpen,
		},
	}
	if bodyOpen {
		if vv, ok := reqHeader["Content-Length"]; ok {
			req.ContentLength, _ = strconv.ParseInt(vv[0], 10, 64)
		} else {
			req.ContentLength = -1
		}
	}
	rw := &responseWriter{
		sc:       sc,
		streamID: streamID,
	}
	defer rw.handlerDone()
	// TODO: catch panics like net/http.Server
	sc.handler.ServeHTTP(rw, req)
}

// called from handler goroutines
func (sc *serverConn) writeData(streamID uint32, p []byte) (n int, err error) {
	// TODO: implement
	log.Printf("WRITE on %d: %q", streamID, p)
	return len(p), nil
}

// headerWriteReq is a request to write an HTTP response header from a server Handler.
type headerWriteReq struct {
	streamID    uint32
	httpResCode int
	h           http.Header // may be nil
	endStream   bool
}

// called from handler goroutines.
// h may be nil.
func (sc *serverConn) writeHeader(req headerWriteReq) {
	sc.writeHeaderCh <- req
}

func (sc *serverConn) writeHeaderInLoop(req headerWriteReq) error {
	sc.serveG.check()
	sc.headerWriteBuf.Reset()
	sc.hpackEncoder.WriteField(hpack.HeaderField{Name: ":status", Value: httpCodeString(req.httpResCode)})
	for k, vv := range req.h {
		for _, v := range vv {
			// TODO: for gargage, cache lowercase copies of headers at
			// least for common ones and/or popular recent ones for
			// this serverConn. LRU?
			sc.hpackEncoder.WriteField(hpack.HeaderField{Name: strings.ToLower(k), Value: v})
		}
	}
	headerBlock := sc.headerWriteBuf.Bytes()
	if len(headerBlock) > int(sc.maxWriteFrameSize) {
		// we'll need continuation ones.
		panic("TODO")
	}
	return sc.framer.WriteHeaders(HeadersFrameParam{
		StreamID:      req.streamID,
		BlockFragment: headerBlock,
		EndStream:     req.endStream,
		EndHeaders:    true, // no continuation yet
	})
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

type requestBody struct {
	sc       *serverConn
	streamID uint32
	hasBody  bool
	closed   bool
}

func (b *requestBody) Close() error {
	b.closed = true
	return nil
}

func (b *requestBody) Read(p []byte) (n int, err error) {
	if !b.hasBody {
		return 0, io.EOF
	}
	// TODO: implement
	return 0, errors.New("TODO: we don't handle request bodies yet")
}

type responseWriter struct {
	sc           *serverConn
	streamID     uint32
	wroteHeaders bool
	h            http.Header
}

// TODO: bufio writing of responseWriter. add Flush, add pools of
// bufio.Writers, adjust bufio writer sized based on frame size
// updates from peer? For now: naive.

func (w *responseWriter) Header() http.Header {
	if w.h == nil {
		w.h = make(http.Header)
	}
	return w.h
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeaders {
		return
	}
	// TODO: defer actually writing this frame until a Flush or
	// handlerDone, like net/http's Server. then we can coalesce
	// e.g. a 204 response to have a Header response frame with
	// END_STREAM set, without a separate frame being sent in
	// handleDone.
	w.wroteHeaders = true
	w.sc.writeHeader(headerWriteReq{
		streamID:    w.streamID,
		httpResCode: code,
		h:           w.h,
	})
}

// TODO: responseWriter.WriteString too?

func (w *responseWriter) Write(p []byte) (n int, err error) {
	if !w.wroteHeaders {
		w.WriteHeader(200)
	}
	return w.sc.writeData(w.streamID, p) // blocks waiting for tokens
}

func (w *responseWriter) handlerDone() {
	if !w.wroteHeaders {
		w.sc.writeHeader(headerWriteReq{
			streamID:    w.streamID,
			httpResCode: 200,
			h:           w.h,
			endStream:   true, // handler has finished; can't be any data.
		})
	}
}

var testHookOnConn func() // for testing

func validHeader(v string) bool {
	if len(v) == 0 {
		return false
	}
	for _, r := range v {
		// "Just as in HTTP/1.x, header field names are
		// strings of ASCII characters that are compared in a
		// case-insensitive fashion. However, header field
		// names MUST be converted to lowercase prior to their
		// encoding in HTTP/2. "
		if r >= 127 || ('A' <= r && r <= 'Z') {
			return false
		}
	}
	return true
}

var httpCodeStringCommon = map[int]string{} // n -> strconv.Itoa(n)

func init() {
	for i := 100; i <= 999; i++ {
		if v := http.StatusText(i); v != "" {
			httpCodeStringCommon[i] = strconv.Itoa(i)
		}
	}
}

func httpCodeString(code int) string {
	if s, ok := httpCodeStringCommon[code]; ok {
		return s
	}
	return strconv.Itoa(code)
}
