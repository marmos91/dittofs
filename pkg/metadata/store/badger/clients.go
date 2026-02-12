package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// BadgerDB ClientRegistrationStore Implementation
// ============================================================================

// Key prefixes for client registration storage
const (
	// Primary key: nsm:client:{clientID} -> JSON(PersistedClientRegistration)
	prefixNSMClient = "nsm:client:"

	// Index by MonName: nsm:monname:{monName}:{clientID} -> clientID
	prefixNSMByMonName = "nsm:monname:"
)

// badgerClientStore implements lock.ClientRegistrationStore using BadgerDB.
//
// This implementation is suitable for:
//   - Production deployments requiring NSM registration persistence
//   - Crash recovery where client registrations must survive server restarts
//
// Storage Model:
//   - Primary storage: nsm:client:{clientID} -> JSON(PersistedClientRegistration)
//   - Secondary index: nsm:monname:{monName}:{clientID} -> clientID
//
// Thread Safety:
// All operations use BadgerDB's transaction support for atomicity.
type badgerClientStore struct {
	db *badgerdb.DB
}

// newBadgerClientStore creates a new BadgerDB client registration store.
func newBadgerClientStore(db *badgerdb.DB) *badgerClientStore {
	return &badgerClientStore{
		db: db,
	}
}

// PutClientRegistration stores or updates a client registration.
func (s *badgerClientStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		return s.putClientRegistrationTx(txn, reg)
	})
}

// putClientRegistrationTx stores a registration within an existing transaction.
func (s *badgerClientStore) putClientRegistrationTx(txn *badgerdb.Txn, reg *lock.PersistedClientRegistration) error {
	// Serialize registration to JSON
	data, err := json.Marshal(reg)
	if err != nil {
		return fmt.Errorf("failed to marshal client registration: %w", err)
	}

	// Store primary key
	primaryKey := []byte(prefixNSMClient + reg.ClientID)
	if err := txn.Set(primaryKey, data); err != nil {
		return err
	}

	// Store MonName index (for DeleteClientRegistrationsByMonName)
	if reg.MonName != "" {
		monNameKey := []byte(prefixNSMByMonName + reg.MonName + ":" + reg.ClientID)
		if err := txn.Set(monNameKey, []byte(reg.ClientID)); err != nil {
			return err
		}
	}

	return nil
}

// GetClientRegistration retrieves a registration by client ID.
func (s *badgerClientStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var reg *lock.PersistedClientRegistration

	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get([]byte(prefixNSMClient + clientID))
		if err == badgerdb.ErrKeyNotFound {
			return nil // Not found, not an error
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			reg = &lock.PersistedClientRegistration{}
			return json.Unmarshal(val, reg)
		})
	})

	if err != nil {
		return nil, err
	}
	return reg, nil
}

// DeleteClientRegistration removes a registration by client ID.
func (s *badgerClientStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		return s.deleteClientRegistrationTx(txn, clientID)
	})
}

// deleteClientRegistrationTx removes a registration within an existing transaction.
func (s *badgerClientStore) deleteClientRegistrationTx(txn *badgerdb.Txn, clientID string) error {
	// First, get the registration to find its MonName for index cleanup
	item, err := txn.Get([]byte(prefixNSMClient + clientID))
	if err == badgerdb.ErrKeyNotFound {
		return nil // Already gone
	}
	if err != nil {
		return err
	}

	var reg lock.PersistedClientRegistration
	err = item.Value(func(val []byte) error {
		return json.Unmarshal(val, &reg)
	})
	if err != nil {
		return err
	}

	// Delete MonName index
	if reg.MonName != "" {
		monNameKey := []byte(prefixNSMByMonName + reg.MonName + ":" + clientID)
		if err := txn.Delete(monNameKey); err != nil && err != badgerdb.ErrKeyNotFound {
			return err
		}
	}

	// Delete primary key
	primaryKey := []byte(prefixNSMClient + clientID)
	return txn.Delete(primaryKey)
}

// ListClientRegistrations returns all stored registrations.
func (s *badgerClientStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result []*lock.PersistedClientRegistration

	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixNSMClient)

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()

			err := item.Value(func(val []byte) error {
				reg := &lock.PersistedClientRegistration{}
				if err := json.Unmarshal(val, reg); err != nil {
					return err
				}
				result = append(result, reg)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteAllClientRegistrations removes all registrations.
func (s *badgerClientStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var count int

	// First, collect all client IDs
	var clientIDs []string
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixNSMClient)
		opts.PrefetchValues = false // Only need keys

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			key := it.Item().KeyCopy(nil)
			clientID := strings.TrimPrefix(string(key), prefixNSMClient)
			clientIDs = append(clientIDs, clientID)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Delete each registration (including indexes)
	err = s.db.Update(func(txn *badgerdb.Txn) error {
		for _, clientID := range clientIDs {
			if err := s.deleteClientRegistrationTx(txn, clientID); err != nil {
				return err
			}
			count++
		}
		return nil
	})

	return count, err
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *badgerClientStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var count int

	// First, collect all client IDs for this MonName using the index
	var clientIDs []string
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixNSMByMonName + monName + ":")
		opts.PrefetchValues = false // Only need keys

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			key := it.Item().KeyCopy(nil)
			// Key format: nsm:monname:{monName}:{clientID}
			// Extract clientID from the key
			prefix := prefixNSMByMonName + monName + ":"
			clientID := strings.TrimPrefix(string(key), prefix)
			clientIDs = append(clientIDs, clientID)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Delete each registration
	err = s.db.Update(func(txn *badgerdb.Txn) error {
		for _, clientID := range clientIDs {
			if err := s.deleteClientRegistrationTx(txn, clientID); err != nil {
				return err
			}
			count++
		}
		return nil
	})

	return count, err
}

// ============================================================================
// BadgerMetadataStore ClientRegistrationStore Integration
// ============================================================================

// Ensure BadgerMetadataStore implements ClientRegistrationStore
var _ lock.ClientRegistrationStore = (*BadgerMetadataStore)(nil)

// getClientStore returns the client store, initializing if needed.
func (s *BadgerMetadataStore) getClientStore() *badgerClientStore {
	s.clientStoreMu.Lock()
	defer s.clientStoreMu.Unlock()
	if s.clientStore == nil {
		s.clientStore = newBadgerClientStore(s.db)
	}
	return s.clientStore
}

// PutClientRegistration stores or updates a client registration.
func (s *BadgerMetadataStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	return s.getClientStore().PutClientRegistration(ctx, reg)
}

// GetClientRegistration retrieves a registration by client ID.
func (s *BadgerMetadataStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	return s.getClientStore().GetClientRegistration(ctx, clientID)
}

// DeleteClientRegistration removes a registration by client ID.
func (s *BadgerMetadataStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	return s.getClientStore().DeleteClientRegistration(ctx, clientID)
}

// ListClientRegistrations returns all stored registrations.
func (s *BadgerMetadataStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	return s.getClientStore().ListClientRegistrations(ctx)
}

// DeleteAllClientRegistrations removes all registrations.
func (s *BadgerMetadataStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	return s.getClientStore().DeleteAllClientRegistrations(ctx)
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *BadgerMetadataStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	return s.getClientStore().DeleteClientRegistrationsByMonName(ctx, monName)
}

// ClientRegistrationStore returns this store as a ClientRegistrationStore.
// This allows direct access to the interface for handler initialization.
func (s *BadgerMetadataStore) ClientRegistrationStore() lock.ClientRegistrationStore {
	return s
}
