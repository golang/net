// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"golang.org/x/net/internal/quic/qlog"
)

func TestQLogHandshake(t *testing.T) {
	testSides(t, "", func(t *testing.T, side connSide) {
		qr := &qlogRecord{}
		tc := newTestConn(t, side, qr.config)
		tc.handshake()
		tc.conn.Abort(nil)
		tc.wantFrame("aborting connection generates CONN_CLOSE",
			packetType1RTT, debugFrameConnectionCloseTransport{
				code: errNo,
			})
		tc.writeFrames(packetType1RTT, debugFrameConnectionCloseTransport{})
		tc.advanceToTimer() // let the conn finish draining

		var src, dst []byte
		if side == clientSide {
			src = testLocalConnID(0)
			dst = testLocalConnID(-1)
		} else {
			src = testPeerConnID(-1)
			dst = testPeerConnID(0)
		}
		qr.wantEvents(t, jsonEvent{
			"name": "connectivity:connection_started",
			"data": map[string]any{
				"src_cid": hex.EncodeToString(src),
				"dst_cid": hex.EncodeToString(dst),
			},
		}, jsonEvent{
			"name": "connectivity:connection_closed",
			"data": map[string]any{
				"trigger": "clean",
			},
		})
	})
}

type nopCloseWriter struct {
	io.Writer
}

func (nopCloseWriter) Close() error { return nil }

type jsonEvent map[string]any

func (j jsonEvent) String() string {
	b, _ := json.MarshalIndent(j, "", "  ")
	return string(b)
}

// eventPartialEqual verifies that every field set in want matches the corresponding field in got.
// It ignores additional fields in got.
func eventPartialEqual(got, want jsonEvent) bool {
	for k := range want {
		ge, gok := got[k].(map[string]any)
		we, wok := want[k].(map[string]any)
		if gok && wok {
			if !eventPartialEqual(ge, we) {
				return false
			}
		} else {
			if !reflect.DeepEqual(got[k], want[k]) {
				return false
			}
		}
	}
	return true
}

// A qlogRecord records events.
type qlogRecord struct {
	ev []jsonEvent
}

func (q *qlogRecord) Write(b []byte) (int, error) {
	// This relies on the property that the Handler always makes one Write call per event.
	if len(b) < 1 || b[0] != 0x1e {
		panic(fmt.Errorf("trace Write should start with record separator, got %q", string(b)))
	}
	var val map[string]any
	if err := json.Unmarshal(b[1:], &val); err != nil {
		panic(fmt.Errorf("log unmarshal failure: %v\n%v", err, string(b)))
	}
	q.ev = append(q.ev, val)
	return len(b), nil
}

func (q *qlogRecord) Close() error { return nil }

// config may be passed to newTestConn to configure the conn to use this logger.
func (q *qlogRecord) config(c *Config) {
	c.QLogLogger = slog.New(qlog.NewJSONHandler(qlog.HandlerOptions{
		NewTrace: func(info qlog.TraceInfo) (io.WriteCloser, error) {
			return q, nil
		},
	}))
}

// wantEvents checks that every event in want occurs in the order specified.
func (q *qlogRecord) wantEvents(t *testing.T, want ...jsonEvent) {
	t.Helper()
	got := q.ev
	unseen := want
	for _, g := range got {
		if eventPartialEqual(g, unseen[0]) {
			unseen = unseen[1:]
			if len(unseen) == 0 {
				return
			}
		}
	}
	t.Fatalf("got events:\n%v\n\nwant events:\n%v", got, want)
}
