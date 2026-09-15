// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dict

import (
	"bufio"
	"net"
	"net/textproto"
	"testing"
)

func TestDefineSkipsMalformedEntry(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	tc := textproto.NewConn(client)
	c := &Client{text: tc}

	go func() {
		br := bufio.NewReader(server)
		// Consume the DEFINE command sent by Cmd.
		br.ReadString('\n')

		// Send a response with two entries:
		// - first is malformed (only 1 field, no database/description)
		// - second is valid
		resp := "150 2 definitions found\r\n" +
			"151 bad-entry\r\n" +
			"should be skipped body\r\n" +
			".\r\n" +
			"151 word1 db1 \"Description 1\"\r\n" +
			"definition text here\r\n" +
			".\r\n" +
			"250 ok\r\n"
		server.Write([]byte(resp))
	}()

	defs, err := c.Define("*", "test")
	if err != nil {
		t.Fatalf("Define failed: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(defs))
	}
	if defs[0].Word != "word1" {
		t.Fatalf("expected word 'word1', got %q", defs[0].Word)
	}
	// ReadDotBytes joins dot-body lines with \n.
	if string(defs[0].Text) != "definition text here\n" {
		t.Fatalf("unexpected definition text: %q", string(defs[0].Text))
	}
}

func TestDefineAllValid(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	tc := textproto.NewConn(client)
	c := &Client{text: tc}

	go func() {
		br := bufio.NewReader(server)
		br.ReadString('\n')

		resp := "150 2 definitions found\r\n" +
			"151 word1 db1 \"Description 1\"\r\n" +
			"text one\r\n" +
			".\r\n" +
			"151 word2 db2 \"Description 2\"\r\n" +
			"text two\r\n" +
			".\r\n" +
			"250 ok\r\n"
		server.Write([]byte(resp))
	}()

	defs, err := c.Define("*", "test")
	if err != nil {
		t.Fatalf("Define failed: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
}
