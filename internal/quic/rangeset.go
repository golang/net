// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package quic

// A rangeset is a set of int64s, stored as an ordered list of non-overlapping,
// non-empty ranges.
//
// Rangesets are efficient for small numbers of ranges,
// which is expected to be the common case.
//
// Once we're willing to drop support for pre-generics versions of Go, this can
// be made into a parameterized type to permit use with packetNumber without casts.
type rangeset []i64range

type i64range struct {
	start, end int64 // [start, end)
}

// size returns the size of the range.
func (r i64range) size() int64 {
	return r.end - r.start
}

// contains reports whether v is in the range.
func (r i64range) contains(v int64) bool {
	return r.start <= v && v < r.end
}

// add adds [start, end) to the set, combining it with existing ranges if necessary.
func (s *rangeset) add(start, end int64) {
	if start == end {
		return
	}
	for i := range *s {
		r := &(*s)[i]
		if r.start > end {
			// The new range comes before range i.
			s.insertrange(i, start, end)
			return
		}
		if start > r.end {
			// The new range comes after range i.
			continue
		}
		// The new range is adjacent to or overlapping range i.
		if start < r.start {
			r.start = start
		}
		if end <= r.end {
			return
		}
		// Possibly coalesce subsquent ranges into range i.
		r.end = end
		j := i + 1
		for ; j < len(*s) && r.end >= (*s)[j].start; j++ {
			if e := (*s)[j].end; e > r.end {
				// Range j ends after the new range.
				r.end = e
			}
		}
		s.removeranges(i+1, j)
		return
	}
	*s = append(*s, i64range{start, end})
}

// sub removes [start, end) from the set.
func (s *rangeset) sub(start, end int64) {
	removefrom, removeto := -1, -1
	for i := range *s {
		r := &(*s)[i]
		if end < r.start {
			break
		}
		if r.end < start {
			continue
		}
		switch {
		case start <= r.start && end >= r.end:
			// Remove the entire range.
			if removefrom == -1 {
				removefrom = i
			}
			removeto = i + 1
		case start <= r.start:
			// Remove a prefix.
			r.start = end
		case end >= r.end:
			// Remove a suffix.
			r.end = start
		default:
			// Remove the middle, leaving two new ranges.
			rend := r.end
			r.end = start
			s.insertrange(i+1, end, rend)
			return
		}
	}
	if removefrom != -1 {
		s.removeranges(removefrom, removeto)
	}
}

// contains reports whether s contains v.
func (s rangeset) contains(v int64) bool {
	for _, r := range s {
		if v >= r.end {
			continue
		}
		if r.start <= v {
			return true
		}
		return false
	}
	return false
}

// rangeContaining returns the range containing v, or the range [0,0) if v is not in s.
func (s rangeset) rangeContaining(v int64) i64range {
	for _, r := range s {
		if v >= r.end {
			continue
		}
		if r.start <= v {
			return r
		}
		break
	}
	return i64range{0, 0}
}

// min returns the minimum value in the set, or 0 if empty.
func (s rangeset) min() int64 {
	if len(s) == 0 {
		return 0
	}
	return s[0].start
}

// max returns the maximum value in the set, or 0 if empty.
func (s rangeset) max() int64 {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1].end - 1
}

// end returns the end of the last range in the set, or 0 if empty.
func (s rangeset) end() int64 {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1].end
}

// isrange reports if the rangeset covers exactly the range [start, end).
func (s rangeset) isrange(start, end int64) bool {
	switch len(s) {
	case 0:
		return start == 0 && end == 0
	case 1:
		return s[0].start == start && s[0].end == end
	}
	return false
}

// removeranges removes ranges [i,j).
func (s *rangeset) removeranges(i, j int) {
	if i == j {
		return
	}
	copy((*s)[i:], (*s)[j:])
	*s = (*s)[:len(*s)-(j-i)]
}

// insert adds a new range at index i.
func (s *rangeset) insertrange(i int, start, end int64) {
	*s = append(*s, i64range{})
	copy((*s)[i+1:], (*s)[i:])
	(*s)[i] = i64range{start, end}
}
