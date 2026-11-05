package shares

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// newDurabilityTestShare brings up one share on the strict metadata tier,
// which is where a share starts unless it says otherwise.
func newDurabilityTestShare(t *testing.T) (*Service, string) {
	t.Helper()
	ctx := context.Background()

	mds := badgertest.NewInMemory(t)
	t.Cleanup(func() { _ = mds.Close() })

	svc := New()
	const name = "/durable"
	if err := svc.AddShare(
		ctx,
		&ShareConfig{Name: name, MetadataStore: "meta-test", Enabled: true, BlockStoreID: "blocks"},
		&metaStoreProvider{name: "meta-test", store: mds},
		metaSvcRegistrar{},
		memBlockStoreProvider{},
		&LocalStoreDefaults{JournalRoot: t.TempDir()},
	); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	t.Cleanup(func() { svc.CloseBlockStores(context.Background()) })
	return svc, name
}

// TestApplyDurability_TakesEffectOnTheRunningShare pins the point of the call:
// the relaxed metadata tier is otherwise read only at AddShare, so without this
// the setting would sit in the database while the share ran on the old terms.
func TestApplyDurability_TakesEffectOnTheRunningShare(t *testing.T) {
	svc, shareName := newDurabilityTestShare(t)

	if err := svc.ApplyDurability(shareName, true, nil); err != nil {
		t.Fatalf("ApplyDurability: %v", err)
	}
	share, err := svc.GetShare(shareName)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if !share.writeback {
		t.Error("the registry still reports the share's old relaxed-metadata setting")
	}

	// Back again: the call must be a toggle, not a one-way change.
	if err := svc.ApplyDurability(shareName, false, nil); err != nil {
		t.Fatalf("ApplyDurability (back): %v", err)
	}
	if share, _ = svc.GetShare(shareName); share.writeback {
		t.Error("the share could not be moved back off the relaxed tier")
	}
}

// TestApplyDurability_UnknownShare refuses rather than reporting success for a
// share that is not running, so the caller can tell the operator the change
// needs a restart instead of silently doing nothing.
func TestApplyDurability_UnknownShare(t *testing.T) {
	svc, _ := newDurabilityTestShare(t)
	if err := svc.ApplyDurability("/not-running", false, nil); err == nil {
		t.Fatal("got nil, want an error for a share that is not running")
	}
}
