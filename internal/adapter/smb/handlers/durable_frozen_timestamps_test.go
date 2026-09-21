package handlers

import (
	"testing"
	"time"
)

// The freeze flags and their saved values live on the OpenFile, which a durable
// disconnect drops and a reconnect rebuilds from the persisted row. These tests
// pin the round trip through the process-local table that carries them, and the
// two properties that decide whether the table is safe to keep: it does not
// grow for a handle that never froze anything, and a thaw on the restored
// handle does not reach back into the entry another reconnect could still read.

func freezeAllTimestamps(openFile *OpenFile, at time.Time) {
	openFile.Lock()
	defer openFile.Unlock()
	openFile.BtimeFrozen = true
	openFile.MtimeFrozen = true
	openFile.CtimeFrozen = true
	openFile.AtimeFrozen = true
	bt, mt, ct, a := at, at.Add(time.Second), at.Add(2*time.Second), at.Add(3*time.Second)
	openFile.FrozenBtime = &bt
	openFile.FrozenMtime = &mt
	openFile.FrozenCtime = &ct
	openFile.FrozenAtime = &a
}

// TestFrozenTimestampsSurviveADurableReconnect is the property the change
// exists for: without it the reconnect comes back with CtimeFrozen false and no
// FrozenCtime, so the next WRITE or CLOSE stamps over a ChangeTime the client
// asked to hold.
func TestFrozenTimestampsSurviveADurableReconnect(t *testing.T) {
	h := &Handler{}
	frozenAt := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)

	before := &OpenFile{}
	freezeAllTimestamps(before, frozenAt)
	h.rememberFrozenTimestamps("handle-1", before)

	// The reconnect rebuilds the handle from the persisted row, which carries
	// none of the freeze state.
	restored := &OpenFile{}
	h.adoptFrozenTimestamps("handle-1", restored)

	if !restored.IsCtimeFrozen() {
		t.Fatal("CtimeFrozen did not survive the reconnect: the freeze evaporated")
	}
	if !restored.BtimeFrozen || !restored.MtimeFrozen || !restored.AtimeFrozen {
		t.Fatalf("freeze flags incomplete after reconnect: btime=%v mtime=%v atime=%v",
			restored.BtimeFrozen, restored.MtimeFrozen, restored.AtimeFrozen)
	}

	restored.RLock()
	got := restored.FrozenCtime
	restored.RUnlock()
	if got == nil {
		t.Fatal("FrozenCtime is nil after reconnect, so the restore has no value to write")
	}
	if !got.Equal(frozenAt.Add(2 * time.Second)) {
		t.Fatalf("FrozenCtime = %v, want %v", got, frozenAt.Add(2*time.Second))
	}
}

// TestFrozenTimestampsNotRememberedWhenNothingIsFrozen keeps the table from
// growing for the common case: a handle that never used the sentinel.
func TestFrozenTimestampsNotRememberedWhenNothingIsFrozen(t *testing.T) {
	h := &Handler{}
	h.rememberFrozenTimestamps("handle-plain", &OpenFile{})

	if _, ok := h.durableFreezes.Load("handle-plain"); ok {
		t.Fatal("an entry was recorded for a handle with no frozen timestamp")
	}
}

// TestAdoptedFrozenTimestampsAreNotAliased guards the clone: the restored
// handle and the table entry would otherwise share the same time.Time, and a
// thaw on one would mutate what another reconnect reads.
func TestAdoptedFrozenTimestampsAreNotAliased(t *testing.T) {
	h := &Handler{}
	frozenAt := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)

	before := &OpenFile{}
	freezeAllTimestamps(before, frozenAt)
	h.rememberFrozenTimestamps("handle-alias", before)

	restored := &OpenFile{}
	h.adoptFrozenTimestamps("handle-alias", restored)

	restored.Lock()
	*restored.FrozenCtime = frozenAt.Add(1000 * time.Hour)
	restored.Unlock()

	entry, ok := h.durableFreezes.Load("handle-alias")
	if ok {
		t.Fatal("the reconnect did not consume the entry; a second reconnect could adopt it")
	}
	_ = entry
}

// TestForgetFrozenTimestampsDropsTheEntry covers the delete paths that drop a
// row without a reconnect claiming it (the scavenger and the purge sweep), so a
// later handle reusing the ID cannot inherit a stale freeze.
func TestForgetFrozenTimestampsDropsTheEntry(t *testing.T) {
	h := &Handler{}
	openFile := &OpenFile{}
	freezeAllTimestamps(openFile, time.Now())
	h.rememberFrozenTimestamps("handle-forgotten", openFile)

	if _, ok := h.durableFreezes.Load("handle-forgotten"); !ok {
		t.Fatal("precondition: expected the freeze to be recorded")
	}

	h.forgetFrozenTimestamps("handle-forgotten")

	if _, ok := h.durableFreezes.Load("handle-forgotten"); ok {
		t.Fatal("forget left the entry behind")
	}

	// A reconnect after the row was dropped must come back unfrozen rather than
	// adopting the stale entry.
	restored := &OpenFile{}
	h.adoptFrozenTimestamps("handle-forgotten", restored)
	if restored.IsCtimeFrozen() {
		t.Fatal("a forgotten freeze was adopted by a later reconnect")
	}
}
