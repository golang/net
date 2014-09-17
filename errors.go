// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import "fmt"

// An ErrCode is an unsigned 32-bit error code as defined in the HTTP/2 spec.
type ErrCode uint32

const (
	ErrCodeNo                 ErrCode = 0x0
	ErrCodeProtocol           ErrCode = 0x1
	ErrCodeInternal           ErrCode = 0x2
	ErrCodeFlowControl        ErrCode = 0x3
	ErrCodeSettingsTimeout    ErrCode = 0x4
	ErrCodeStreamClosed       ErrCode = 0x5
	ErrCodeFrameSize          ErrCode = 0x6
	ErrCodeRefusedStream      ErrCode = 0x7
	ErrCodeCancel             ErrCode = 0x8
	ErrCodeCompression        ErrCode = 0x9
	ErrCodeConnect            ErrCode = 0xa
	ErrCodeEnhanceYourCalm    ErrCode = 0xb
	ErrCodeInadequateSecurity ErrCode = 0xc
)

var errCodeName = map[ErrCode]string{
	ErrCodeNo:                 "NO_ERROR",
	ErrCodeProtocol:           "PROTOCOL_ERROR",
	ErrCodeInternal:           "INTERNAL_ERROR",
	ErrCodeFlowControl:        "FLOW_CONTROL_ERROR",
	ErrCodeSettingsTimeout:    "SETTINGS_TIMEOUT",
	ErrCodeStreamClosed:       "STREAM_CLOSED",
	ErrCodeFrameSize:          "FRAME_SIZE_ERROR",
	ErrCodeRefusedStream:      "REFUSED_STREAM",
	ErrCodeCancel:             "CANCEL",
	ErrCodeCompression:        "COMPRESSION_ERROR",
	ErrCodeConnect:            "CONNECT_ERROR",
	ErrCodeEnhanceYourCalm:    "ENHANCE_YOUR_CALM",
	ErrCodeInadequateSecurity: "INADEQUATE_SECURITY",
}

func (e ErrCode) String() string {
	if s, ok := errCodeName[e]; ok {
		return s
	}
	return fmt.Sprintf("unknown error code 0x%x", e)
}

type Error interface {
	IsStreamError() bool
	IsConnectionError() bool
	error
}

// ConnectionError is an error that results in the termination of the
// entire connection.
type ConnectionError ErrCode

var _ Error = ConnectionError(0)

func (e ConnectionError) IsStreamError() bool     { return false }
func (e ConnectionError) IsConnectionError() bool { return true }
func (e ConnectionError) Error() string           { return fmt.Sprintf("connection error: %s", ErrCode(e)) }

// StreamError is an error that only affects one stream within an
// HTTP/2 connection.
type StreamError uint32

var _ Error = StreamError(0)

func (e StreamError) IsStreamError() bool     { return true }
func (e StreamError) IsConnectionError() bool { return false }
func (e StreamError) Error() string           { return fmt.Sprintf("stream error: stream ID = %d", uint32(e)) }
