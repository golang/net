// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package quic

import (
	"reflect"
	"testing"
)

func TestRangeSize(t *testing.T) {
	for _, test := range []struct {
		r    i64range
		want int64
	}{{
		r:    i64range{0, 100},
		want: 100,
	}, {
		r:    i64range{10, 20},
		want: 10,
	}} {
		if got := test.r.size(); got != test.want {
			t.Errorf("%+v.size = %v, want %v", test.r, got, test.want)
		}
	}
}

func TestRangeContains(t *testing.T) {
	r := i64range{5, 10}
	for _, i := range []int64{0, 4, 10, 15} {
		if r.contains(i) {
			t.Errorf("%v.contains(%v) = true, want false", r, i)
		}
	}
	for _, i := range []int64{5, 6, 7, 8, 9} {
		if !r.contains(i) {
			t.Errorf("%v.contains(%v) = false, want true", r, i)
		}
	}
}

func TestRangesetAdd(t *testing.T) {
	for _, test := range []struct {
		desc string
		set  rangeset
		add  i64range
		want rangeset
	}{{
		desc: "add to empty set",
		set:  rangeset{},
		add:  i64range{0, 100},
		want: rangeset{{0, 100}},
	}, {
		desc: "add empty range",
		set:  rangeset{},
		add:  i64range{100, 100},
		want: rangeset{},
	}, {
		desc: "append nonadjacent range",
		set:  rangeset{{100, 200}},
		add:  i64range{300, 400},
		want: rangeset{{100, 200}, {300, 400}},
	}, {
		desc: "prepend nonadjacent range",
		set:  rangeset{{100, 200}},
		add:  i64range{0, 50},
		want: rangeset{{0, 50}, {100, 200}},
	}, {
		desc: "insert nonadjacent range",
		set:  rangeset{{100, 200}, {500, 600}},
		add:  i64range{300, 400},
		want: rangeset{{100, 200}, {300, 400}, {500, 600}},
	}, {
		desc: "prepend adjacent range",
		set:  rangeset{{100, 200}},
		add:  i64range{50, 100},
		want: rangeset{{50, 200}},
	}, {
		desc: "append adjacent range",
		set:  rangeset{{100, 200}},
		add:  i64range{200, 250},
		want: rangeset{{100, 250}},
	}, {
		desc: "prepend overlapping range",
		set:  rangeset{{100, 200}},
		add:  i64range{50, 150},
		want: rangeset{{50, 200}},
	}, {
		desc: "append overlapping range",
		set:  rangeset{{100, 200}},
		add:  i64range{150, 250},
		want: rangeset{{100, 250}},
	}, {
		desc: "replace range",
		set:  rangeset{{100, 200}},
		add:  i64range{50, 250},
		want: rangeset{{50, 250}},
	}, {
		desc: "prepend and combine",
		set:  rangeset{{100, 200}, {300, 400}, {500, 600}},
		add:  i64range{50, 300},
		want: rangeset{{50, 400}, {500, 600}},
	}, {
		desc: "combine several ranges",
		set:  rangeset{{100, 200}, {300, 400}, {500, 600}, {700, 800}, {900, 1000}},
		add:  i64range{300, 850},
		want: rangeset{{100, 200}, {300, 850}, {900, 1000}},
	}} {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			got := test.set
			got.add(test.add.start, test.add.end)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("add [%v,%v) to %v", test.add.start, test.add.end, test.set)
				t.Errorf("  got: %v", got)
				t.Errorf(" want: %v", test.want)
			}
		})
	}
}

func TestRangesetSub(t *testing.T) {
	for _, test := range []struct {
		desc string
		set  rangeset
		sub  i64range
		want rangeset
	}{{
		desc: "subtract from empty set",
		set:  rangeset{},
		sub:  i64range{0, 100},
		want: rangeset{},
	}, {
		desc: "subtract empty range",
		set:  rangeset{{0, 100}},
		sub:  i64range{0, 0},
		want: rangeset{{0, 100}},
	}, {
		desc: "subtract not present in set",
		set:  rangeset{{0, 100}, {200, 300}},
		sub:  i64range{100, 200},
		want: rangeset{{0, 100}, {200, 300}},
	}, {
		desc: "subtract prefix",
		set:  rangeset{{100, 200}},
		sub:  i64range{0, 150},
		want: rangeset{{150, 200}},
	}, {
		desc: "subtract suffix",
		set:  rangeset{{100, 200}},
		sub:  i64range{150, 300},
		want: rangeset{{100, 150}},
	}, {
		desc: "subtract middle",
		set:  rangeset{{0, 100}},
		sub:  i64range{40, 60},
		want: rangeset{{0, 40}, {60, 100}},
	}, {
		desc: "subtract from two ranges",
		set:  rangeset{{0, 100}, {200, 300}},
		sub:  i64range{50, 250},
		want: rangeset{{0, 50}, {250, 300}},
	}, {
		desc: "subtract removes range",
		set:  rangeset{{0, 100}, {200, 300}, {400, 500}},
		sub:  i64range{200, 300},
		want: rangeset{{0, 100}, {400, 500}},
	}, {
		desc: "subtract removes multiple ranges",
		set:  rangeset{{0, 100}, {200, 300}, {400, 500}, {600, 700}},
		sub:  i64range{50, 650},
		want: rangeset{{0, 50}, {650, 700}},
	}, {
		desc: "subtract only range",
		set:  rangeset{{0, 100}},
		sub:  i64range{0, 100},
		want: rangeset{},
	}} {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			got := test.set
			got.sub(test.sub.start, test.sub.end)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("sub [%v,%v) from %v", test.sub.start, test.sub.end, test.set)
				t.Errorf("  got: %v", got)
				t.Errorf(" want: %v", test.want)
			}
		})
	}
}

func TestRangesetContains(t *testing.T) {
	var s rangeset
	s.add(10, 20)
	s.add(30, 40)
	for i := int64(0); i < 50; i++ {
		want := (i >= 10 && i < 20) || (i >= 30 && i < 40)
		if got := s.contains(i); got != want {
			t.Errorf("%v.contains(%v) = %v, want %v", s, i, got, want)
		}
	}
}

func TestRangesetRangeContaining(t *testing.T) {
	var s rangeset
	s.add(10, 20)
	s.add(30, 40)
	for _, test := range []struct {
		v    int64
		want i64range
	}{
		{0, i64range{0, 0}},
		{9, i64range{0, 0}},
		{10, i64range{10, 20}},
		{15, i64range{10, 20}},
		{19, i64range{10, 20}},
		{20, i64range{0, 0}},
		{29, i64range{0, 0}},
		{30, i64range{30, 40}},
		{39, i64range{30, 40}},
		{40, i64range{0, 0}},
	} {
		got := s.rangeContaining(test.v)
		if got != test.want {
			t.Errorf("%v.rangeContaining(%v) = %v, want %v", s, test.v, got, test.want)
		}
	}
}

func TestRangesetLimits(t *testing.T) {
	for _, test := range []struct {
		s       rangeset
		wantMin int64
		wantMax int64
		wantEnd int64
	}{{
		s:       rangeset{},
		wantMin: 0,
		wantMax: 0,
		wantEnd: 0,
	}, {
		s:       rangeset{{10, 20}},
		wantMin: 10,
		wantMax: 19,
		wantEnd: 20,
	}, {
		s:       rangeset{{10, 20}, {30, 40}, {50, 60}},
		wantMin: 10,
		wantMax: 59,
		wantEnd: 60,
	}} {
		if got, want := test.s.min(), test.wantMin; got != want {
			t.Errorf("%+v.min() = %v, want %v", test.s, got, want)
		}
		if got, want := test.s.max(), test.wantMax; got != want {
			t.Errorf("%+v.max() = %v, want %v", test.s, got, want)
		}
		if got, want := test.s.end(), test.wantEnd; got != want {
			t.Errorf("%+v.end() = %v, want %v", test.s, got, want)
		}
	}
}

func TestRangesetIsRange(t *testing.T) {
	for _, test := range []struct {
		s    rangeset
		r    i64range
		want bool
	}{{
		s:    rangeset{{0, 100}},
		r:    i64range{0, 100},
		want: true,
	}, {
		s:    rangeset{{0, 100}},
		r:    i64range{0, 101},
		want: false,
	}, {
		s:    rangeset{{0, 10}, {11, 100}},
		r:    i64range{0, 100},
		want: false,
	}, {
		s:    rangeset{},
		r:    i64range{0, 0},
		want: true,
	}, {
		s:    rangeset{},
		r:    i64range{0, 1},
		want: false,
	}} {
		if got := test.s.isrange(test.r.start, test.r.end); got != test.want {
			t.Errorf("%+v.isrange(%v, %v) = %v, want %v", test.s, test.r.start, test.r.end, got, test.want)
		}
	}
}
