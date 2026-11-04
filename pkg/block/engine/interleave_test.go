package engine_test

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A read of a truncated extent landing between the journal's Truncate and
// the insert of the truncate's pending operation waits on the file's entry
// lock and then finds the pending truncate: it reads short, never lost. A
// build releasing that lock before the insert lets the read return inside
// the journal's call, missing the extent with no pending removal.
func TestAReadBetweenATruncateAndItsInsertIsNeverLost(t *testing.T) {
	u, h := newHookedUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	type result struct {
		n   int
		err error
	}
	read := make(chan result, 1)
	h.truncate = func() {
		go func() {
			p := make([]byte, 8)
			n, err := u.e.Read(context.Background(), u.share.ID, f, 4, p)
			read <- result{n, err}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for u.e.FileStateWaiting() == 0 && len(read) == 0 {
			if time.Now().After(deadline) {
				t.Error("the read neither returned nor waited on the file's lock")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	r := <-read
	if errors.Is(r.err, vfserr.ErrLost) || r.n != 0 {
		t.Errorf("the read in between: %d bytes, %v; want a short read past the truncate", r.n, r.err)
	}
}

// Writes, truncates, deallocates and commits, interleaved at random with the
// group commit running on its own, leave the file reading what their
// version order says.
func TestRemovalsInterleavedWithWritesReadTheirOrder(t *testing.T) {
	u := newUnit(t)
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Nanosecond, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	f := u.create(t, "a")
	rng := rand.New(rand.NewPCG(1, 2))
	var model []byte
	const span = 64
	for i := range 300 {
		ctx := t.Context()
		switch k := rng.IntN(10); {
		case k < 5:
			off, n := rng.Int64N(span), 1+rng.Int64N(16)
			p := bytes.Repeat([]byte{byte('a' + i%26)}, int(n))
			u.write(t, f, off, p)
			if end := int(off + n); end > len(model) {
				model = append(model, make([]byte, end-len(model))...)
			}
			copy(model[off:], p)
		case k < 7:
			size := rng.Int64N(span + 16)
			if err := u.e.Truncate(ctx, u.share.ID, f, size); err != nil {
				t.Fatalf("op %d: truncate to %d: %v", i, size, err)
			}
			if int(size) <= len(model) {
				model = model[:size]
			} else {
				model = append(model, make([]byte, int(size)-len(model))...)
			}
		case k < 9:
			off, n := rng.Int64N(span), 1+rng.Int64N(16)
			if err := u.e.Deallocate(ctx, u.share.ID, f, off, n); err != nil {
				t.Fatalf("op %d: deallocate [%d, %d): %v", i, off, off+n, err)
			}
			for j := off; j < off+n && int(j) < len(model); j++ {
				model[j] = 0
			}
		default:
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := u.e.CommitNow(cctx, u.share.ID, f)
			cancel()
			if err != nil {
				t.Fatalf("op %d: commit: %v", i, err)
			}
		}
		u.clock.Advance(time.Millisecond)
		got := make([]byte, span+32)
		n, err := u.e.Read(ctx, u.share.ID, f, 0, got)
		if err != nil {
			t.Fatalf("op %d: read: %v", i, err)
		}
		if !bytes.Equal(got[:n], model) {
			t.Fatalf("op %d: read %q, want %q", i, got[:n], model)
		}
	}
}
