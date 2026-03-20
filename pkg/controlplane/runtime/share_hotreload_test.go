package runtime

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
)

// addShareViaRuntime creates a share in the DB and adds it to the runtime.
// This mirrors what the API handler does: DB create + runtime AddShare.
func addShareViaRuntime(t *testing.T, rt *Runtime, s cpstore.Store, shareName, localID string) {
	t.Helper()
	ctx := context.Background()
	metaStores, _ := s.ListMetadataStores(ctx)
	share := &models.Share{
		Name:              shareName,
		MetadataStoreID:   metaStores[0].ID,
		LocalBlockStoreID: localID,
	}
	if _, err := s.CreateShare(ctx, share); err != nil {
		t.Fatalf("failed to create share in DB: %v", err)
	}
	cfg := &ShareConfig{
		Name:              shareName,
		MetadataStore:     "test-meta",
		LocalBlockStoreID: localID,
	}
	if err := rt.AddShare(ctx, cfg); err != nil {
		t.Fatalf("AddShare failed: %v", err)
	}
}

func TestShareHotReload_AddTriggersCallback(t *testing.T) {
	rt, s := setupTestRuntime(t)
	localID := createLocalBlockStoreConfig(t, s, "local-hot-add")

	ch := make(chan []string, 1)
	unsubscribe := rt.OnShareChange(func(shares []string) {
		ch <- shares
	})
	defer unsubscribe()

	addShareViaRuntime(t, rt, s, "/hot-add", localID)

	select {
	case shares := <-ch:
		sort.Strings(shares)
		found := false
		for _, name := range shares {
			if name == "/hot-add" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("callback share list %v does not contain /hot-add", shares)
		}
	case <-time.After(time.Second):
		t.Fatal("callback not invoked within 1s after AddShare")
	}
}

func TestShareHotReload_RemoveTriggersCallback(t *testing.T) {
	rt, s := setupTestRuntime(t)
	localID := createLocalBlockStoreConfig(t, s, "local-hot-remove")

	// Add share first (consume callback).
	addCh := make(chan []string, 1)
	unsubscribe := rt.OnShareChange(func(shares []string) {
		addCh <- shares
	})

	addShareViaRuntime(t, rt, s, "/hot-remove", localID)

	select {
	case <-addCh:
		// consumed
	case <-time.After(time.Second):
		t.Fatal("callback not invoked within 1s after AddShare")
	}
	unsubscribe()

	// Register a fresh callback for the remove.
	removeCh := make(chan []string, 1)
	unsubRemove := rt.OnShareChange(func(shares []string) {
		removeCh <- shares
	})
	defer unsubRemove()

	if err := rt.RemoveShare("/hot-remove"); err != nil {
		t.Fatalf("RemoveShare failed: %v", err)
	}

	select {
	case shares := <-removeCh:
		for _, name := range shares {
			if name == "/hot-remove" {
				t.Errorf("callback share list %v should not contain /hot-remove after removal", shares)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("callback not invoked within 1s after RemoveShare")
	}
}

func TestShareHotReload_MultipleCallbacks(t *testing.T) {
	rt, s := setupTestRuntime(t)
	localID := createLocalBlockStoreConfig(t, s, "local-hot-multi")

	ch1 := make(chan []string, 1)
	ch2 := make(chan []string, 1)

	unsub1 := rt.OnShareChange(func(shares []string) {
		ch1 <- shares
	})
	defer unsub1()

	unsub2 := rt.OnShareChange(func(shares []string) {
		ch2 <- shares
	})
	defer unsub2()

	addShareViaRuntime(t, rt, s, "/hot-multi", localID)

	var shares1, shares2 []string
	for i := 0; i < 2; i++ {
		select {
		case s := <-ch1:
			shares1 = s
		case s := <-ch2:
			shares2 = s
		case <-time.After(time.Second):
			t.Fatal("not all callbacks invoked within 1s")
		}
	}

	if shares1 == nil || shares2 == nil {
		t.Fatal("both callbacks should have received a notification")
	}

	sort.Strings(shares1)
	sort.Strings(shares2)

	if len(shares1) != len(shares2) {
		t.Errorf("callbacks received different list lengths: %v vs %v", shares1, shares2)
	}
}

func TestShareHotReload_Unsubscribe(t *testing.T) {
	rt, s := setupTestRuntime(t)
	localID := createLocalBlockStoreConfig(t, s, "local-hot-unsub")

	ch1 := make(chan []string, 1)
	ch2 := make(chan []string, 1)

	unsub1 := rt.OnShareChange(func(shares []string) {
		ch1 <- shares
	})
	defer unsub1()

	unsub2 := rt.OnShareChange(func(shares []string) {
		ch2 <- shares
	})

	// Unsubscribe callback2 BEFORE adding the share.
	unsub2()

	addShareViaRuntime(t, rt, s, "/hot-unsub", localID)

	// callback1 should fire.
	select {
	case <-ch1:
		// expected
	case <-time.After(time.Second):
		t.Fatal("subscribed callback not invoked within 1s")
	}

	// callback2 should NOT fire.
	select {
	case <-ch2:
		t.Fatal("unsubscribed callback should not fire")
	case <-time.After(100 * time.Millisecond):
		// expected: callback did not fire
	}
}

func TestShareHotReload_FullLifecycle(t *testing.T) {
	rt, s := setupTestRuntime(t)
	localID := createLocalBlockStoreConfig(t, s, "local-hot-lifecycle")

	ch := make(chan []string, 2)
	unsub := rt.OnShareChange(func(shares []string) {
		ch <- shares
	})
	defer unsub()

	// Add share.
	addShareViaRuntime(t, rt, s, "/hot-lifecycle", localID)

	select {
	case shares := <-ch:
		found := false
		for _, name := range shares {
			if name == "/hot-lifecycle" {
				found = true
			}
		}
		if !found {
			t.Errorf("after add: callback list %v missing /hot-lifecycle", shares)
		}
	case <-time.After(time.Second):
		t.Fatal("callback not invoked within 1s after AddShare")
	}

	// Verify ListShares contains the share.
	shareList := rt.ListShares()
	found := false
	for _, name := range shareList {
		if name == "/hot-lifecycle" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListShares %v does not contain /hot-lifecycle", shareList)
	}

	// Verify ShareExists.
	if !rt.ShareExists("/hot-lifecycle") {
		t.Error("ShareExists should return true for /hot-lifecycle")
	}

	// Remove share.
	if err := rt.RemoveShare("/hot-lifecycle"); err != nil {
		t.Fatalf("RemoveShare failed: %v", err)
	}

	select {
	case shares := <-ch:
		for _, name := range shares {
			if name == "/hot-lifecycle" {
				t.Errorf("after remove: callback list %v should not contain /hot-lifecycle", shares)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("callback not invoked within 1s after RemoveShare")
	}

	// Verify ListShares excludes the share.
	shareList = rt.ListShares()
	for _, name := range shareList {
		if name == "/hot-lifecycle" {
			t.Errorf("ListShares %v should not contain /hot-lifecycle after removal", shareList)
		}
	}

	// Verify ShareExists returns false.
	if rt.ShareExists("/hot-lifecycle") {
		t.Error("ShareExists should return false for /hot-lifecycle after removal")
	}
}

func TestShareHotReload_SequentialAdds(t *testing.T) {
	rt, s := setupTestRuntime(t)
	localID1 := createLocalBlockStoreConfig(t, s, "local-hot-seq-1")
	localID2 := createLocalBlockStoreConfig(t, s, "local-hot-seq-2")

	ch := make(chan []string, 2)
	unsub := rt.OnShareChange(func(shares []string) {
		ch <- shares
	})
	defer unsub()

	// Add first share.
	addShareViaRuntime(t, rt, s, "/hot-seq-1", localID1)

	select {
	case shares := <-ch:
		found := false
		for _, name := range shares {
			if name == "/hot-seq-1" {
				found = true
			}
		}
		if !found {
			t.Errorf("after first add: callback list %v missing /hot-seq-1", shares)
		}
	case <-time.After(time.Second):
		t.Fatal("callback not invoked within 1s after first AddShare")
	}

	// Add second share.
	addShareViaRuntime(t, rt, s, "/hot-seq-2", localID2)

	select {
	case shares := <-ch:
		sort.Strings(shares)
		foundFirst := false
		foundSecond := false
		for _, name := range shares {
			if name == "/hot-seq-1" {
				foundFirst = true
			}
			if name == "/hot-seq-2" {
				foundSecond = true
			}
		}
		if !foundFirst {
			t.Errorf("after second add: callback list %v missing /hot-seq-1", shares)
		}
		if !foundSecond {
			t.Errorf("after second add: callback list %v missing /hot-seq-2", shares)
		}
	case <-time.After(time.Second):
		t.Fatal("callback not invoked within 1s after second AddShare")
	}
}
