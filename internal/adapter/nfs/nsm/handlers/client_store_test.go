package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// recordingClientStore captures what a late-installed store is handed.
type recordingClientStore struct {
	put []*lock.PersistedClientRegistration
}

func (r *recordingClientStore) PutClientRegistration(_ context.Context, reg *lock.PersistedClientRegistration) error {
	r.put = append(r.put, reg)
	return nil
}

func (r *recordingClientStore) GetClientRegistration(context.Context, string) (*lock.PersistedClientRegistration, error) {
	return nil, nil
}
func (r *recordingClientStore) DeleteClientRegistration(context.Context, string) error { return nil }
func (r *recordingClientStore) ListClientRegistrations(context.Context) ([]*lock.PersistedClientRegistration, error) {
	return nil, nil
}
func (r *recordingClientStore) DeleteAllClientRegistrations(context.Context) (int, error) {
	return 0, nil
}
func (r *recordingClientStore) DeleteClientRegistrationsByMonName(context.Context, string) (int, error) {
	return 0, nil
}

// TestSetClientStore_BackfillsClientsMonitoredBeforeIt: SM_MON persists only on
// the call that registers a client, and the startup load has already run, so a
// store installed later would otherwise never learn about the clients that
// registered during exactly the window it exists to close.
func TestSetClientStore_BackfillsClientsMonitoredBeforeIt(t *testing.T) {
	t.Parallel()

	tracker := lock.NewConnectionTracker(lock.DefaultConnectionTrackerConfig())
	t.Cleanup(tracker.Close)

	h := NewHandler(HandlerConfig{Tracker: tracker, ServerName: "server"})
	if h.GetClientStore() != nil {
		t.Fatal("handler built with no store should report none")
	}

	// A client registers over SM_MON while no store exists.
	if err := tracker.RegisterClient("client-a", "nfs", "10.0.0.7:51234", 0); err != nil {
		t.Fatal(err)
	}
	tracker.UpdateNSMInfo("client-a", "client-a.example", [16]byte{1}, &lock.NSMCallback{Hostname: "server"})
	tracker.UpdateSMState("client-a", 7)

	store := &recordingClientStore{}
	h.SetClientStore(context.Background(), store)

	if h.GetClientStore() == nil {
		t.Fatal("store was not installed")
	}
	if len(store.put) != 1 {
		t.Fatalf("want the already-monitored client persisted, got %d registrations", len(store.put))
	}
	if store.put[0].ClientID != "client-a" {
		t.Fatalf("wrong client backfilled: %q", store.put[0].ClientID)
	}
}
