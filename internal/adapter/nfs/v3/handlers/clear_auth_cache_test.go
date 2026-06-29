package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestClearAuthCache verifies ClearAuthCache drops all cached auth contexts so
// active clients re-resolve permissions after a share-config change.
func TestClearAuthCache(t *testing.T) {
	h := &Handler{}

	// Seed a few entries directly (the cache is keyed "share:uid:gid").
	h.authCache.Store("/export:1000:1000", &metadata.AuthContext{})
	h.authCache.Store("/export:0:0", &metadata.AuthContext{})
	h.authCache.Store("/other:2000:2000", &metadata.AuthContext{})

	count := func() int {
		n := 0
		h.authCache.Range(func(_, _ any) bool {
			n++
			return true
		})
		return n
	}

	if got := count(); got != 3 {
		t.Fatalf("expected 3 seeded entries, got %d", got)
	}

	h.ClearAuthCache()

	if got := count(); got != 0 {
		t.Fatalf("expected cache empty after ClearAuthCache, got %d", got)
	}

	// Idempotent: clearing an empty cache is a no-op.
	h.ClearAuthCache()
	if got := count(); got != 0 {
		t.Fatalf("expected cache still empty, got %d", got)
	}
}
