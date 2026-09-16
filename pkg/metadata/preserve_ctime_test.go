package metadata_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupPreserveCtimeFile wires a service over a memory store with one share and
// one regular file, and returns the service, a root auth context, the file's
// handle, and the share root's handle.
func setupPreserveCtimeFile(t *testing.T) (*metadata.Service, *metadata.AuthContext, metadata.FileHandle, metadata.FileHandle) {
	t.Helper()
	const share = "/pc"
	store := memory.NewMemoryMetadataStoreWithDefaults()
	rootFile, err := store.CreateRootDirectory(context.Background(), share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o777,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeShareHandle(share, rootFile.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle: %v", err)
	}
	svc := metadata.New()
	if err := svc.RegisterStoreForShare(share, store); err != nil {
		t.Fatalf("RegisterStoreForShare: %v", err)
	}
	ctx := &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0), GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
	file, _, err := svc.CreateFile(ctx, rootHandle, "f", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	return svc, ctx, handle, rootHandle
}

// A PreserveCtime write must carry the row's current ChangeTime forward, not the
// snapshot SetFileAttributes read before its transaction opened. Otherwise a
// writer that advances ChangeTime in between is silently reverted — the
// backwards move NFSv4's change attribute must never make.
//
// The interleaving is a real race, so this drives it rather than staging it: the
// assertion only fires when the revert actually happens, so the test can fail
// only in the presence of the defect, never because the window was missed.
func TestSetFileAttributes_PreserveCtimeDoesNotRevertAConcurrentAdvance(t *testing.T) {
	svc, ctx, handle, _ := setupPreserveCtimeFile(t)

	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	const rounds = 300

	// Seed the stored Ctime to base. Without this it starts at the file's
	// creation time — now, years after base — and every floor derived from base
	// sits below it, so a stale write reverting to that creation time lands
	// *above* the floor and the assertion below never fires. The test would pass
	// on a build that reverts ChangeTime on every single round.
	if _, err := svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{Ctime: &base}); err != nil {
		t.Fatalf("seeding Ctime: %v", err)
	}
	if seeded, err := svc.GetFile(context.Background(), handle); err != nil {
		t.Fatalf("reading back the seed: %v", err)
	} else if !seeded.Ctime.Equal(base) {
		t.Fatalf("seed did not land: Ctime=%v, want %v — every floor below would be vacuous",
			seeded.Ctime.UTC(), base.UTC())
	}

	for i := range rounds {
		advanced := base.Add(time.Duration(i+1) * time.Hour)

		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		// Both writers' errors are kept. Discarding them lets a round where the
		// PreserveCtime write failed outright still reach the assertion below
		// and satisfy it on the other writer alone — a green round that tested
		// nothing, which is exactly the shape this test exists to rule out.
		var preserveErr, advanceErr error

		go func() {
			defer wg.Done()
			<-start
			atime := base.Add(time.Duration(i+1) * time.Minute)
			_, preserveErr = svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{
				Atime: &atime, PreserveCtime: true,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, advanceErr = svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{Ctime: &advanced})
		}()

		close(start)
		wg.Wait()

		if preserveErr != nil {
			t.Fatalf("round %d: the PreserveCtime write failed, so this round proves nothing: %v", i, preserveErr)
		}
		if advanceErr != nil {
			t.Fatalf("round %d: the Ctime advance failed, so this round proves nothing: %v", i, advanceErr)
		}

		got, err := svc.GetFile(context.Background(), handle)
		if err != nil {
			t.Fatalf("round %d: GetFile: %v", i, err)
		}
		// The floor is this round's own advance, not the previous round's. Both
		// writers have RETURNED by the wg.Wait above, so `advanced` is committed
		// — there is no "may not have landed yet" left to allow for. Asserting
		// against the previous round's value instead would accept exactly the
		// defect: a stale PreserveCtime write landing after the advance reverts
		// the stored value to the earlier one, which clears a floor set one
		// round back and clears every round-0 check outright.
		if got.Ctime.Before(advanced) {
			t.Fatalf("round %d: ChangeTime is %v, below the %v this round already committed — "+
				"the PreserveCtime write reverted a peer's advance",
				i, got.Ctime.UTC(), advanced.UTC())
		}
	}
}

// PreserveCtime must leave ChangeTime alone while still landing the change that
// carried it — the sequential half of the guarantee.
func TestSetFileAttributes_PreserveCtimeHoldsTheStoredValue(t *testing.T) {
	svc, ctx, handle, _ := setupPreserveCtimeFile(t)

	pinned := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{Ctime: &pinned}); err != nil {
		t.Fatalf("seed ctime: %v", err)
	}

	atime := time.Date(2024, 7, 8, 9, 10, 11, 0, time.UTC)
	if _, err := svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{
		Atime: &atime, PreserveCtime: true,
	}); err != nil {
		t.Fatalf("atime bump: %v", err)
	}

	got, err := svc.GetFile(context.Background(), handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !got.Ctime.Equal(pinned) {
		t.Errorf("ChangeTime = %v; want the held %v", got.Ctime.UTC(), pinned.UTC())
	}
	if !got.Atime.Equal(atime) {
		t.Errorf("LastAccessTime = %v; want %v — the change itself must still land",
			got.Atime.UTC(), atime.UTC())
	}
}

// A PreserveCtime write on a DIRECTORY must not drop the directory's pending
// timestamp bump. Creates and removes coalesce the parent's mtime/ctime/atime
// in the DirTimesTracker rather than writing the row each time, and the read
// overlay is what makes those bumps visible. The in-transaction re-read that
// holds ChangeTime forward reads the durable row, which does not carry them —
// so assigning it outright reverted the pending advance, and the tracker Clear
// that follows the write then discarded it for good. The observable result is
// the move this whole mechanism exists to prevent: a peer's ChangeTime going
// backwards.
func TestPreserveCtime_DoesNotDropAPendingDirectoryBump(t *testing.T) {
	svc, ctx, _, rootHandle := setupPreserveCtimeFile(t)

	// The file created by the fixture already bumped the root's pending times;
	// one more makes the advance unambiguous.
	if _, _, err := svc.CreateFile(ctx, rootHandle, "g", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	before, err := svc.GetFile(ctx.Context, rootHandle)
	if err != nil || before == nil {
		t.Fatalf("GetFile(root) before: %v", err)
	}

	// An explicit directory-time set is what clears the pending overlay, and it
	// is exactly what an SMB frozen-timestamp restore sends: mtime/atime written
	// deliberately, ChangeTime held. Without the set, the bump stays pending and
	// the read overlay hides the problem.
	frozenMtime := before.Mtime.Add(-time.Hour)
	if _, err := svc.SetFileAttributes(ctx, rootHandle, &metadata.SetAttrs{
		Mtime: &frozenMtime, PreserveCtime: true,
	}); err != nil {
		t.Fatalf("SetFileAttributes(PreserveCtime) on the directory: %v", err)
	}

	after, err := svc.GetFile(ctx.Context, rootHandle)
	if err != nil || after == nil {
		t.Fatalf("GetFile(root) after: %v", err)
	}
	if after.Ctime.Before(before.Ctime) {
		t.Errorf("directory ChangeTime moved backwards across a PreserveCtime write: "+
			"before=%s after=%s — the pending bump was replaced by the durable row and then cleared",
			before.Ctime.Format(time.RFC3339Nano), after.Ctime.Format(time.RFC3339Nano))
	}
}

// A truncate must honour PreserveCtime like every other branch. The size branch
// stamps ChangeTime because POSIX says a truncate changes it, but a caller that
// asked for the stored value to be left alone has said the opposite — and an
// SMB handle with a frozen ChangeTime sends exactly that pairing on a
// SET_INFO EndOfFile. Held forward-only, this call's own `now` stamp is later
// than anything it could be compared against, so it would win every time.
func TestPreserveCtime_IsHonouredOnTruncate(t *testing.T) {
	svc, ctx, handle, _ := setupPreserveCtimeFile(t)

	// Push the modify time clearly into the past first. Asserting that it
	// "advanced" against a value this test just created compares two clock reads
	// taken inside one tick — true on a fine-grained clock, false on Windows,
	// and about the clock either way rather than about the behaviour.
	past := time.Now().Add(-time.Hour)
	if _, err := svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{Mtime: &past}); err != nil {
		t.Fatalf("seed the modify time: %v", err)
	}

	before, err := svc.GetFile(ctx.Context, handle)
	if err != nil || before == nil {
		t.Fatalf("GetFile before: %v", err)
	}

	size := uint64(4096)
	if _, err := svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{
		Size: &size, PreserveCtime: true,
	}); err != nil {
		t.Fatalf("SetFileAttributes(Size, PreserveCtime): %v", err)
	}

	after, err := svc.GetFile(ctx.Context, handle)
	if err != nil || after == nil {
		t.Fatalf("GetFile after: %v", err)
	}
	if !after.Ctime.Equal(before.Ctime) {
		t.Errorf("truncate stamped ChangeTime despite PreserveCtime: before=%s after=%s",
			before.Ctime.Format(time.RFC3339Nano), after.Ctime.Format(time.RFC3339Nano))
	}
	if after.Size != size {
		t.Errorf("Size=%d, want %d: the truncate itself must still apply", after.Size, size)
	}
	if !after.Mtime.After(before.Mtime) {
		t.Errorf("Mtime did not advance: PreserveCtime holds the change time, not the modify time")
	}
}

// A held change time must not undo a peer's deliberate LOWERING of it. SMB's
// restoreFrozenTimestamps writes an explicitly older ChangeTime, and a
// PreserveCtime write that had read a higher value before it must leave that
// restore standing rather than carrying its own snapshot forward. The stored
// value is the authority — that is what "hold it" means, in both directions.
func TestPreserveCtime_DoesNotUndoADeliberateLowering(t *testing.T) {
	svc, ctx, handle, _ := setupPreserveCtimeFile(t)

	// A peer restores an explicitly older ChangeTime, the way a frozen-timestamp
	// restore does.
	restored := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{Ctime: &restored}); err != nil {
		t.Fatalf("seed the lowered ChangeTime: %v", err)
	}

	// A PreserveCtime write lands afterwards and must leave it alone.
	atime := time.Now().Add(time.Minute)
	if _, err := svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{
		Atime: &atime, PreserveCtime: true,
	}); err != nil {
		t.Fatalf("SetFileAttributes(Atime, PreserveCtime): %v", err)
	}

	got, err := svc.GetFile(ctx.Context, handle)
	if err != nil || got == nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !got.Ctime.Equal(restored) {
		t.Errorf("ChangeTime is %s, want the restored %s — the held write carried its own "+
			"snapshot forward and undid a peer's deliberate lowering",
			got.Ctime.UTC().Format(time.RFC3339Nano), restored.UTC().Format(time.RFC3339Nano))
	}
}
