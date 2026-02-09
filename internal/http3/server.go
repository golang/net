// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"

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
		st:         st,
		headers:    make(http.Header),
		isHeadResp: req.Method == "HEAD",
		bw: &bodyWriter{
			st:     st,
			remain: -1,
			flush:  false,
			name:   "response",
		},
	}
	defer rw.close()
	if reqInfo.NeedsContinue {
		req.Body.(*bodyReader).send100Continue = func() {
			rw.WriteHeader(http.StatusContinue)
			rw.Flush()
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

type responseWriter struct {
	st          *stream
	bw          *bodyWriter
	mu          sync.Mutex
	headers     http.Header
	wroteHeader bool // Non-1xx header has been (logically) written.
	isHeadResp  bool // response is for a HEAD request.
}

func (rw *responseWriter) Header() http.Header {
	return rw.headers
}

// Caller must hold rw.mu. If rw.wroteHeader is true, calling this method is a
// no-op.
func (rw *responseWriter) writeHeaderLockedOnce(statusCode int) {
	if rw.wroteHeader {
		return
	}
	enc := &qpackEncoder{}
	enc.init()
	encHeaders := enc.encode(func(f func(itype indexType, name, value string)) {
		f(mayIndex, ":status", strconv.Itoa(statusCode))
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
	if statusCode >= http.StatusOK {
		rw.wroteHeader = true
	}
}

func (rw *responseWriter) WriteHeader(statusCode int) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.writeHeaderLockedOnce(statusCode)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.writeHeaderLockedOnce(http.StatusOK)
	if rw.isHeadResp {
		return 0, nil
	}
	return rw.bw.Write(b)
}

func (rw *responseWriter) Flush() {
	rw.mu.Lock()
	rw.writeHeaderLockedOnce(http.StatusOK)
	rw.mu.Unlock()
	rw.bw.st.Flush()
}

func (rw *responseWriter) close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.writeHeaderLockedOnce(http.StatusOK)
	return rw.st.stream.Close()
}
