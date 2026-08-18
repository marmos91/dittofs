package badger

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// A share name containing the key separator must not leak its handles into
// another share's listing.
func TestListDurableHandlesByShare_SeparatorInShareName(t *testing.T) {
	s := newDurableStoreForTest(t)
	ctx := context.Background()

	nested := durableHandleForFile("nested", [16]byte{1})
	nested.ShareName = "export:sub"
	if err := s.PutDurableHandle(ctx, nested); err != nil {
		t.Fatalf("put nested handle: %v", err)
	}

	own := durableHandleForFile("own", [16]byte{2})
	own.ShareName = "export"
	if err := s.PutDurableHandle(ctx, own); err != nil {
		t.Fatalf("put own handle: %v", err)
	}

	handles, err := s.ListDurableHandlesByShare(ctx, "export")
	if err != nil {
		t.Fatalf("list by share: %v", err)
	}
	if len(handles) != 1 || handles[0].ID != "own" {
		t.Fatalf("expected only the \"export\" handle, got %v", handleIDs(handles))
	}

	nestedHandles, err := s.ListDurableHandlesByShare(ctx, "export:sub")
	if err != nil {
		t.Fatalf("list nested share: %v", err)
	}
	if len(nestedHandles) != 1 || nestedHandles[0].ID != "nested" {
		t.Fatalf("expected only the \"export:sub\" handle, got %v", handleIDs(nestedHandles))
	}
}

func handleIDs(handles []*lock.PersistedDurableHandle) []string {
	ids := make([]string, 0, len(handles))
	for _, h := range handles {
		ids = append(ids, h.ID)
	}
	return ids
}
