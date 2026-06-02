package smb

import (
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestResolver_GetLockManagerForShare_MissingShareReturnsNilInterface guards
// against a typed-nil regression: metadata.Service.GetLockManagerForShare
// returns a concrete *metadata.LockManager that is nil for an unknown/removed
// share. If the resolver returns that nil pointer through its lock.LockManager
// interface return value, the interface is non-nil (it carries a type) and every
// downstream `lockMgr == nil` guard evaluates false — the first method call then
// nil-derefs the receiver and crashes the SMB server mid-battery.
//
// Reproduces the panic seen in CI:
//
//	lock.(*Manager).breakOpLocks(0x0, ...)            manager.go
//	lock.(*Manager).BreakLeasesOnOpenConflict(0x0, ...)
//	lease.(*LeaseManager).BreakParentDirLeasesOnDestructiveCreate(0x0, ...)
//
// triggered when a CREATE raced a DeleteShare.
func TestResolver_GetLockManagerForShare_MissingShareReturnsNilInterface(t *testing.T) {
	// Fresh service has no registered shares, so the underlying lookup returns
	// a nil *metadata.LockManager.
	resolver := &metadataServiceResolver{metaSvc: metadata.New()}

	lm := resolver.GetLockManagerForShare("/does-not-exist")
	if lm != nil {
		t.Fatalf("GetLockManagerForShare for a missing share must return a nil "+
			"interface, got non-nil interface %#v (typed-nil regression)", lm)
	}
}

// TestResolver_GetLockManagerForHandle_MissingShareReturnsNilInterface is the
// handle-keyed twin of the test above. The handle encodes a share that has no
// registered LockManager, so the resolver must surface a true nil interface.
func TestResolver_GetLockManagerForHandle_MissingShareReturnsNilInterface(t *testing.T) {
	resolver := &metadataServiceResolver{metaSvc: metadata.New()}

	// "shareName:uuid" is the on-the-wire handle format the resolver decodes.
	handle, err := metadata.EncodeShareHandle("/does-not-exist", uuid.New())
	if err != nil {
		t.Fatalf("EncodeShareHandle: %v", err)
	}

	lm := resolver.GetLockManagerForHandle(string(handle))
	if lm != nil {
		t.Fatalf("GetLockManagerForHandle for a missing share must return a nil "+
			"interface, got non-nil interface %#v (typed-nil regression)", lm)
	}
}

// TestResolver_NilMetaSvc_ReturnsNil exercises the metaSvc==nil short-circuits.
func TestResolver_NilMetaSvc_ReturnsNil(t *testing.T) {
	resolver := &metadataServiceResolver{metaSvc: nil}

	if lm := resolver.GetLockManagerForShare("/x"); lm != nil {
		t.Fatalf("nil metaSvc must yield nil interface, got %#v", lm)
	}
	if lm := resolver.GetLockManagerForHandle("/x:00000000-0000-0000-0000-000000000001"); lm != nil {
		t.Fatalf("nil metaSvc must yield nil interface, got %#v", lm)
	}
}
