package http2

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"sync"
)

const frameHeaderLen = 8

type FrameType uint8

// Defined in http://http2.github.io/http2-spec/#rfc.section.11.2
const (
	FrameData         FrameType = 0x0
	FrameHeaders      FrameType = 0x1
	FramePriority     FrameType = 0x2
	FrameRSTStream    FrameType = 0x3
	FrameSettings     FrameType = 0x4
	FramePushPromise  FrameType = 0x5
	FramePing         FrameType = 0x6
	FrameGoAway       FrameType = 0x7
	FrameWindowUpdate FrameType = 0x8
	FrameContinuation FrameType = 0x9
)

var frameName = map[FrameType]string{
	FrameData:         "DATA",
	FrameHeaders:      "HEADERS",
	FramePriority:     "PRIORITY",
	FrameRSTStream:    "RST_STREAM",
	FrameSettings:     "SETTINGS",
	FramePushPromise:  "PUSH_PROMISE",
	FramePing:         "PING",
	FrameGoAway:       "GOAWAY",
	FrameWindowUpdate: "WINDOW_UPDATE",
	FrameContinuation: "CONTINUATION",
}

func (t FrameType) String() string {
	if s, ok := frameName[t]; ok {
		return s
	}
	return fmt.Sprintf("UNKNOWN_FRAME_TYPE_%d", uint8(t))
}

// Frame-specific FrameHeader flag bits.
const (
	// Settings Frame
	FlagSettingsAck Flags = 0x1

	// Headers Frame
	FlagHeadersEndStream  Flags = 0x1
	FlagHeadersEndSegment Flags = 0x2
	FlagHeadersEndHeaders Flags = 0x4
	FlagHeadersPadded     Flags = 0x8
	FlagHeadersPriority   Flags = 0x20
)

type SettingID uint16

const (
	SettingHeaderTableSize      SettingID = 0x1
	SettingEnablePush           SettingID = 0x2
	SettingMaxConcurrentStreams SettingID = 0x3
	SettingInitialWindowSize    SettingID = 0x4
)

func knownSetting(id SettingID) bool {
	// TODO: permit registration of custom settings values?
	// Per server type?
	return id >= 1 && id <= 4
}

// a frameParser parses a frame. The parser can assume that the Reader will
// not read past the length of a frame (e.g. it acts like an io.LimitReader
// bounded by the FrameHeader.Length)
type frameParser func(FrameHeader, io.Reader) (Frame, error)

var FrameParsers = map[FrameType]frameParser{
	FrameSettings:     parseSettingsFrame,
	FrameWindowUpdate: parseWindowUpdateFrame,
	FrameHeaders:      parseHeadersFrame,
}

func typeFrameParser(t FrameType) frameParser {
	if f, ok := FrameParsers[t]; ok {
		return f
	}
	return parseUnknownFrame
}

type Flags uint8

func (f Flags) Has(v Flags) bool {
	return (f & v) == v
}

// A FrameHeader is the 8 byte header of all HTTP/2 frames.
//
// See http://http2.github.io/http2-spec/#FrameHeader
type FrameHeader struct {
	Type     FrameType
	Flags    Flags
	Length   uint16
	StreamID uint32
}

func (h FrameHeader) Header() FrameHeader { return h }

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
		Length:   (uint16(buf[0])<<8 + uint16(buf[1])) & (1<<14 - 1),
		Flags:    Flags(buf[3]),
		Type:     FrameType(buf[2]),
		StreamID: binary.BigEndian.Uint32(buf[4:]) & (1<<31 - 1),
	}, nil
}

type Frame interface {
	Header() FrameHeader
}

type SettingsFrame struct {
	FrameHeader
	Settings map[SettingID]uint32
}

func parseSettingsFrame(fh FrameHeader, r io.Reader) (Frame, error) {
	if fh.Flags.Has(FlagSettingsAck) && fh.Length > 0 {
		// When this (ACK 0x1) bit is set, the payload of the
		// SETTINGS frame MUST be empty.  Receipt of a
		// SETTINGS frame with the ACK flag set and a length
		// field value other than 0 MUST be treated as a
		// connection error (Section 5.4.1) of type
		// FRAME_SIZE_ERROR.
		return nil, ConnectionError(ErrCodeFrameSize)
	}
	if fh.StreamID != 0 {
		// SETTINGS frames always apply to a connection,
		// never a single stream.  The stream identifier for a
		// SETTINGS frame MUST be zero (0x0).  If an endpoint
		// receives a SETTINGS frame whose stream identifier
		// field is anything other than 0x0, the endpoint MUST
		// respond with a connection error (Section 5.4.1) of
		// type PROTOCOL_ERROR.
		log.Printf("Bogus StreamID in settings: %+v", fh)
		return nil, ConnectionError(ErrCodeProtocol)
	}
	if fh.Length%6 != 0 {
		// Expecting even number of 6 byte settings.
		return nil, ConnectionError(ErrCodeFrameSize)
	}
	s := make(map[SettingID]uint32)
	nSettings := int(fh.Length / 6)
	var buf [4]byte
	for i := 0; i < nSettings; i++ {
		if _, err := io.ReadFull(r, buf[:2]); err != nil {
			return nil, err
		}
		settingID := SettingID(binary.BigEndian.Uint16(buf[:2]))
		if _, err := io.ReadFull(r, buf[:4]); err != nil {
			return nil, err
		}
		value := binary.BigEndian.Uint32(buf[:4])
		if settingID == SettingInitialWindowSize && value > (1<<31)-1 {
			// Values above the maximum flow control window size of 2^31 - 1 MUST
			// be treated as a connection error (Section 5.4.1) of type
			// FLOW_CONTROL_ERROR.
			return nil, ConnectionError(ErrCodeFlowControl)
		}
		if knownSetting(settingID) {
			s[settingID] = value
		}
	}

	return &SettingsFrame{
		FrameHeader: fh,
		Settings:    s,
	}, nil
}

type UnknownFrame struct {
	FrameHeader
}

func parseUnknownFrame(fh FrameHeader, r io.Reader) (Frame, error) {
	_, err := io.CopyN(ioutil.Discard, r, int64(fh.Length))
	return UnknownFrame{fh}, err
}

type WindowUpdateFrame struct {
	FrameHeader
	Increment uint32
}

func parseWindowUpdateFrame(fh FrameHeader, r io.Reader) (Frame, error) {
	if fh.Length < 4 {
		// Too short.
		return nil, ConnectionError(ErrCodeProtocol)
	}
	f := WindowUpdateFrame{
		FrameHeader: fh,
	}
	var err error
	f.Increment, err = readUint32(r)
	if err != nil {
		return nil, err
	}
	f.Increment &= 0x7fffffff // mask off high reserved bit

	// Future-proof: ignore any extra length in the frame. The spec doesn't
	// say what to do if Length is too large.
	if fh.Length > 4 {
		if _, err := io.CopyN(ioutil.Discard, r, int64(fh.Length-4)); err != nil {
			return nil, err
		}
	}
	return f, nil
}

type HeaderFrame struct {
	FrameHeader

	// If FlagHeadersPriority:
	ExclusiveDep bool
	StreamDep    uint32

	// Weight is [0,255]. Only valid if FrameHeader.Flags has the
	// FlagHeadersPriority bit set, in which case the caller must
	// also add 1 to get to spec-defined [1,256] range.
	Weight uint8

	HeaderFragBuf []byte
}

func parseHeadersFrame(fh FrameHeader, r io.Reader) (_ Frame, err error) {
	hf := HeaderFrame{
		FrameHeader: fh,
	}
	if fh.StreamID == 0 {
		// HEADERS frames MUST be associated with a stream.  If a HEADERS frame
		// is received whose stream identifier field is 0x0, the recipient MUST
		// respond with a connection error (Section 5.4.1) of type
		// PROTOCOL_ERROR.
		return nil, ConnectionError(ErrCodeProtocol)
	}
	var padLength uint8
	var notHeaders int // Header Block Fragment length = fh.Length - notHeaders
	if fh.Flags.Has(FlagHeadersPadded) {
		notHeaders += 1
		if padLength, err = readByte(r); err != nil {
			return
		}
	}
	if fh.Flags.Has(FlagHeadersPriority) {
		notHeaders += 4
		v, err := readUint32(r)
		if err != nil {
			return nil, err
		}
		hf.StreamDep = v & 0x7fffffff
		hf.ExclusiveDep = (v != hf.StreamDep) // high bit was set
	}
	if fh.Flags.Has(FlagHeadersPriority) {
		notHeaders += 1
		hf.Weight, err = readByte(r)
		if err != nil {
			return
		}
	}
	headerFragLen := int(fh.Length) - notHeaders
	if headerFragLen <= 0 {
		return nil, StreamError(fh.StreamID)
	}
	buf := make([]byte, headerFragLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	if _, err := io.CopyN(ioutil.Discard, r, int64(padLength)); err != nil {
		return nil, err
	}
	hf.HeaderFragBuf = buf
	return hf, nil
}

func readByte(r io.Reader) (uint8, error) {
	// TODO: optimize, reuse buffers
	var buf [1]byte
	_, err := io.ReadFull(r, buf[:1])
	return buf[0], err
}

func readUint32(r io.Reader) (uint32, error) {
	// TODO: optimize, reuse buffers
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:4]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:4]), nil
}
