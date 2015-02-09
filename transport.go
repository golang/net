// Copyright 2015 The Go Authors.
// See https://go.googlesource.com/go/+/master/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://go.googlesource.com/go/+/master/LICENSE

package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/bradfitz/http2/hpack"
)

type Transport struct {
	Fallback http.RoundTripper

	// TODO: remove this and make more general with a TLS dial hook, like http
	InsecureTLSDial bool
}

type clientConn struct {
	tconn *tls.Conn
	bw    *bufio.Writer
	br    *bufio.Reader
	fr    *Framer

	readerDone chan struct{} // closed on error
	readerErr  error         // set before readerDone is closed

	werr error // first write error that has occurred

	hbuf bytes.Buffer // HPACK encoder writes into this
	henc *hpack.Encoder

	hdec *hpack.Decoder

	nextRes *http.Response

	// Settings from peer:
	maxFrameSize uint32

	mu           sync.Mutex
	streams      map[uint32]*clientStream
	nextStreamID uint32
}

type clientStream struct {
	ID   uint32
	resc chan *http.Response
	pw   *io.PipeWriter
	pr   *io.PipeReader
}

type stickyErrWriter struct {
	w   io.Writer
	err *error
}

func (sew stickyErrWriter) Write(p []byte) (n int, err error) {
	if *sew.err != nil {
		return 0, *sew.err
	}
	n, err = sew.w.Write(p)
	*sew.err = err
	return
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		if t.Fallback == nil {
			return nil, errors.New("http2: unsupported scheme and no Fallback")
		}
		return t.Fallback.RoundTrip(req)
	}

	host, port, err := net.SplitHostPort(req.URL.Host)
	if err != nil {
		host = req.URL.Host
		port = "443"
	}
	cfg := &tls.Config{
		ServerName:         host,
		NextProtos:         []string{NextProtoTLS},
		InsecureSkipVerify: t.InsecureTLSDial,
	}
	tconn, err := tls.Dial("tcp", host+":"+port, cfg)
	if err != nil {
		return nil, err
	}
	if err := tconn.Handshake(); err != nil {
		return nil, err
	}
	if !t.InsecureTLSDial {
		if err := tconn.VerifyHostname(cfg.ServerName); err != nil {
			return nil, err
		}
	}
	state := tconn.ConnectionState()
	if p := state.NegotiatedProtocol; p != NextProtoTLS {
		// TODO(bradfitz): fall back to Fallback
		return nil, fmt.Errorf("bad protocol: %v", p)
	}
	if !state.NegotiatedProtocolIsMutual {
		return nil, errors.New("could not negotiate protocol mutually")
	}
	if _, err := tconn.Write(clientPreface); err != nil {
		return nil, err
	}

	cc := &clientConn{
		tconn:        tconn,
		readerDone:   make(chan struct{}),
		nextStreamID: 1,
		streams:      make(map[uint32]*clientStream),
	}
	cc.bw = bufio.NewWriter(stickyErrWriter{tconn, &cc.werr})
	cc.br = bufio.NewReader(tconn)
	cc.fr = NewFramer(cc.bw, cc.br)
	cc.henc = hpack.NewEncoder(&cc.hbuf)

	cc.fr.WriteSettings()
	// TODO: re-send more conn-level flow control tokens when server uses all these.
	cc.fr.WriteWindowUpdate(0, 1<<30) // um, 0x7fffffff doesn't work to Google? it hangs?
	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}

	// Read the obligatory SETTINGS frame
	f, err := cc.fr.ReadFrame()
	if err != nil {
		return nil, err
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		return nil, fmt.Errorf("expected settings frame, got: %T", f)
	}
	cc.fr.WriteSettingsAck()
	cc.bw.Flush()

	sf.ForeachSetting(func(s Setting) error {
		switch s.ID {
		case SettingMaxFrameSize:
			cc.maxFrameSize = s.Val
		// TODO(bradfitz): handle the others
		default:
			log.Printf("Unhandled Setting: %v", s)
		}
		return nil
	})
	// TODO: figure out henc size
	cc.hdec = hpack.NewDecoder(initialHeaderTableSize, cc.onNewHeaderField)

	go cc.readLoop()

	cs := cc.newStream()
	hasBody := false // TODO

	// we send: HEADERS[+CONTINUATION] + (DATA?)
	hdrs := cc.encodeHeaders(req)
	first := true
	for len(hdrs) > 0 {
		chunk := hdrs
		if len(chunk) > int(cc.maxFrameSize) {
			chunk = chunk[:cc.maxFrameSize]
		}
		hdrs = hdrs[len(chunk):]
		endHeaders := len(hdrs) == 0
		if first {
			cc.fr.WriteHeaders(HeadersFrameParam{
				StreamID:      cs.ID,
				BlockFragment: chunk,
				EndStream:     !hasBody,
				EndHeaders:    endHeaders,
			})
			first = false
		} else {
			cc.fr.WriteContinuation(cs.ID, endHeaders, chunk)
		}
	}
	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}

	return <-cs.resc, nil
}

func (cc *clientConn) encodeHeaders(req *http.Request) []byte {
	cc.hbuf.Reset()

	// TODO(bradfitz): figure out :authority-vs-Host stuff between http2 and Go
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	path := req.URL.Path
	if path == "" {
		path = "/"
	}

	cc.writeHeader(":authority", host) // probably not right for all sites
	cc.writeHeader(":method", req.Method)
	cc.writeHeader(":path", path)
	cc.writeHeader(":scheme", "https")

	for k, vv := range req.Header {
		lowKey := strings.ToLower(k)
		if lowKey == "host" {
			continue
		}
		for _, v := range vv {
			cc.writeHeader(lowKey, v)
		}
	}
	return cc.hbuf.Bytes()
}

func (cc *clientConn) writeHeader(name, value string) {
	log.Printf("sending %q = %q", name, value)
	cc.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
}

func (cc *clientConn) newStream() *clientStream {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cs := &clientStream{
		ID:   cc.nextStreamID,
		resc: make(chan *http.Response, 1),
	}
	cc.nextStreamID += 2
	cc.streams[cs.ID] = cs

	return cs
}

func (cc *clientConn) streamByID(id uint32) *clientStream {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.streams[id]
}

// runs in its own goroutine.
func (cc *clientConn) readLoop() {
	defer close(cc.readerDone)

	for {
		f, err := cc.fr.ReadFrame()
		if err != nil {
			cc.readerErr = err
			// TODO: don't log it.
			log.Printf("ReadFrame: %v", err)
			return
		}
		cs := cc.streamByID(f.Header().StreamID)

		log.Printf("Transport received %v: %#v", f.Header(), f)
		headersEnded := false
		streamEnded := false
		if ff, ok := f.(interface {
			StreamEnded() bool
		}); ok {
			streamEnded = ff.StreamEnded()
		}
		switch f := f.(type) {
		case *HeadersFrame:
			cc.nextRes = &http.Response{
				Proto:      "HTTP/2.0",
				ProtoMajor: 2,
				Header:     make(http.Header),
				Request:    nil, // TODO: set this
				TLS:        nil, // TODO: set this
			}
			cs.pr, cs.pw = io.Pipe()
			cc.hdec.Write(f.HeaderBlockFragment())
			headersEnded = f.HeadersEnded()
		case *ContinuationFrame:
			// TODO: verify stream id is the same
			cc.hdec.Write(f.HeaderBlockFragment())
			headersEnded = f.HeadersEnded()
		case *DataFrame:
			log.Printf("DATA: %q", f.Data())
			cs.pw.Write(f.Data())
		default:
		}
		if streamEnded {
			cs.pw.Close()
		}
		if headersEnded {
			if cs == nil {
				panic("couldn't find stream") // TODO be graceful
			}
			cc.nextRes.Body = cs.pr
			cs.resc <- cc.nextRes
		}
	}
}

func (cc *clientConn) onNewHeaderField(f hpack.HeaderField) {
	// TODO: verifiy pseudo headers come before non-pseudo headers
	// TODO: verifiy the status is set
	log.Printf("Header field: %+v", f)
	if f.Name == ":status" {
		code, err := strconv.Atoi(f.Value)
		if err != nil {
			panic("TODO: be graceful")
		}
		cc.nextRes.Status = f.Value + " " + http.StatusText(code)
		cc.nextRes.StatusCode = code
		return
	}
	if strings.HasPrefix(f.Name, ":") {
		// "Endpoints MUST NOT generate pseudo-header fields other than those defined in this document."
		// TODO: treat as invalid?
		return
	}
	cc.nextRes.Header.Add(http.CanonicalHeaderKey(f.Name), f.Value)
}
