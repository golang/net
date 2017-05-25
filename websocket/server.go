// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
)

var ErrNoHijacker = fmt.Errorf("ResponseWriter does not implement http.Hijacker interface")

type HandshakeFn func(*Config, *http.Request) error

func upgrade(r *http.Request, w http.ResponseWriter, hs serverHandshaker, handshake HandshakeFn) (conn net.Conn, buf *bufio.ReadWriter, err error) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, ErrNoHijacker
	}
	conn, buf, err = hijacker.Hijack()
	if err != nil {
		return
	}

	code, err := hs.ReadHandshake(buf.Reader, r)
	if err == ErrBadWebSocketVersion {
		fmt.Fprintf(buf, "HTTP/1.1 %03d %s\r\n", code, http.StatusText(code))
		fmt.Fprintf(buf, "Sec-WebSocket-Version: %s\r\n", SupportedProtocolVersion)
		buf.WriteString("\r\n")
		buf.WriteString(err.Error())
		buf.Flush()
		return
	}
	if err != nil {
		fmt.Fprintf(buf, "HTTP/1.1 %03d %s\r\n", code, http.StatusText(code))
		buf.WriteString("\r\n")
		buf.WriteString(err.Error())
		buf.Flush()
		return
	}
	if handshake != nil {
		err = handshake(hs.HandshakeConfig(), r)
		if err != nil {
			code = http.StatusForbidden
			fmt.Fprintf(buf, "HTTP/1.1 %03d %s\r\n", code, http.StatusText(code))
			buf.WriteString("\r\n")
			buf.Flush()
			return
		}
	}
	err = hs.AcceptHandshake(buf.Writer)
	if err != nil {
		code = http.StatusBadRequest
		fmt.Fprintf(buf, "HTTP/1.1 %03d %s\r\n", code, http.StatusText(code))
		buf.WriteString("\r\n")
		buf.Flush()
		return
	}

	return
}

// Upgrade upgrades request to WebSocket protocol.
// It returns plain net.Conn and buffer, corresponding to that conn.
func Upgrade(r *http.Request, w http.ResponseWriter, c *Config, handshake HandshakeFn) (net.Conn, *bufio.ReadWriter, error) {
	hs := &hybiServerHandshaker{Config: c}
	return upgrade(r, w, hs, handshake)
}

// NewServerConn wraps upgraded rwc into Conn.
func NewServerConn(rwc io.ReadWriteCloser, buf *bufio.ReadWriter, r *http.Request, c *Config) *Conn {
	hs := &hybiServerHandshaker{Config: c}
	return hs.NewServerConn(buf, rwc, r)
}

// Server represents a server of a WebSocket.
type Server struct {
	// Config is a WebSocket configuration for new WebSocket connection.
	Config

	// Handshake is an optional function in WebSocket handshake.
	// For example, you can check, or don't check Origin header.
	// Another example, you can select config.Protocol.
	Handshake HandshakeFn

	// Handler handles a WebSocket connection.
	Handler
}

// ServeHTTP implements the http.Handler interface for a WebSocket
func (s Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.serveWebSocket(w, req)
}

func (s Server) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	hs := &hybiServerHandshaker{Config: &s.Config}
	rwc, buf, err := upgrade(r, w, hs, s.Handshake)
	if err != nil {
		return
	}
	// The server should abort the WebSocket connection if it finds
	// the client did not send a handshake that matches with protocol
	// specification.
	defer rwc.Close()

	conn := hs.NewServerConn(buf, rwc, r)
	if conn == nil {
		panic("unexpected nil conn")
	}
	s.Handler(conn)
}

// Handler is a simple interface to a WebSocket browser client.
// It checks if Origin header is valid URL by default.
// You might want to verify websocket.Conn.Config().Origin in the func.
// If you use Server instead of Handler, you could call websocket.Origin and
// check the origin in your Handshake func. So, if you want to accept
// non-browser clients, which do not send an Origin header, set a
// Server.Handshake that does not check the origin.
type Handler func(*Conn)

func checkOrigin(config *Config, req *http.Request) (err error) {
	config.Origin, err = Origin(config, req)
	if err == nil && config.Origin == nil {
		return fmt.Errorf("null origin")
	}
	return err
}

// ServeHTTP implements the http.Handler interface for a WebSocket
func (h Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s := Server{Handler: h, Handshake: checkOrigin}
	s.serveWebSocket(w, req)
}
