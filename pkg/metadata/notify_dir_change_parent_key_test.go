package metadata_test

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/stretchr/testify/assert"
)

// recordingNotifier records every OnDirChange invocation so tests can pin
// the parent-key arguments flowing from MetadataService.notifyDirChange.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []recordedDirChange
}

type recordedDirChange struct {
	ParentHandle          lock.FileHandle
	ChangeType            lock.DirChangeType
	OriginClient          string
	ExcludeParentLeaseKey [16]byte
	HasExcludeKey         bool
}

func (n *recordingNotifier) OnDirChange(parentHandle lock.FileHandle, changeType lock.DirChangeType, originClientID string, excludeParentLeaseKey [16]byte, hasExcludeKey bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, recordedDirChange{
		ParentHandle:          parentHandle,
		ChangeType:            changeType,
		OriginClient:          originClientID,
		ExcludeParentLeaseKey: excludeParentLeaseKey,
		HasExcludeKey:         hasExcludeKey,
	})
}

// TestNotifyDirChange_ThreadsParentLeaseKey asserts that
// MetadataService.notifyDirChange forwards AuthContext.ParentLeaseKey /
// HasParentLeaseKey to the DirChangeNotifier so the dir-lease parent-key
// suppression rule (MS-SMB2 §3.3.4.20, #470 C2) can apply. This is the
// "wire" between the SMB handler (which captures the key at CREATE) and
// the lock manager (which performs the suppression in breakOpLocks).
func TestNotifyDirChange_ThreadsParentLeaseKey(t *testing.T) {
	t.Parallel()

	fx := newTestFixture(t)
	notifier := &recordingNotifier{}
	fx.service.SetDirChangeNotifier(fx.shareName, notifier)

	parentKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	ctx := &metadata.AuthContext{
		Context:           context.Background(),
		Identity:          &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		LockClientID:      "smb:42",
		ParentLeaseKey:    parentKey,
		HasParentLeaseKey: true,
	}

	// CreateFile triggers notifyDirChange(DirChangeAddEntry, ctx).
	_, err := fx.service.CreateFile(ctx, fx.rootHandle, "foo.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  0,
		GID:  0,
	})
	if err != nil {
		t.Fatalf("CreateFile failed: %v", err)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()

	if len(notifier.calls) == 0 {
		t.Fatal("notifier never invoked")
	}
	call := notifier.calls[0]
	assert.True(t, call.HasExcludeKey, "HasExcludeKey must be true when AuthContext.HasParentLeaseKey is true")
	assert.Equal(t, parentKey, call.ExcludeParentLeaseKey, "ExcludeParentLeaseKey must match AuthContext.ParentLeaseKey")
	assert.Equal(t, "smb:42", call.OriginClient, "originClient must come from AuthContext.LockClientID")
}

// TestRemoveFile_UnlinkSameSetAndClose verifies the #470 C6 path:
// when an SMB CLOSE triggers the actual delete (delete-on-close set via
// SET_INFO on the SAME handle that's now closing), the parent dir-lease
// matching that handle's ParentLeaseKey MUST be suppressed.
//
// Maps to smbtorture smb2.dirlease.unlink_same_set_and_close — H2 set DOC,
// H2 closes last, H2's ParentLeaseKey == dir2-key, so dir2 stays unbroken
// and dir1 gets the RemoveEntry break.
func TestRemoveFile_UnlinkSameSetAndClose(t *testing.T) {
	t.Parallel()

	fx := newTestFixture(t)
	notifier := &recordingNotifier{}
	fx.service.SetDirChangeNotifier(fx.shareName, notifier)

	// Pre-create the victim file under root so RemoveFile has something to unlink.
	rootCtx := fx.rootContext()
	_, err := fx.service.CreateFile(rootCtx, fx.rootHandle, "victim.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0644, UID: 0, GID: 0,
	})
	if err != nil {
		t.Fatalf("CreateFile failed: %v", err)
	}

	// CLOSE-triggered delete: the closing handle carries its ParentLeaseKey
	// (set at CREATE by lease_context). Same-handle: the parent-key in the
	// AuthContext is the SAME handle's key.
	parentKey := [16]byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8}
	closeCtx := &metadata.AuthContext{
		Context:           context.Background(),
		Identity:          &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		LockClientID:      "smb:H2",
		ParentLeaseKey:    parentKey,
		HasParentLeaseKey: true,
		HasDeleteAccess:   true,
	}
	if _, err := fx.service.RemoveFile(closeCtx, fx.rootHandle, "victim.txt"); err != nil {
		t.Fatalf("RemoveFile failed: %v", err)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()

	// Find the RemoveEntry call (the AddEntry from the pre-create is also in calls).
	var removeCalls []recordedDirChange
	for _, c := range notifier.calls {
		if c.ChangeType == lock.DirChangeRemoveEntry {
			removeCalls = append(removeCalls, c)
		}
	}
	if len(removeCalls) != 1 {
		t.Fatalf("expected exactly 1 RemoveEntry notification, got %d", len(removeCalls))
	}
	assert.True(t, removeCalls[0].HasExcludeKey,
		"same_set_and_close: closing handle's HasParentLeaseKey must reach the notifier")
	assert.Equal(t, parentKey, removeCalls[0].ExcludeParentLeaseKey,
		"same_set_and_close: exclude key MUST be the closing handle's ParentLeaseKey")
	assert.Equal(t, "smb:H2", removeCalls[0].OriginClient,
		"originClient must come from the closing handle's session")
}

// TestRemoveFile_UnlinkDifferentSetAndClose verifies the #470 C6 path:
// when H1 set DOC but H2 (no parent-key on its own handle) closes last and
// triggers the actual delete, the exclude key comes from H2 — i.e.
// HasParentLeaseKey=false on the AuthContext — so all dir leases break.
//
// Maps to smbtorture smb2.dirlease.unlink_different_set_and_close.
func TestRemoveFile_UnlinkDifferentSetAndClose(t *testing.T) {
	t.Parallel()

	fx := newTestFixture(t)
	notifier := &recordingNotifier{}
	fx.service.SetDirChangeNotifier(fx.shareName, notifier)

	rootCtx := fx.rootContext()
	_, err := fx.service.CreateFile(rootCtx, fx.rootHandle, "victim.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0644, UID: 0, GID: 0,
	})
	if err != nil {
		t.Fatalf("CreateFile failed: %v", err)
	}

	// Closing handle is H2 — never carried a ParentLeaseKey (e.g. opened
	// without RqLs or with Flags=0). The AuthContext therefore has
	// HasParentLeaseKey=false even though the file has DOC set by H1.
	closeCtx := &metadata.AuthContext{
		Context:           context.Background(),
		Identity:          &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		LockClientID:      "smb:H2",
		HasParentLeaseKey: false,
		HasDeleteAccess:   true,
	}
	if _, err := fx.service.RemoveFile(closeCtx, fx.rootHandle, "victim.txt"); err != nil {
		t.Fatalf("RemoveFile failed: %v", err)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()

	var removeCalls []recordedDirChange
	for _, c := range notifier.calls {
		if c.ChangeType == lock.DirChangeRemoveEntry {
			removeCalls = append(removeCalls, c)
		}
	}
	if len(removeCalls) != 1 {
		t.Fatalf("expected exactly 1 RemoveEntry notification, got %d", len(removeCalls))
	}
	assert.False(t, removeCalls[0].HasExcludeKey,
		"different_set_and_close: closing handle has no parent-key; HasExcludeKey must be false so ALL dir leases break")
	assert.Equal(t, [16]byte{}, removeCalls[0].ExcludeParentLeaseKey,
		"different_set_and_close: zero key on the wire when hasExcludeKey=false")
}

// TestRemoveFile_UnlinkSameInitialAndClose verifies the #470 C7 path:
// when DOC was set via FILE_DELETE_ON_CLOSE create option (rather than later
// SET_INFO) on the SAME handle that's now closing, the parent-key suppression
// still applies — OpenFile.ParentLeaseKey is captured at CREATE regardless of
// when DOC was set, so the AuthContext propagation is symmetric.
//
// Maps to smbtorture smb2.dirlease.unlink_same_initial_and_close.
func TestRemoveFile_UnlinkSameInitialAndClose(t *testing.T) {
	t.Parallel()

	fx := newTestFixture(t)
	notifier := &recordingNotifier{}
	fx.service.SetDirChangeNotifier(fx.shareName, notifier)

	rootCtx := fx.rootContext()
	_, err := fx.service.CreateFile(rootCtx, fx.rootHandle, "victim.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0644, UID: 0, GID: 0,
	})
	if err != nil {
		t.Fatalf("CreateFile failed: %v", err)
	}

	// Initial DOC: same wire shape — close.go path propagates OpenFile.ParentLeaseKey
	// into AuthContext.ParentLeaseKey via PropagateOpenFileParentLeaseKey,
	// regardless of whether DOC came from CREATE option or SET_INFO.
	parentKey := [16]byte{0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0xB7, 0xB8}
	closeCtx := &metadata.AuthContext{
		Context:           context.Background(),
		Identity:          &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		LockClientID:      "smb:H_initial",
		ParentLeaseKey:    parentKey,
		HasParentLeaseKey: true,
		HasDeleteAccess:   true,
	}
	if _, err := fx.service.RemoveFile(closeCtx, fx.rootHandle, "victim.txt"); err != nil {
		t.Fatalf("RemoveFile failed: %v", err)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()

	var removeCalls []recordedDirChange
	for _, c := range notifier.calls {
		if c.ChangeType == lock.DirChangeRemoveEntry {
			removeCalls = append(removeCalls, c)
		}
	}
	if len(removeCalls) != 1 {
		t.Fatalf("expected exactly 1 RemoveEntry notification, got %d", len(removeCalls))
	}
	assert.True(t, removeCalls[0].HasExcludeKey,
		"same_initial_and_close: parent-key suppression MUST work for create-option DOC, not just SET_INFO DOC")
	assert.Equal(t, parentKey, removeCalls[0].ExcludeParentLeaseKey,
		"same_initial_and_close: exclude key MUST be the closing handle's ParentLeaseKey")
}

// TestRemoveFile_UnlinkDifferentInitialAndClose verifies the #470 C7 path:
// when H1 created with FILE_DELETE_ON_CLOSE create option and H2 (no
// parent-key) closes last, the closing handle's HasParentLeaseKey=false
// reaches the notifier so all dir leases break.
//
// Maps to smbtorture smb2.dirlease.unlink_different_initial_and_close.
func TestRemoveFile_UnlinkDifferentInitialAndClose(t *testing.T) {
	t.Parallel()

	fx := newTestFixture(t)
	notifier := &recordingNotifier{}
	fx.service.SetDirChangeNotifier(fx.shareName, notifier)

	rootCtx := fx.rootContext()
	_, err := fx.service.CreateFile(rootCtx, fx.rootHandle, "victim.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0644, UID: 0, GID: 0,
	})
	if err != nil {
		t.Fatalf("CreateFile failed: %v", err)
	}

	closeCtx := &metadata.AuthContext{
		Context:           context.Background(),
		Identity:          &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		LockClientID:      "smb:H2",
		HasParentLeaseKey: false,
		HasDeleteAccess:   true,
	}
	if _, err := fx.service.RemoveFile(closeCtx, fx.rootHandle, "victim.txt"); err != nil {
		t.Fatalf("RemoveFile failed: %v", err)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()

	var removeCalls []recordedDirChange
	for _, c := range notifier.calls {
		if c.ChangeType == lock.DirChangeRemoveEntry {
			removeCalls = append(removeCalls, c)
		}
	}
	if len(removeCalls) != 1 {
		t.Fatalf("expected exactly 1 RemoveEntry notification, got %d", len(removeCalls))
	}
	assert.False(t, removeCalls[0].HasExcludeKey,
		"different_initial_and_close: no parent-key on closing handle ⇒ all dir leases must break")
	assert.Equal(t, [16]byte{}, removeCalls[0].ExcludeParentLeaseKey,
		"different_initial_and_close: zero key on the wire when hasExcludeKey=false")
}

// TestNotifyDirChange_NFSCallerHasNoParentKey asserts the NFS-style callsite
// (HasParentLeaseKey=false on the AuthContext) results in hasExcludeKey=false
// at the notifier — never accidentally suppressing a dir lease.
func TestNotifyDirChange_NFSCallerHasNoParentKey(t *testing.T) {
	t.Parallel()

	fx := newTestFixture(t)
	notifier := &recordingNotifier{}
	fx.service.SetDirChangeNotifier(fx.shareName, notifier)

	// Even with a non-zero ParentLeaseKey present, HasParentLeaseKey=false
	// must short-circuit and forward hasExcludeKey=false. This pins the NFS
	// contract: NFS handlers never set HasParentLeaseKey, so any garbage in
	// ParentLeaseKey is ignored.
	bogus := [16]byte{0xFF, 0xFF, 0xFF, 0xFF}
	ctx := &metadata.AuthContext{
		Context:           context.Background(),
		Identity:          &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		LockClientID:      "nfs:client",
		ParentLeaseKey:    bogus,
		HasParentLeaseKey: false,
	}

	_, err := fx.service.CreateFile(ctx, fx.rootHandle, "foo.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  0,
		GID:  0,
	})
	if err != nil {
		t.Fatalf("CreateFile failed: %v", err)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()

	if len(notifier.calls) == 0 {
		t.Fatal("notifier never invoked")
	}
	call := notifier.calls[0]
	assert.False(t, call.HasExcludeKey, "HasExcludeKey must be false when AuthContext.HasParentLeaseKey is false (NFS contract)")
	assert.Equal(t, [16]byte{}, call.ExcludeParentLeaseKey, "ExcludeParentLeaseKey must be zero when hasExcludeKey is false")
}
