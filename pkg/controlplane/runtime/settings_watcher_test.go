//go:build integration

package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// createTestStoreWithAdapter creates an in-memory SQLite store with an NFS adapter
// and default settings. This is the standard setup for settings watcher tests.
func createTestStoreWithAdapter(t *testing.T) (store.Store, *models.AdapterConfig) {
	t.Helper()

	dbConfig := store.Config{
		Type: "sqlite",
		SQLite: store.SQLiteConfig{
			Path: ":memory:",
		},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	ctx := context.Background()

	adapter := &models.AdapterConfig{
		ID:      uuid.New().String(),
		Type:    "nfs",
		Enabled: true,
		Port:    12049,
	}
	if _, err := cpStore.CreateAdapter(ctx, adapter); err != nil {
		t.Fatalf("Failed to create NFS adapter: %v", err)
	}

	if err := cpStore.EnsureAdapterSettings(ctx); err != nil {
		t.Fatalf("Failed to ensure adapter settings: %v", err)
	}

	return cpStore, adapter
}

func TestSettingsWatcher_LoadInitial(t *testing.T) {
	cpStore, adapter := createTestStoreWithAdapter(t)
	ctx := context.Background()

	watcher := NewSettingsWatcher(cpStore, 50*time.Millisecond)

	if err := watcher.LoadInitial(ctx); err != nil {
		t.Fatalf("LoadInitial failed: %v", err)
	}

	settings := watcher.GetNFSSettings()
	if settings == nil {
		t.Fatal("Expected non-nil NFS settings after LoadInitial")
	}

	if settings.AdapterID != adapter.ID {
		t.Errorf("AdapterID = %s, want %s", settings.AdapterID, adapter.ID)
	}

	defaults := models.NewDefaultNFSSettings("")
	if settings.LeaseTime != defaults.LeaseTime {
		t.Errorf("LeaseTime = %d, want %d", settings.LeaseTime, defaults.LeaseTime)
	}
	if settings.Version != 1 {
		t.Errorf("Version = %d, want 1", settings.Version)
	}
}

func TestSettingsWatcher_DetectsChange(t *testing.T) {
	cpStore, adapter := createTestStoreWithAdapter(t)
	ctx := context.Background()

	watcher := NewSettingsWatcher(cpStore, 50*time.Millisecond)

	if err := watcher.LoadInitial(ctx); err != nil {
		t.Fatalf("LoadInitial failed: %v", err)
	}

	// Verify initial settings
	initial := watcher.GetNFSSettings()
	if initial == nil {
		t.Fatal("Expected non-nil NFS settings")
	}
	if initial.LeaseTime != 90 {
		t.Fatalf("Expected default LeaseTime 90, got %d", initial.LeaseTime)
	}

	// Update settings in DB
	settings, err := cpStore.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings failed: %v", err)
	}
	settings.LeaseTime = 300
	if err := cpStore.UpdateNFSAdapterSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateNFSAdapterSettings failed: %v", err)
	}

	// Start watcher and wait for a poll cycle
	watcher.Start(ctx)
	defer watcher.Stop()

	// Wait for watcher to detect the change (poll interval is 50ms, give it a few cycles)
	time.Sleep(200 * time.Millisecond)

	updated := watcher.GetNFSSettings()
	if updated == nil {
		t.Fatal("Expected non-nil NFS settings after poll")
	}
	if updated.LeaseTime != 300 {
		t.Errorf("LeaseTime = %d, want 300 (updated)", updated.LeaseTime)
	}
	if updated.Version != 2 {
		t.Errorf("Version = %d, want 2 (incremented by update)", updated.Version)
	}
}

func TestSettingsWatcher_NoChangeNoUpdate(t *testing.T) {
	cpStore, _ := createTestStoreWithAdapter(t)
	ctx := context.Background()

	watcher := NewSettingsWatcher(cpStore, 50*time.Millisecond)

	if err := watcher.LoadInitial(ctx); err != nil {
		t.Fatalf("LoadInitial failed: %v", err)
	}

	// Get pointer to initial settings
	settings1 := watcher.GetNFSSettings()
	if settings1 == nil {
		t.Fatal("Expected non-nil NFS settings")
	}

	// Trigger a manual poll without changing DB
	watcher.poll(ctx)

	// Settings pointer should be the SAME (no swap happened because version is unchanged)
	settings2 := watcher.GetNFSSettings()
	if settings1 != settings2 {
		t.Error("Settings pointer changed despite no DB update -- expected same pointer")
	}
}

func TestSettingsWatcher_AtomicSwap(t *testing.T) {
	cpStore, adapter := createTestStoreWithAdapter(t)
	ctx := context.Background()

	watcher := NewSettingsWatcher(cpStore, 50*time.Millisecond)

	if err := watcher.LoadInitial(ctx); err != nil {
		t.Fatalf("LoadInitial failed: %v", err)
	}

	// Start watcher with short poll interval
	watcher.Start(ctx)
	defer watcher.Stop()

	// Spawn readers that continuously read settings
	var wg sync.WaitGroup
	readErrors := make(chan string, 100)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				settings := watcher.GetNFSSettings()
				if settings == nil {
					readErrors <- "got nil settings during concurrent read"
					return
				}
				// Verify the settings are a consistent snapshot
				// (LeaseTime and Version should be from the same write)
				if settings.Version < 1 {
					readErrors <- "got invalid version"
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	// Spawn a writer that updates settings in DB
	go func() {
		for i := 0; i < 5; i++ {
			settings, _ := cpStore.GetNFSAdapterSettings(ctx, adapter.ID)
			settings.LeaseTime = 100 + i*10
			_ = cpStore.UpdateNFSAdapterSettings(ctx, settings)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	wg.Wait()
	close(readErrors)

	for errMsg := range readErrors {
		t.Errorf("Concurrent read error: %s", errMsg)
	}
}

func TestSettingsWatcher_StopClean(t *testing.T) {
	cpStore, _ := createTestStoreWithAdapter(t)
	ctx := context.Background()

	watcher := NewSettingsWatcher(cpStore, 50*time.Millisecond)

	if err := watcher.LoadInitial(ctx); err != nil {
		t.Fatalf("LoadInitial failed: %v", err)
	}

	watcher.Start(ctx)

	// Give goroutine time to start
	time.Sleep(100 * time.Millisecond)

	// Stop should return without hanging
	done := make(chan struct{})
	go func() {
		watcher.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK - stopped cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2 seconds -- goroutine leak?")
	}

	// Double stop should be safe
	watcher.Stop()
}

func TestSettingsWatcher_DBError(t *testing.T) {
	cpStore, _ := createTestStoreWithAdapter(t)
	ctx := context.Background()

	watcher := NewSettingsWatcher(cpStore, 50*time.Millisecond)

	if err := watcher.LoadInitial(ctx); err != nil {
		t.Fatalf("LoadInitial failed: %v", err)
	}

	// Get initial settings
	initial := watcher.GetNFSSettings()
	if initial == nil {
		t.Fatal("Expected non-nil initial settings")
	}
	initialLeaseTime := initial.LeaseTime

	// Close the underlying DB to simulate a DB error
	cpStore.(*store.GORMStore).DB().Exec("DROP TABLE nfs_adapter_settings")

	// Poll should fail gracefully (not panic) and keep old settings
	watcher.poll(ctx)

	// Settings should be preserved from before the error
	preserved := watcher.GetNFSSettings()
	if preserved == nil {
		t.Fatal("Expected settings to be preserved after DB error")
	}
	if preserved.LeaseTime != initialLeaseTime {
		t.Errorf("LeaseTime = %d, want %d (preserved after DB error)", preserved.LeaseTime, initialLeaseTime)
	}
}

// TestSettingsWatcher_PollPanicRecovers verifies that a panic raised inside a
// poll cycle (here from an SMB change callback) does not kill the background
// goroutine: after the panicking tick the watcher recovers and keeps detecting
// subsequent changes. Without the recover wrapper the goroutine would die
// silently and hot-reload would freeze for the process lifetime.
func TestSettingsWatcher_PollPanicRecovers(t *testing.T) {
	dbConfig := store.Config{Type: "sqlite", SQLite: store.SQLiteConfig{Path: ":memory:"}}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	ctx := context.Background()

	smbAdapter := &models.AdapterConfig{
		ID:      uuid.New().String(),
		Type:    "smb",
		Enabled: true,
		Port:    12445,
	}
	if _, err := cpStore.CreateAdapter(ctx, smbAdapter); err != nil {
		t.Fatalf("create SMB adapter: %v", err)
	}
	if err := cpStore.EnsureAdapterSettings(ctx); err != nil {
		t.Fatalf("ensure adapter settings: %v", err)
	}

	watcher := NewSettingsWatcher(cpStore, 25*time.Millisecond)

	// Callback panics exactly once (on the first change it observes) so we can
	// assert recovery on later changes.
	var panicked atomic.Bool
	watcher.OnSMBSettingsChange(func(*models.SMBAdapterSettings) {
		if panicked.CompareAndSwap(false, true) {
			panic("boom from settings callback")
		}
	})

	// Prime currentVersion > 0 so subsequent changes fire the callback.
	if err := watcher.LoadInitial(ctx); err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}

	watcher.Start(ctx)
	defer watcher.Stop()

	// First change: triggers the panicking callback. The watcher must recover.
	bumpSMBSessionTimeout(t, cpStore, smbAdapter.ID, 111)
	waitFor(t, 2*time.Second, func() bool { return panicked.Load() })

	// Second change AFTER the panic: if the goroutine had died this would never
	// be observed. The callback no longer panics, so we assert the cached
	// settings reflect the new value — proving polling resumed.
	bumpSMBSessionTimeout(t, cpStore, smbAdapter.ID, 222)
	waitFor(t, 2*time.Second, func() bool {
		s := watcher.GetSMBSettings()
		return s != nil && s.SessionTimeout == 222
	})
}

// bumpSMBSessionTimeout updates the SMB adapter's SessionTimeout, which bumps
// the settings Version and triggers the watcher's change path.
func bumpSMBSessionTimeout(t *testing.T, cpStore store.Store, adapterID string, v int) {
	t.Helper()
	ctx := context.Background()
	settings, err := cpStore.GetSMBAdapterSettings(ctx, adapterID)
	if err != nil {
		t.Fatalf("GetSMBAdapterSettings: %v", err)
	}
	settings.SessionTimeout = v
	if err := cpStore.UpdateSMBAdapterSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSMBAdapterSettings: %v", err)
	}
}

// waitFor polls cond until true or the timeout elapses, failing the test on
// timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met within timeout")
		case <-tick.C:
		}
	}
}

func TestSettingsWatcher_DefaultPollInterval(t *testing.T) {
	watcher := NewSettingsWatcher(nil, 0)
	if watcher.pollInterval != DefaultPollInterval {
		t.Errorf("Expected default poll interval %v, got %v", DefaultPollInterval, watcher.pollInterval)
	}
}

func TestSettingsWatcher_NilBeforeLoad(t *testing.T) {
	cpStore, _ := createTestStoreWithAdapter(t)
	watcher := NewSettingsWatcher(cpStore, 50*time.Millisecond)

	// Before LoadInitial, settings should be nil
	if watcher.GetNFSSettings() != nil {
		t.Error("Expected nil NFS settings before LoadInitial")
	}
	if watcher.GetSMBSettings() != nil {
		t.Error("Expected nil SMB settings before LoadInitial")
	}
}
