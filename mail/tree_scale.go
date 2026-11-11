package mail

import "sync"

// pfTreeChunks lends ReadTree the buffer it reads one leaf's body through.
// Every leaf used to allocate its own 32 KiB, which for a message of 10 000
// small parts was 320 MiB of buffers to read a few kilobytes (ROB-120-002).
// What is read into one is hashed and copied before the next read, so nothing
// keeps it once the leaf is done.
var pfTreeChunks = sync.Pool{New: func() any {
	b := make([]byte, 32<<10)
	return &b
}}

// Classifying a tree in time proportional to its size (ROB-120-002).
//
// roleOf asks two things about a part that used to be answered by searching
// its siblings: where it stands among them, and whether it is the root part of
// the multipart/related it is in. Asked for every part, each search made a
// container of n parts cost n*n — a second of processor time for the 10 000
// parts the default MaxPartCount admits, when the container was a
// multipart/related whose start parameter named none of them. The position is
// now read from the parent's list, and the root part is found once per
// container.

// pfRelatedRoots remembers the root part of each multipart/related asked
// about while one tree is classified. A nil map remembers nothing and asks
// the node every time, which is what roleOf alone does.
type pfRelatedRoots map[*Node]*Node

// of returns rel.RelatedRoot().
func (m pfRelatedRoots) of(rel *Node) *Node {
	if m == nil {
		return rel.RelatedRoot()
	}
	root, ok := m[rel]
	if !ok {
		root = rel.RelatedRoot()
		m[rel] = root
	}
	return root
}

// under reports whether n is the root part of the multipart/related rel, or a
// part of it, as underRoot does.
func (m pfRelatedRoots) under(rel, n *Node) bool {
	root := m.of(rel)
	for c := n; c != nil && c != rel; c = c.Parent {
		if c == root {
			return true
		}
	}
	return false
}

// pfChildAt returns the i-th child of p, or nil when it has none there.
func pfChildAt(p *Node, i int) *Node {
	if i < len(p.Children) {
		return p.Children[i]
	}
	return nil
}
