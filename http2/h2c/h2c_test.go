// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package h2c

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/net/http2"
)

func TestSettingsAckSwallowWriter(t *testing.T) {
	var buf bytes.Buffer
	swallower := newSettingsAckSwallowWriter(bufio.NewWriter(&buf))
	fw := http2.NewFramer(swallower, nil)
	fw.WriteSettings(http2.Setting{http2.SettingMaxFrameSize, 2})
	fw.WriteSettingsAck()
	fw.WriteData(1, true, []byte{})
	swallower.Flush()

	fr := http2.NewFramer(nil, bufio.NewReader(&buf))

	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Header().Type != http2.FrameSettings {
		t.Fatalf("Expected first frame to be SETTINGS. Got: %v", f.Header().Type)
	}

	f, err = fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Header().Type != http2.FrameData {
		t.Fatalf("Expected first frame to be DATA. Got: %v", f.Header().Type)
	}
}

func ExampleNewHandler() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Hello world")
	})
	h2s := &http2.Server{
		// ...
	}
	h1s := &http.Server{
		Addr:    ":8080",
		Handler: NewHandler(handler, h2s),
	}
	log.Fatal(h1s.ListenAndServe())
}

func TestContext(t *testing.T) {
	baseCtx := context.WithValue(context.Background(), "testkey", "testvalue")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("Request wasn't handled by h2c.  Got ProtoMajor=%v", r.ProtoMajor)
		}
		if r.Context().Value("testkey") != "testvalue" {
			t.Errorf("Request doesn't have expected base context: %v", r.Context())
		}
		fmt.Fprint(w, "Hello world")
	})

	h2s := &http2.Server{}
	h1s := httptest.NewUnstartedServer(NewHandler(handler, h2s))
	h1s.Config.BaseContext = func(_ net.Listener) context.Context {
		return baseCtx
	}
	h1s.Start()
	defer h1s.Close()

	client := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
	}

	resp, err := client.Get(h1s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConvertH1ReqToH2WithPOST(t *testing.T) {
	postBody := "Some POST Body"

	r, err := http.NewRequest("POST", "http://localhost:80", bytes.NewBufferString(postBody))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Upgrade", "h2c")
	r.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	r.Header.Set("HTTP2-Settings", "AAEAAEAAAAIAAAABAAMAAABkAAQBAAAAAAUAAEAA") // Some Default Settings
	h2Bytes, _, err := convertH1ReqToH2(r)

	if err != nil {
		t.Fatal(err)
	}

	// Read off the preface
	preface := []byte(http2.ClientPreface)
	if h2Bytes.Len() < len(preface) {
		t.Fatal("Could not read HTTP/2 ClientPreface")
	}
	readPreface := h2Bytes.Next(len(preface))
	if string(readPreface) != http2.ClientPreface {
		t.Fatalf("Expected Preface %s but got: %s", http2.ClientPreface, string(readPreface))
	}

	framer := http2.NewFramer(nil, h2Bytes)

	// Should get a SETTINGS, HEADERS, and then DATA
	expectedFrameTypes := []http2.FrameType{http2.FrameSettings, http2.FrameHeaders, http2.FrameData}
	for frameNumber := 0; h2Bytes.Len() > 0; {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}

		if frameNumber >= len(expectedFrameTypes) {
			t.Errorf("Got more than %d frames, wanted only %d", len(expectedFrameTypes), len(expectedFrameTypes))
		}

		if frame.Header().Type != expectedFrameTypes[frameNumber] {
			t.Errorf("Got FrameType %v, wanted %v", frame.Header().Type, expectedFrameTypes[frameNumber])
		}

		frameNumber += 1

		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if frameNumber != 1 {
				t.Errorf("Got SETTINGS frame as frame #%d, wanted it as frame #1", frameNumber)
			}
		case *http2.HeadersFrame:
			if frameNumber != 2 {
				t.Errorf("Got HEADERS frame as frame #%d, wanted it as frame #2", frameNumber)
			}
			if f.FrameHeader.StreamID != 1 {
				t.Fatalf("Got StreamID %v, wanted StreamID 1", f.FrameHeader.StreamID)
			}
		case *http2.DataFrame:
			if frameNumber != 3 {
				t.Errorf("Got DATA frame as frame #%d, wanted it as frame #3", frameNumber)
			}
			if f.FrameHeader.StreamID != 1 {
				t.Fatalf("Got StreamID %v, wanted StreamID 1", f.FrameHeader.StreamID)
			}

			body := string(f.Data())

			if body != postBody {
				t.Errorf("Got DATA body %s, wanted %s", body, postBody)
			}
		}
	}
}

func TestConvertH1ReqToH2WithPOSTBodyMultipleOfFrameSize(t *testing.T) {
	frameSize := 1024
	fillByte := byte(0x45)
	postBody := bytes.Repeat([]byte{fillByte}, frameSize*2)

	r, err := http.NewRequest("POST", "http://localhost:80", bytes.NewBuffer(postBody))
	if err != nil {
		t.Fatal(err)
	}

	var settingsBuffer bytes.Buffer
	settingsFramer := http2.NewFramer(&settingsBuffer, nil)
	settingsFramer.WriteSettings(http2.Setting{http2.SettingMaxFrameSize, uint32(frameSize)})
	settingsEncoded := base64.URLEncoding.EncodeToString(settingsBuffer.Bytes())

	r.Header.Set("Upgrade", "h2c")
	r.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	r.Header.Set("HTTP2-Settings", settingsEncoded)
	h2Bytes, _, err := convertH1ReqToH2(r)

	if err != nil {
		t.Fatal(err)
	}

	// Read off the preface
	preface := []byte(http2.ClientPreface)
	if h2Bytes.Len() < len(preface) {
		t.Fatal("Could not read HTTP/2 ClientPreface")
	}
	readPreface := h2Bytes.Next(len(preface))
	if string(readPreface) != http2.ClientPreface {
		t.Fatalf("Expected Preface %s but got: %s", http2.ClientPreface, string(readPreface))
	}

	framer := http2.NewFramer(nil, h2Bytes)

	// Should get a SETTINGS, HEADERS, and then DATA
	expectedFrameTypes := []http2.FrameType{http2.FrameSettings, http2.FrameHeaders, http2.FrameData, http2.FrameData, http2.FrameData}
	for frameNumber := 0; h2Bytes.Len() > 0; {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}

		if frameNumber >= len(expectedFrameTypes) {
			t.Errorf("Got more than %d frames, wanted only %d", len(expectedFrameTypes), len(expectedFrameTypes))
		}

		if frame.Header().Type != expectedFrameTypes[frameNumber] {
			t.Errorf("Got FrameType %v, wanted %v", frame.Header().Type, expectedFrameTypes[frameNumber])
		}

		frameNumber += 1

		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if frameNumber != 1 {
				t.Errorf("Got SETTINGS frame as frame #%d, wanted it as frame #1", frameNumber)
			}
		case *http2.HeadersFrame:
			if frameNumber != 2 {
				t.Errorf("Got HEADERS frame as frame #%d, wanted it as frame #2", frameNumber)
			}
			if f.FrameHeader.StreamID != 1 {
				t.Fatalf("Got StreamID %v, wanted StreamID 1", f.FrameHeader.StreamID)
			}
		case *http2.DataFrame:
			if frameNumber < 3 {
				t.Errorf("Got DATA frame as frame #%d, wanted it as frame #3 or later", frameNumber)
			}
			if f.FrameHeader.StreamID != 1 {
				t.Fatalf("Got StreamID %v, wanted StreamID 1", f.FrameHeader.StreamID)
			}

			if frameNumber < len(expectedFrameTypes) && len(f.Data()) < frameSize {
				t.Errorf("Expected data frame with length %d, got %d", frameSize, len(f.Data()))
			}

			if frameNumber == len(expectedFrameTypes) && len(f.Data()) != 0 {
				t.Errorf("Got non-empty DATA frame as last frame, expected it to be empty")
			}

			if frameNumber == len(expectedFrameTypes) && (f.FrameHeader.Flags&http2.FlagHeadersEndStream) == 0 {
				t.Errorf("Got last DATA frame with end stream not set, expected end stream set")
			}

			for _, b := range f.Data() {
				if b != fillByte {
					t.Errorf("Got data byte %d, wanted %d", b, fillByte)
					break
				}
			}
		}
	}
}
