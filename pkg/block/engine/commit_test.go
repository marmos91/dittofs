package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// verifierOf is the verifier a one-byte write of a new file answers.
func (u *unit) verifierOf(t *testing.T) vfserr.Verifier {
	t.Helper()
	v, err := u.e.Write(t.Context(), u.share.ID, entity.NewFileID(), 0, []byte("x"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Writes in one process and binding answer one verifier however far the
// clock moved, and raising any one of its inputs alone answers another.
func TestTheVerifierChangesWithEachInputAndNothingElse(t *testing.T) {
	u := newUnit(t)
	node := entity.NodeID{1}
	u.e.SetPrimary(u.share.ID, node, 1, 1)
	v := u.verifierOf(t)
	u.clock.Advance(time.Hour)
	if got := u.verifierOf(t); got != v {
		t.Fatalf("an hour later the verifier is %x, want %x: the clock is not an input", got, v)
	}
	for _, c := range []struct {
		name  string
		raise func()
	}{
		{"the node", func() { u.e.SetPrimary(u.share.ID, entity.NodeID{2}, 1, 1) }},
		{"the node epoch", func() { u.e.SetPrimary(u.share.ID, node, 2, 1) }},
		{"the incarnation", func() { u.e.SetPrimary(u.share.ID, node, 1, 2) }},
		{"the process instance", u.e.NewInstance},
		{"the loss generation", func() {
			f := entity.NewFileID()
			u.write(t, f, 0, []byte("lost"))
			u.lose(f, 0, 4)
		}},
	} {
		u.e.SetPrimary(u.share.ID, node, 1, 1) // the others at their base
		base := u.verifierOf(t)
		c.raise()
		if got := u.verifierOf(t); got == base {
			t.Errorf("with %s raised alone the verifier is unchanged, %x", c.name, got)
		}
	}
	// Each input has its own place in the derivation: two that trade a step
	// between them answer two verifiers.
	u.e.SetPrimary(u.share.ID, node, 2, 1)
	a := u.verifierOf(t)
	u.e.SetPrimary(u.share.ID, node, 1, 2)
	if b := u.verifierOf(t); a == b {
		t.Errorf("node epoch 2 with incarnation 1 and the reverse answer one verifier, %x", a)
	}
}

// loseDuring is a handle whose WriteAt runs *lose, when set, once the write
// is staged, before the engine answers it: a test sets it to raise the
// journal's loss generation.
type loseDuring struct {
	engine.Handle
	lose *func()
}

func (h loseDuring) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	v, err := h.Handle.WriteAt(ctx, id, off, p, mtime)
	if *h.lose != nil {
		(*h.lose)()
	}
	return v, err
}

// A write's verifier is sampled before it is staged: a loss between the
// staging and the answer leaves that write's verifier and changes the next.
func TestALossDuringAWriteShowsInTheNextVerifier(t *testing.T) {
	var lose func()
	u := newUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle { return loseDuring{h, &lose} }})
	before := u.verifierOf(t)
	f := entity.NewFileID()
	u.write(t, f, 0, []byte("held"))
	lose = func() { u.lose(f, 0, 4) }
	if got := u.verifierOf(t); got != before {
		t.Errorf("the write a loss followed answered %x, want the %x sampled before it", got, before)
	}
	lose = nil
	if got := u.verifierOf(t); got == before {
		t.Errorf("the write after the loss answered the verifier from before it, %x", got)
	}
}
