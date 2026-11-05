package runtime

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/controlplane/runtime/shares"
	"github.com/cubbit/dittofs/pkg/metadata"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// TestRuntimeTrash_RecycleListEmpty_EndToEnd goes through the runtime's own
// trash wiring: the metadata.TrashPolicy New installs makes RemoveFile on a
// trash-enabled share recycle rather than destroy, and rt.Trash().List and
// Empty run through the real trashDeps.
func TestRuntimeTrash_RecycleListEmpty_EndToEnd(t *testing.T) {
	ctx := context.Background()
	rt, bsID := newRuntimeWithBlockStore(t)
	if err := rt.RegisterMetadataStore("test-meta", badgertest.NewInMemory(t)); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	const share = "/trash"
	if err := rt.AddShare(ctx, &ShareConfig{Name: share, MetadataStore: "test-meta", BlockStoreID: bsID}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	if err := rt.sharesSvc.SetShareTrashConfig(nil, share, shares.TrashSettings{Enabled: true}); err != nil {
		t.Fatalf("SetShareTrashConfig: %v", err)
	}

	root, err := rt.GetRootHandle(share)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	actx := metadata.NewSystemAuthContext(ctx)
	svc := rt.GetMetadataService()
	const name = "doc.txt"
	if _, _, err := svc.CreateFile(actx, root, name, &metadata.FileAttr{Mode: 0o644}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	removed, _, err := svc.RemoveFile(actx, root, name)
	if err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if removed.PayloadID != "" {
		t.Fatalf("RemoveFile destroyed the file (PayloadID %q), want it recycled", removed.PayloadID)
	}
	if _, err := svc.GetChild(ctx, root, name); !metadata.IsNotFoundError(err) {
		t.Fatalf("%q still in the root after recycle: err=%v", name, err)
	}

	entries, err := rt.Trash().List(actx, share)
	if err != nil {
		t.Fatalf("Trash().List: %v", err)
	}
	if len(entries) != 1 || entries[0].IsDir || (entries[0].OriginalPath != name && entries[0].OriginalPath != "/"+name) {
		t.Fatalf("Trash().List = %+v, want one file %q", entries, name)
	}

	n, err := rt.Trash().Empty(actx, share, false)
	if err != nil {
		t.Fatalf("Trash().Empty: %v", err)
	}
	if n != 1 {
		t.Fatalf("Trash().Empty removed %d, want 1", n)
	}
	if after, err := rt.Trash().List(actx, share); err != nil || len(after) != 0 {
		t.Fatalf("Trash().List after Empty = %+v, %v; want empty", after, err)
	}
}
