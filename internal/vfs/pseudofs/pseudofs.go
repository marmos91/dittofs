// Package pseudofs is the NFSv4 pseudo-filesystem: the directories above
// the shares, a function of the share list alone. Every handle, numeric id
// and listing cursor is a digest of the installation and the path, and every
// change attribute is the share list's version, so every node builds the same
// tree from the same list, before and after a restart, and keeps no counter.
package pseudofs

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"strings"
)

// Kind is the first byte of a pseudo directory's handle: its own kind, which
// no file handle of the metadata store starts with.
const Kind = 2

// handleLen is a pseudo handle's length: its kind, then its digest.
const handleLen = 1 + sha256.Size

// Share is one share as the tree places it.
type Share struct {
	ID   [16]byte
	Path string // absolute and normalised, never "/"
}

// Tree is the pseudo-filesystem one share list builds. It is never changed:
// a new list builds a new tree.
type Tree struct {
	version uint64
	root    *Node
	nodes   map[[sha256.Size]byte]*Node
}

// Node is one directory of the tree: the root, a directory leading to
// shares, or the place a share is mounted, which a lookup crosses into the
// share's own root.
type Node struct {
	Path     string
	Name     string
	Parent   *Node      // the root's is itself
	Children []*Node    // in cursor order
	Share    *[16]byte  // the share mounted here; nil for a directory leading to shares
	Shares   [][16]byte // every share at or below this node
	digest   [sha256.Size]byte
}

// Build is the tree of shares, as installation install names it at share
// list version v.
func Build(install [16]byte, v uint64, shares []Share) *Tree {
	t := &Tree{version: v, nodes: map[[sha256.Size]byte]*Node{}}
	t.root = t.node(install, nil, "/", "")
	for _, s := range shares {
		n := t.root
		parts := strings.Split(s.Path[1:], "/")
		for i, name := range parts {
			c := n.Child(name)
			if c == nil {
				c = t.node(install, n, "/"+strings.Join(parts[:i+1], "/"), name)
				n.Children = append(n.Children, c)
			}
			n = c
			n.Shares = append(n.Shares, s.ID)
		}
		n.Share = &s.ID
		t.root.Shares = append(t.root.Shares, s.ID)
	}
	for _, n := range t.nodes {
		slices.SortFunc(n.Children, func(a, b *Node) int { return cmp.Compare(a.Cursor(), b.Cursor()) })
	}
	return t
}

func (t *Tree) node(install [16]byte, parent *Node, path, name string) *Node {
	n := &Node{Path: path, Name: name, Parent: parent, digest: sha256.Sum256(append(install[:], path...))}
	if parent == nil {
		n.Parent = n
	}
	t.nodes[n.digest] = n
	return n
}

// Version is the share list version the tree was built at: every node's
// change attribute.
func (t *Tree) Version() uint64 { return t.version }

// Root is the pseudo-root.
func (t *Tree) Root() *Node { return t.root }

// Node is the node a pseudo handle names; false when h is not one, or names
// a path that leads to no share in this tree.
func (t *Tree) Node(h []byte) (*Node, bool) {
	if !IsHandle(h) {
		return nil, false
	}
	n, ok := t.nodes[[sha256.Size]byte(h[1:])]
	return n, ok
}

// IsHandle reports whether h has a pseudo handle's form; whether it names a
// node is the tree's answer.
func IsHandle(h []byte) bool { return len(h) == handleLen && h[0] == Kind }

// Handle is the node's handle.
func (n *Node) Handle() []byte { return append([]byte{Kind}, n.digest[:]...) }

// ID is the node's 16-byte file ID.
func (n *Node) ID() [16]byte { return [16]byte(n.digest[:16]) }

// Number is the node's numeric file id: its top bit set, so never zero.
func (n *Node) Number() uint64 { return binary.BigEndian.Uint64(n.digest[16:24]) | 1<<63 }

// Cursor is where a listing of the node's parent resumes after it: 63 bits,
// top bit clear, never one of the reserved 0 to 2.
func (n *Node) Cursor() uint64 { return max(binary.BigEndian.Uint64(n.digest[24:])>>1, 3) }

// Child is the child named name; nil for none.
func (n *Node) Child(name string) *Node {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}
