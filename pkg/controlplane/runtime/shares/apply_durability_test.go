package shares

import (
	"context"
	"testing"

	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newDurabilityTestShare brings up one share on the journal tier, which is
// where a share starts unless it says otherwise.
func newDurabilityTestShare(t *testing.T) (*Service, string) {
	t.Helper()
	ctx := context.Background()

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
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
		nil,
	); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	t.Cleanup(svc.CloseBlockStores)
	return svc, name
}

// TestApplyDurability_TakesEffectOnTheRunningShare pins the point of the call:
// both axes are otherwise read only when the block store is built, so without
// this the setting would sit in the database while acknowledgement carried on
// under the old terms.
func TestApplyDurability_TakesEffectOnTheRunningShare(t *testing.T) {
	svc, shareName := newDurabilityTestShare(t)

	bs, err := svc.GetBlockStoreForShare(shareName)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}
	if bs.RequireDurableCommit() {
		t.Fatalf("the fixture must start on the journal tier for the change to be observable")
	}

	if err := svc.ApplyDurability(shareName, true, true, nil); err != nil {
		t.Fatalf("ApplyDurability: %v", err)
	}
	if !bs.RequireDurableCommit() {
		t.Error("a COMMIT still acknowledges at the journal after the share was moved to the block-store tier")
	}

	share, err := svc.GetShare(shareName)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if !share.writeback {
		t.Error("the registry still reports the share's old relaxed-metadata setting")
	}

	// Back again: the call must be a toggle, not a one-way tightening.
	if err := svc.ApplyDurability(shareName, false, false, nil); err != nil {
		t.Fatalf("ApplyDurability (back): %v", err)
	}
	if bs.RequireDurableCommit() {
		t.Error("the share could not be moved back to the journal tier")
	}
}

// TestApplyDurability_UnknownShare refuses rather than reporting success for a
// share that is not running, so the caller can tell the operator the change
// needs a restart instead of silently doing nothing.
func TestApplyDurability_UnknownShare(t *testing.T) {
	svc, _ := newDurabilityTestShare(t)
	if err := svc.ApplyDurability("/not-running", true, false, nil); err == nil {
		t.Fatal("got nil, want an error for a share with no running block store")
	}
}
