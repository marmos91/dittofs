package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

func TestNew(t *testing.T) {
	rt := New(nil)

	if rt == nil {
		t.Fatal("expected non-nil runtime")
	}

	if rt.storesSvc == nil {
		t.Error("expected stores service to be initialized")
	}
	if rt.sharesSvc == nil {
		t.Error("expected shares service to be initialized")
	}
	if rt.mountTracker == nil {
		t.Error("expected mount tracker to be initialized")
	}
	if rt.adaptersSvc == nil {
		t.Error("expected adapters service to be initialized")
	}
	if rt.lifecycleSvc == nil {
		t.Error("expected lifecycle service to be initialized")
	}
	if rt.identitySvc == nil {
		t.Error("expected identity service to be initialized")
	}
}

func TestSetShutdownTimeout(t *testing.T) {
	rt := New(nil)

	t.Run("set custom timeout does not panic", func(t *testing.T) {
		rt.SetShutdownTimeout(60 * time.Second)
		// Timeout is delegated to adapters and lifecycle sub-services;
		// we verify it doesn't panic.
	})

	t.Run("zero uses default does not panic", func(t *testing.T) {
		rt.SetShutdownTimeout(0)
		// Zero is normalized to DefaultShutdownTimeout in sub-services.
	})
}

func TestRegisterMetadataStore(t *testing.T) {
	rt := New(nil)
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()

	t.Run("register valid store", func(t *testing.T) {
		err := rt.RegisterMetadataStore("test-store", metaStore)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("duplicate registration fails", func(t *testing.T) {
		err := rt.RegisterMetadataStore("test-store", metaStore)
		if err == nil {
			t.Fatal("expected error for duplicate registration")
		}
	})

	t.Run("nil store fails", func(t *testing.T) {
		err := rt.RegisterMetadataStore("nil-store", nil)
		if err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("empty name fails", func(t *testing.T) {
		err := rt.RegisterMetadataStore("", metaStore)
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})
}

func TestGetMetadataStore(t *testing.T) {
	rt := New(nil)
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()
	_ = rt.RegisterMetadataStore("test-store", metaStore)

	t.Run("existing store", func(t *testing.T) {
		store, err := rt.GetMetadataStore("test-store")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if store != metaStore {
			t.Error("returned store doesn't match registered store")
		}
	})

	t.Run("non-existing store", func(t *testing.T) {
		_, err := rt.GetMetadataStore("not-found")
		if err == nil {
			t.Fatal("expected error for non-existing store")
		}
	})
}

func TestListMetadataStores(t *testing.T) {
	rt := New(nil)

	t.Run("empty list", func(t *testing.T) {
		names := rt.ListMetadataStores()
		if len(names) != 0 {
			t.Errorf("expected empty list, got %d items", len(names))
		}
	})

	t.Run("with registered stores", func(t *testing.T) {
		_ = rt.RegisterMetadataStore("store1", memory.NewMemoryMetadataStoreWithDefaults())
		_ = rt.RegisterMetadataStore("store2", memory.NewMemoryMetadataStoreWithDefaults())

		names := rt.ListMetadataStores()
		if len(names) != 2 {
			t.Errorf("expected 2 stores, got %d", len(names))
		}
	})
}

func TestCountMetadataStores(t *testing.T) {
	rt := New(nil)

	if rt.CountMetadataStores() != 0 {
		t.Errorf("expected 0, got %d", rt.CountMetadataStores())
	}

	_ = rt.RegisterMetadataStore("store1", memory.NewMemoryMetadataStoreWithDefaults())
	if rt.CountMetadataStores() != 1 {
		t.Errorf("expected 1, got %d", rt.CountMetadataStores())
	}
}

func TestMountTracking(t *testing.T) {
	rt := New(nil)

	t.Run("record mount", func(t *testing.T) {
		rt.RecordMount("192.168.1.100:12345", "/export", 1000)

		mounts := rt.ListMounts()
		if len(mounts) != 1 {
			t.Fatalf("expected 1 mount, got %d", len(mounts))
		}
		if mounts[0].ClientAddr != "192.168.1.100:12345" {
			t.Errorf("expected client addr '192.168.1.100:12345', got %q", mounts[0].ClientAddr)
		}
		if mounts[0].ShareName != "/export" {
			t.Errorf("expected share '/export', got %q", mounts[0].ShareName)
		}
		if mounts[0].MountTime != 1000 {
			t.Errorf("expected mount time 1000, got %d", mounts[0].MountTime)
		}
	})

	t.Run("remove mount", func(t *testing.T) {
		removed := rt.RemoveMount("192.168.1.100:12345")
		if !removed {
			t.Error("expected mount to be removed")
		}

		removed = rt.RemoveMount("192.168.1.100:12345")
		if removed {
			t.Error("expected false for already removed mount")
		}

		if len(rt.ListMounts()) != 0 {
			t.Error("expected no mounts after removal")
		}
	})

	t.Run("remove non-existing mount", func(t *testing.T) {
		removed := rt.RemoveMount("non-existing")
		if removed {
			t.Error("expected false for non-existing mount")
		}
	})

	t.Run("remove all mounts", func(t *testing.T) {
		rt.RecordMount("client1", "/share1", 1000)
		rt.RecordMount("client2", "/share2", 2000)
		rt.RecordMount("client3", "/share3", 3000)

		count := rt.RemoveAllMounts()
		if count != 3 {
			t.Errorf("expected 3 removed, got %d", count)
		}

		if len(rt.ListMounts()) != 0 {
			t.Error("expected no mounts after removing all")
		}
	})
}

func TestListMountsIsolation(t *testing.T) {
	rt := New(nil)
	rt.RecordMount("client1", "/share1", 1000)

	mounts := rt.ListMounts()
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}

	// Modify returned mount to verify it's a copy
	mounts[0].ClientAddr = "modified"

	// Original should be unchanged
	originalMounts := rt.ListMounts()
	if originalMounts[0].ClientAddr != "client1" {
		t.Error("ListMounts should return copies, not references")
	}
}

func TestShareOperations(t *testing.T) {
	rt := New(nil)
	ctx := context.Background()
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()
	_ = rt.RegisterMetadataStore("test-meta", metaStore)

	t.Run("add share", func(t *testing.T) {
		config := &ShareConfig{
			Name:          "/export",
			MetadataStore: "test-meta",
		}

		err := rt.AddShare(ctx, config)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !rt.ShareExists("/export") {
			t.Error("share should exist after adding")
		}
	})

	t.Run("add share with empty name fails", func(t *testing.T) {
		config := &ShareConfig{
			Name:          "",
			MetadataStore: "test-meta",
		}

		err := rt.AddShare(ctx, config)
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})

	t.Run("add duplicate share fails", func(t *testing.T) {
		config := &ShareConfig{
			Name:          "/export",
			MetadataStore: "test-meta",
		}

		err := rt.AddShare(ctx, config)
		if err == nil {
			t.Fatal("expected error for duplicate share")
		}
	})

	t.Run("add share with non-existing metadata store fails", func(t *testing.T) {
		config := &ShareConfig{
			Name:          "/new-share",
			MetadataStore: "non-existing",
		}

		err := rt.AddShare(ctx, config)
		if err == nil {
			t.Fatal("expected error for non-existing metadata store")
		}
	})

	t.Run("get share", func(t *testing.T) {
		share, err := rt.GetShare("/export")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if share.Name != "/export" {
			t.Errorf("expected name '/export', got %q", share.Name)
		}
	})

	t.Run("get non-existing share", func(t *testing.T) {
		_, err := rt.GetShare("/not-found")
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})

	t.Run("list shares", func(t *testing.T) {
		names := rt.ListShares()
		if len(names) != 1 {
			t.Errorf("expected 1 share, got %d", len(names))
		}
	})

	t.Run("count shares", func(t *testing.T) {
		if rt.CountShares() != 1 {
			t.Errorf("expected 1, got %d", rt.CountShares())
		}
	})

	t.Run("get root handle", func(t *testing.T) {
		handle, err := rt.GetRootHandle("/export")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if handle == nil {
			t.Error("expected non-nil root handle")
		}
	})

	t.Run("get root handle for non-existing share", func(t *testing.T) {
		_, err := rt.GetRootHandle("/not-found")
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})

	t.Run("update share", func(t *testing.T) {
		readOnly := true
		perm := "read"
		err := rt.UpdateShare("/export", &readOnly, &perm)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		share, _ := rt.GetShare("/export")
		if !share.ReadOnly {
			t.Error("expected ReadOnly to be true")
		}
		if share.DefaultPermission != "read" {
			t.Errorf("expected permission 'read', got %q", share.DefaultPermission)
		}
	})

	t.Run("update non-existing share fails", func(t *testing.T) {
		readOnly := true
		err := rt.UpdateShare("/not-found", &readOnly, nil)
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})

	t.Run("remove share", func(t *testing.T) {
		err := rt.RemoveShare("/export")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rt.ShareExists("/export") {
			t.Error("share should not exist after removal")
		}
	})

	t.Run("remove non-existing share fails", func(t *testing.T) {
		err := rt.RemoveShare("/not-found")
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})
}

func TestApplyIdentityMapping(t *testing.T) {
	// Helper to create a fresh runtime with a share for each test
	setupRuntime := func(squash models.SquashMode) *Runtime {
		rt := New(nil)
		rt.sharesSvc.InjectShareForTesting(&Share{
			Name:         "/export",
			Squash:       squash,
			AnonymousUID: 65534,
			AnonymousGID: 65534,
		})
		return rt
	}

	// Helper to create identity with UID/GID
	makeIdentity := func(uid, gid uint32, username string) *metadata.Identity {
		return &metadata.Identity{
			UID:      &uid,
			GID:      &gid,
			Username: username,
		}
	}

	t.Run("AUTH_NULL always maps to anonymous", func(t *testing.T) {
		rt := setupRuntime(models.SquashRootToAdmin)
		identity := &metadata.Identity{UID: nil, GID: nil}

		effective, err := rt.ApplyIdentityMapping("/export", identity)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if *effective.UID != 65534 || *effective.GID != 65534 {
			t.Errorf("expected anonymous (65534/65534), got (%d/%d)", *effective.UID, *effective.GID)
		}
	})

	t.Run("non-existing share fails", func(t *testing.T) {
		rt := setupRuntime(models.SquashRootToAdmin)
		uid := uint32(1000)
		identity := &metadata.Identity{UID: &uid}

		_, err := rt.ApplyIdentityMapping("/not-found", identity)
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})

	// Table-driven tests for squash modes
	tests := []struct {
		name       string
		squash     models.SquashMode
		inputUID   uint32
		inputGID   uint32
		wantUID    uint32
		wantGID    uint32
		wantIsAnon bool // true if username should be "anonymous(N)"
		wantIsRoot bool // true if username should be "root"
	}{
		// SquashNone: all UIDs pass through unchanged
		{"none/normal_user", models.SquashNone, 1000, 1000, 1000, 1000, false, false},
		{"none/root", models.SquashNone, 0, 0, 0, 0, false, false},

		// SquashRootToAdmin: all UIDs pass through unchanged (root keeps admin)
		{"root_to_admin/normal_user", models.SquashRootToAdmin, 1000, 1000, 1000, 1000, false, false},
		{"root_to_admin/root", models.SquashRootToAdmin, 0, 0, 0, 0, false, false},

		// SquashRootToGuest: root mapped to anonymous, others pass through
		{"root_to_guest/normal_user", models.SquashRootToGuest, 1000, 1000, 1000, 1000, false, false},
		{"root_to_guest/root", models.SquashRootToGuest, 0, 0, 65534, 65534, true, false},

		// SquashAllToAdmin: all users mapped to root
		{"all_to_admin/normal_user", models.SquashAllToAdmin, 1000, 1000, 0, 0, false, true},
		{"all_to_admin/root", models.SquashAllToAdmin, 0, 0, 0, 0, false, true},

		// SquashAllToGuest: all users mapped to anonymous
		{"all_to_guest/normal_user", models.SquashAllToGuest, 1000, 1000, 65534, 65534, true, false},
		{"all_to_guest/root", models.SquashAllToGuest, 0, 0, 65534, 65534, true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := setupRuntime(tc.squash)
			identity := makeIdentity(tc.inputUID, tc.inputGID, "testuser")

			effective, err := rt.ApplyIdentityMapping("/export", identity)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if *effective.UID != tc.wantUID {
				t.Errorf("expected UID %d, got %d", tc.wantUID, *effective.UID)
			}
			if *effective.GID != tc.wantGID {
				t.Errorf("expected GID %d, got %d", tc.wantGID, *effective.GID)
			}

			if tc.wantIsAnon && effective.Username != "anonymous(65534)" {
				t.Errorf("expected anonymous username, got %q", effective.Username)
			}
			if tc.wantIsRoot && effective.Username != "root" {
				t.Errorf("expected 'root' username, got %q", effective.Username)
			}
		})
	}
}

func TestGetMetadataStoreForShare(t *testing.T) {
	rt := New(nil)
	ctx := context.Background()
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()
	_ = rt.RegisterMetadataStore("test-meta", metaStore)

	config := &ShareConfig{
		Name:          "/export",
		MetadataStore: "test-meta",
	}
	_ = rt.AddShare(ctx, config)

	t.Run("existing share", func(t *testing.T) {
		store, err := rt.GetMetadataStoreForShare("/export")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if store != metaStore {
			t.Error("returned store doesn't match expected store")
		}
	})

	t.Run("non-existing share", func(t *testing.T) {
		_, err := rt.GetMetadataStoreForShare("/not-found")
		if err == nil {
			t.Fatal("expected error for non-existing share")
		}
	})
}

func TestGetServices(t *testing.T) {
	rt := New(nil)

	t.Run("get metadata service", func(t *testing.T) {
		svc := rt.GetMetadataService()
		if svc == nil {
			t.Error("expected non-nil metadata service")
		}
	})

	t.Run("get payload service (nil when not set)", func(t *testing.T) {
		svc := rt.GetPayloadService()
		if svc != nil {
			t.Error("expected nil payload service when not set")
		}
	})

	t.Run("get block service returns payload service", func(t *testing.T) {
		// GetBlockService is deprecated but should return same as GetPayloadService
		svc := rt.GetBlockService()
		if svc != rt.GetPayloadService() {
			t.Error("GetBlockService should return same as GetPayloadService")
		}
	})
}

func TestAdapterManagementBasics(t *testing.T) {
	rt := New(nil)

	t.Run("list running adapters empty", func(t *testing.T) {
		adapters := rt.ListRunningAdapters()
		if len(adapters) != 0 {
			t.Errorf("expected empty list, got %d items", len(adapters))
		}
	})

	t.Run("is adapter running false for non-existing", func(t *testing.T) {
		if rt.IsAdapterRunning("nfs") {
			t.Error("expected false for non-existing adapter")
		}
	})

	t.Run("set adapter factory", func(t *testing.T) {
		rt.SetAdapterFactory(func(cfg *models.AdapterConfig) (ProtocolAdapter, error) {
			return nil, nil
		})
		// Factory should be set (no error means success)
	})
}

func TestCloseMetadataStores(t *testing.T) {
	rt := New(nil)

	// Register a memory store (which implements io.Closer via its Close method if any)
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()
	_ = rt.RegisterMetadataStore("test-store", metaStore)

	// CloseMetadataStores should not panic and should handle stores gracefully
	rt.CloseMetadataStores()

	// After close, stores are still in the map (we don't remove them)
	if rt.CountMetadataStores() != 1 {
		t.Error("stores should still be registered after close")
	}
}

func TestStore(t *testing.T) {
	rt := New(nil)

	t.Run("nil store", func(t *testing.T) {
		if rt.Store() != nil {
			t.Error("expected nil store")
		}
	})
}
