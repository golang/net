// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"bytes"
	"time"

	"github.com/bradfitz/http2/hpack"
)

// writeContext is the interface needed by the various frame writing
// functions below. All the functions below are scheduled via the
// frame writing scheduler (see writeScheduler in writesched.go).
//
// This interface is implemented by *serverConn.
// TODO: use it from the client code, once it exists.
type writeContext interface {
	Framer() *Framer
	Flush() error
	CloseConn() error
	// HeaderEncoder returns an HPACK encoder that writes to the
	// returned buffer.
	HeaderEncoder() (*hpack.Encoder, *bytes.Buffer)
}

func flushFrameWriter(ctx writeContext, _ interface{}) error {
	return ctx.Flush()
}

func writeSettings(ctx writeContext, v interface{}) error {
	settings := v.([]Setting)
	return ctx.Framer().WriteSettings(settings...)
}

type goAwayParams struct {
	maxStreamID uint32
	code        ErrCode
}

func writeGoAwayFrame(ctx writeContext, v interface{}) error {
	p := v.(*goAwayParams)
	err := ctx.Framer().WriteGoAway(p.maxStreamID, p.code, nil)
	if p.code != 0 {
		ctx.Flush() // ignore error: we're hanging up on them anyway
		time.Sleep(50 * time.Millisecond)
		ctx.CloseConn()
	}
	return err
}

type dataWriteParams struct {
	streamID uint32
	p        []byte
	end      bool
}

func writeRSTStreamFrame(ctx writeContext, v interface{}) error {
	se := v.(*StreamError)
	return ctx.Framer().WriteRSTStream(se.StreamID, se.Code)
}

func writePingAck(ctx writeContext, v interface{}) error {
	pf := v.(*PingFrame) // contains the data we need to write back
	return ctx.Framer().WritePing(true, pf.Data)
}

func writeSettingsAck(ctx writeContext, _ interface{}) error {
	return ctx.Framer().WriteSettingsAck()
}

func writeHeadersFrame(ctx writeContext, v interface{}) error {
	req := v.(headerWriteReq)
	enc, buf := ctx.HeaderEncoder()
	buf.Reset()
	enc.WriteField(hpack.HeaderField{Name: ":status", Value: httpCodeString(req.httpResCode)})
	for k, vv := range req.h {
		k = lowerHeader(k)
		for _, v := range vv {
			// TODO: more of "8.1.2.2 Connection-Specific Header Fields"
			if k == "transfer-encoding" && v != "trailers" {
				continue
			}
			enc.WriteField(hpack.HeaderField{Name: k, Value: v})
		}
	}
	if req.contentType != "" {
		enc.WriteField(hpack.HeaderField{Name: "content-type", Value: req.contentType})
	}
	if req.contentLength != "" {
		enc.WriteField(hpack.HeaderField{Name: "content-length", Value: req.contentLength})
	}

	headerBlock := buf.Bytes()
	if len(headerBlock) == 0 {
		panic("unexpected empty hpack")
	}

	// For now we're lazy and just pick the minimum MAX_FRAME_SIZE
	// that all peers must support (16KB). Later we could care
	// more and send larger frames if the peer advertised it, but
	// there's little point. Most headers are small anyway (so we
	// generally won't have CONTINUATION frames), and extra frames
	// only waste 9 bytes anyway.
	const maxFrameSize = 16384

	first := true
	for len(headerBlock) > 0 {
		frag := headerBlock
		if len(frag) > maxFrameSize {
			frag = frag[:maxFrameSize]
		}
		headerBlock = headerBlock[len(frag):]
		endHeaders := len(headerBlock) == 0
		var err error
		if first {
			first = false
			err = ctx.Framer().WriteHeaders(HeadersFrameParam{
				StreamID:      req.stream.id,
				BlockFragment: frag,
				EndStream:     req.endStream,
				EndHeaders:    endHeaders,
			})
		} else {
			err = ctx.Framer().WriteContinuation(req.stream.id, endHeaders, frag)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func write100ContinueHeadersFrame(ctx writeContext, v interface{}) error {
	st := v.(*stream)
	enc, buf := ctx.HeaderEncoder()
	buf.Reset()
	enc.WriteField(hpack.HeaderField{Name: ":status", Value: "100"})
	return ctx.Framer().WriteHeaders(HeadersFrameParam{
		StreamID:      st.id,
		BlockFragment: buf.Bytes(),
		EndStream:     false,
		EndHeaders:    true,
	})
}

func writeDataFrame(ctx writeContext, v interface{}) error {
	req := v.(*dataWriteParams)
	return ctx.Framer().WriteData(req.streamID, req.end, req.p)
}

type windowUpdateReq struct {
	streamID uint32
	n        uint32
}

func writeWindowUpdate(ctx writeContext, v interface{}) error {
	wu := v.(windowUpdateReq)
	fr := ctx.Framer()
	if err := fr.WriteWindowUpdate(0, wu.n); err != nil {
		return err
	}
	if err := fr.WriteWindowUpdate(wu.streamID, wu.n); err != nil {
		return err
	}
	return nil
}
