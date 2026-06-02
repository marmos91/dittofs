package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/stretchr/testify/assert"
)

// TestCreateHardLink_NotifyDirChangeMatrix pins the four sub-cases of the
// Samba `dlt_hardlinks` matrix (source4/torture/smb2/lease.c::dlt_hardlinks)
// against MetadataService.CreateHardLink, asserting that the dst-parent
// DirChangeNotifier is invoked with the correct (excludeParentLeaseKey,
// hasExcludeKey) tuple. The dir-lease parent-key suppression rule lives in
// lock.Manager.OnDirChange (covered separately); this test pins the
// MetadataService → notifier wire — i.e. the C5 plumbing #470 expects.
//
// Sub-cases (rows of dlt_hardlinks):
//   - samedir + samekey         → hasExcludeKey=true,  key matches
//   - samedir + diffkey         → hasExcludeKey=true,  key differs
//   - differentdir + samekey    → hasExcludeKey=true,  key matches
//   - differentdir + diffkey    → hasExcludeKey=true,  key differs
//
// The "samekey" / "diffkey" axis is verified by the OnDirChange suppression
// matrix in pkg/metadata/lock/directory_test.go — here we only verify the
// AuthContext.ParentLeaseKey lands on the notifier verbatim.
func TestCreateHardLink_NotifyDirChangeMatrix(t *testing.T) {
	t.Parallel()

	type sub struct {
		name         string
		differentDir bool
		parentKey    [16]byte
	}
	keyA := [16]byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8}
	keyB := [16]byte{0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0xB7, 0xB8}

	cases := []sub{
		{name: "samedir_samekey", differentDir: false, parentKey: keyA},
		{name: "samedir_diffkey", differentDir: false, parentKey: keyB},
		{name: "differentdir_samekey", differentDir: true, parentKey: keyA},
		{name: "differentdir_diffkey", differentDir: true, parentKey: keyB},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTestFixture(t)
			notifier := &recordingNotifier{}
			fx.service.SetDirChangeNotifier(fx.shareName, notifier)

			rootCtx := fx.rootContext()

			// Source dir + file (always linked from "srcdir").
			_, _, err := fx.service.CreateDirectory(rootCtx, fx.rootHandle, "srcdir", &metadata.FileAttr{
				Type: metadata.FileTypeDirectory, Mode: 0o777,
			})
			if err != nil {
				t.Fatalf("CreateDirectory(srcdir): %v", err)
			}
			srcDirHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "srcdir")
			if err != nil {
				t.Fatalf("GetChild(srcdir): %v", err)
			}
			srcFile, _, err := fx.service.CreateFile(rootCtx, srcDirHandle, "src.txt", &metadata.FileAttr{
				Type: metadata.FileTypeRegular, Mode: 0o644,
			})
			if err != nil {
				t.Fatalf("CreateFile(src.txt): %v", err)
			}
			srcHandle, err := metadata.EncodeFileHandle(srcFile)
			if err != nil {
				t.Fatalf("EncodeFileHandle(src): %v", err)
			}

			// Destination directory: either same as src or distinct.
			dstDirHandle := srcDirHandle
			if tc.differentDir {
				dstDirName := "dstdir"
				_, _, err := fx.service.CreateDirectory(rootCtx, fx.rootHandle, dstDirName, &metadata.FileAttr{
					Type: metadata.FileTypeDirectory, Mode: 0o777,
				})
				if err != nil {
					t.Fatalf("CreateDirectory(dstdir): %v", err)
				}
				dstDirHandle, err = fx.store.GetChild(context.Background(), fx.rootHandle, dstDirName)
				if err != nil {
					t.Fatalf("GetChild(dstdir): %v", err)
				}
			}

			// Reset notifier (CreateDirectory / CreateFile above already fired
			// add-entry notifications we don't care about for this assertion).
			notifier.mu.Lock()
			notifier.calls = nil
			notifier.mu.Unlock()

			// AuthContext mirrors what the SMB hardlink handler builds after
			// PropagateOpenFileParentLeaseKey: the opening handle's
			// ParentLeaseKey lands on the AuthContext.
			linkCtx := &metadata.AuthContext{
				Context:           context.Background(),
				Identity:          &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
				LockClientID:      "smb:hardlink-client",
				ParentLeaseKey:    tc.parentKey,
				HasParentLeaseKey: true,
			}

			if _, err := fx.service.CreateHardLink(linkCtx, dstDirHandle, "link.txt", srcHandle); err != nil {
				t.Fatalf("CreateHardLink: %v", err)
			}

			notifier.mu.Lock()
			defer notifier.mu.Unlock()
			if len(notifier.calls) == 0 {
				t.Fatalf("CreateHardLink did not fire a directory-change notification on dst parent")
			}

			// Expect exactly one notification, targeted at dstDirHandle.
			call := notifier.calls[0]
			assert.Equal(t, lock.FileHandle(dstDirHandle), call.ParentHandle,
				"notification must target the destination parent directory (dst-parent break)")
			assert.Equal(t, lock.DirChangeAddEntry, call.ChangeType,
				"hardlink is an AddEntry on the destination parent")
			assert.Equal(t, "smb:hardlink-client", call.OriginClient,
				"originClient must come from AuthContext.LockClientID for ClientID-scoped exclusion")
			assert.True(t, call.HasExcludeKey,
				"HasExcludeKey must be true so the dst-parent dir-lease parent-key suppression rule can fire")
			assert.Equal(t, tc.parentKey, call.ExcludeParentLeaseKey,
				"ExcludeParentLeaseKey must equal AuthContext.ParentLeaseKey (verbatim hand-off)")
		})
	}
}

// TestCreateHardLink_NFSCallerHasNoParentKey asserts that an NFS-style caller
// (HasParentLeaseKey=false on the AuthContext) does NOT enable parent-key
// suppression on the dst-parent notification. POSIX clients have no
// dir-lease concept and any bytes in ParentLeaseKey must be ignored — same
// short-circuit invariant pinned by TestNotifyDirChange_NFSCallerHasNoParentKey
// for the CreateFile path, extended here to CreateHardLink (#470 C5).
func TestCreateHardLink_NFSCallerHasNoParentKey(t *testing.T) {
	t.Parallel()

	fx := newTestFixture(t)
	notifier := &recordingNotifier{}
	fx.service.SetDirChangeNotifier(fx.shareName, notifier)

	rootCtx := fx.rootContext()

	_, _, err := fx.service.CreateDirectory(rootCtx, fx.rootHandle, "d", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o777,
	})
	if err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	dirHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "d")
	if err != nil {
		t.Fatalf("GetChild: %v", err)
	}
	srcFile, _, err := fx.service.CreateFile(rootCtx, dirHandle, "src.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	srcHandle, err := metadata.EncodeFileHandle(srcFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	notifier.mu.Lock()
	notifier.calls = nil
	notifier.mu.Unlock()

	bogus := [16]byte{0xFF, 0xFF, 0xFF, 0xFF}
	nfsCtx := &metadata.AuthContext{
		Context:           context.Background(),
		Identity:          &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		LockClientID:      "nfs:client",
		ParentLeaseKey:    bogus,
		HasParentLeaseKey: false,
	}
	if _, err := fx.service.CreateHardLink(nfsCtx, dirHandle, "link.txt", srcHandle); err != nil {
		t.Fatalf("CreateHardLink: %v", err)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.calls) == 0 {
		t.Fatalf("CreateHardLink did not fire a notification")
	}
	call := notifier.calls[0]
	assert.False(t, call.HasExcludeKey,
		"NFS path (HasParentLeaseKey=false) must NOT enable parent-key suppression")
	assert.Equal(t, [16]byte{}, call.ExcludeParentLeaseKey,
		"ExcludeParentLeaseKey must be zero when hasExcludeKey is false")
}
