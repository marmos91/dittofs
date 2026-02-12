package lock

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestNewMetrics_CreatesAllMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}

	if m.lockAcquireTotal == nil {
		t.Error("lockAcquireTotal not initialized")
	}
	if m.lockReleaseTotal == nil {
		t.Error("lockReleaseTotal not initialized")
	}
	if m.lockActiveGauge == nil {
		t.Error("lockActiveGauge not initialized")
	}
	if m.lockBlockedGauge == nil {
		t.Error("lockBlockedGauge not initialized")
	}
	if m.lockBlockingDuration == nil {
		t.Error("lockBlockingDuration not initialized")
	}
	if m.lockHoldDuration == nil {
		t.Error("lockHoldDuration not initialized")
	}
	if m.connectionActiveGauge == nil {
		t.Error("connectionActiveGauge not initialized")
	}
	if m.connectionTotal == nil {
		t.Error("connectionTotal not initialized")
	}
	if m.gracePeriodActive == nil {
		t.Error("gracePeriodActive not initialized")
	}
	if m.gracePeriodRemaining == nil {
		t.Error("gracePeriodRemaining not initialized")
	}
	if m.reclaimTotal == nil {
		t.Error("reclaimTotal not initialized")
	}
	if m.lockLimitHits == nil {
		t.Error("lockLimitHits not initialized")
	}
	if m.deadlockDetected == nil {
		t.Error("deadlockDetected not initialized")
	}
}

func TestMetrics_ObserveLockAcquire_IncrementsCounter(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.ObserveLockAcquire("share1", LockTypeExclusive, true)
	m.ObserveLockAcquire("share1", LockTypeShared, false)
	m.ObserveLockAcquire("share1", LockTypeExclusive, true)

	// Verify metrics are gathered without error
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "dittofs_locks_acquire_total" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected dittofs_locks_acquire_total metric")
	}
}

func TestMetrics_ObserveBlockingDuration_RecordsHistogram(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.ObserveBlockingDuration("share1", 100*time.Millisecond)
	m.ObserveBlockingDuration("share1", 500*time.Millisecond)
	m.ObserveBlockingDuration("share1", 1*time.Second)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "dittofs_locks_blocking_duration_seconds" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected dittofs_locks_blocking_duration_seconds metric")
	}
}

func TestMetrics_SetActiveLocks_UpdatesGauge(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.SetActiveLocks("share1", LockTypeExclusive, 5)
	m.SetActiveLocks("share1", LockTypeShared, 10)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "dittofs_locks_active" {
			found = true
			for _, m := range mf.GetMetric() {
				// Check that values are set
				if m.GetGauge().GetValue() != 5 && m.GetGauge().GetValue() != 10 {
					t.Errorf("Unexpected gauge value: %v", m.GetGauge().GetValue())
				}
			}
			break
		}
	}
	if !found {
		t.Error("Expected dittofs_locks_active metric")
	}
}

func TestMetrics_GracePeriodMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.SetGracePeriodActive(true)
	m.SetGracePeriodRemaining(45.5)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	foundActive := false
	foundRemaining := false
	for _, mf := range mfs {
		switch mf.GetName() {
		case "dittofs_locks_grace_period_active":
			foundActive = true
			if len(mf.GetMetric()) > 0 {
				val := mf.GetMetric()[0].GetGauge().GetValue()
				if val != 1.0 {
					t.Errorf("Expected grace_period_active=1, got %v", val)
				}
			}
		case "dittofs_locks_grace_period_remaining_seconds":
			foundRemaining = true
			if len(mf.GetMetric()) > 0 {
				val := mf.GetMetric()[0].GetGauge().GetValue()
				if val != 45.5 {
					t.Errorf("Expected grace_period_remaining=45.5, got %v", val)
				}
			}
		}
	}

	if !foundActive {
		t.Error("Expected dittofs_locks_grace_period_active metric")
	}
	if !foundRemaining {
		t.Error("Expected dittofs_locks_grace_period_remaining_seconds metric")
	}
}

func TestMetrics_NilRegistry_NoPanic(t *testing.T) {
	// Should not panic with nil registry
	m := NewMetrics(nil)

	// Methods should handle nil safely
	m.ObserveLockAcquire("share", LockTypeExclusive, true)
	m.ObserveLockRelease("share", ReasonExplicit)
	m.SetActiveLocks("share", LockTypeShared, 5)
	m.SetBlockedLocks("share", 2)
	m.ObserveBlockingDuration("share", time.Second)
	m.ObserveLockHoldDuration("share", LockTypeExclusive, time.Minute)
	m.SetActiveConnections("nfs", 10)
	m.ObserveConnection("nfs", "connect")
	m.SetGracePeriodActive(true)
	m.SetGracePeriodRemaining(30)
	m.ObserveReclaim(true)
	m.ObserveLockLimitHit("file")
	m.ObserveDeadlock()
}

func TestMetrics_NilMetrics_NoPanic(t *testing.T) {
	// Nil Metrics should not panic
	var m *Metrics

	// All methods should handle nil receiver safely
	m.ObserveLockAcquire("share", LockTypeExclusive, true)
	m.ObserveLockRelease("share", ReasonExplicit)
	m.SetActiveLocks("share", LockTypeShared, 5)
	m.SetBlockedLocks("share", 2)
	m.ObserveBlockingDuration("share", time.Second)
	m.ObserveLockHoldDuration("share", LockTypeExclusive, time.Minute)
	m.SetActiveConnections("nfs", 10)
	m.ObserveConnection("nfs", "connect")
	m.SetGracePeriodActive(true)
	m.SetGracePeriodRemaining(30)
	m.ObserveReclaim(true)
	m.ObserveLockLimitHit("file")
	m.ObserveDeadlock()
}

func TestMetrics_ConnectionMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.SetActiveConnections("nfs", 5)
	m.SetActiveConnections("smb", 3)
	m.ObserveConnection("nfs", "connect")
	m.ObserveConnection("nfs", "disconnect")

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	foundActive := false
	foundTotal := false
	for _, mf := range mfs {
		switch mf.GetName() {
		case "dittofs_connections_active":
			foundActive = true
		case "dittofs_connections_total":
			foundTotal = true
		}
	}

	if !foundActive {
		t.Error("Expected dittofs_connections_active metric")
	}
	if !foundTotal {
		t.Error("Expected dittofs_connections_total metric")
	}
}

func TestMetrics_DeadlockAndLimits(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.ObserveDeadlock()
	m.ObserveDeadlock()
	m.ObserveLockLimitHit("file")
	m.ObserveLockLimitHit("client")
	m.ObserveLockLimitHit("total")

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	foundDeadlock := false
	foundLimits := false
	for _, mf := range mfs {
		switch mf.GetName() {
		case "dittofs_locks_deadlock_detected_total":
			foundDeadlock = true
			if len(mf.GetMetric()) > 0 {
				val := mf.GetMetric()[0].GetCounter().GetValue()
				if val != 2 {
					t.Errorf("Expected 2 deadlocks, got %v", val)
				}
			}
		case "dittofs_locks_limit_hits_total":
			foundLimits = true
		}
	}

	if !foundDeadlock {
		t.Error("Expected dittofs_locks_deadlock_detected_total metric")
	}
	if !foundLimits {
		t.Error("Expected dittofs_locks_limit_hits_total metric")
	}
}

func TestMetrics_ObserveLockRelease(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.ObserveLockRelease("share1", ReasonExplicit)
	m.ObserveLockRelease("share1", ReasonTimeout)
	m.ObserveLockRelease("share1", ReasonDisconnect)
	m.ObserveLockRelease("share1", ReasonGraceExpired)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "dittofs_locks_release_total" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected dittofs_locks_release_total metric")
	}
}

func TestMetrics_ObserveLockHoldDuration(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.ObserveLockHoldDuration("share1", LockTypeExclusive, 5*time.Second)
	m.ObserveLockHoldDuration("share1", LockTypeShared, 10*time.Second)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "dittofs_locks_hold_duration_seconds" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected dittofs_locks_hold_duration_seconds metric")
	}
}

func TestMetrics_SetBlockedLocks(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.SetBlockedLocks("share1", 5)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "dittofs_locks_blocked" {
			found = true
			if len(mf.GetMetric()) > 0 {
				val := mf.GetMetric()[0].GetGauge().GetValue()
				if val != 5 {
					t.Errorf("Expected blocked=5, got %v", val)
				}
			}
			break
		}
	}
	if !found {
		t.Error("Expected dittofs_locks_blocked metric")
	}
}

func TestMetrics_ObserveReclaim(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.ObserveReclaim(true)
	m.ObserveReclaim(false)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "dittofs_locks_reclaim_total" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected dittofs_locks_reclaim_total metric")
	}
}

func TestMetrics_Describe(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	ch := make(chan *prometheus.Desc, 100)
	m.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}

	if count == 0 {
		t.Error("Expected some metric descriptions")
	}
}

func TestMetrics_Collect(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Set some values first
	m.SetActiveLocks("share1", LockTypeExclusive, 5)
	m.SetGracePeriodActive(true)

	ch := make(chan prometheus.Metric, 100)
	m.Collect(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}

	if count == 0 {
		t.Error("Expected some metrics to be collected")
	}
}

func TestMetrics_Describe_NilAndUnregistered(t *testing.T) {
	// Test nil receiver
	var m *Metrics
	ch := make(chan *prometheus.Desc, 10)
	m.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 0 {
		t.Error("Expected no descriptions from nil receiver")
	}

	// Test unregistered metrics
	m2 := NewMetrics(nil) // Not registered
	ch2 := make(chan *prometheus.Desc, 10)
	m2.Describe(ch2)
	close(ch2)

	count2 := 0
	for range ch2 {
		count2++
	}
	if count2 != 0 {
		t.Error("Expected no descriptions from unregistered metrics")
	}
}

func TestMetrics_Collect_NilAndUnregistered(t *testing.T) {
	// Test nil receiver
	var m *Metrics
	ch := make(chan prometheus.Metric, 10)
	m.Collect(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 0 {
		t.Error("Expected no metrics from nil receiver")
	}

	// Test unregistered metrics
	m2 := NewMetrics(nil) // Not registered
	ch2 := make(chan prometheus.Metric, 10)
	m2.Collect(ch2)
	close(ch2)

	count2 := 0
	for range ch2 {
		count2++
	}
	if count2 != 0 {
		t.Error("Expected no metrics from unregistered metrics")
	}
}

func TestMetrics_SetGracePeriodActive_False(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.SetGracePeriodActive(false)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	for _, mf := range mfs {
		if mf.GetName() == "dittofs_locks_grace_period_active" {
			if len(mf.GetMetric()) > 0 {
				val := mf.GetMetric()[0].GetGauge().GetValue()
				if val != 0.0 {
					t.Errorf("Expected grace_period_active=0, got %v", val)
				}
			}
			return
		}
	}
	t.Error("Expected dittofs_locks_grace_period_active metric")
}
