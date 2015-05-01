// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// The h2i command is an interactive HTTP/2 console.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh/terminal"
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: h2i <hostname>\n\n")
	flag.PrintDefaults()
	os.Exit(1)
}

// withPort adds ":443" if another port isn't already present.
func withPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err != nil {
		return net.JoinHostPort(host, "443")
	}
	return host
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 1 {
		usage()
	}

	host := flag.Arg(0)
	c, err := net.Dial("tcp", withPort(host))
	if err != nil {
		log.Fatalf("Error dialing %s: %v", withPort(host), err)
	}
	defer c.Close()
	log.Printf("Connected to %v", c.RemoteAddr())

	oldState, err := terminal.MakeRaw(0)
	if err != nil {
		panic(err)
	}
	defer terminal.Restore(0, oldState)

	var screen = struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stdout}

	term := terminal.NewTerminal(screen, "> ")

	go func() {
		for t := range time.Tick(1 * time.Second) {
			term.Write([]byte(t.String() + "\n"))
		}
	}()

	for {
		line, err := term.ReadLine()
		if err != nil {
			log.Fatal("ReadLine:", err)
		}
		if line == "q" || line == "quit" {
			return
		}
		_, err = term.Write([]byte("boom - " + line + "\n"))
		if err != nil {
			log.Fatal("Write:", err)
		}
	}
}
