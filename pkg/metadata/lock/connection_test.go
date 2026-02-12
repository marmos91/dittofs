package lock

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

func TestRegisterClient_Success(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	err := ct.RegisterClient("client1", "nfs", "192.168.1.1", 0)
	if err != nil {
		t.Errorf("RegisterClient failed: %v", err)
	}

	client, exists := ct.GetClient("client1")
	if !exists {
		t.Error("Client should exist after registration")
	}
	if client.AdapterType != "nfs" {
		t.Errorf("Expected adapter type 'nfs', got %q", client.AdapterType)
	}
	if client.RemoteAddr != "192.168.1.1" {
		t.Errorf("Expected remote addr '192.168.1.1', got %q", client.RemoteAddr)
	}
}

func TestRegisterClient_LimitExceeded(t *testing.T) {
	config := DefaultConnectionTrackerConfig()
	config.MaxConnectionsPerAdapter["nfs"] = 2
	ct := NewConnectionTracker(config)

	// Register up to limit
	if err := ct.RegisterClient("client1", "nfs", "192.168.1.1", 0); err != nil {
		t.Fatalf("First registration failed: %v", err)
	}
	if err := ct.RegisterClient("client2", "nfs", "192.168.1.2", 0); err != nil {
		t.Fatalf("Second registration failed: %v", err)
	}

	// Third should fail
	err := ct.RegisterClient("client3", "nfs", "192.168.1.3", 0)
	if err == nil {
		t.Error("Expected error for connection limit exceeded")
	}

	storeErr, ok := err.(*errors.StoreError)
	if !ok {
		t.Fatalf("Expected StoreError, got %T", err)
	}
	if storeErr.Code != errors.ErrConnectionLimitReached {
		t.Errorf("Expected ErrConnectionLimitReached, got %v", storeErr.Code)
	}
}

func TestRegisterClient_Idempotent(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	// First registration
	if err := ct.RegisterClient("client1", "nfs", "192.168.1.1", 0); err != nil {
		t.Fatalf("First registration failed: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	// Second registration (should update LastSeen)
	if err := ct.RegisterClient("client1", "nfs", "192.168.1.100", 0); err != nil {
		t.Fatalf("Second registration failed: %v", err)
	}

	client, _ := ct.GetClient("client1")
	if client.RemoteAddr != "192.168.1.100" {
		t.Errorf("Expected updated remote addr '192.168.1.100', got %q", client.RemoteAddr)
	}

	// Count should still be 1
	if count := ct.GetClientCount("nfs"); count != 1 {
		t.Errorf("Expected count 1, got %d", count)
	}
}

func TestUnregisterClient_ImmediateRelease_TTL0(t *testing.T) {
	var mu sync.Mutex
	var disconnectedClient string
	config := DefaultConnectionTrackerConfig()
	config.OnClientDisconnect = func(clientID string) {
		mu.Lock()
		disconnectedClient = clientID
		mu.Unlock()
	}
	ct := NewConnectionTracker(config)

	_ = ct.RegisterClient("client1", "nfs", "192.168.1.1", 0)
	ct.UnregisterClient("client1")

	// Give callback time to execute (it runs in goroutine)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := disconnectedClient
	mu.Unlock()
	if got != "client1" {
		t.Errorf("Expected disconnect callback for client1, got %q", got)
	}

	_, exists := ct.GetClient("client1")
	if exists {
		t.Error("Client should not exist after unregistration")
	}
}

func TestUnregisterClient_DeferredRelease_TTL_Positive(t *testing.T) {
	var mu sync.Mutex
	var disconnectedClient string
	config := DefaultConnectionTrackerConfig()
	config.OnClientDisconnect = func(clientID string) {
		mu.Lock()
		disconnectedClient = clientID
		mu.Unlock()
	}
	ct := NewConnectionTracker(config)

	_ = ct.RegisterClient("client1", "nfs", "192.168.1.1", 100*time.Millisecond)
	ct.UnregisterClient("client1")

	// Immediately after unregister, callback should not have fired
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	got := disconnectedClient
	mu.Unlock()
	if got != "" {
		t.Error("Callback should not have fired immediately with positive TTL")
	}

	// After TTL, callback should fire
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	got = disconnectedClient
	mu.Unlock()
	if got != "client1" {
		t.Errorf("Expected disconnect callback after TTL, got %q", got)
	}
}

func TestCancelDisconnect_StopsTimer(t *testing.T) {
	var mu sync.Mutex
	var disconnectedClient string
	config := DefaultConnectionTrackerConfig()
	config.OnClientDisconnect = func(clientID string) {
		mu.Lock()
		disconnectedClient = clientID
		mu.Unlock()
	}
	ct := NewConnectionTracker(config)

	_ = ct.RegisterClient("client1", "nfs", "192.168.1.1", 100*time.Millisecond)
	ct.UnregisterClient("client1")

	// Cancel before TTL expires
	time.Sleep(30 * time.Millisecond)
	if !ct.CancelDisconnect("client1") {
		t.Error("CancelDisconnect should return true for pending timer")
	}

	// Wait past TTL - callback should not fire
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	got := disconnectedClient
	mu.Unlock()
	if got != "" {
		t.Error("Callback should not fire after cancel")
	}
}

func TestUpdateLastSeen(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	_ = ct.RegisterClient("client1", "nfs", "192.168.1.1", 0)
	client1, _ := ct.GetClient("client1")
	initialLastSeen := client1.LastSeen

	time.Sleep(10 * time.Millisecond)
	ct.UpdateLastSeen("client1")

	client2, _ := ct.GetClient("client1")
	if !client2.LastSeen.After(initialLastSeen) {
		t.Error("LastSeen should be updated")
	}
}

func TestListClients_FilterByAdapter(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	_ = ct.RegisterClient("nfs1", "nfs", "192.168.1.1", 0)
	_ = ct.RegisterClient("nfs2", "nfs", "192.168.1.2", 0)
	_ = ct.RegisterClient("smb1", "smb", "192.168.1.3", 0)

	nfsClients := ct.ListClients("nfs")
	if len(nfsClients) != 2 {
		t.Errorf("Expected 2 NFS clients, got %d", len(nfsClients))
	}

	smbClients := ct.ListClients("smb")
	if len(smbClients) != 1 {
		t.Errorf("Expected 1 SMB client, got %d", len(smbClients))
	}

	allClients := ct.ListClients("")
	if len(allClients) != 3 {
		t.Errorf("Expected 3 total clients, got %d", len(allClients))
	}
}

func TestGetClientCount(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	_ = ct.RegisterClient("nfs1", "nfs", "192.168.1.1", 0)
	_ = ct.RegisterClient("nfs2", "nfs", "192.168.1.2", 0)
	_ = ct.RegisterClient("smb1", "smb", "192.168.1.3", 0)

	if count := ct.GetClientCount("nfs"); count != 2 {
		t.Errorf("Expected 2 NFS clients, got %d", count)
	}

	if count := ct.GetClientCount("smb"); count != 1 {
		t.Errorf("Expected 1 SMB client, got %d", count)
	}

	if count := ct.GetClientCount(""); count != 3 {
		t.Errorf("Expected 3 total clients, got %d", count)
	}
}

func TestLockCount(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	_ = ct.RegisterClient("client1", "nfs", "192.168.1.1", 0)

	ct.IncrementLockCount("client1")
	ct.IncrementLockCount("client1")

	client, _ := ct.GetClient("client1")
	if client.LockCount != 2 {
		t.Errorf("Expected lock count 2, got %d", client.LockCount)
	}

	ct.DecrementLockCount("client1")
	client, _ = ct.GetClient("client1")
	if client.LockCount != 1 {
		t.Errorf("Expected lock count 1, got %d", client.LockCount)
	}
}

func TestConnectionTracker_Close(t *testing.T) {
	var mu sync.Mutex
	var disconnected bool
	config := DefaultConnectionTrackerConfig()
	config.OnClientDisconnect = func(clientID string) {
		mu.Lock()
		disconnected = true
		mu.Unlock()
	}
	ct := NewConnectionTracker(config)

	_ = ct.RegisterClient("client1", "nfs", "192.168.1.1", 1*time.Second)
	ct.UnregisterClient("client1")

	// Close should cancel pending timers
	ct.Close()

	// Wait past TTL - callback should not fire
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	fired := disconnected
	mu.Unlock()
	if fired {
		t.Error("Callback should not fire after Close")
	}

	if count := ct.GetClientCount(""); count != 0 {
		t.Errorf("Expected 0 clients after Close, got %d", count)
	}
}

func TestConnectionTracker_ThreadSafety(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())
	var wg sync.WaitGroup

	// Concurrent registrations
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			clientID := string(rune('A' + n%26))
			_ = ct.RegisterClient(clientID, "nfs", "192.168.1.1", 0)
			ct.UpdateLastSeen(clientID)
			ct.IncrementLockCount(clientID)
			ct.GetClient(clientID)
			ct.ListClients("")
			ct.GetClientCount("")
		}(i)
	}

	wg.Wait()

	// Should not panic and state should be consistent
	count := ct.GetClientCount("")
	if count < 1 {
		t.Error("Expected at least 1 client after concurrent operations")
	}
}

func TestGetPendingDisconnectCount(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	// Initially no pending disconnects
	if ct.GetPendingDisconnectCount() != 0 {
		t.Error("Expected 0 pending disconnects initially")
	}

	// Register with TTL and unregister
	_ = ct.RegisterClient("client1", "nfs", "192.168.1.1", 1*time.Second)
	_ = ct.RegisterClient("client2", "nfs", "192.168.1.2", 1*time.Second)
	ct.UnregisterClient("client1")
	ct.UnregisterClient("client2")

	if ct.GetPendingDisconnectCount() != 2 {
		t.Errorf("Expected 2 pending disconnects, got %d", ct.GetPendingDisconnectCount())
	}

	// Cancel one
	ct.CancelDisconnect("client1")

	if ct.GetPendingDisconnectCount() != 1 {
		t.Errorf("Expected 1 pending disconnect after cancel, got %d", ct.GetPendingDisconnectCount())
	}

	ct.Close()
}

func TestDefaultConnectionTrackerConfig(t *testing.T) {
	config := DefaultConnectionTrackerConfig()

	if config.DefaultMaxConnections != 10000 {
		t.Errorf("Expected default max connections 10000, got %d", config.DefaultMaxConnections)
	}

	if config.StaleCheckInterval != 30*time.Second {
		t.Errorf("Expected stale check interval 30s, got %v", config.StaleCheckInterval)
	}

	if config.MaxConnectionsPerAdapter == nil {
		t.Error("Expected MaxConnectionsPerAdapter to be initialized")
	}
}

func TestRegisterClient_DefaultLimitUsedWhenNoAdapterLimit(t *testing.T) {
	config := DefaultConnectionTrackerConfig()
	config.DefaultMaxConnections = 2
	// No adapter-specific limit for "nfs"
	ct := NewConnectionTracker(config)

	// Register up to default limit
	if err := ct.RegisterClient("client1", "nfs", "192.168.1.1", 0); err != nil {
		t.Fatalf("First registration failed: %v", err)
	}
	if err := ct.RegisterClient("client2", "nfs", "192.168.1.2", 0); err != nil {
		t.Fatalf("Second registration failed: %v", err)
	}

	// Third should fail due to default limit
	err := ct.RegisterClient("client3", "nfs", "192.168.1.3", 0)
	if err == nil {
		t.Error("Expected error for connection limit exceeded")
	}
}

func TestCancelDisconnect_NonExistent(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	// Should return false for non-existent timer
	if ct.CancelDisconnect("nonexistent") {
		t.Error("CancelDisconnect should return false for non-existent timer")
	}
}

func TestDecrementLockCount_DoesNotGoBelowZero(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	_ = ct.RegisterClient("client1", "nfs", "192.168.1.1", 0)

	// Decrement without any increments
	ct.DecrementLockCount("client1")

	client, _ := ct.GetClient("client1")
	if client.LockCount != 0 {
		t.Errorf("Expected lock count 0, got %d", client.LockCount)
	}
}

func TestIncrementDecrementLockCount_NonExistentClient(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	// Should not panic for non-existent client
	ct.IncrementLockCount("nonexistent")
	ct.DecrementLockCount("nonexistent")
}

func TestUpdateLastSeen_NonExistentClient(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	// Should not panic for non-existent client
	ct.UpdateLastSeen("nonexistent")
}

func TestUnregisterClient_NonExistent(t *testing.T) {
	ct := NewConnectionTracker(DefaultConnectionTrackerConfig())

	// Should not panic for non-existent client
	ct.UnregisterClient("nonexistent")
}
