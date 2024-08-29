// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.23

package html

import "iter"

// Ancestors returns an iterator over the ancestors of n, starting with n.Parent.
//
// Example:
//
//	for ancestor := range n.Ancestors() { ... }
//
// Mutating a Node or its parents while iterating may have unexpected results.
func (n *Node) Ancestors() iter.Seq[*Node] {
	_ = n.Parent // eager nil check

	return func(yield func(*Node) bool) {
		for p := n.Parent; p != nil && yield(p); p = p.Parent {
		}
	}
}

// Children returns an iterator over the immediate children of n,
// starting with n.FirstChild.
//
// Example:
//
//	for child := range n.Children() { ... }
func (n *Node) Children() iter.Seq[*Node] {
	_ = n.FirstChild // eager nil check

	return func(yield func(*Node) bool) {
		for c := n.FirstChild; c != nil && yield(c); c = c.NextSibling {
		}
	}

}

// Descendants returns an iterator over all nodes recursively beneath
// n, excluding n itself. Nodes are visited in depth-first preorder.
//
// Example:
//
//	for desc := range n.Descendants() { ... }
func (n *Node) Descendants() iter.Seq[*Node] {
	_ = n.FirstChild // eager nil check

	return func(yield func(*Node) bool) {
		_ = n.descendants(yield)
	}
}

func (n *Node) descendants(yield func(*Node) bool) bool {
	for c := range n.Children() {
		if !yield(c) || !c.descendants(yield) {
			return false
		}
	}
	return true
}
