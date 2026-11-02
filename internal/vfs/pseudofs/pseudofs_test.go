package pseudofs

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

var (
	install = [16]byte{1, 2, 3}
	a, b, c = [16]byte{0xa}, [16]byte{0xb}, [16]byte{0xc}
	list    = []Share{{a, "/home/alice"}, {b, "/home/bob"}, {c, "/photos"}}
)

// walk visits every node of t in tree order.
func walk(t *Tree) []*Node {
	var out []*Node
	var visit func(n *Node)
	visit = func(n *Node) {
		out = append(out, n)
		for _, c := range n.Children {
			visit(c)
		}
	}
	visit(t.Root())
	return out
}

// Two trees built from one share list, in any order, are identical: every
// handle, numeric id and cursor, in the same order.
func TestOneListBuildsOneTree(t *testing.T) {
	x := walk(Build(install, 7, list))
	y := walk(Build(install, 7, []Share{list[2], list[1], list[0]}))
	if len(x) != 5 || len(x) != len(y) {
		t.Fatalf("%d and %d nodes, want 5", len(x), len(y))
	}
	for i := range x {
		if x[i].Path != y[i].Path || !bytes.Equal(x[i].Handle(), y[i].Handle()) ||
			x[i].Number() != y[i].Number() || x[i].Cursor() != y[i].Cursor() {
			t.Errorf("node %d: %s %x %d %d against %s %x %d %d", i,
				x[i].Path, x[i].Handle(), x[i].Number(), x[i].Cursor(), y[i].Path, y[i].Handle(), y[i].Number(), y[i].Cursor())
		}
	}
	other := walk(Build([16]byte{9}, 7, list))
	if bytes.Equal(x[0].Handle(), other[0].Handle()) || x[0].Number() == other[0].Number() {
		t.Error("two installations mint the same pseudo-root")
	}
}

// A share added raises the version every node reports, and keeps every
// handle the old tree minted; a handle whose path leads to no share any more
// names nothing.
func TestAShareAddedKeepsHandlesAndRaisesTheVersion(t *testing.T) {
	old := Build(install, 7, list)
	grown := Build(install, 8, append([]Share{{[16]byte{0xd}, "/home/carol"}}, list...))
	if grown.Version() <= old.Version() {
		t.Errorf("version %d after %d", grown.Version(), old.Version())
	}
	for _, n := range walk(old) {
		if m, ok := grown.Node(n.Handle()); !ok || m.Path != n.Path {
			t.Errorf("%s's handle names %v in the grown tree", n.Path, m)
		}
	}
	if len(grown.Root().Child("home").Children) != 3 {
		t.Error("the added share is not under /home")
	}
	shrunk := Build(install, 9, list[2:])
	if _, ok := shrunk.Node(old.Root().Child("home").Handle()); ok {
		t.Error("/home names a node with no share below it")
	}
}

// The tree places each share under its path, lists children in cursor
// order, and records which shares lie below each node.
func TestTheTreeIsTheShareList(t *testing.T) {
	tr := Build(install, 1, list)
	root := tr.Root()
	if root.Parent != root || root.Path != "/" || root.Share != nil || len(root.Shares) != 3 {
		t.Errorf("root: %+v", root)
	}
	home := root.Child("home")
	if home == nil || home.Share != nil || len(home.Shares) != 2 || home.Parent != root {
		t.Fatalf("/home: %+v", home)
	}
	if bob := home.Child("bob"); bob == nil || bob.Share == nil || *bob.Share != b || bob.Path != "/home/bob" {
		t.Errorf("/home/bob: %+v", bob)
	}
	for _, n := range walk(tr) {
		if n.Number()>>63 != 1 || n.Cursor() < 3 || n.Cursor()>>63 != 0 {
			t.Errorf("%s: number %#x, cursor %#x", n.Path, n.Number(), n.Cursor())
		}
		for i := 1; i < len(n.Children); i++ {
			if n.Children[i-1].Cursor() >= n.Children[i].Cursor() {
				t.Errorf("%s's children out of cursor order", n.Path)
			}
		}
		if got, ok := tr.Node(n.Handle()); !ok || got != n {
			t.Errorf("%s's handle names %v", n.Path, got)
		}
	}
}

// A pseudo handle is no file handle, and a file handle no pseudo handle.
func TestAPseudoHandleIsItsOwnKind(t *testing.T) {
	h := Build(install, 1, list).Root().Handle()
	if _, _, err := entity.Handle(h).Parse(); err == nil {
		t.Error("a pseudo handle parses as a file handle")
	}
	if IsHandle(entity.NewHandle(entity.ShareID(a), entity.FileID(b))) {
		t.Error("a file handle has a pseudo handle's form")
	}
}
