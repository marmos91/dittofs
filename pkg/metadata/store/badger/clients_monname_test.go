package badger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestDeleteClientRegistrationsByMonName_NoKeyInjection asserts that a MonName
// containing the key separator ':' cannot forge an extra key segment that a
// prefix scan for a different (victim) MonName would match. Without
// hex-encoding the MonName, registering MonName "victim:victimClient" plants a
// secondary-index key under the "victim" prefix, so deleting registrations for
// MonName "victim" would also delete the attacker's client. With encoding, the
// "victim" scan must touch only genuine "victim" registrations.
func TestDeleteClientRegistrationsByMonName_NoKeyInjection(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	defer store.Close()

	// Victim: a legitimate registration monitoring host "victim".
	if err := store.PutClientRegistration(ctx, &lock.PersistedClientRegistration{
		ClientID: "victimClient",
		MonName:  "victim",
	}); err != nil {
		t.Fatalf("Put victim registration: %v", err)
	}

	// Attacker: MonName crafted to inject the victim's prefix + clientID.
	if err := store.PutClientRegistration(ctx, &lock.PersistedClientRegistration{
		ClientID: "attackerClient",
		MonName:  "victim:victimClient",
	}); err != nil {
		t.Fatalf("Put attacker registration: %v", err)
	}

	// Deleting registrations for MonName "victim" must remove exactly one
	// registration (the genuine victim), not the attacker's injected entry.
	count, err := store.DeleteClientRegistrationsByMonName(ctx, "victim")
	if err != nil {
		t.Fatalf("DeleteClientRegistrationsByMonName(victim): %v", err)
	}
	if count != 1 {
		t.Fatalf("DeleteClientRegistrationsByMonName(victim) deleted %d registrations; want 1 (key-injection regression)", count)
	}

	// The attacker registration must still be present (GetClientRegistration
	// returns (nil, nil) when not found, so check the pointer).
	attacker, err := store.GetClientRegistration(ctx, "attackerClient")
	if err != nil {
		t.Fatalf("GetClientRegistration(attackerClient): %v", err)
	}
	if attacker == nil {
		t.Fatalf("attackerClient registration was unexpectedly deleted by the victim scan")
	}

	// The victim registration must be gone.
	victim, err := store.GetClientRegistration(ctx, "victimClient")
	if err != nil {
		t.Fatalf("GetClientRegistration(victimClient): %v", err)
	}
	if victim != nil {
		t.Fatalf("victimClient registration still present after delete; want removed")
	}
}

// TestDeleteClientRegistrationsByMonName_RoundTrip is a control: a normal
// MonName still indexes, scans, and deletes correctly after encoding.
func TestDeleteClientRegistrationsByMonName_RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	defer store.Close()

	for _, id := range []string{"c1", "c2"} {
		if err := store.PutClientRegistration(ctx, &lock.PersistedClientRegistration{
			ClientID: id,
			MonName:  "host.example.com",
		}); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}

	count, err := store.DeleteClientRegistrationsByMonName(ctx, "host.example.com")
	if err != nil {
		t.Fatalf("DeleteClientRegistrationsByMonName: %v", err)
	}
	if count != 2 {
		t.Fatalf("DeleteClientRegistrationsByMonName deleted %d; want 2", count)
	}
}
