// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package route

import (
	"syscall"
	"testing"
)

func TestFetchAndParseRIBOnDarwin(t *testing.T) {
	for _, typ := range []RIBType{syscall.NET_RT_FLAGS, syscall.NET_RT_DUMP2, syscall.NET_RT_IFLIST2} {
		var lastErr error
		var ms []Message
		for _, af := range []int{syscall.AF_UNSPEC, syscall.AF_INET, syscall.AF_INET6} {
			rs, err := fetchAndParseRIB(af, typ)
			if err != nil {
				lastErr = err
				continue
			}
			ms = append(ms, rs...)
		}
		if len(ms) == 0 && lastErr != nil {
			t.Error(typ, lastErr)
			continue
		}
		ss, err := msgs(ms).validate()
		if err != nil {
			t.Error(typ, err)
			continue
		}
		for _, s := range ss {
			t.Log(s)
		}
	}
}

func TestParseRIBOnDarwinRTMGET2DoesNotTreatUseAsErr(t *testing.T) {
	b := makeRouteMessageBytes(syscall.RTM_GET2, sizeofRtMsghdr2Darwin15)
	nativeEndian.PutUint32(b[16:20], 5)   // rtm_refcnt in rt_msghdr2, not rtm_pid.
	nativeEndian.PutUint32(b[20:24], 800) // rtm_parentflags in rt_msghdr2, not rtm_seq.
	nativeEndian.PutUint32(b[28:32], 140) // rtm_use in rt_msghdr2, not rtm_errno.

	ms, err := ParseRIB(RIBType(syscall.NET_RT_DUMP2), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("got %d messages; want 1", len(ms))
	}
	rm, ok := ms[0].(*RouteMessage)
	if !ok {
		t.Fatalf("got %T; want *RouteMessage", ms[0])
	}
	if rm.Err != nil {
		t.Fatalf("got Err %v; want nil", rm.Err)
	}
	if rm.ID != 0 {
		t.Fatalf("got ID %d; want 0", rm.ID)
	}
	if rm.Seq != 0 {
		t.Fatalf("got Seq %d; want 0", rm.Seq)
	}
}

func TestParseRIBOnDarwinRTMGETTreatsErrnoAsErr(t *testing.T) {
	b := makeRouteMessageBytes(syscall.RTM_GET, sizeofRtMsghdrDarwin15)
	nativeEndian.PutUint32(b[16:20], 1234)
	nativeEndian.PutUint32(b[20:24], 5678)
	nativeEndian.PutUint32(b[28:32], 140)

	ms, err := ParseRIB(0, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("got %d messages; want 1", len(ms))
	}
	rm, ok := ms[0].(*RouteMessage)
	if !ok {
		t.Fatalf("got %T; want *RouteMessage", ms[0])
	}
	if rm.Err == nil {
		t.Fatal("got nil Err; want non-nil")
	}
	if rm.ID != 1234 {
		t.Fatalf("got ID %d; want 1234", rm.ID)
	}
	if rm.Seq != 5678 {
		t.Fatalf("got Seq %d; want 5678", rm.Seq)
	}
}

func makeRouteMessageBytes(typ, size int) []byte {
	b := make([]byte, size)
	nativeEndian.PutUint16(b[:2], uint16(len(b)))
	b[2] = rtmVersion
	b[3] = byte(typ)
	return b
}
