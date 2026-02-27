// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"golang.org/x/net/quic"
)

// A transport is an HTTP/3 transport.
//
// It does not manage a pool of connections,
// and therefore does not implement net/http.RoundTripper.
//
// TODO: Provide a way to register an HTTP/3 transport with a net/http.transport's
// connection pool.
type transport struct {
	// endpoint is the QUIC endpoint used by connections created by the transport.
	// If unset, it is initialized by the first call to Dial.
	endpoint *quic.Endpoint

	// config is the QUIC configuration used for client connections.
	// The config may be nil.
	//
	// Dial may clone and modify the config.
	// The config must not be modified after calling Dial.
	config *quic.Config

	initOnce sync.Once
	initErr  error
}

// netHTTPTransport implements the net/http.dialClientConner interface,
// allowing our HTTP/3 transport to integrate with net/http.
type netHTTPTransport struct {
	*transport
}

// RoundTrip is defined since Transport.RegisterProtocol takes in a
// RoundTripper. However, this method will never be used as net/http's
// dialClientConner interface does not have a RoundTrip method and will only
// use DialClientConn to create a new RoundTripper.
func (t netHTTPTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("netHTTPTransport.RoundTrip should never be called")
}

func (t netHTTPTransport) DialClientConn(ctx context.Context, addr string, _ *url.URL, _ func()) (http.RoundTripper, error) {
	return t.transport.dial(ctx, addr)
}

// RegisterTransport configures a net/http HTTP/1 Transport to use HTTP/3.
//
// TODO: most likely, add another arg for transport configuration.
func RegisterTransport(tr *http.Transport) {
	tr3 := &transport{
		config: &quic.Config{
			TLSConfig: tr.TLSClientConfig.Clone(),
		},
	}
	tr.RegisterProtocol("http/3", netHTTPTransport{tr3})
}

func (tr *transport) init() error {
	tr.initOnce.Do(func() {
		tr.config = initConfig(tr.config)
		if tr.endpoint == nil {
			tr.endpoint, tr.initErr = quic.Listen("udp", ":0", nil)
		}
	})
	return tr.initErr
}

// dial creates a new HTTP/3 client connection.
func (tr *transport) dial(ctx context.Context, target string) (*clientConn, error) {
	if err := tr.init(); err != nil {
		return nil, err
	}
	qconn, err := tr.endpoint.Dial(ctx, "udp", target, tr.config)
	if err != nil {
		return nil, err
	}
	return newClientConn(ctx, qconn)
}

// A clientConn is a client HTTP/3 connection.
//
// Multiple goroutines may invoke methods on a clientConn simultaneously.
type clientConn struct {
	qconn *quic.Conn
	genericConn

	enc qpackEncoder
	dec qpackDecoder
}

func newClientConn(ctx context.Context, qconn *quic.Conn) (*clientConn, error) {
	cc := &clientConn{
		qconn: qconn,
	}
	cc.enc.init()

	// Create control stream and send SETTINGS frame.
	controlStream, err := newConnStream(ctx, cc.qconn, streamTypeControl)
	if err != nil {
		return nil, fmt.Errorf("http3: cannot create control stream: %v", err)
	}
	controlStream.writeSettings()
	controlStream.Flush()

	go cc.acceptStreams(qconn, cc)
	return cc, nil
}

// close closes the connection.
// Any in-flight requests are canceled.
// close does not wait for the peer to acknowledge the connection closing.
func (cc *clientConn) close() error {
	// Close the QUIC connection immediately with a status of NO_ERROR.
	cc.qconn.Abort(nil)

	// Return any existing error from the peer, but don't wait for it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return cc.qconn.Wait(ctx)
}

func (cc *clientConn) handleControlStream(st *stream) error {
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
			// "If a CANCEL_PUSH frame is received that references a push ID
			// greater than currently allowed on the connection,
			// this MUST be treated as a connection error of type H3_ID_ERROR."
			// https://www.rfc-editor.org/rfc/rfc9114.html#section-7.2.3-7
			return &connectionError{
				code:    errH3IDError,
				message: "CANCEL_PUSH received when no MAX_PUSH_ID has been sent",
			}
		case frameTypeGoaway:
			// TODO: Wait for requests to complete before closing connection.
			return errH3NoError
		default:
			// Unknown frames are ignored.
			if err := st.discardUnknownFrame(ftype); err != nil {
				return err
			}
		}
	}
}

func (cc *clientConn) handleEncoderStream(*stream) error {
	// TODO
	return nil
}

func (cc *clientConn) handleDecoderStream(*stream) error {
	// TODO
	return nil
}

func (cc *clientConn) handlePushStream(*stream) error {
	// "A client MUST treat receipt of a push stream as a connection error
	// of type H3_ID_ERROR when no MAX_PUSH_ID frame has been sent [...]"
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-4.6-3
	return &connectionError{
		code:    errH3IDError,
		message: "push stream created when no MAX_PUSH_ID has been sent",
	}
}

func (cc *clientConn) handleRequestStream(st *stream) error {
	// "Clients MUST treat receipt of a server-initiated bidirectional
	// stream as a connection error of type H3_STREAM_CREATION_ERROR [...]"
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.1-3
	return &connectionError{
		code:    errH3StreamCreationError,
		message: "server created bidirectional stream",
	}
}

// abort closes the connection with an error.
func (cc *clientConn) abort(err error) {
	if e, ok := err.(*connectionError); ok {
		cc.qconn.Abort(&quic.ApplicationError{
			Code:   uint64(e.code),
			Reason: e.message,
		})
	} else {
		cc.qconn.Abort(err)
	}
}
