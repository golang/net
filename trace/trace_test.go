// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package trace

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

type s struct{}

func (s) String() string { return "lazy string" }

// TestReset checks whether all the fields are zeroed after reset.
func TestReset(t *testing.T) {
	tr := New("foo", "bar")
	tr.LazyLog(s{}, false)
	tr.LazyPrintf("%d", 1)
	tr.SetRecycler(func(_ interface{}) {})
	tr.SetTraceInfo(3, 4)
	tr.SetMaxEvents(100)
	tr.SetError()
	tr.Finish()

	tr.(*trace).reset()

	if !reflect.DeepEqual(tr, new(trace)) {
		t.Errorf("reset didn't clear all fields: %+v", tr)
	}
}

// TestResetLog checks whether all the fields are zeroed after reset.
func TestResetLog(t *testing.T) {
	el := NewEventLog("foo", "bar")
	el.Printf("message")
	el.Errorf("error")
	el.Finish()

	el.(*eventLog).reset()

	if !reflect.DeepEqual(el, new(eventLog)) {
		t.Errorf("reset didn't clear all fields: %+v", el)
	}
}

func TestAuthRequest(t *testing.T) {
	testCases := []struct {
		host string
		want bool
	}{
		{host: "192.168.23.1", want: false},
		{host: "192.168.23.1:8080", want: false},
		{host: "malformed remote addr", want: false},
		{host: "localhost", want: true},
		{host: "localhost:8080", want: true},
		{host: "127.0.0.1", want: true},
		{host: "127.0.0.1:8080", want: true},
		{host: "::1", want: true},
		{host: "[::1]:8080", want: true},
	}
	for _, tt := range testCases {
		req := &http.Request{RemoteAddr: tt.host}
		any, sensitive := AuthRequest(req)
		if any != tt.want || sensitive != tt.want {
			t.Errorf("AuthRequest(%q) = %t, %t; want %t, %t", tt.host, any, sensitive, tt.want, tt.want)
		}
	}
}

func benchmarkTrace(b *testing.B, maxEvents, numEvents int) {
	numSpans := (b.N + numEvents + 1) / numEvents

	for i := 0; i < numSpans; i++ {
		tr := New("test", "test")
		tr.SetMaxEvents(maxEvents)
		for j := 0; j < numEvents; j++ {
			tr.LazyPrintf("%d", j)
		}
		tr.Finish()
	}
}

func BenchmarkTrace(b *testing.B) {
	for _, maxEvents := range []int{0, 10, 100, 1000} {
		for _, numEvents := range []int{2, 10, 100, 1000, 10000} {
			name := fmt.Sprintf("%d-%d", maxEvents, numEvents)
			b.Run(name, func(b *testing.B) {
				benchmarkTrace(b, maxEvents, numEvents)
			})
		}
	}
}
