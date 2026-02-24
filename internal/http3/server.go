// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"context"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/internal/httpcommon"
	"golang.org/x/net/quic"
)

// A Server is an HTTP/3 server.
// The zero value for Server is a valid server.
type Server struct {
	// Handler to invoke for requests, http.DefaultServeMux if nil.
	Handler http.Handler

	// Config is the QUIC configuration used by the server.
	// The Config may be nil.
	//
	// ListenAndServe may clone and modify the Config.
	// The Config must not be modified after calling ListenAndServe.
	Config *quic.Config

	initOnce sync.Once
}

func (s *Server) init() {
	s.initOnce.Do(func() {
		s.Config = initConfig(s.Config)
		if s.Handler == nil {
			s.Handler = http.DefaultServeMux
		}
	})
}

// ListenAndServe listens on the UDP network address addr
// and then calls Serve to handle requests on incoming connections.
func (s *Server) ListenAndServe(addr string) error {
	s.init()
	e, err := quic.Listen("udp", addr, s.Config)
	if err != nil {
		return err
	}
	return s.Serve(e)
}

// Serve accepts incoming connections on the QUIC endpoint e,
// and handles requests from those connections.
func (s *Server) Serve(e *quic.Endpoint) error {
	s.init()
	for {
		qconn, err := e.Accept(context.Background())
		if err != nil {
			return err
		}
		go newServerConn(qconn, s.Handler)
	}
}

type serverConn struct {
	qconn *quic.Conn

	genericConn // for handleUnidirectionalStream
	enc         qpackEncoder
	dec         qpackDecoder
	handler     http.Handler
}

func newServerConn(qconn *quic.Conn, handler http.Handler) {
	sc := &serverConn{
		qconn:   qconn,
		handler: handler,
	}
	sc.enc.init()

	// Create control stream and send SETTINGS frame.
	// TODO: Time out on creating stream.
	controlStream, err := newConnStream(context.Background(), sc.qconn, streamTypeControl)
	if err != nil {
		return
	}
	controlStream.writeSettings()
	controlStream.Flush()

	sc.acceptStreams(sc.qconn, sc)
}

func (sc *serverConn) handleControlStream(st *stream) error {
	// "A SETTINGS frame MUST be sent as the first frame of each control stream [...]"
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-7.2.4-2
	if err := st.readSettings(func(settingsType, settingsValue int64) error {
		switch settingsType {
		case settingsMaxFieldSectionSize:
			_ = settingsValue // TODO
		case settingsQPACKMaxTableCapacity:
			_ = settingsValue // TODO
		case settingsQPACKBlockedStreams:
			_ = settingsValue // TODO
		default:
			// Unknown settings types are ignored.
		}
		return nil
	}); err != nil {
		return err
	}

	for {
		ftype, err := st.readFrameHeader()
		if err != nil {
			return err
		}
		switch ftype {
		case frameTypeCancelPush:
			// "If a server receives a CANCEL_PUSH frame for a push ID
			// that has not yet been mentioned by a PUSH_PROMISE frame,
			// this MUST be treated as a connection error of type H3_ID_ERROR."
			// https://www.rfc-editor.org/rfc/rfc9114.html#section-7.2.3-8
			return &connectionError{
				code:    errH3IDError,
				message: "CANCEL_PUSH for unsent push ID",
			}
		case frameTypeGoaway:
			return errH3NoError
		default:
			// Unknown frames are ignored.
			if err := st.discardUnknownFrame(ftype); err != nil {
				return err
			}
		}
	}
}

func (sc *serverConn) handleEncoderStream(*stream) error {
	// TODO
	return nil
}

func (sc *serverConn) handleDecoderStream(*stream) error {
	// TODO
	return nil
}

func (sc *serverConn) handlePushStream(*stream) error {
	// "[...] if a server receives a client-initiated push stream,
	// this MUST be treated as a connection error of type H3_STREAM_CREATION_ERROR."
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.2.2-3
	return &connectionError{
		code:    errH3StreamCreationError,
		message: "client created push stream",
	}
}

type pseudoHeader struct {
	method    string
	scheme    string
	path      string
	authority string
}

func (sc *serverConn) parseHeader(st *stream) (http.Header, pseudoHeader, error) {
	ftype, err := st.readFrameHeader()
	if err != nil {
		return nil, pseudoHeader{}, err
	}
	if ftype != frameTypeHeaders {
		return nil, pseudoHeader{}, err
	}
	header := make(http.Header)
	var pHeader pseudoHeader
	var dec qpackDecoder
	if err := dec.decode(st, func(_ indexType, name, value string) error {
		switch name {
		case ":method":
			pHeader.method = value
		case ":scheme":
			pHeader.scheme = value
		case ":path":
			pHeader.path = value
		case ":authority":
			pHeader.authority = value
		default:
			header.Add(name, value)
		}
		return nil
	}); err != nil {
		return nil, pseudoHeader{}, err
	}
	if err := st.endFrame(); err != nil {
		return nil, pseudoHeader{}, err
	}
	return header, pHeader, nil
}

func (sc *serverConn) handleRequestStream(st *stream) error {
	header, pHeader, err := sc.parseHeader(st)
	if err != nil {
		return err
	}

	reqInfo := httpcommon.NewServerRequest(httpcommon.ServerRequestParam{
		Method:    pHeader.method,
		Scheme:    pHeader.scheme,
		Authority: pHeader.authority,
		Path:      pHeader.path,
		Header:    header,
	})
	if reqInfo.InvalidReason != "" {
		return &streamError{
			code:    errH3MessageError,
			message: reqInfo.InvalidReason,
		}
	}

	var body io.ReadCloser
	contentLength := int64(-1)
	if n, err := strconv.Atoi(header.Get("Content-Length")); err == nil {
		contentLength = int64(n)
	}
	if contentLength != 0 || len(reqInfo.Trailer) != 0 {
		body = &bodyReader{
			st:      st,
			remain:  contentLength,
			trailer: reqInfo.Trailer,
		}
	} else {
		body = http.NoBody
	}

	req := &http.Request{
		Proto:         "HTTP/3.0",
		Method:        pHeader.method,
		Host:          pHeader.authority,
		URL:           reqInfo.URL,
		RequestURI:    reqInfo.RequestURI,
		Trailer:       reqInfo.Trailer,
		ProtoMajor:    3,
		RemoteAddr:    sc.qconn.RemoteAddr().String(),
		Body:          body,
		Header:        header,
		ContentLength: contentLength,
	}
	defer req.Body.Close()

	rw := &responseWriter{
		st:             st,
		headers:        make(http.Header),
		trailer:        make(http.Header),
		bb:             make(bodyBuffer, 0, defaultBodyBufferCap),
		cannotHaveBody: req.Method == "HEAD",
		bw: &bodyWriter{
			st:     st,
			remain: -1,
			flush:  false,
			name:   "response",
			enc:    &sc.enc,
		},
	}
	defer rw.close()
	if reqInfo.NeedsContinue {
		req.Body.(*bodyReader).send100Continue = func() {
			rw.mu.Lock()
			defer rw.mu.Unlock()
			if rw.wroteHeader {
				return
			}
			encHeaders := rw.bw.enc.encode(func(f func(itype indexType, name, value string)) {
				f(mayIndex, ":status", strconv.Itoa(http.StatusContinue))
			})
			rw.st.writeVarint(int64(frameTypeHeaders))
			rw.st.writeVarint(int64(len(encHeaders)))
			rw.st.Write(encHeaders)
			rw.st.Flush()
		}
	}

	// TODO: handle panic coming from the HTTP handler.
	sc.handler.ServeHTTP(rw, req)
	return nil
}

// abort closes the connection with an error.
func (sc *serverConn) abort(err error) {
	if e, ok := err.(*connectionError); ok {
		sc.qconn.Abort(&quic.ApplicationError{
			Code:   uint64(e.code),
			Reason: e.message,
		})
	} else {
		sc.qconn.Abort(err)
	}
}

// responseCanHaveBody reports whether a given response status code permits a
// body. See RFC 7230, section 3.3.
func responseCanHaveBody(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == 204:
		return false
	case status == 304:
		return false
	}
	return true
}

type responseWriter struct {
	st             *stream
	bw             *bodyWriter
	mu             sync.Mutex
	headers        http.Header
	trailer        http.Header
	bb             bodyBuffer
	wroteHeader    bool // Non-1xx header has been (logically) written.
	statusCode     int  // Status of the response that will be sent in HEADERS frame.
	statusCodeSet  bool // Status of the response has been set via a call to WriteHeader.
	cannotHaveBody bool // Response should not have a body (e.g. response to a HEAD request).
	bodyLenLeft    int  // How much of the content body is left to be sent, set via "Content-Length" header. -1 if unknown.
}

func (rw *responseWriter) Header() http.Header {
	return rw.headers
}

// prepareTrailerForWriteLocked populates any pre-declared trailer header with
// its value, and passes it to bodyWriter so it can be written after body EOF.
// Caller must hold rw.mu.
func (rw *responseWriter) prepareTrailerForWriteLocked() {
	for name := range rw.trailer {
		if val, ok := rw.headers[name]; ok {
			rw.trailer[name] = val
		} else {
			delete(rw.trailer, name)
		}
	}
	if len(rw.trailer) > 0 {
		rw.bw.trailer = rw.trailer
	}
}

// Caller must hold rw.mu. If rw.wroteHeader is true, calling this method is a
// no-op.
func (rw *responseWriter) writeHeaderLockedOnce() {
	if rw.wroteHeader {
		return
	}
	if !responseCanHaveBody(rw.statusCode) {
		rw.cannotHaveBody = true
	}
	// If there is any Trailer declared in headers, save them so we know which
	// trailers have been pre-declared. Also, write back the extracted value,
	// which is canonicalized, to rw.Header for consistency.
	if _, ok := rw.headers["Trailer"]; ok {
		extractTrailerFromHeader(rw.headers, rw.trailer)
		rw.headers.Set("Trailer", strings.Join(slices.Sorted(maps.Keys(rw.trailer)), ", "))
	}

	rw.bb.inferHeader(rw.headers, rw.statusCode)
	encHeaders := rw.bw.enc.encode(func(f func(itype indexType, name, value string)) {
		f(mayIndex, ":status", strconv.Itoa(rw.statusCode))
		for name, values := range rw.headers {
			if !httpguts.ValidHeaderFieldName(name) {
				continue
			}
			for _, val := range values {
				if !httpguts.ValidHeaderFieldValue(val) {
					continue
				}
				// Issue #71374: Consider supporting never-indexed fields.
				f(mayIndex, name, val)
			}
		}
	})

	rw.st.writeVarint(int64(frameTypeHeaders))
	rw.st.writeVarint(int64(len(encHeaders)))
	rw.st.Write(encHeaders)
	if rw.statusCode >= http.StatusOK {
		rw.wroteHeader = true
	}
}

func (rw *responseWriter) WriteHeader(statusCode int) {
	// TODO: handle sending informational status headers (e.g. 103).
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.statusCodeSet {
		return
	}
	rw.statusCodeSet = true
	rw.statusCode = statusCode

	if n, err := strconv.Atoi(rw.Header().Get("Content-Length")); err == nil {
		rw.bodyLenLeft = n
	} else {
		rw.bodyLenLeft = -1 // Unknown.
	}
}

// trimWriteLocked trims a byte slice, b, such that the length of b will not
// exceed rw.bodyLenLeft. This method will update rw.bodyLenLeft when trimming
// b, and will also return whether b was trimmed or not.
// Caller must hold rw.mu.
func (rw *responseWriter) trimWriteLocked(b []byte) ([]byte, bool) {
	if rw.bodyLenLeft < 0 {
		return b, false
	}
	n := min(len(b), rw.bodyLenLeft)
	rw.bodyLenLeft -= n
	return b[:n], n != len(b)
}

func (rw *responseWriter) Write(b []byte) (n int, err error) {
	// Calling Write implicitly calls WriteHeader(200) if WriteHeader has not
	// been called before.
	rw.WriteHeader(http.StatusOK)
	rw.mu.Lock()
	defer rw.mu.Unlock()

	b, trimmed := rw.trimWriteLocked(b)
	if trimmed {
		defer func() {
			err = http.ErrContentLength
		}()
	}

	// If b fits entirely in our body buffer, save it to the buffer and return
	// early so we can coalesce small writes.
	// As a special case, we always want to save b to the buffer even when b is
	// big if we had yet to write our header, so we can infer headers like
	// "Content-Type" with as much information as possible.
	initialBLen := len(b)
	initialBufLen := len(rw.bb)
	if !rw.wroteHeader || len(b) <= cap(rw.bb)-len(rw.bb) {
		b = rw.bb.write(b)
		if len(b) == 0 {
			return initialBLen, nil
		}
	}

	// Reaching this point means that our buffer has been sufficiently filled.
	// Therefore, we now want to:
	// 1. Infer and write response headers based on our body buffer, if not
	// done yet.
	// 2. Write our body buffer and the rest of b (if any).
	// 3. Reset the current body buffer so it can be used again.
	rw.writeHeaderLockedOnce()
	if rw.cannotHaveBody {
		return initialBLen, nil
	}
	if n, err := rw.bw.write(rw.bb, b); err != nil {
		return max(0, n-initialBufLen), err
	}
	rw.bb.discard()
	return initialBLen, nil
}

func (rw *responseWriter) Flush() {
	// Calling Flush implicitly calls WriteHeader(200) if WriteHeader has not
	// been called before.
	rw.WriteHeader(http.StatusOK)
	rw.mu.Lock()
	rw.writeHeaderLockedOnce()
	if !rw.cannotHaveBody {
		rw.bw.Write(rw.bb)
		rw.bb.discard()
	}
	rw.mu.Unlock()
	rw.st.Flush()
}

func (rw *responseWriter) close() error {
	rw.Flush()
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.prepareTrailerForWriteLocked()
	if err := rw.bw.Close(); err != nil {
		return err
	}
	return rw.st.stream.Close()
}

// defaultBodyBufferCap is the default number of bytes of body that we are
// willing to save in a buffer for the sake of inferring headers and coalescing
// small writes. 512 was chosen to be consistent with how much
// http.DetectContentType is willing to read.
const defaultBodyBufferCap = 512

// bodyBuffer is a buffer used to store body content of a response.
type bodyBuffer []byte

// write writes b to the buffer. It returns a new slice of b, which contains
// any remaining data that could not be written to the buffer, if any.
func (bb *bodyBuffer) write(b []byte) []byte {
	n := min(len(b), cap(*bb)-len(*bb))
	*bb = append(*bb, b[:n]...)
	return b[n:]
}

// discard resets the buffer so it can be used again.
func (bb *bodyBuffer) discard() {
	*bb = (*bb)[:0]
}

// inferHeader populates h with the header values that we can infer from our
// current buffer content, if not already explicitly set. This method should be
// called only once with as much body content as possible in the buffer, before
// a HEADERS frame is sent, and before discard has been called. Doing so
// properly is the responsibility of the caller.
func (bb *bodyBuffer) inferHeader(h http.Header, status int) {
	if _, ok := h["Date"]; !ok {
		h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}
	// If the Content-Encoding is non-blank, we shouldn't
	// sniff the body. See Issue golang.org/issue/31753.
	_, hasCE := h["Content-Encoding"]
	_, hasCT := h["Content-Type"]
	if !hasCE && !hasCT && responseCanHaveBody(status) && len(*bb) > 0 {
		h.Set("Content-Type", http.DetectContentType(*bb))
	}
	// We can technically infer Content-Length too here, as long as the entire
	// response body fits within hi.buf and does not require flushing. However,
	// we have chosen not to do so for now as Content-Length is not very
	// important for HTTP/3, and such inconsistent behavior might be confusing.
}
