// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
)

func TestConnIDClientHandshake(t *testing.T) {
	// On initialization, the client chooses local and remote IDs.
	//
	// The order in which we allocate the two isn't actually important,
	// but test is a lot simpler if we assume.
	var s connIDState
	s.initClient(newConnIDSequence())
	if got, want := string(s.srcConnID()), "local-1"; got != want {
		t.Errorf("after initClient: srcConnID = %q, want %q", got, want)
	}
	dstConnID, _ := s.dstConnID()
	if got, want := string(dstConnID), "local-2"; got != want {
		t.Errorf("after initClient: dstConnID = %q, want %q", got, want)
	}

	// The server's first Initial packet provides the client with a
	// non-transient remote connection ID.
	s.handlePacket(clientSide, packetTypeInitial, []byte("remote-1"))
	dstConnID, _ = s.dstConnID()
	if got, want := string(dstConnID), "remote-1"; got != want {
		t.Errorf("after receiving Initial: dstConnID = %q, want %q", got, want)
	}

	wantLocal := []connID{{
		cid: []byte("local-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.local, wantLocal) {
		t.Errorf("local ids: %v, want %v", s.local, wantLocal)
	}
	wantRemote := []connID{{
		cid: []byte("remote-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.remote, wantRemote) {
		t.Errorf("remote ids: %v, want %v", s.remote, wantRemote)
	}
}

func TestConnIDServerHandshake(t *testing.T) {
	// On initialization, the server is provided with the client-chosen
	// transient connection ID, and allocates an ID of its own.
	// The Initial packet sets the remote connection ID.
	var s connIDState
	s.initServer(newConnIDSequence(), []byte("transient"))
	s.handlePacket(serverSide, packetTypeInitial, []byte("remote-1"))
	if got, want := string(s.srcConnID()), "local-1"; got != want {
		t.Errorf("after initClient: srcConnID = %q, want %q", got, want)
	}
	dstConnID, _ := s.dstConnID()
	if got, want := string(dstConnID), "remote-1"; got != want {
		t.Errorf("after initClient: dstConnID = %q, want %q", got, want)
	}

	wantLocal := []connID{{
		cid: []byte("transient"),
		seq: -1,
	}, {
		cid: []byte("local-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.local, wantLocal) {
		t.Errorf("local ids: %v, want %v", s.local, wantLocal)
	}
	wantRemote := []connID{{
		cid: []byte("remote-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.remote, wantRemote) {
		t.Errorf("remote ids: %v, want %v", s.remote, wantRemote)
	}

	// The client's first Handshake packet permits the server to discard the
	// transient connection ID.
	s.handlePacket(serverSide, packetTypeHandshake, []byte("remote-1"))
	wantLocal = []connID{{
		cid: []byte("local-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.local, wantLocal) {
		t.Errorf("after handshake local ids: %v, want %v", s.local, wantLocal)
	}
}

func newConnIDSequence() newConnIDFunc {
	var n uint64
	return func(_ int64) ([]byte, error) {
		n++
		return []byte(fmt.Sprintf("local-%v", n)), nil
	}
}

func TestNewRandomConnID(t *testing.T) {
	cid, err := newRandomConnID(0)
	if len(cid) != connIDLen || err != nil {
		t.Fatalf("newConnID() = %x, %v; want %v bytes", cid, connIDLen, err)
	}
}

func TestConnIDPeerRequestsManyIDs(t *testing.T) {
	// "An endpoint SHOULD ensure that its peer has a sufficient number
	// of available and unused connection IDs."
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.1-4
	//
	// "An endpoint MAY limit the total number of connection IDs
	// issued for each connection [...]"
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.1-6
	//
	// Peer requests 100 connection IDs.
	// We give them 4 in total.
	tc := newTestConn(t, serverSide, func(p *transportParameters) {
		p.activeConnIDLimit = 100
	})
	tc.ignoreFrame(frameTypeAck)
	tc.ignoreFrame(frameTypeCrypto)

	tc.writeFrames(packetTypeInitial,
		debugFrameCrypto{
			data: tc.cryptoDataIn[tls.QUICEncryptionLevelInitial],
		})
	tc.wantFrame("provide additional connection ID 1",
		packetType1RTT, debugFrameNewConnectionID{
			seq:    1,
			connID: testLocalConnID(1),
		})
	tc.wantFrame("provide additional connection ID 2",
		packetType1RTT, debugFrameNewConnectionID{
			seq:    2,
			connID: testLocalConnID(2),
		})
	tc.wantFrame("provide additional connection ID 3",
		packetType1RTT, debugFrameNewConnectionID{
			seq:    3,
			connID: testLocalConnID(3),
		})
	tc.wantIdle("connection ID limit reached, no more to provide")
}

func TestConnIDPeerProvidesTooManyIDs(t *testing.T) {
	// "An endpoint MUST NOT provide more connection IDs than the peer's limit."
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.1-4
	tc := newTestConn(t, serverSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:    2,
			connID: testLocalConnID(2),
		})
	tc.wantFrame("peer provided 3 connection IDs, our limit is 2",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errConnectionIDLimit,
		})
}

func TestConnIDPeerTemporarilyExceedsActiveConnIDLimit(t *testing.T) {
	// "An endpoint MAY send connection IDs that temporarily exceed a peer's limit
	// if the NEW_CONNECTION_ID frame also requires the retirement of any excess [...]"
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.1-4
	tc := newTestConn(t, serverSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			retirePriorTo: 2,
			seq:           2,
			connID:        testPeerConnID(2),
		}, debugFrameNewConnectionID{
			retirePriorTo: 2,
			seq:           3,
			connID:        testPeerConnID(3),
		})
	tc.wantFrame("peer requested we retire conn id 0",
		packetType1RTT, debugFrameRetireConnectionID{
			seq: 0,
		})
	tc.wantFrame("peer requested we retire conn id 1",
		packetType1RTT, debugFrameRetireConnectionID{
			seq: 1,
		})
}

func TestConnIDPeerRetiresConnID(t *testing.T) {
	// "An endpoint SHOULD supply a new connection ID when the peer retires a connection ID."
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.1-6
	for _, side := range []connSide{
		clientSide,
		serverSide,
	} {
		t.Run(side.String(), func(t *testing.T) {
			tc := newTestConn(t, side)
			tc.handshake()
			tc.ignoreFrame(frameTypeAck)

			tc.writeFrames(packetType1RTT,
				debugFrameRetireConnectionID{
					seq: 0,
				})
			tc.wantFrame("provide replacement connection ID",
				packetType1RTT, debugFrameNewConnectionID{
					seq:           2,
					retirePriorTo: 1,
					connID:        testLocalConnID(2),
				})
		})
	}
}

func TestConnIDPeerWithZeroLengthConnIDSendsNewConnectionID(t *testing.T) {
	// An endpoint that selects a zero-length connection ID during the handshake
	// cannot issue a new connection ID."
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.1-8
	tc := newTestConn(t, clientSide)
	tc.peerConnID = []byte{}
	tc.ignoreFrame(frameTypeAck)
	tc.uncheckedHandshake()

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:    1,
			connID: testPeerConnID(1),
		})
	tc.wantFrame("invalid NEW_CONNECTION_ID: previous conn id is zero-length",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errProtocolViolation,
		})
}

func TestConnIDPeerRequestsRetirement(t *testing.T) {
	// "Upon receipt of an increased Retire Prior To field, the peer MUST
	// stop using the corresponding connection IDs and retire them with
	// RETIRE_CONNECTION_ID frames [...]"
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.2-5
	tc := newTestConn(t, clientSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:           2,
			retirePriorTo: 1,
			connID:        testPeerConnID(2),
		})
	tc.wantFrame("peer asked for conn id 0 to be retired",
		packetType1RTT, debugFrameRetireConnectionID{
			seq: 0,
		})
	if got, want := tc.sentFramePacket.dstConnID, testPeerConnID(1); !bytes.Equal(got, want) {
		t.Fatalf("used destination conn id {%x}, want {%x}", got, want)
	}
}

func TestConnIDPeerDoesNotAcknowledgeRetirement(t *testing.T) {
	// "An endpoint SHOULD limit the number of connection IDs it has retired locally
	// for which RETIRE_CONNECTION_ID frames have not yet been acknowledged."
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.2-6
	tc := newTestConn(t, clientSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)
	tc.ignoreFrame(frameTypeRetireConnectionID)

	// Send a number of NEW_CONNECTION_ID frames, each retiring an old one.
	for seq := int64(0); seq < 7; seq++ {
		tc.writeFrames(packetType1RTT,
			debugFrameNewConnectionID{
				seq:           seq + 2,
				retirePriorTo: seq + 1,
				connID:        testPeerConnID(seq + 2),
			})
		// We're ignoring the RETIRE_CONNECTION_ID frames.
	}
	tc.wantFrame("number of retired, unacked conn ids is too large",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errConnectionIDLimit,
		})
}

func TestConnIDRepeatedNewConnectionIDFrame(t *testing.T) {
	// "Receipt of the same [NEW_CONNECTION_ID] frame multiple times
	// MUST NOT be treated as a connection error.
	// https://www.rfc-editor.org/rfc/rfc9000#section-19.15-7
	tc := newTestConn(t, clientSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	for i := 0; i < 4; i++ {
		tc.writeFrames(packetType1RTT,
			debugFrameNewConnectionID{
				seq:           2,
				retirePriorTo: 1,
				connID:        testPeerConnID(2),
			})
	}
	tc.wantFrame("peer asked for conn id to be retired",
		packetType1RTT, debugFrameRetireConnectionID{
			seq: 0,
		})
	tc.wantIdle("repeated NEW_CONNECTION_ID frames are not an error")
}

func TestConnIDForSequenceNumberChanges(t *testing.T) {
	// "[...] if a sequence number is used for different connection IDs,
	// the endpoint MAY treat that receipt as a connection error
	// of type PROTOCOL_VIOLATION."
	// https://www.rfc-editor.org/rfc/rfc9000#section-19.15-8
	tc := newTestConn(t, clientSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)
	tc.ignoreFrame(frameTypeRetireConnectionID)

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:           2,
			retirePriorTo: 1,
			connID:        testPeerConnID(2),
		})
	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:           2,
			retirePriorTo: 1,
			connID:        testPeerConnID(3),
		})
	tc.wantFrame("connection ID for sequence 0 has changed",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errProtocolViolation,
		})
}

func TestConnIDRetirePriorToAfterNewConnID(t *testing.T) {
	// "Receiving a value in the Retire Prior To field that is greater than
	// that in the Sequence Number field MUST be treated as a connection error
	// of type FRAME_ENCODING_ERROR.
	// https://www.rfc-editor.org/rfc/rfc9000#section-19.15-9
	tc := newTestConn(t, serverSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			retirePriorTo: 3,
			seq:           2,
			connID:        testPeerConnID(2),
		})
	tc.wantFrame("invalid NEW_CONNECTION_ID: retired the new conn id",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errFrameEncoding,
		})
}

func TestConnIDAlreadyRetired(t *testing.T) {
	// "An endpoint that receives a NEW_CONNECTION_ID frame with a
	// sequence number smaller than the Retire Prior To field of a
	// previously received NEW_CONNECTION_ID frame MUST send a
	// corresponding RETIRE_CONNECTION_ID frame [...]"
	// https://www.rfc-editor.org/rfc/rfc9000#section-19.15-11
	tc := newTestConn(t, clientSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:           4,
			retirePriorTo: 3,
			connID:        testPeerConnID(4),
		})
	tc.wantFrame("peer asked for conn id to be retired",
		packetType1RTT, debugFrameRetireConnectionID{
			seq: 0,
		})
	tc.wantFrame("peer asked for conn id to be retired",
		packetType1RTT, debugFrameRetireConnectionID{
			seq: 1,
		})
	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:           2,
			retirePriorTo: 0,
			connID:        testPeerConnID(2),
		})
	tc.wantFrame("NEW_CONNECTION_ID was for an already-retired ID",
		packetType1RTT, debugFrameRetireConnectionID{
			seq: 2,
		})
}

func TestConnIDRepeatedRetireConnectionIDFrame(t *testing.T) {
	tc := newTestConn(t, clientSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	for i := 0; i < 4; i++ {
		tc.writeFrames(packetType1RTT,
			debugFrameRetireConnectionID{
				seq: 0,
			})
	}
	tc.wantFrame("issue new conn id after peer retires one",
		packetType1RTT, debugFrameNewConnectionID{
			retirePriorTo: 1,
			seq:           2,
			connID:        testLocalConnID(2),
		})
	tc.wantIdle("repeated RETIRE_CONNECTION_ID frames are not an error")
}

func TestConnIDRetiredUnsent(t *testing.T) {
	// "Receipt of a RETIRE_CONNECTION_ID frame containing a sequence number
	// greater than any previously sent to the peer MUST be treated as a
	// connection error of type PROTOCOL_VIOLATION."
	// https://www.rfc-editor.org/rfc/rfc9000#section-19.16-7
	tc := newTestConn(t, clientSide)
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	tc.writeFrames(packetType1RTT,
		debugFrameRetireConnectionID{
			seq: 2,
		})
	tc.wantFrame("invalid NEW_CONNECTION_ID: previous conn id is zero-length",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errProtocolViolation,
		})
}

func TestConnIDUsePreferredAddressConnID(t *testing.T) {
	// Peer gives us a connection ID in the preferred address transport parameter.
	// We don't use the preferred address at this time, but we should use the
	// connection ID. (It isn't tied to any specific address.)
	//
	// This test will probably need updating if/when we start using the preferred address.
	cid := testPeerConnID(10)
	tc := newTestConn(t, serverSide, func(p *transportParameters) {
		p.preferredAddrV4 = netip.MustParseAddrPort("0.0.0.0:0")
		p.preferredAddrV6 = netip.MustParseAddrPort("[::0]:0")
		p.preferredAddrConnID = cid
		p.preferredAddrResetToken = make([]byte, 16)
	})
	tc.uncheckedHandshake()
	tc.ignoreFrame(frameTypeAck)

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:           2,
			retirePriorTo: 1,
			connID:        []byte{0xff},
		})
	tc.wantFrame("peer asked for conn id 0 to be retired",
		packetType1RTT, debugFrameRetireConnectionID{
			seq: 0,
		})
	if got, want := tc.sentFramePacket.dstConnID, cid; !bytes.Equal(got, want) {
		t.Fatalf("used destination conn id {%x}, want {%x} from preferred address transport parameter", got, want)
	}
}

func TestConnIDPeerProvidesPreferredAddrAndTooManyConnIDs(t *testing.T) {
	// Peer gives us more conn ids than our advertised limit,
	// including a conn id in the preferred address transport parameter.
	cid := testPeerConnID(10)
	tc := newTestConn(t, serverSide, func(p *transportParameters) {
		p.preferredAddrV4 = netip.MustParseAddrPort("0.0.0.0:0")
		p.preferredAddrV6 = netip.MustParseAddrPort("[::0]:0")
		p.preferredAddrConnID = cid
		p.preferredAddrResetToken = make([]byte, 16)
	})
	tc.uncheckedHandshake()
	tc.ignoreFrame(frameTypeAck)

	tc.writeFrames(packetType1RTT,
		debugFrameNewConnectionID{
			seq:           2,
			retirePriorTo: 0,
			connID:        testPeerConnID(2),
		})
	tc.wantFrame("peer provided 3 connection IDs, our limit is 2",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errConnectionIDLimit,
		})
}

func TestConnIDPeerWithZeroLengthIDProvidesPreferredAddr(t *testing.T) {
	// Peer gives us more conn ids than our advertised limit,
	// including a conn id in the preferred address transport parameter.
	tc := newTestConn(t, serverSide, func(p *transportParameters) {
		p.preferredAddrV4 = netip.MustParseAddrPort("0.0.0.0:0")
		p.preferredAddrV6 = netip.MustParseAddrPort("[::0]:0")
		p.preferredAddrConnID = testPeerConnID(1)
		p.preferredAddrResetToken = make([]byte, 16)
	})
	tc.peerConnID = []byte{}

	tc.writeFrames(packetTypeInitial,
		debugFrameCrypto{
			data: tc.cryptoDataIn[tls.QUICEncryptionLevelInitial],
		})
	tc.wantFrame("peer with zero-length connection ID tried to provide another in transport parameters",
		packetTypeInitial, debugFrameConnectionCloseTransport{
			code: errProtocolViolation,
		})
}
