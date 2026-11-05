package model

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

const (
	shareA journal.ShareTag = 1
	shareB journal.ShareTag = 2
)

func ver(n uint64) journal.Version { return journal.Version{Counter: n} }

func fid(n byte) journal.FileID { return journal.NewFileID([16]byte{15: n}) }

func fill(c byte, n int) []byte { return bytes.Repeat([]byte{c}, n) }

func ext(off, n int64) journal.Extent { return journal.Extent{Off: off, Len: n} }

// span is one run of a file's held bytes as the model derives them: one
// record's bytes at one version with one offloaded bit.
type span struct {
	Off, Len  int64
	Ver       uint64
	Offloaded bool
}

func heldSpans(t *testing.T, m *Model, id journal.FileID) []span {
	t.Helper()
	var out []span
	for _, r := range m.current(id).runs() {
		out = append(out, span{r.ext.Off, r.ext.Len, r.ver.Counter, r.offloaded})
	}
	return out
}

func heldBytes(t *testing.T, m *Model, id journal.FileID, off, n int64) []byte {
	t.Helper()
	st := m.current(id)
	out := make([]byte, n)
	for i := range n {
		c, ok := st.held[off+i]
		if !ok {
			t.Fatalf("byte %d is not held", off+i)
		}
		out[i] = c.b
	}
	return out
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// diverged asserts the model reported the journal doing what the RFC does not
// allow.
func diverged(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("want the journal reported wrong, got %v", err)
	}
	t.Logf("refused: %v", err)
}

// misused asserts the model reported a call it cannot take.
func misused(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrMisuse) {
		t.Fatalf("want the call reported as one the model cannot take, got %v", err)
	}
	t.Logf("refused: %v", err)
}

func equalSpans(t *testing.T, got, want []span) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("held spans:\n got %v\nwant %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("held spans:\n got %v\nwant %v", got, want)
		}
	}
}

// markers returns the live markers of file id above from as the changes
// Since names them, in version order.
func markers(t *testing.T, m *Model, id journal.FileID, from journal.Version) []journal.Change {
	t.Helper()
	var out []journal.Change
	for _, r := range m.current(id).markers {
		if r.ver.Compare(from) > 0 {
			out = append(out, journal.Change{Kind: changeKind(r.kind), Extent: r.ext, Version: r.ver})
		}
	}
	slices.SortStableFunc(out, func(a, b journal.Change) int { return a.Version.Compare(b.Version) })
	return out
}

// String prints a span as [off,end) v1, with "offloaded" when its bit is set.
func (s span) String() string {
	out := fmt.Sprintf("%s v%d", fmtExtent(ext(s.Off, s.Len)), s.Ver)
	if s.Offloaded {
		out += " offloaded"
	}
	return out
}

// fmtChange prints a change as Since names it: write [0,4) @v1 mtime 7.
func fmtChange(c journal.Change) string {
	kind := map[journal.ChangeKind]string{
		journal.ChangeWrite:       "write",
		journal.ChangeTruncate:    "truncate",
		journal.ChangeDeallocate:  "deallocate",
		journal.ChangeDelete:      "delete",
		journal.ChangeCloneTarget: "clone target",
		journal.ChangeLoss:        "loss",
	}[c.Kind]
	out := fmt.Sprintf("%s %s @%s", kind, fmtExtent(c.Extent), fmtVersion(c.Version))
	if c.Mtime != 0 {
		out += fmt.Sprintf(" mtime %d", c.Mtime)
	}
	return out
}

func fmtChanges(cs []journal.Change) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = fmtChange(c)
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// fmtFile prints a file the tests built with fid by its number.
func fmtFile(id journal.FileID) string { return fmt.Sprintf("file %d", id.Bytes()[15]) }

func fmtFiles(ids []journal.FileID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmtFile(id)
	}
	return "[" + strings.Join(out, ", ") + "]"
}
