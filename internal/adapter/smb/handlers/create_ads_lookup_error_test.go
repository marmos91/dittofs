package handlers

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Coverage for the ADS base-file lookup error surface on CREATE: when the
// base-file lookup fails with a real error (not NotFound), that error must
// become the create status. Before the fix, the lookup error was discarded and
// the auto-create path ran anyway, masking a store failure as a fresh
// file+stream create.

// adsLookupFailStore wraps the memory metadata store and fails the SECOND
// listing of the seeded parent directory. The main base-file lookup performs
// its own listing first (the exact-case GetChild misses and falls to
// ListChildren); failing that first listing would return the error from the
// main lookup before the fixed ADS-base check runs. Counting listings and
// failing only the second one lets the main lookup succeed through listing so
// the injected failure lands on the adsBaseFileName lookup the fix governs.
type adsLookupFailStore struct {
	metadata.Store
	parent   metadata.FileHandle
	listings int
}

func (s *adsLookupFailStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int, attrs metadata.ChildAttrs) ([]metadata.DirEntry, string, error) {
	if bytes.Equal(dirHandle, s.parent) && cursor == "" {
		s.listings++
		if s.listings == 2 {
			return nil, "", fmt.Errorf("injected list failure")
		}
	}
	return s.Store.ListChildren(ctx, dirHandle, cursor, limit, attrs)
}

// TestCreate_ADSLookupErrorSurfacesAsStatus drives an isADS create whose base
// does not exist and whose base-file listing fails: the create must fail with
// the mapped lookup error, not silently create the base file and the stream.
func TestCreate_ADSLookupErrorSurfacesAsStatus(t *testing.T) {
	rt := runtime.New(nil)

	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	failStore := &adsLookupFailStore{Store: memStore, parent: nil}
	if err := rt.RegisterMetadataStore("test-meta", failStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	shareName := "/ads-lookup-err"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	failStore.parent = rootHandle

	h := NewHandler()
	h.Registry = rt
	// Register the authenticated session the CREATE dispatch requires (SessionId
	// lookup) with root identity so the share-root write gate passes and the test
	// reaches the ADS base-file lookup.
	rootUID := uint32(0)
	h.CreateSessionWithUser(1, "127.0.0.1:1234", &models.User{Username: "root", UID: &rootUID, Enabled: true}, "")

	tree := &TreeConnection{TreeID: 1, SessionID: 1, ShareName: shareName}
	h.StoreTree(tree)

	smbCtx := &SMBHandlerContext{
		Context:   context.Background(),
		SessionID: 1,
		TreeID:    1,
		ShareName: shareName,
	}

	resp, err := h.Create(smbCtx, &CreateRequest{
		FileName:          "base.txt:stream",
		CreateDisposition: types.FileCreate,
		DesiredAccess:     uint32(types.FileWriteData | types.FileReadData),
		ShareAccess:       uint32(types.FileShareRead | types.FileShareWrite),
		CreateOptions:     0,
	})
	if err != nil {
		t.Fatalf("Create returned transport error: %v", err)
	}
	if failStore.listings < 2 {
		t.Fatalf("ADS base listing was never attempted; test does not exercise the lookup path (status=%v err=%v)", resp.Status, err)
	}
	if resp.Status == types.StatusSuccess {
		t.Fatalf("create succeeded despite the failing ADS base lookup; the lookup error was masked")
	}
	if !resp.Status.IsError() {
		t.Fatalf("status %v does not look like a mapped error", resp.Status)
	}

	// The base file must NOT have been auto-created by the masked path.
	if _, childErr := failStore.Store.GetChild(context.Background(), rootHandle, "base.txt"); childErr == nil {
		t.Fatalf("base.txt was auto-created despite the lookup failure; the create status lied about the stream")
	}
}
