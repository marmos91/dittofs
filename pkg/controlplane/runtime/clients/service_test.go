package clients

import (
	"context"
	"testing"
	"time"
)

func TestRegisterAndList(t *testing.T) {
	reg := NewRegistry(0)
	rec := &ClientRecord{
		ClientID: "client-1",
		Protocol: "nfs",
		Address:  "192.168.1.1:1234",
		User:     "alice",
		Shares:   []string{"/export"},
		NFS: &NfsDetails{
			Version:    "4.1",
			AuthFlavor: "AUTH_UNIX",
			UID:        1000,
			GID:        1000,
		},
	}
	reg.Register(rec)

	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 client, got %d", len(list))
	}
	c := list[0]
	if c.ClientID != "client-1" {
		t.Errorf("expected client-1, got %s", c.ClientID)
	}
	if c.Protocol != "nfs" {
		t.Errorf("expected nfs, got %s", c.Protocol)
	}
	if c.Address != "192.168.1.1:1234" {
		t.Errorf("expected address, got %s", c.Address)
	}
	if c.User != "alice" {
		t.Errorf("expected alice, got %s", c.User)
	}
	if c.ConnectedAt.IsZero() {
		t.Error("ConnectedAt should be set automatically")
	}
	if c.LastActivity.IsZero() {
		t.Error("LastActivity should be set automatically")
	}
	if c.NFS == nil || c.NFS.Version != "4.1" {
		t.Error("NFS details should be preserved")
	}
}

func TestDeregister(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs"})
	removed := reg.Deregister("c1")
	if removed == nil {
		t.Fatal("expected removed record, got nil")
	}
	if removed.ClientID != "c1" {
		t.Errorf("expected c1, got %s", removed.ClientID)
	}
	list := reg.List()
	if len(list) != 0 {
		t.Fatalf("expected 0 clients after deregister, got %d", len(list))
	}

	// Deregister non-existent returns nil.
	if reg.Deregister("c1") != nil {
		t.Error("deregister of non-existent should return nil")
	}
}

func TestListByProtocol(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "nfs-1", Protocol: "nfs"})
	reg.Register(&ClientRecord{ClientID: "smb-1", Protocol: "smb"})

	nfsClients := reg.ListByProtocol("nfs")
	if len(nfsClients) != 1 {
		t.Fatalf("expected 1 NFS client, got %d", len(nfsClients))
	}
	if nfsClients[0].ClientID != "nfs-1" {
		t.Errorf("expected nfs-1, got %s", nfsClients[0].ClientID)
	}

	smbClients := reg.ListByProtocol("smb")
	if len(smbClients) != 1 {
		t.Fatalf("expected 1 SMB client, got %d", len(smbClients))
	}
}

func TestListByShare(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs", Shares: []string{"/export", "/data"}})
	reg.Register(&ClientRecord{ClientID: "c2", Protocol: "smb", Shares: []string{"/data"}})

	exportClients := reg.ListByShare("/export")
	if len(exportClients) != 1 {
		t.Fatalf("expected 1 client on /export, got %d", len(exportClients))
	}
	if exportClients[0].ClientID != "c1" {
		t.Errorf("expected c1, got %s", exportClients[0].ClientID)
	}

	dataClients := reg.ListByShare("/data")
	if len(dataClients) != 2 {
		t.Fatalf("expected 2 clients on /data, got %d", len(dataClients))
	}
}

func TestGet(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs", User: "bob"})

	c := reg.Get("c1")
	if c == nil {
		t.Fatal("expected record, got nil")
	}
	if c.User != "bob" {
		t.Errorf("expected bob, got %s", c.User)
	}

	// Get returns copy — mutating should not affect registry.
	c.User = "changed"
	c2 := reg.Get("c1")
	if c2.User != "bob" {
		t.Error("Get should return copy, mutation should not affect registry")
	}

	// Non-existent returns nil.
	if reg.Get("nope") != nil {
		t.Error("expected nil for non-existent client")
	}
}

func TestUpdateActivity(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs"})

	before := reg.Get("c1").LastActivity
	time.Sleep(5 * time.Millisecond)
	reg.UpdateActivity("c1")
	after := reg.Get("c1").LastActivity

	if !after.After(before) {
		t.Error("LastActivity should have been updated")
	}

	// No-op for non-existent.
	reg.UpdateActivity("nope")
}

func TestAddShare(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs", Shares: []string{"/export"}})

	reg.AddShare("c1", "/data")
	c := reg.Get("c1")
	if len(c.Shares) != 2 {
		t.Fatalf("expected 2 shares, got %d", len(c.Shares))
	}

	// Adding duplicate should not create duplicates.
	reg.AddShare("c1", "/data")
	c = reg.Get("c1")
	if len(c.Shares) != 2 {
		t.Fatalf("expected 2 shares (no dup), got %d", len(c.Shares))
	}

	// No-op for non-existent.
	reg.AddShare("nope", "/x")
}

func TestRemoveShare(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs", Shares: []string{"/export", "/data"}})

	reg.RemoveShare("c1", "/export")
	c := reg.Get("c1")
	if len(c.Shares) != 1 {
		t.Fatalf("expected 1 share, got %d", len(c.Shares))
	}
	if c.Shares[0] != "/data" {
		t.Errorf("expected /data, got %s", c.Shares[0])
	}

	// Removing non-existent share is no-op.
	reg.RemoveShare("c1", "/nope")
	c = reg.Get("c1")
	if len(c.Shares) != 1 {
		t.Fatalf("expected 1 share still, got %d", len(c.Shares))
	}

	// No-op for non-existent client.
	reg.RemoveShare("nope", "/data")
}

func TestSweep(t *testing.T) {
	ttl := 100 * time.Millisecond
	reg := NewRegistry(ttl)

	reg.Register(&ClientRecord{ClientID: "old", Protocol: "nfs"})
	time.Sleep(150 * time.Millisecond)
	reg.Register(&ClientRecord{ClientID: "fresh", Protocol: "smb"})

	reg.sweep()

	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 client after sweep, got %d", len(list))
	}
	if list[0].ClientID != "fresh" {
		t.Errorf("expected fresh, got %s", list[0].ClientID)
	}
}

func TestStartSweeperStopsOnCancel(t *testing.T) {
	ttl := 50 * time.Millisecond
	reg := NewRegistry(ttl)

	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs"})

	ctx, cancel := context.WithCancel(context.Background())
	reg.StartSweeper(ctx)

	// Wait long enough for sweep to run and remove the stale record.
	time.Sleep(150 * time.Millisecond)

	if reg.Count() != 0 {
		t.Errorf("expected 0 clients after sweep, got %d", reg.Count())
	}

	cancel()
	reg.Stop()
}

func TestCount(t *testing.T) {
	reg := NewRegistry(0)
	if reg.Count() != 0 {
		t.Fatalf("expected 0, got %d", reg.Count())
	}

	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs"})
	reg.Register(&ClientRecord{ClientID: "c2", Protocol: "smb"})

	if reg.Count() != 2 {
		t.Fatalf("expected 2, got %d", reg.Count())
	}
}

func TestCopyOnReadSharesSlice(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs", Shares: []string{"/a", "/b"}})

	c := reg.Get("c1")
	c.Shares[0] = "MUTATED"

	c2 := reg.Get("c1")
	if c2.Shares[0] != "/a" {
		t.Error("copy-on-read failed: shares slice was mutated through returned copy")
	}
}

func TestSmbDetails(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{
		ClientID: "smb-1",
		Protocol: "smb",
		Address:  "10.0.0.1:445",
		User:     "DOMAIN\\admin",
		SMB: &SmbDetails{
			SessionID: 12345,
			Dialect:   "3.1.1",
			Domain:    "EXAMPLE",
			Signed:    true,
			Encrypted: true,
		},
	})

	c := reg.Get("smb-1")
	if c.SMB == nil {
		t.Fatal("SMB details should not be nil")
	}
	if c.SMB.SessionID != 12345 {
		t.Errorf("expected session 12345, got %d", c.SMB.SessionID)
	}
	if c.SMB.Dialect != "3.1.1" {
		t.Errorf("expected 3.1.1, got %s", c.SMB.Dialect)
	}
	if !c.SMB.Signed {
		t.Error("expected signed=true")
	}
	if !c.SMB.Encrypted {
		t.Error("expected encrypted=true")
	}
}
