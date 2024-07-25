// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.23

package html

import "iter"

// ChildNodes returns a sequence yielding the immediate children of n.
//
// Mutating a Node while iterating through its ChildNodes may have unexpected results.
func (n *Node) ChildNodes() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if n == nil {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if !yield(c) {
				return
			}
		}
	}

}

func (n *Node) all(yield func(*Node) bool) bool {
	if !yield(n) {
		return false
	}

	for c := range n.ChildNodes() {
		if !c.all(yield) {
			return false
		}
	}
	return true
}

// All returns a sequence yielding all descendents of n in depth-first pre-order.
//
// Mutating a Node while iterating through it or its descendents may have unexpected results.
func (n *Node) All() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if n == nil {
			return
		}
		n.all(yield)
	}
}

// Parents returns an sequence yielding the node and its parents.
//
// Mutating a Node while iterating through it or its parents may have unexpected results.
func (n *Node) Parents() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for p := n; p != nil; p = p.Parent {
			if !yield(p) {
				return
			}
		}
	}
}
