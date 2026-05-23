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
