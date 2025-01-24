// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.24

package http3

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"golang.org/x/net/quic"
)

// A Transport is an HTTP/3 transport.
//
// It does not manage a pool of connections,
// and therefore does not implement net/http.RoundTripper.
//
// TODO: Provide a way to register an HTTP/3 transport with a net/http.Transport's
// connection pool.
type Transport struct {
	// Endpoint is the QUIC endpoint used by connections created by the transport.
	// If unset, it is initialized by the first call to Dial.
	Endpoint *quic.Endpoint

	// Config is the QUIC configuration used for client connections.
	// The Config may be nil.
	//
	// Dial may clone and modify the Config.
	// The Config must not be modified after calling Dial.
	Config *quic.Config

	initOnce sync.Once
	initErr  error
}

func (tr *Transport) init() error {
	tr.initOnce.Do(func() {
		if tr.Config == nil {
			tr.Config = &quic.Config{}
		}

		// maybeCloneTLSConfig clones the user-provided tls.Config (but only once)
		// prior to us modifying it.
		needCloneTLSConfig := true
		maybeCloneTLSConfig := func() *tls.Config {
			if needCloneTLSConfig {
				tr.Config.TLSConfig = tr.Config.TLSConfig.Clone()
				needCloneTLSConfig = false
			}
			return tr.Config.TLSConfig
		}

		if tr.Config.TLSConfig == nil {
			tr.Config.TLSConfig = &tls.Config{}
			needCloneTLSConfig = false
		}
		if tr.Config.TLSConfig.MinVersion == 0 {
			maybeCloneTLSConfig().MinVersion = tls.VersionTLS13
		}
		if tr.Config.TLSConfig.NextProtos == nil {
			maybeCloneTLSConfig().NextProtos = []string{"h3"}
		}
		if tr.Endpoint == nil {
			tr.Endpoint, tr.initErr = quic.Listen("udp", ":0", nil)
		}
	})
	return tr.initErr
}

// Dial creates a new HTTP/3 client connection.
func (tr *Transport) Dial(ctx context.Context, target string) (*ClientConn, error) {
	if err := tr.init(); err != nil {
		return nil, err
	}
	qconn, err := tr.Endpoint.Dial(ctx, "udp", target, tr.Config)
	if err != nil {
		return nil, err
	}
	return newClientConn(ctx, qconn)
}

// A ClientConn is a client HTTP/3 connection.
//
// Multiple goroutines may invoke methods on a ClientConn simultaneously.
type ClientConn struct {
	qconn *quic.Conn

	mu sync.Mutex

	// The peer may create exactly one control, encoder, and decoder stream.
	// streamsCreated is a bitset of streams created so far.
	// Bits are 1 << streamType.
	streamsCreated uint8
}

func newClientConn(ctx context.Context, qconn *quic.Conn) (*ClientConn, error) {
	cc := &ClientConn{
		qconn: qconn,
	}

	// Create control stream and send SETTINGS frame.
	controlStream, err := newConnStream(ctx, cc.qconn, streamTypeControl)
	if err != nil {
		return nil, fmt.Errorf("http3: cannot create control stream: %v", err)
	}
	controlStream.writeSettings()
	controlStream.Flush()

	go cc.acceptStreams()
	return cc, nil
}

// Close closes the connection.
// Any in-flight requests are canceled.
// Close does not wait for the peer to acknowledge the connection closing.
func (cc *ClientConn) Close() error {
	// Close the QUIC connection immediately with a status of NO_ERROR.
	cc.qconn.Abort(nil)

	// Return any existing error from the peer, but don't wait for it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return cc.qconn.Wait(ctx)
}

// RoundTrip sends a request on the connection.
func (cc *ClientConn) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, errors.New("TODO")
}

func (cc *ClientConn) acceptStreams() {
	for {
		// Use context.Background: This blocks until a stream is accepted
		// or the connection closes.
		st, err := cc.qconn.AcceptStream(context.Background())
		if err != nil {
			return // connection closed
		}
		if !st.IsReadOnly() {
			// "Clients MUST treat receipt of a server-initiated bidirectional
			// stream as a connection error of type H3_STREAM_CREATION_ERROR [...]"
			// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.1-3
			cc.abort(&connectionError{
				code:    errH3StreamCreationError,
				message: "server created bidirectional stream",
			})
			return
		}
		go cc.handleStream(newStream(st))
	}
}

func (cc *ClientConn) handleStream(st *stream) {
	// Unidirectional stream header: One varint with the stream type.
	stype, err := st.readVarint()
	if err != nil {
		cc.abort(&connectionError{
			code:    errH3StreamCreationError,
			message: "error reading unidirectional stream header",
		})
		return
	}
	switch streamType(stype) {
	case streamTypeControl:
		err = cc.handleControlStream(st)
	case streamTypePush:
		err = cc.handlePushStream(st)
	case streamTypeEncoder:
		err = cc.handleEncoderStream(st)
	case streamTypeDecoder:
		err = cc.handleDecoderStream(st)
	default:
		// "Recipients of unknown stream types MUST either abort reading
		// of the stream or discard incoming data without further processing."
		// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.2-7
		//
		// We should send the H3_STREAM_CREATION_ERROR error code,
		// but the quic package currently doesn't allow setting error codes
		// for STOP_SENDING frames.
		// TODO: Should CloseRead take an error code?
		st.stream.CloseRead()
		err = nil
	}
	if err == io.EOF {
		err = &connectionError{
			code:    errH3ClosedCriticalStream,
			message: streamType(stype).String() + " stream closed",
		}
	}
	if err != nil {
		cc.abort(err)
	}
}

func (cc *ClientConn) checkStreamCreation(stype streamType, name string) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	bit := uint8(1) << stype
	if cc.streamsCreated&bit != 0 {
		return &connectionError{
			code:    errH3StreamCreationError,
			message: "multiple " + name + " streams created",
		}
	}
	cc.streamsCreated |= bit
	return nil
}

func (cc *ClientConn) handleControlStream(st *stream) error {
	if err := cc.checkStreamCreation(streamTypeControl, "control"); err != nil {
		// "[...] receipt of a second stream claiming to be a control stream
		// MUST be treated as a connection error of type H3_STREAM_CREATION_ERROR."
		// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.2.1-2
		return err
	}

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

func (cc *ClientConn) handleEncoderStream(*stream) error {
	if err := cc.checkStreamCreation(streamTypeEncoder, "encoder"); err != nil {
		// "Receipt of a second instance of [an encoder stream] MUST
		// be treated as a connection error of type H3_STREAM_CREATION_ERROR."
		// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.2.1-2
		return err
	}
	// TODO
	return nil
}

func (cc *ClientConn) handleDecoderStream(*stream) error {
	if err := cc.checkStreamCreation(streamTypeDecoder, "decoder"); err != nil {
		// "Receipt of a second instance of [a decoder stream] MUST
		// be treated as a connection error of type H3_STREAM_CREATION_ERROR."
		// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.2.1-2
		return err
	}
	// TODO
	return nil
}

func (cc *ClientConn) handlePushStream(*stream) error {
	// "A client MUST treat receipt of a push stream as a connection error
	// of type H3_ID_ERROR when no MAX_PUSH_ID frame has been sent [...]"
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-4.6-3
	return &connectionError{
		code:    errH3IDError,
		message: "push stream created when no MAX_PUSH_ID has been sent",
	}
}

// abort closes the connection with an error.
func (cc *ClientConn) abort(err error) {
	if e, ok := err.(*connectionError); ok {
		cc.qconn.Abort(&quic.ApplicationError{
			Code:   uint64(e.code),
			Reason: e.message,
		})
	} else {
		cc.qconn.Abort(err)
	}
}
