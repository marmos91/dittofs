package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/stretchr/testify/require"
)

// handleForFile encodes the handle for a file returned by CreateFile.
func handleForFile(t *testing.T, f *metadata.File) metadata.FileHandle {
	t.Helper()
	h, err := metadata.EncodeShareHandle(f.ShareName, f.ID)
	require.NoError(t, err)
	return h
}

// isQuotaErr reports whether err is a quota-exceeded StoreError.
func isQuotaErr(err error) bool {
	var se *metadata.StoreError
	if as, ok := err.(*metadata.StoreError); ok {
		se = as
	}
	return se != nil && se.Code == merrs.ErrQuotaExceeded
}

func TestQuotaEnforcement_HardByteBlock(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	// uid=1000 gets a 1000-byte hard quota.
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:      metadata.QuotaScopeUser,
		ID:         1000,
		LimitBytes: 1000,
	})

	file, _, err := fx.service.CreateFile(fx.userContext(), fx.rootHandle, "a.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	handle := handleForFile(t, file)

	// A write up to the limit is allowed.
	if _, err := fx.service.PrepareWrite(fx.userContext(), handle, 1000); err != nil {
		t.Fatalf("PrepareWrite at limit should succeed, got %v", err)
	}

	// A write past the limit is blocked with a quota error.
	_, err = fx.service.PrepareWrite(fx.userContext(), handle, 1001)
	if !isQuotaErr(err) {
		t.Fatalf("PrepareWrite past hard limit: got %v, want ErrQuotaExceeded", err)
	}
}

func TestQuotaEnforcement_AnotherUserUnaffected(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	// Only uid=1000 is quota'd.
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:      metadata.QuotaScopeUser,
		ID:         1000,
		LimitBytes: 100,
	})

	// uid=1001 (no quota) can write freely.
	other := fx.authContext(1001, 1001)
	file, _, err := fx.service.CreateFile(other, fx.rootHandle, "b.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	handle := handleForFile(t, file)
	if _, err := fx.service.PrepareWrite(other, handle, 1_000_000); err != nil {
		t.Fatalf("unquota'd user write should succeed, got %v", err)
	}
}

func TestQuotaEnforcement_InodeCap(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	// uid=1000 may own at most 2 files.
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:      metadata.QuotaScopeUser,
		ID:         1000,
		LimitFiles: 2,
	})

	uc := fx.userContext()
	if _, _, err := fx.service.CreateFile(uc, fx.rootHandle, "f1", &metadata.FileAttr{Mode: 0o644}); err != nil {
		t.Fatalf("first create should succeed: %v", err)
	}
	if _, _, err := fx.service.CreateFile(uc, fx.rootHandle, "f2", &metadata.FileAttr{Mode: 0o644}); err != nil {
		t.Fatalf("second create should succeed: %v", err)
	}
	// Third create exceeds the inode cap.
	_, _, err := fx.service.CreateFile(uc, fx.rootHandle, "f3", &metadata.FileAttr{Mode: 0o644})
	if !isQuotaErr(err) {
		t.Fatalf("third create past inode cap: got %v, want ErrQuotaExceeded", err)
	}
}

func TestQuotaEnforcement_DefaultUserFallthrough(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	// Default-user quota: any uid without an explicit quota gets 500 bytes.
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:      metadata.QuotaScopeUser,
		ID:         metadata.DefaultUserID,
		LimitBytes: 500,
	})
	// uid=2000 gets an explicit, larger quota that overrides the default.
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:      metadata.QuotaScopeUser,
		ID:         2000,
		LimitBytes: 10000,
	})

	// uid=1000 falls through to the default 500-byte limit.
	def := fx.authContext(1000, 1000)
	dfile, _, err := fx.service.CreateFile(def, fx.rootHandle, "d.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	dh := handleForFile(t, dfile)
	if _, err := fx.service.PrepareWrite(def, dh, 600); !isQuotaErr(err) {
		t.Fatalf("default-user past 500: got %v, want ErrQuotaExceeded", err)
	}

	// uid=2000 uses its explicit larger quota.
	exp := fx.authContext(2000, 2000)
	efile, _, err := fx.service.CreateFile(exp, fx.rootHandle, "e.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	eh := handleForFile(t, efile)
	if _, err := fx.service.PrepareWrite(exp, eh, 600); err != nil {
		t.Fatalf("explicit-quota user under its limit should succeed, got %v", err)
	}
}

func TestQuotaEnforcement_GroupQuota(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	// gid=3000 hard byte limit of 800.
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:      metadata.QuotaScopeGroup,
		ID:         3000,
		LimitBytes: 800,
	})

	ctx := fx.authContext(1234, 3000)
	file, _, err := fx.service.CreateFile(ctx, fx.rootHandle, "g.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	handle := handleForFile(t, file)
	if _, err := fx.service.PrepareWrite(ctx, handle, 801); !isQuotaErr(err) {
		t.Fatalf("group quota past limit: got %v, want ErrQuotaExceeded", err)
	}
}

func TestQuotaEnforcement_SoftGraceHard(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	// Soft=100, hard=10000, grace=1s. Crossing soft starts the grace timer;
	// after grace expires the soft threshold is enforced as hard.
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:        metadata.QuotaScopeUser,
		ID:           1000,
		SoftBytes:    100,
		LimitBytes:   10000,
		GraceSeconds: 1,
	})

	uc := fx.userContext()
	file, _, err := fx.service.CreateFile(uc, fx.rootHandle, "s.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	handle := handleForFile(t, file)

	// First write over soft (but under hard) is allowed and starts grace.
	if _, err := fx.service.PrepareWrite(uc, handle, 200); err != nil {
		t.Fatalf("over-soft within grace should be allowed, got %v", err)
	}
	// Grace just started, still within window: still allowed.
	if _, err := fx.service.PrepareWrite(uc, handle, 300); err != nil {
		t.Fatalf("over-soft still within grace should be allowed, got %v", err)
	}

	// The grace timer is persisted in the live quota map; verify it is running.
	iq, ok := fx.service.GetIdentityQuota(fx.shareName, metadata.QuotaScopeUser, 1000)
	require.True(t, ok)
	require.False(t, iq.GraceStartedAt.IsZero(), "grace timer should be running after crossing soft")

	// Simulate grace expiry by rewinding the persisted start far enough back,
	// then a further over-soft write must be blocked as hard.
	iq.GraceStartedAt = iq.GraceStartedAt.Add(-10 * 1e9) // -10s
	fx.service.SetIdentityQuota(fx.shareName, iq)
	if _, err := fx.service.PrepareWrite(uc, handle, 400); !isQuotaErr(err) {
		t.Fatalf("over-soft after grace expiry: got %v, want ErrQuotaExceeded", err)
	}
}
