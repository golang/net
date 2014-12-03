// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package icmp

// A ParamProb represents an ICMP parameter problem message body.
type ParamProb struct {
	Pointer uintptr // offset within the data where the error was detected
	Data    []byte  // data
}

// Len implements the Len method of MessageBody interface.
func (p *ParamProb) Len() int {
	if p == nil {
		return 0
	}
	return 4 + len(p.Data)
}

// Marshal implements the Marshal method of MessageBody interface.
func (p *ParamProb) Marshal() ([]byte, error) {
	b := make([]byte, 4+len(p.Data))
	b[0], b[1], b[2], b[3] = byte(p.Pointer>>24), byte(p.Pointer>>16), byte(p.Pointer>>8), byte(p.Pointer)
	copy(b[4:], p.Data)
	return b, nil
}

// parseParamProb parses b as an ICMP parameter problem message body.
func parseParamProb(b []byte) (MessageBody, error) {
	bodyLen := len(b)
	if bodyLen < 4 {
		return nil, errMessageTooShort
	}
	p := &ParamProb{Pointer: uintptr(b[0])<<24 | uintptr(b[1])<<16 | uintptr(b[2])<<8 | uintptr(b[3])}
	if bodyLen > 4 {
		p.Data = make([]byte, bodyLen-4)
		copy(p.Data, b[4:])
	}
	return p, nil
}
