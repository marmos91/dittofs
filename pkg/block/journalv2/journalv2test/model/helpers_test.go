package model

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

const (
	shareA journalv2.ShareTag = 1
	shareB journalv2.ShareTag = 2
)

func ver(n uint64) journalv2.Version { return journalv2.Version{Counter: n} }

func fid(n byte) journalv2.FileID { return journalv2.NewFileID([16]byte{15: n}) }

func fill(c byte, n int) []byte { return bytes.Repeat([]byte{c}, n) }

func ext(off, n int64) journalv2.Extent { return journalv2.Extent{Off: off, Len: n} }

// span is one run of a file's held bytes as the model derives them: one
// record's bytes at one version with one offloaded bit.
type span struct {
	Off, Len  int64
	Ver       uint64
	Offloaded bool
}

func heldSpans(t *testing.T, m *Model, id journalv2.FileID) []span {
	t.Helper()
	var out []span
	for _, r := range m.current(id).runs() {
		out = append(out, span{r.ext.Off, r.ext.Len, r.ver.Counter, r.offloaded})
	}
	return out
}

func heldBytes(t *testing.T, m *Model, id journalv2.FileID, off, n int64) []byte {
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

func refused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the model accepted an outcome the RFC forbids")
	}
	t.Logf("refused: %v", err)
}

func equalSpans(t *testing.T, got, want []span) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("held spans:\n got %+v\nwant %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("held spans:\n got %+v\nwant %+v", got, want)
		}
	}
}
