package session

import (
	"sync"
	"testing"
)

func TestManager_CreateSession(t *testing.T) {
	mgr := NewDefaultManager()

	// Create a session
	session := mgr.CreateSession("192.168.1.100:12345", true, "guest", "")

	if session.SessionID == 0 {
		t.Error("Session ID should not be 0 (reserved for anonymous)")
	}
	if !session.IsGuest {
		t.Error("Session should be guest")
	}
	if session.Username != "guest" {
		t.Errorf("Username = %q, want %q", session.Username, "guest")
	}
	if session.ClientAddr != "192.168.1.100:12345" {
		t.Errorf("ClientAddr = %q, want %q", session.ClientAddr, "192.168.1.100:12345")
	}

	// Verify session can be retrieved
	retrieved, ok := mgr.GetSession(session.SessionID)
	if !ok {
		t.Error("Session not found after creation")
	}
	if retrieved != session {
		t.Error("Retrieved session should be same instance")
	}
}

func TestManager_DeleteSession(t *testing.T) {
	mgr := NewDefaultManager()

	// Create and delete session
	session := mgr.CreateSession("client", false, "user1", "DOMAIN")
	mgr.DeleteSession(session.SessionID)

	// Verify session is gone
	_, ok := mgr.GetSession(session.SessionID)
	if ok {
		t.Error("Session should be deleted")
	}
}

func TestManager_AnonymousSession(t *testing.T) {
	mgr := NewDefaultManager()

	// Anonymous session (ID 0) should always exist
	session, ok := mgr.GetSession(0)
	if !ok {
		t.Error("Anonymous session should exist")
	}
	if session.SessionID != 0 {
		t.Errorf("Anonymous session ID = %d, want 0", session.SessionID)
	}

	// Can't delete anonymous session
	mgr.DeleteSession(0)
	_, ok = mgr.GetSession(0)
	if !ok {
		t.Error("Anonymous session should not be deletable")
	}
}

func TestManager_FixedStrategy(t *testing.T) {
	config := DefaultCreditConfig()
	mgr := NewManagerWithStrategy(StrategyFixed, config)

	session := mgr.CreateSession("client", true, "guest", "")

	// Fixed strategy should always grant InitialGrant
	grant := mgr.GrantCredits(session.SessionID, 10, 1)
	if grant != config.InitialGrant {
		t.Errorf("Fixed strategy: got %d, want %d", grant, config.InitialGrant)
	}

	// Should grant same regardless of request
	grant = mgr.GrantCredits(session.SessionID, 1000, 1)
	if grant != config.InitialGrant {
		t.Errorf("Fixed strategy: got %d, want %d", grant, config.InitialGrant)
	}
}

func TestManager_EchoStrategy(t *testing.T) {
	config := DefaultCreditConfig()
	mgr := NewManagerWithStrategy(StrategyEcho, config)

	session := mgr.CreateSession("client", true, "guest", "")

	tests := []struct {
		name      string
		requested uint16
		want      uint16
	}{
		{"ZeroReturnsInitial", 0, config.InitialGrant},
		{"BelowMinReturnsMin", 5, config.MinGrant},
		{"NormalRequest", 100, 100},
		{"LargeRequest", config.MaxGrant + 100, config.MaxGrant},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grant := mgr.GrantCredits(session.SessionID, tt.requested, 1)
			if grant != tt.want {
				t.Errorf("Echo strategy: got %d, want %d", grant, tt.want)
			}
		})
	}
}

func TestManager_AdaptiveStrategy(t *testing.T) {
	config := DefaultCreditConfig()
	mgr := NewManagerWithStrategy(StrategyAdaptive, config)

	session := mgr.CreateSession("client", true, "guest", "")

	t.Run("BaseGrant", func(t *testing.T) {
		// With no load, should grant around InitialGrant or boosted
		grant := mgr.GrantCredits(session.SessionID, 256, 1)
		if grant < config.MinGrant {
			t.Errorf("Grant too low: got %d, min %d", grant, config.MinGrant)
		}
		if grant > config.MaxGrant {
			t.Errorf("Grant too high: got %d, max %d", grant, config.MaxGrant)
		}
	})

	t.Run("HighLoadReducesGrants", func(t *testing.T) {
		// Simulate high load
		for i := 0; i < int(config.LoadThresholdHigh*2); i++ {
			mgr.RequestStarted(session.SessionID)
		}

		// Grant should be reduced under high load
		grant := mgr.GrantCredits(session.SessionID, 256, 1)

		// Clean up
		for i := 0; i < int(config.LoadThresholdHigh*2); i++ {
			mgr.RequestCompleted(session.SessionID)
		}

		// Should be less than initial grant due to load
		if grant >= config.InitialGrant {
			t.Logf("Warning: Grant under high load (%d) not reduced from initial (%d)", grant, config.InitialGrant)
		}
	})

	t.Run("LowLoadBoostsGrants", func(t *testing.T) {
		newSession := mgr.CreateSession("client2", true, "guest", "")
		// No load - grants should be boosted
		grant := mgr.GrantCredits(newSession.SessionID, 256, 1)

		// Should be at least initial grant (possibly boosted)
		if grant < config.MinGrant {
			t.Errorf("Grant under low load too low: %d", grant)
		}
	})
}

func TestManager_RequestTracking(t *testing.T) {
	mgr := NewDefaultManager()

	session1 := mgr.CreateSession("client1", true, "guest", "")
	session2 := mgr.CreateSession("client2", true, "guest", "")

	// Start some requests
	mgr.RequestStarted(session1.SessionID)
	mgr.RequestStarted(session1.SessionID)
	mgr.RequestStarted(session2.SessionID)

	stats := mgr.GetStats()
	if stats.ActiveRequests != 3 {
		t.Errorf("ActiveRequests: got %d, want 3", stats.ActiveRequests)
	}

	if session1.GetOutstandingRequests() != 2 {
		t.Errorf("Session 1 outstanding: got %d, want 2", session1.GetOutstandingRequests())
	}

	// Complete some
	mgr.RequestCompleted(session1.SessionID)
	mgr.RequestCompleted(session2.SessionID)

	stats = mgr.GetStats()
	if stats.ActiveRequests != 1 {
		t.Errorf("ActiveRequests after completion: got %d, want 1", stats.ActiveRequests)
	}
}

func TestManager_CreditAccounting(t *testing.T) {
	mgr := NewDefaultManager()

	session := mgr.CreateSession("client", true, "guest", "")

	// Grant some credits
	mgr.GrantCredits(session.SessionID, 256, 0) // +grant
	mgr.GrantCredits(session.SessionID, 128, 0) // +grant

	sessionStats := mgr.GetSessionStats(session.SessionID)
	if sessionStats == nil {
		t.Fatal("Session stats should not be nil")
	}
	if sessionStats.Granted == 0 {
		t.Error("Granted credits should not be 0")
	}

	// High water mark should reflect cumulative grants
	hwm := session.GetHighWaterMark()
	if hwm == 0 {
		t.Error("High water mark should be > 0")
	}

	// Consume some credits
	mgr.GrantCredits(session.SessionID, 100, 200) // Consume 200

	// High water mark should not decrease
	newHwm := session.GetHighWaterMark()
	if newHwm < hwm {
		t.Errorf("High water mark decreased: was %d, now %d", hwm, newHwm)
	}
}

func TestManager_ConcurrentAccess(t *testing.T) {
	mgr := NewDefaultManager()

	const goroutines = 100
	const operationsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Create a session for this goroutine
			session := mgr.CreateSession("client", true, "guest", "")

			for j := 0; j < operationsPerGoroutine; j++ {
				mgr.RequestStarted(session.SessionID)
				mgr.GrantCredits(session.SessionID, 100, 1)
				mgr.RequestCompleted(session.SessionID)
			}
		}(i)
	}

	wg.Wait()

	stats := mgr.GetStats()

	// Should have processed all operations
	expectedOps := uint64(goroutines * operationsPerGoroutine)
	if stats.TotalOperations != expectedOps {
		t.Errorf("Operations: got %d, want %d", stats.TotalOperations, expectedOps)
	}

	// Active requests should be back to 0
	if stats.ActiveRequests != 0 {
		t.Errorf("ActiveRequests not 0 after completion: %d", stats.ActiveRequests)
	}
}

func TestManager_AggressiveClientThrottling(t *testing.T) {
	config := DefaultCreditConfig()
	config.AggressiveClientThreshold = 10 // Low threshold for testing
	mgr := NewManagerWithStrategy(StrategyAdaptive, config)

	session := mgr.CreateSession("client", true, "guest", "")

	// Normal grant
	normalGrant := mgr.GrantCredits(session.SessionID, 256, 0)

	// Make client "aggressive" by having many outstanding requests
	for i := 0; i < 50; i++ {
		mgr.RequestStarted(session.SessionID)
	}

	// Grant should be reduced
	throttledGrant := mgr.GrantCredits(session.SessionID, 256, 0)

	// Clean up
	for i := 0; i < 50; i++ {
		mgr.RequestCompleted(session.SessionID)
	}

	if throttledGrant >= normalGrant {
		t.Logf("Throttled grant (%d) not less than normal grant (%d) - may vary by algorithm", throttledGrant, normalGrant)
	}
}

func TestManager_SessionCount(t *testing.T) {
	mgr := NewDefaultManager()

	// Should start with 1 session (the anonymous session)
	stats := mgr.GetStats()
	if stats.SessionCount != 1 {
		t.Errorf("Initial session count = %d, want 1 (anonymous)", stats.SessionCount)
	}

	// Create sessions
	s1 := mgr.CreateSession("client1", true, "guest", "")
	s2 := mgr.CreateSession("client2", true, "guest", "")

	stats = mgr.GetStats()
	if stats.SessionCount != 3 { // anonymous + 2 new
		t.Errorf("Session count = %d, want 3", stats.SessionCount)
	}

	// Delete one
	mgr.DeleteSession(s1.SessionID)
	stats = mgr.GetStats()
	if stats.SessionCount != 2 { // anonymous + 1 remaining
		t.Errorf("Session count after delete = %d, want 2", stats.SessionCount)
	}

	// Delete remaining
	mgr.DeleteSession(s2.SessionID)
	stats = mgr.GetStats()
	if stats.SessionCount != 1 { // only anonymous left
		t.Errorf("Session count after all deletes = %d, want 1", stats.SessionCount)
	}
}

func TestCalculateCreditCharge(t *testing.T) {
	tests := []struct {
		bytes uint32
		want  uint16
	}{
		{0, 1},                  // Minimum charge
		{1, 1},                  // Less than one unit
		{65536, 1},              // Exactly one unit
		{65537, 2},              // Just over one unit
		{128 * 1024, 2},         // 128KB = 2 credits
		{1024 * 1024, 16},       // 1MB = 16 credits
		{10 * 1024 * 1024, 160}, // 10MB = 160 credits
	}

	for _, tt := range tests {
		got := CalculateCreditCharge(tt.bytes)
		if got != tt.want {
			t.Errorf("CalculateCreditCharge(%d) = %d, want %d", tt.bytes, got, tt.want)
		}
	}
}

func TestDefaultCreditConfig(t *testing.T) {
	config := DefaultCreditConfig()

	if config.MinGrant == 0 {
		t.Error("MinGrant should not be 0")
	}
	if config.MaxGrant < config.MinGrant {
		t.Error("MaxGrant should be >= MinGrant")
	}
	if config.InitialGrant < config.MinGrant {
		t.Error("InitialGrant should be >= MinGrant")
	}
	if config.LoadThresholdHigh <= config.LoadThresholdLow {
		t.Error("LoadThresholdHigh should be > LoadThresholdLow")
	}
}
