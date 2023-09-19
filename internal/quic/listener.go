// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// A Listener listens for QUIC traffic on a network address.
// It can accept inbound connections or create outbound ones.
//
// Multiple goroutines may invoke methods on a Listener simultaneously.
type Listener struct {
	config    *Config
	udpConn   udpConn
	testHooks connTestHooks

	acceptQueue queue[*Conn] // new inbound connections

	connsMu sync.Mutex
	conns   map[*Conn]struct{}
	closing bool          // set when Close is called
	closec  chan struct{} // closed when the listen loop exits

	// The datagram receive loop keeps a mapping of connection IDs to conns.
	// When a conn's connection IDs change, we add it to connIDUpdates and set
	// connIDUpdateNeeded, and the receive loop updates its map.
	connIDUpdateMu     sync.Mutex
	connIDUpdateNeeded atomic.Bool
	connIDUpdates      []connIDUpdate
}

// A udpConn is a UDP connection.
// It is implemented by net.UDPConn.
type udpConn interface {
	Close() error
	LocalAddr() net.Addr
	ReadMsgUDPAddrPort(b, control []byte) (n, controln, flags int, _ netip.AddrPort, _ error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
}

type connIDUpdate struct {
	conn    *Conn
	retired bool
	cid     []byte
}

// Listen listens on a local network address.
// The configuration config must be non-nil.
func Listen(network, address string, config *Config) (*Listener, error) {
	if config.TLSConfig == nil {
		return nil, errors.New("TLSConfig is not set")
	}
	a, err := net.ResolveUDPAddr(network, address)
	if err != nil {
		return nil, err
	}
	udpConn, err := net.ListenUDP(network, a)
	if err != nil {
		return nil, err
	}
	return newListener(udpConn, config, nil), nil
}

func newListener(udpConn udpConn, config *Config, hooks connTestHooks) *Listener {
	l := &Listener{
		config:      config,
		udpConn:     udpConn,
		testHooks:   hooks,
		conns:       make(map[*Conn]struct{}),
		acceptQueue: newQueue[*Conn](),
		closec:      make(chan struct{}),
	}
	go l.listen()
	return l
}

// LocalAddr returns the local network address.
func (l *Listener) LocalAddr() netip.AddrPort {
	a, _ := l.udpConn.LocalAddr().(*net.UDPAddr)
	return a.AddrPort()
}

// Close closes the listener.
// Any blocked operations on the Listener or associated Conns and Stream will be unblocked
// and return errors.
//
// Close aborts every open connection.
// Data in stream read and write buffers is discarded.
// It waits for the peers of any open connection to acknowledge the connection has been closed.
func (l *Listener) Close(ctx context.Context) error {
	l.acceptQueue.close(errors.New("listener closed"))
	l.connsMu.Lock()
	if !l.closing {
		l.closing = true
		for c := range l.conns {
			c.Abort(errors.New("listener closed"))
		}
		if len(l.conns) == 0 {
			l.udpConn.Close()
		}
	}
	l.connsMu.Unlock()
	select {
	case <-l.closec:
	case <-ctx.Done():
		l.connsMu.Lock()
		for c := range l.conns {
			c.exit()
		}
		l.connsMu.Unlock()
		return ctx.Err()
	}
	return nil
}

// Accept waits for and returns the next connection to the listener.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	return l.acceptQueue.get(ctx, nil)
}

// Dial creates and returns a connection to a network address.
func (l *Listener) Dial(ctx context.Context, network, address string) (*Conn, error) {
	u, err := net.ResolveUDPAddr(network, address)
	if err != nil {
		return nil, err
	}
	addr := u.AddrPort()
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	c, err := l.newConn(time.Now(), clientSide, nil, addr)
	if err != nil {
		return nil, err
	}
	if err := c.waitReady(ctx); err != nil {
		c.Abort(nil)
		return nil, err
	}
	return c, nil
}

func (l *Listener) newConn(now time.Time, side connSide, initialConnID []byte, peerAddr netip.AddrPort) (*Conn, error) {
	l.connsMu.Lock()
	defer l.connsMu.Unlock()
	if l.closing {
		return nil, errors.New("listener closed")
	}
	c, err := newConn(now, side, initialConnID, peerAddr, l.config, l, l.testHooks)
	if err != nil {
		return nil, err
	}
	l.conns[c] = struct{}{}
	return c, nil
}

// serverConnEstablished is called by a conn when the handshake completes
// for an inbound (serverSide) connection.
func (l *Listener) serverConnEstablished(c *Conn) {
	l.acceptQueue.put(c)
}

// connDrained is called by a conn when it leaves the draining state,
// either when the peer acknowledges connection closure or the drain timeout expires.
func (l *Listener) connDrained(c *Conn) {
	l.connsMu.Lock()
	defer l.connsMu.Unlock()
	delete(l.conns, c)
	if l.closing && len(l.conns) == 0 {
		l.udpConn.Close()
	}
}

// connIDsChanged is called by a conn when its connection IDs change.
func (l *Listener) connIDsChanged(c *Conn, retired bool, cids []connID) {
	l.connIDUpdateMu.Lock()
	defer l.connIDUpdateMu.Unlock()
	for _, cid := range cids {
		l.connIDUpdates = append(l.connIDUpdates, connIDUpdate{
			conn:    c,
			retired: retired,
			cid:     cid.cid,
		})
	}
	l.connIDUpdateNeeded.Store(true)
}

// updateConnIDs is called by the datagram receive loop to update its connection ID map.
func (l *Listener) updateConnIDs(conns map[string]*Conn) {
	l.connIDUpdateMu.Lock()
	defer l.connIDUpdateMu.Unlock()
	for i, u := range l.connIDUpdates {
		if u.retired {
			delete(conns, string(u.cid))
		} else {
			conns[string(u.cid)] = u.conn
		}
		l.connIDUpdates[i] = connIDUpdate{} // drop refs
	}
	l.connIDUpdates = l.connIDUpdates[:0]
	l.connIDUpdateNeeded.Store(false)
}

func (l *Listener) listen() {
	defer close(l.closec)
	conns := map[string]*Conn{}
	for {
		m := newDatagram()
		// TODO: Read and process the ECN (explicit congestion notification) field.
		// https://tools.ietf.org/html/draft-ietf-quic-transport-32#section-13.4
		n, _, _, addr, err := l.udpConn.ReadMsgUDPAddrPort(m.b, nil)
		if err != nil {
			// The user has probably closed the listener.
			// We currently don't surface errors from other causes;
			// we could check to see if the listener has been closed and
			// record the unexpected error if it has not.
			return
		}
		if n == 0 {
			continue
		}
		if l.connIDUpdateNeeded.Load() {
			l.updateConnIDs(conns)
		}
		m.addr = addr
		m.b = m.b[:n]
		l.handleDatagram(m, conns)
	}
}

func (l *Listener) handleDatagram(m *datagram, conns map[string]*Conn) {
	dstConnID, ok := dstConnIDForDatagram(m.b)
	if !ok {
		m.recycle()
		return
	}
	c := conns[string(dstConnID)]
	if c == nil {
		// TODO: Move this branch into a separate goroutine to avoid blocking
		// the listener while processing packets.
		l.handleUnknownDestinationDatagram(m)
		return
	}

	// TODO: This can block the listener while waiting for the conn to accept the dgram.
	// Think about buffering between the receive loop and the conn.
	c.sendMsg(m)
}

func (l *Listener) handleUnknownDestinationDatagram(m *datagram) {
	defer func() {
		if m != nil {
			m.recycle()
		}
	}()
	if len(m.b) < minimumClientInitialDatagramSize {
		return
	}
	p, ok := parseGenericLongHeaderPacket(m.b)
	if !ok {
		// Not a long header packet, or not parseable.
		// Short header (1-RTT) packets don't contain enough information
		// to do anything useful with if we don't recognize the
		// connection ID.
		return
	}

	switch p.version {
	case quicVersion1:
	case 0:
		// Version Negotiation for an unknown connection.
		return
	default:
		// Unknown version.
		l.sendVersionNegotiation(p, m.addr)
		return
	}
	if getPacketType(m.b) != packetTypeInitial {
		// This packet isn't trying to create a new connection.
		// It might be associated with some connection we've lost state for.
		// TODO: Send a stateless reset when appropriate.
		// https://www.rfc-editor.org/rfc/rfc9000.html#section-10.3
		return
	}
	var now time.Time
	if l.testHooks != nil {
		now = l.testHooks.timeNow()
	} else {
		now = time.Now()
	}
	var err error
	c, err := l.newConn(now, serverSide, p.dstConnID, m.addr)
	if err != nil {
		// The accept queue is probably full.
		// We could send a CONNECTION_CLOSE to the peer to reject the connection.
		// Currently, we just drop the datagram.
		// https://www.rfc-editor.org/rfc/rfc9000.html#section-5.2.2-5
		return
	}
	c.sendMsg(m)
	m = nil // don't recycle, sendMsg takes ownership
}

func (l *Listener) sendVersionNegotiation(p genericLongPacket, addr netip.AddrPort) {
	m := newDatagram()
	m.b = appendVersionNegotiation(m.b[:0], p.srcConnID, p.dstConnID, quicVersion1)
	l.sendDatagram(m.b, addr)
	m.recycle()
}

func (l *Listener) sendDatagram(p []byte, addr netip.AddrPort) error {
	_, err := l.udpConn.WriteToUDPAddrPort(p, addr)
	return err
}
