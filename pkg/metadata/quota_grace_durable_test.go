package metadata_test

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// fakeGracePersister stands in for the control-plane durable grace store. It
// captures the per-real-user default-user grace timers the enforcer persists, so
// a test can simulate a restart by reseeding a fresh Service from the captured
// state.
type fakeGracePersister struct {
	mu sync.Mutex
	// dynamic maps real uid -> grace start for default-user fallbacks. A reap
	// (zero t) deletes the entry, mirroring the user_grace side table.
	dynamic map[uint32]time.Time
}

func newFakeGracePersister() *fakeGracePersister {
	return &fakeGracePersister{dynamic: map[uint32]time.Time{}}
}

func (f *fakeGracePersister) PersistQuotaGrace(string, metadata.QuotaScope, uint32, time.Time) {
}

func (f *fakeGracePersister) PersistDefaultUserGrace(_ string, uid uint32, t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.IsZero() {
		delete(f.dynamic, uid)
		return
	}
	f.dynamic[uid] = t
}

func (f *fakeGracePersister) snapshot() map[uint32]time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[uint32]time.Time, len(f.dynamic))
	for k, v := range f.dynamic {
		out[k] = v
	}
	return out
}

// TestDefaultUserGrace_PersistsAndReaps verifies the enforcer durably records a
// default-user grace timer on a soft breach and reaps it when projected usage
// falls back under soft. The check is on PROJECTED usage (live counter + delta),
// so two PrepareWrite calls on the same (still size-0) file drive the
// start→clear transition without committing any writes.
func TestDefaultUserGrace_PersistsAndReaps(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	persist := newFakeGracePersister()
	fx.service.SetQuotaGracePersister(persist)

	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:        metadata.QuotaScopeUser,
		ID:           metadata.DefaultUserID,
		SoftBytes:    100,
		LimitBytes:   100000,
		GraceSeconds: 3600,
	})

	user := fx.authContext(1000, 1000)
	file, _, err := fx.service.CreateFile(user, fx.rootHandle, "f.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	h := handleForFile(t, file)

	// Projected 200 > soft 100 → grace starts and is persisted durably.
	_, err = fx.service.PrepareWrite(user, h, 200)
	require.NoError(t, err, "over-soft within grace must be allowed")
	require.Contains(t, persist.snapshot(), uint32(1000), "soft breach must persist a durable grace timer")

	// Projected 90 < soft 100 (file still size 0) → grace cleared and reaped.
	_, err = fx.service.PrepareWrite(user, h, 90)
	require.NoError(t, err)
	require.NotContains(t, persist.snapshot(), uint32(1000), "drop under soft must reap the durable grace timer")
}

// TestDefaultUserGrace_SurvivesRestart verifies that a default-user's expired
// grace, restored from the durable store after a restart, is enforced as hard —
// rather than handing the user a fresh grace window (the #1200 gap). A user with
// no seeded timer on the same restarted service still gets its own fresh grace,
// proving the seed (not a blanket block) is what drives enforcement.
func TestDefaultUserGrace_SurvivesRestart(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	quota := metadata.IdentityQuota{
		Scope:        metadata.QuotaScopeUser,
		ID:           metadata.DefaultUserID,
		SoftBytes:    100,
		LimitBytes:   100000,
		GraceSeconds: 3600, // 1h
	}

	// --- before restart: user 1000 breaches soft, starting a grace window. ---
	persist := newFakeGracePersister()
	fx.service.SetQuotaGracePersister(persist)
	fx.service.SetIdentityQuota(fx.shareName, quota)

	user := fx.authContext(1000, 1000)
	file, _, err := fx.service.CreateFile(user, fx.rootHandle, "f.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	h := handleForFile(t, file)
	_, err = fx.service.PrepareWrite(user, h, 200)
	require.NoError(t, err)
	require.Contains(t, persist.snapshot(), uint32(1000))

	// --- restart: fresh Service over the SAME store (usage survives). Seed the
	// durable grace, backdated so the 1h window has already elapsed. ---
	svc2 := metadata.New()
	require.NoError(t, svc2.RegisterStoreForShare(fx.shareName, fx.store))
	svc2.SetQuotaGracePersister(newFakeGracePersister())
	svc2.SetIdentityQuota(fx.shareName, quota)
	svc2.SeedDefaultUserGrace(fx.shareName, map[uint32]time.Time{
		1000: time.Now().Add(-2 * time.Hour),
	})

	// User 1000's restored grace is expired → soft is enforced as hard.
	_, err = svc2.PrepareWrite(user, h, 200)
	require.Error(t, err, "expired grace restored across restart must block")
	require.True(t, isQuotaErr(err), "want ErrQuotaExceeded, got %v", err)

	// A different user with no seeded timer gets its own fresh grace window.
	other := fx.authContext(1001, 1001)
	ofile, _, err := svc2.CreateFile(other, fx.rootHandle, "g.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	oh := handleForFile(t, ofile)
	_, err = svc2.PrepareWrite(other, oh, 200)
	require.NoError(t, err, "an unseen user's first breach must start a fresh grace, not inherit 1000's")
}

// TestDefaultUserGrace_ClearOnExplicitQuotaRemoval guards the edge case the
// reviewer flagged: a uid that was in default-user grace, then got an explicit
// quota, then had it removed, must NOT inherit the stale (possibly expired)
// default-user timer when it reverts to the fallback — it should start fresh.
// The runtime calls Service.ClearDefaultUserGrace on explicit-quota removal;
// this test exercises that method directly.
func TestDefaultUserGrace_ClearOnExplicitQuotaRemoval(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	fx.service.SetIdentityQuota(fx.shareName, metadata.IdentityQuota{
		Scope:        metadata.QuotaScopeUser,
		ID:           metadata.DefaultUserID,
		SoftBytes:    100,
		LimitBytes:   100000,
		GraceSeconds: 3600,
	})

	user := fx.authContext(1000, 1000)
	file, _, err := fx.service.CreateFile(user, fx.rootHandle, "f.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	h := handleForFile(t, file)

	// Leftover stale default-user grace timer for uid 1000, already expired.
	fx.service.SeedDefaultUserGrace(fx.shareName, map[uint32]time.Time{
		1000: time.Now().Add(-2 * time.Hour),
	})
	// Sanity: while the stale timer is present, uid 1000 is wrongly blocked.
	_, err = fx.service.PrepareWrite(user, h, 200)
	require.True(t, isQuotaErr(err), "stale expired default-user grace should block until cleared, got %v", err)

	// Clearing (what the runtime does on explicit-quota removal) drops the timer.
	fx.service.ClearDefaultUserGrace(fx.shareName, 1000)

	// uid 1000 now starts a fresh grace window instead of inheriting the expired one.
	_, err = fx.service.PrepareWrite(user, h, 200)
	require.NoError(t, err, "after clear, uid 1000 must start a fresh grace, not inherit the expired timer")
}
