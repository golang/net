package main

import (
	"io"
	"log"
	"os"
	"time"

	"code.google.com/p/go.crypto/ssh/terminal"
)

func main() {
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
		if line == "quit" {
			return
		}
		_, err = term.Write([]byte("boom - " + line + "\n"))
		if err != nil {
			log.Fatal("Write:", err)
		}
	}
}
