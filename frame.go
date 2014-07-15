package http2

import (
	"io"
	"sync"
)

const frameHeaderLen = 8

type FrameType uint8

// Defined in http://http2.github.io/http2-spec/#rfc.section.11.2
const (
	FrameData         FrameType = 0
	FrameHeaders      FrameType = 1
	FramePriority     FrameType = 2
	FrameRSTStream    FrameType = 3
	FrameSettings     FrameType = 4
	FramePushPromise  FrameType = 5
	FramePing         FrameType = 6
	FrameGoAway       FrameType = 7
	FrameWindowUpdate FrameType = 8
	FrameContinuation FrameType = 9
)

// A FrameHeader is the 8 byte header of all HTTP/2 frames.
//
// See http://http2.github.io/http2-spec/#FrameHeader
type FrameHeader struct {
	Type   FrameType
	Flags  uint8
	Length uint16
}

// frame header bytes
var fhBytes = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, frameHeaderLen)
		return &buf
	},
}

func ReadFrameHeader(r io.Reader) (FrameHeader, error) {
	bufp := fhBytes.Get().(*[]byte)
	defer fhBytes.Put(bufp)
	buf := *bufp
	_, err := io.ReadFull(r, buf)
	if err != nil {
		return FrameHeader{}, err
	}
	return FrameHeader{
		Length: (uint16(buf[0])<<8 + uint16(buf[1])) & (1<<14 - 1),
		Flags:  buf[3],
		Type:   FrameType(buf[2]),
	}, nil
}

type Frame struct {
	FrameHeader
	data, unread []byte
}
