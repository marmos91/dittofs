package handlers

import "time"

// durableFrozenTimestamps carries the SET_INFO -1 timestamp freezes a handle
// held across a durable disconnect.
//
// The freeze flags and their saved values live on the OpenFile, and a durable
// disconnect drops the OpenFile: the handle moves into the DurableHandleStore
// and is rebuilt from the persisted row on reconnect. None of the eight fields
// is part of that row, so a reconnect came back with CtimeFrozen false and no
// FrozenCtime — the freeze silently evaporated, and the next WRITE or CLOSE
// stamped over a ChangeTime the client had asked to hold.
//
// This is deliberately not a field on PersistedDurableHandle. That struct is
// persisted by every backend — memory by struct copy, badger through JSON,
// postgres through explicit columns — so carrying the freeze there would write
// it to disk and let it survive a server restart. A freeze is per-open-handle
// state (MS-FSA 2.1.5.15.2) and must not: a reconnect to the same live server
// keeps it, a restart drops it. A process-local side table is the shape that
// says exactly that, and it needs no migration on any backend.
//
// Entries are keyed by the durable handle's ID, which is stable across the
// disconnect — buildPersistedDurableHandle mints it and validateAndRestore
// consumes the same row. The reconnect is the one consumer; the scavenger and
// purgeOneDisconnectedHandle forget the entry alongside the row they delete.
//
// ponytail: validateAndRestore also deletes a row (an expired handle, or a file
// that no longer exists) without forgetting, because it and its two callers are
// free functions holding no *Handler. That leaks one entry per expired reconnect
// attempt until the process restarts; an ID is a UUID and an entry is four bools
// plus four pointers, so growth is bounded by reconnect attempts rather than by
// clients. Thread the handler or a forget callback through those signatures only
// if that ever shows up in memory.
type durableFrozenTimestamps struct {
	btimeFrozen bool
	mtimeFrozen bool
	ctimeFrozen bool
	atimeFrozen bool
	frozenBtime *time.Time
	frozenMtime *time.Time
	frozenCtime *time.Time
	frozenAtime *time.Time
}

// rememberFrozenTimestamps records the timestamp freezes openFile holds, to be
// restored if the handle reconnects under handleID.
//
// Reads under openFile.mu (read), the same lock the SET_INFO freeze/thaw path
// writes these fields under, so a concurrent freeze on the same handle cannot
// tear the snapshot.
//
// Stores nothing when no timestamp is frozen, so a handle that never used the
// sentinel — the common case — leaves no entry behind.
func (h *Handler) rememberFrozenTimestamps(handleID string, openFile *OpenFile) {
	openFile.mu.RLock()
	snap := durableFrozenTimestamps{
		btimeFrozen: openFile.BtimeFrozen,
		mtimeFrozen: openFile.MtimeFrozen,
		ctimeFrozen: openFile.CtimeFrozen,
		atimeFrozen: openFile.AtimeFrozen,
	}
	if openFile.FrozenBtime != nil {
		v := *openFile.FrozenBtime
		snap.frozenBtime = &v
	}
	if openFile.FrozenMtime != nil {
		v := *openFile.FrozenMtime
		snap.frozenMtime = &v
	}
	if openFile.FrozenCtime != nil {
		v := *openFile.FrozenCtime
		snap.frozenCtime = &v
	}
	if openFile.FrozenAtime != nil {
		v := *openFile.FrozenAtime
		snap.frozenAtime = &v
	}
	openFile.mu.RUnlock()

	if !snap.btimeFrozen && !snap.mtimeFrozen && !snap.ctimeFrozen && !snap.atimeFrozen {
		return
	}
	h.durableFreezes.Store(handleID, snap)
}

// adoptFrozenTimestamps puts the freezes remembered for handleID onto the
// freshly rebuilt openFile, and drops the entry — a reconnect is the one
// consume of this state, the way ConsumeDurableHandle is the one consume of
// the row.
//
// The pointers are copied rather than shared: the restored OpenFile and the
// entry would otherwise alias the same time.Time, and a later thaw on the
// handle would mutate the snapshot of an entry another reconnect could still
// read.
//
// Writes under openFile.mu (write) for the same reason remember reads under it.
func (h *Handler) adoptFrozenTimestamps(handleID string, openFile *OpenFile) {
	loaded, ok := h.durableFreezes.LoadAndDelete(handleID)
	if !ok {
		return
	}
	snap := loaded.(durableFrozenTimestamps)

	openFile.mu.Lock()
	defer openFile.mu.Unlock()
	openFile.BtimeFrozen = snap.btimeFrozen
	openFile.MtimeFrozen = snap.mtimeFrozen
	openFile.CtimeFrozen = snap.ctimeFrozen
	openFile.AtimeFrozen = snap.atimeFrozen
	openFile.FrozenBtime = cloneTime(snap.frozenBtime)
	openFile.FrozenMtime = cloneTime(snap.frozenMtime)
	openFile.FrozenCtime = cloneTime(snap.frozenCtime)
	openFile.FrozenAtime = cloneTime(snap.frozenAtime)
}

// forgetFrozenTimestamps drops any freeze remembered for handleID. Called
// wherever a persisted row is deleted without a reconnect claiming it — the
// scavenger, the purge paths, an expired or unresolvable reconnect — so the
// table stays in step with the rows it mirrors.
func (h *Handler) forgetFrozenTimestamps(handleID string) {
	h.durableFreezes.Delete(handleID)
}

// cloneTime returns a copy of t, or nil when t is nil.
func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
