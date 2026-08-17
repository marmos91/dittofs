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
		NFS:      &NfsDetails{Version: "4.1"},
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

func TestGet(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{ClientID: "c1", Protocol: "nfs", NFS: &NfsDetails{Version: "3"}})

	c := reg.Get("c1")
	if c == nil {
		t.Fatal("expected record, got nil")
	}
	if c.NFS == nil || c.NFS.Version != "3" {
		t.Fatalf("expected NFS version 3, got %+v", c.NFS)
	}

	// Get returns a deep copy — mutating it must not reach the registry, and
	// the detail pointers must not alias the stored ones either.
	c.Protocol = "changed"
	c.NFS.Version = "changed"
	c2 := reg.Get("c1")
	if c2.Protocol != "nfs" {
		t.Error("Get should return a copy; protocol mutation reached the registry")
	}
	if c2.NFS.Version != "3" {
		t.Error("Get should deep-copy NFS details; mutation reached the registry")
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

func TestSmbDetails(t *testing.T) {
	reg := NewRegistry(0)
	reg.Register(&ClientRecord{
		ClientID: "smb-1",
		Protocol: "smb",
		Address:  "10.0.0.1:445",
		SMB:      &SmbDetails{SessionID: 12345},
	})

	c := reg.Get("smb-1")
	if c.SMB == nil {
		t.Fatal("SMB details should not be nil")
	}
	if c.SMB.SessionID != 12345 {
		t.Errorf("expected session 12345, got %d", c.SMB.SessionID)
	}
}
