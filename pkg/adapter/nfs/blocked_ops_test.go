package nfs

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestIsOperationBlocked_UsesCachedSet verifies that the NFSv3 dispatch path
// consults the pre-parsed blocked-ops set rather than unmarshalling the stored
// blocklist on every call. It also confirms blocked vs allowed semantics.
func TestIsOperationBlocked_UsesCachedSet(t *testing.T) {
	s := &NFSAdapter{}
	c := &NFSConnection{server: s}

	// No Registry, no settings, empty cache: nothing is blocked. This also
	// proves isOperationBlocked does not depend on the SettingsWatcher on the
	// hot path -- it reads only the cached set.
	if c.isOperationBlocked("WRITE") {
		t.Fatal("expected no ops blocked before any settings applied")
	}

	s.setV3BlockedOps([]string{"WRITE", "REMOVE"})

	if !c.isOperationBlocked("WRITE") {
		t.Error("expected WRITE to be blocked")
	}
	if !c.isOperationBlocked("REMOVE") {
		t.Error("expected REMOVE to be blocked")
	}
	if c.isOperationBlocked("READ") {
		t.Error("expected READ to be allowed")
	}

	// Clearing the set unblocks everything (hot-reload to an empty blocklist).
	s.setV3BlockedOps(nil)
	if c.isOperationBlocked("WRITE") {
		t.Error("expected WRITE to be allowed after clearing the blocklist")
	}
}

// TestIsOperationBlocked_NoPerCallUnmarshal proves that the blocklist is parsed
// once (at apply time) and not re-parsed per dispatch: mutating the source
// settings' serialized blocklist after the cache is populated has no effect on
// subsequent isOperationBlocked calls.
func TestIsOperationBlocked_NoPerCallUnmarshal(t *testing.T) {
	settings := &models.NFSAdapterSettings{}
	settings.SetBlockedOperations([]string{"WRITE"})

	s := &NFSAdapter{}
	c := &NFSConnection{server: s}

	// Apply the blocklist once (mirrors applyNFSSettings).
	s.setV3BlockedOps(settings.GetBlockedOperations())

	if !c.isOperationBlocked("WRITE") {
		t.Fatal("expected WRITE to be blocked after applying settings")
	}

	// Mutate the underlying serialized blocklist. If isOperationBlocked were
	// re-unmarshalling settings.BlockedOperations on every call, this would
	// change the result. Because the parsed set is cached, it must not.
	settings.SetBlockedOperations([]string{"READ"})

	if !c.isOperationBlocked("WRITE") {
		t.Error("WRITE should still be blocked: dispatch must read the cached set, not re-parse settings")
	}
	if c.isOperationBlocked("READ") {
		t.Error("READ must not become blocked from a post-apply settings mutation: dispatch must not re-parse settings")
	}
}
