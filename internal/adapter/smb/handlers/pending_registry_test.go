package handlers

import (
	"testing"
)

// genEntry is a minimal value type used to exercise the generic
// pendingRegistry core directly, independent of the concrete CREATE/LOCK/PIPE
// wrappers.
type genEntry struct {
	AsyncID   uint64
	ConnID    uint64
	MessageID uint64
	SessionID uint64
	TreeID    uint32
	Owner     string
}

// newGenRegistry builds a registry with one (ConnID, MessageID) unique index
// and two bucket indexes (Session, Tree), mirroring the lock registry's shape.
func newGenRegistry(maxOps int) *pendingRegistry[genEntry] {
	return newPendingRegistry(registryConfig[genEntry]{
		asyncID: func(e *genEntry) uint64 { return e.AsyncID },
		indexes: []keyFunc[genEntry]{
			func(e *genEntry) any {
				return [2]uint64{e.ConnID, e.MessageID}
			},
		},
		buckets: []keyFunc[genEntry]{
			func(e *genEntry) any { return e.SessionID },
			func(e *genEntry) any { return e.TreeID },
		},
		maxOps: maxOps,
	})
}

func insertGen(t *testing.T, r *pendingRegistry[genEntry], e *genEntry) {
	t.Helper()
	r.mu.Lock()
	r.insertLocked(e)
	r.mu.Unlock()
}

func TestPendingRegistry_InsertRemoveAndLen(t *testing.T) {
	r := newGenRegistry(0)
	e := &genEntry{AsyncID: 7, ConnID: 1, MessageID: 42, SessionID: 100, TreeID: 5}
	insertGen(t, r, e)

	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	if got := r.unregisterByAsyncID(7); got != e {
		t.Fatalf("unregisterByAsyncID = %v, want %v", got, e)
	}
	if r.Len() != 0 {
		t.Fatalf("Len after remove = %d, want 0", r.Len())
	}
	// Idempotent second removal.
	if got := r.unregisterByAsyncID(7); got != nil {
		t.Fatalf("second unregisterByAsyncID = %v, want nil", got)
	}
}

func TestPendingRegistry_UnregisterByIndex(t *testing.T) {
	r := newGenRegistry(0)
	e := &genEntry{AsyncID: 7, ConnID: 1, MessageID: 42, SessionID: 100, TreeID: 5}
	insertGen(t, r, e)

	// Wrong ConnID does not match (key is the (ConnID, MessageID) pair).
	if got := r.unregisterByIndex(0, [2]uint64{2, 42}); got != nil {
		t.Fatalf("unregisterByIndex(wrong conn) = %v, want nil", got)
	}
	if got := r.unregisterByIndex(0, [2]uint64{1, 42}); got != e {
		t.Fatalf("unregisterByIndex = %v, want %v", got, e)
	}
	if r.Len() != 0 {
		t.Fatalf("Len after index removal = %d, want 0", r.Len())
	}
}

func TestPendingRegistry_UnregisterBucket(t *testing.T) {
	r := newGenRegistry(0)
	a := &genEntry{AsyncID: 1, ConnID: 1, MessageID: 1, SessionID: 100, TreeID: 5}
	b := &genEntry{AsyncID: 2, ConnID: 1, MessageID: 2, SessionID: 100, TreeID: 6}
	c := &genEntry{AsyncID: 3, ConnID: 1, MessageID: 3, SessionID: 200, TreeID: 5}
	for _, e := range []*genEntry{a, b, c} {
		insertGen(t, r, e)
	}

	// Session bucket (index 0): session 100 has a+b.
	gotSess := r.unregisterBucket(0, uint64(100))
	if len(gotSess) != 2 {
		t.Fatalf("unregisterBucket(session 100) = %d, want 2", len(gotSess))
	}
	if r.Len() != 1 {
		t.Fatalf("Len after session teardown = %d, want 1", r.Len())
	}

	// Tree bucket (index 1): only c remains, on tree 5.
	gotTree := r.unregisterBucket(1, uint32(5))
	if len(gotTree) != 1 || gotTree[0] != c {
		t.Fatalf("unregisterBucket(tree 5) = %v, want [c]", gotTree)
	}
	if r.Len() != 0 {
		t.Fatalf("Len after tree teardown = %d, want 0", r.Len())
	}

	// Absent bucket key yields nil.
	if got := r.unregisterBucket(0, uint64(999)); got != nil {
		t.Fatalf("unregisterBucket(absent) = %v, want nil", got)
	}
}

func TestPendingRegistry_UnregisterMatching(t *testing.T) {
	r := newGenRegistry(0)
	a := &genEntry{AsyncID: 1, ConnID: 1, MessageID: 1, Owner: "x"}
	b := &genEntry{AsyncID: 2, ConnID: 1, MessageID: 2, Owner: "y"}
	c := &genEntry{AsyncID: 3, ConnID: 1, MessageID: 3, Owner: "x"}
	for _, e := range []*genEntry{a, b, c} {
		insertGen(t, r, e)
	}

	got := r.unregisterMatching(func(e *genEntry) bool { return e.Owner == "x" })
	if len(got) != 2 {
		t.Fatalf("unregisterMatching(owner=x) = %d, want 2", len(got))
	}
	if r.Len() != 1 {
		t.Fatalf("Len after = %d, want 1 (owner=y remains)", r.Len())
	}
}

// TestPendingRegistry_RemoveDropsAllIndexes asserts removal purges every index,
// so a stale secondary lookup never resurrects a removed entry.
func TestPendingRegistry_RemoveDropsAllIndexes(t *testing.T) {
	r := newGenRegistry(0)
	e := &genEntry{AsyncID: 7, ConnID: 1, MessageID: 42, SessionID: 100, TreeID: 5}
	insertGen(t, r, e)
	r.unregisterByAsyncID(7)

	if got := r.unregisterByIndex(0, [2]uint64{1, 42}); got != nil {
		t.Errorf("msg index not purged: got %v", got)
	}
	if got := r.unregisterBucket(0, uint64(100)); got != nil {
		t.Errorf("session bucket not purged: got %v", got)
	}
	if got := r.unregisterBucket(1, uint32(5)); got != nil {
		t.Errorf("tree bucket not purged: got %v", got)
	}
}
