package engine_test

import (
	"bytes"
	"context"
	"testing"
)

// A deallocate meeting a write staged and not yet in the entry waits it out:
// the write past the extent is committed with it, and the size reflects it.
func TestADeallocateWaitsOutAnAdmittedWrite(t *testing.T) {
	u, h := newHeldUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(50, 'a'))
	u.commit(t, f)
	if err := u.lateRemoval(t, h, f, 40, fill(20, 'w'), func(ctx context.Context) error {
		return u.e.Deallocate(ctx, u.share.ID, f, 0, 10)
	}); err != nil {
		t.Fatal(err)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if n := u.size(t, f); n != 60 {
		t.Errorf("size %d, want 60", n)
	}
}

// Between a deallocate's journal step and its phase 1, a read of the extent
// finds the pending deallocate and reads zeros.
func TestADeallocatedExtentReadsZerosBeforeItsPhase1(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	u.commit(t, f)
	got := make([]byte, 8)
	var n int
	var rerr error
	u.e.OnStaged(func() { n, rerr = u.e.Read(context.Background(), u.share.ID, f, 4, got) })
	if err := u.e.Deallocate(t.Context(), u.share.ID, f, 4, 8); err != nil {
		t.Fatal(err)
	}
	if rerr != nil || n != 8 || !bytes.Equal(got, make([]byte, 8)) {
		t.Errorf("read in the window: %d %q %v; want 8 zeros", n, got, rerr)
	}
}
