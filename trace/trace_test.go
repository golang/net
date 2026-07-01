// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package trace

import (
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
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

// TestParseTemplate checks that all templates used by this package are valid
// as they are parsed on first usage
func TestParseTemplate(t *testing.T) {
	if tmpl := distTmpl(); tmpl == nil {
		t.Error("invalid template returned from distTmpl()")
	}
	if tmpl := pageTmpl(); tmpl == nil {
		t.Error("invalid template returned from pageTmpl()")
	}
	if tmpl := eventsTmpl(); tmpl == nil {
		t.Error("invalid template returned from eventsTmpl()")
	}
}

type recyclableEvent struct{}

func (e *recyclableEvent) String() string { return "recyclable event" }

// TestRecycler checks that all trace events have the recycler called on them.
func TestRecycler(t *testing.T) {
	testData := []struct {
		name                   string
		traces, eventsPerTrace int
		wantRecycled           int
	}{
		{
			name:           "no recycling",
			traces:         tracesPerBucket,
			eventsPerTrace: 3,
			wantRecycled:   0,
		},
		{
			name:           "one extra trace",
			traces:         tracesPerBucket + 1,
			eventsPerTrace: 1,
			wantRecycled:   1,
		},
		{
			name:           "three events per trace",
			traces:         tracesPerBucket + 1,
			eventsPerTrace: 3,
			wantRecycled:   3,
		},
		{ // this is the boundary case
			name:           "four events per trace",
			traces:         tracesPerBucket + 1,
			eventsPerTrace: 4,
			wantRecycled:   4,
		},
		{
			name:           "five events per trace",
			traces:         tracesPerBucket + 1,
			eventsPerTrace: 5,
			wantRecycled:   5,
		},
		{
			name:           "lots of events per trace",
			traces:         1,
			eventsPerTrace: 100,
			wantRecycled:   100 - maxEventsPerTrace + 1, // +1 is the trace that gets recycled to make room for the "(x events discarded)" entry
		},
		{
			name:           "lots of traces with one event",
			traces:         tracesPerBucket + 1,
			eventsPerTrace: 100,
			wantRecycled:   100 + tracesPerBucket*(100-maxEventsPerTrace+1),
		},
		{
			name:           "lots of events in lots of traces",
			traces:         tracesPerBucket + 100,
			eventsPerTrace: 100,
			wantRecycled:   100*100 + tracesPerBucket*(100-maxEventsPerTrace+1),
		},
	}

	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			var recycled int32
			for i := 0; i < test.traces; i++ {
				tr := New("family", "title")
				tr.SetRecycler(func(x interface{}) {
					if _, ok := x.(*recyclableEvent); ok {
						atomic.AddInt32(&recycled, 1)
					}
				})
				for j := 0; j < test.eventsPerTrace; j++ {
					tr.LazyLog(&recyclableEvent{}, false)
				}
				tr.Finish()
			}
			for i := 0; i < 10; i++ {
				if atomic.LoadInt32(&recycled) >= int32(test.wantRecycled) {
					break
				} else {
					time.Sleep(time.Duration(i*10) * time.Millisecond)
				}
			}
			if got, want := recycled, int32(test.wantRecycled); got != want {
				t.Errorf("unexpected number of events recycled:\n  got: %v\n want: %v", got, want)
			}
		})
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

func BenchmarkTrace_Default_2(b *testing.B) {
	benchmarkTrace(b, 0, 2)
}

func BenchmarkTrace_Default_10(b *testing.B) {
	benchmarkTrace(b, 0, 10)
}

func BenchmarkTrace_Default_100(b *testing.B) {
	benchmarkTrace(b, 0, 100)
}

func BenchmarkTrace_Default_1000(b *testing.B) {
	benchmarkTrace(b, 0, 1000)
}

func BenchmarkTrace_Default_10000(b *testing.B) {
	benchmarkTrace(b, 0, 10000)
}

func BenchmarkTrace_10_2(b *testing.B) {
	benchmarkTrace(b, 10, 2)
}

func BenchmarkTrace_10_10(b *testing.B) {
	benchmarkTrace(b, 10, 10)
}

func BenchmarkTrace_10_100(b *testing.B) {
	benchmarkTrace(b, 10, 100)
}

func BenchmarkTrace_10_1000(b *testing.B) {
	benchmarkTrace(b, 10, 1000)
}

func BenchmarkTrace_10_10000(b *testing.B) {
	benchmarkTrace(b, 10, 10000)
}

func BenchmarkTrace_100_2(b *testing.B) {
	benchmarkTrace(b, 100, 2)
}

func BenchmarkTrace_100_10(b *testing.B) {
	benchmarkTrace(b, 100, 10)
}

func BenchmarkTrace_100_100(b *testing.B) {
	benchmarkTrace(b, 100, 100)
}

func BenchmarkTrace_100_1000(b *testing.B) {
	benchmarkTrace(b, 100, 1000)
}

func BenchmarkTrace_100_10000(b *testing.B) {
	benchmarkTrace(b, 100, 10000)
}

func BenchmarkTrace_1000_2(b *testing.B) {
	benchmarkTrace(b, 1000, 2)
}

func BenchmarkTrace_1000_10(b *testing.B) {
	benchmarkTrace(b, 1000, 10)
}

func BenchmarkTrace_1000_100(b *testing.B) {
	benchmarkTrace(b, 1000, 100)
}

func BenchmarkTrace_1000_1000(b *testing.B) {
	benchmarkTrace(b, 1000, 1000)
}

func BenchmarkTrace_1000_10000(b *testing.B) {
	benchmarkTrace(b, 1000, 10000)
}
