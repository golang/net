// Package http2 implements the HTTP/2 protocol.
//
// It currently targets draft-13. See http://http2.github.io/
package http2

import (
	"crypto/tls"
	"log"
	"net/http"
	"sync"
)

const npnProto = "h2-13"

// Server is an HTTP2 server.
type Server struct {
	// MaxStreams optionally ...
	MaxStreams int

	mu sync.Mutex
}

func (srv *Server) handleConn(hs *http.Server, c *tls.Conn, h http.Handler) {
	defer c.Close()
	log.Printf("HTTP/2 connection from %v on %p", c.RemoteAddr(), hs)
}

// ConfigureServer adds HTTP2 support to s as configured by the HTTP/2
// server configuration in conf. The configuration may be nil.
//
// ConfigureServer must be called before s beings serving.
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
		conf.handleConn(hs, c, h)
	}
}
