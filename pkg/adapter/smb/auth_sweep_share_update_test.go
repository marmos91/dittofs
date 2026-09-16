package smb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	cpauth "github.com/marmos91/dittofs/internal/controlplane/api/auth"
	cpapi "github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestDefaultPermissionUpdateSchedulesOneSweep counts the sweeps a single
// default-permission change costs, using the real authSweeper wired the way
// adapter.go wires it.
//
// The test lives in this package because the sweeper does: the cost being
// measured is a full walk of every session and tree, and only a real sweeper
// can say whether two invalidations collapse into one walk or not. Counting
// invalidation callbacks in the control-plane package would measure the signal
// rather than the work.
//
// Coalescing does not save the second signal here. The wake token is taken when
// a sweep STARTS, not when it finishes, so the window in which a second request
// is absorbed is only the scheduling gap before the first sweep begins. The root
// ACL reconcile that sits between the two signals does store reads and a
// root-inode write, which is far longer than that gap — measured at 100 sweeps
// over 50 updates when both signals were raised, i.e. no coalescing at all.
//
// The projection assertion is not decoration: without it this test would pass
// if the reconcile were deleted outright rather than merely stopped from
// raising a second invalidation.
func TestDefaultPermissionUpdateSchedulesOneSweep(t *testing.T) {
	ctx := context.Background()

	cpStore, err := cpstore.New(&cpstore.Config{
		Type:   "sqlite",
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	jwtSvc, err := cpauth.NewJWTService(cpauth.JWTConfig{
		Secret:               "test-secret-key-for-testing-only-32chars",
		Issuer:               "dittofs",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: time.Hour,
	})
	if err != nil {
		t.Fatalf("create jwt service: %v", err)
	}

	rt := runtime.New(cpStore)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	t.Cleanup(func() {
		for _, name := range rt.ListShares() {
			_ = rt.RemoveShare(name)
		}
	})

	metaID, err := cpStore.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "test-meta", Type: "memory"})
	if err != nil {
		t.Fatalf("create metadata store config: %v", err)
	}
	if err := rt.RegisterMetadataStore("test-meta", memory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("register metadata store: %v", err)
	}
	blockID, err := cpStore.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "test-block", Type: "memory"})
	if err != nil {
		t.Fatalf("create block store config: %v", err)
	}
	if _, err := cpStore.CreateShare(ctx, &models.Share{
		Name:              "/export",
		MetadataStoreID:   metaID,
		BlockStoreID:      blockID,
		DefaultPermission: string(models.PermissionNone),
	}); err != nil {
		t.Fatalf("create share: %v", err)
	}
	if err := runtime.LoadSharesFromStore(ctx, rt, cpStore); err != nil {
		t.Fatalf("load shares: %v", err)
	}

	// The real sweeper, subscribed exactly as Start does in adapter.go.
	swept := make(chan struct{}, 8)
	sweeper := newAuthSweeper(func(context.Context) { swept <- struct{}{} })
	t.Cleanup(func() { sweeper.stop(context.Background()) })
	unsub := rt.OnAuthCacheInvalidate(sweeper.request)
	t.Cleanup(unsub)

	router := cpapi.NewRouter(rt, jwtSvc, cpStore, false,
		cpapi.Timeouts{Restore: time.Minute, DrainStall: time.Minute})
	pair, err := jwtSvc.GenerateTokenPair(&models.User{ID: "u", Username: "admin", Role: string(models.RoleAdmin)})
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	// Drain anything share loading raised before the request under test.
	drain(swept, 200*time.Millisecond)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/export",
		strings.NewReader(`{"default_permission":"read-write"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("default-permission update = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	// One sweep must happen: a change to who may access the share has to reach
	// established SMB trees.
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("default-permission change scheduled no sweep at all; established SMB trees keep the old share access")
	}

	// A second must not. In the two-signal shape the second token is already
	// queued when the first sweep returns, so it starts immediately — this
	// window is orders of magnitude more than it needs.
	select {
	case <-swept:
		t.Error("default-permission change scheduled a SECOND full sweep; " +
			"one change to who may access a share should cost one walk of every session and tree")
	case <-time.After(500 * time.Millisecond):
	}

	// The reconcile must still have run: the new default permission has to
	// reach the root directory's EVERYONE@ ACE, or the grantees it stands for
	// cannot traverse a root owned by uid 0 with mode 0755.
	ms := rt.GetMetadataService()
	handle, err := ms.GetRootHandle(ctx, "/export")
	if err != nil {
		t.Fatalf("root handle: %v", err)
	}
	f, err := ms.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("read root inode: %v", err)
	}
	if f.ACL == nil {
		t.Fatal("root inode has no ACL after the default-permission change")
	}
	// The production builder is the oracle: the stored ACL must be what a
	// read-write default with no per-principal grants projects to.
	want := acl.BuildShareRootACL(acl.GrantReadWrite, nil)
	if !reflect.DeepEqual(f.ACL.ACEs, want.ACEs) {
		t.Errorf("default-permission change was not projected onto the root ACL:\n got  %+v\n want %+v",
			f.ACL.ACEs, want.ACEs)
	}
}

// drain empties c until it stays quiet for the given window.
func drain(c <-chan struct{}, quiet time.Duration) {
	for {
		select {
		case <-c:
		case <-time.After(quiet):
			return
		}
	}
}
