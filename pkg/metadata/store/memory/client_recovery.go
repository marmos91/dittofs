package memory

import (
	"context"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Memory ClientRecoveryStore Implementation
// ============================================================================

// memoryRecoveryStore implements lock.ClientRecoveryStore using in-memory
// storage. Records are server-global, keyed by ClientIDString.
//
// Thread Safety:
// All operations are protected by a read-write mutex. Returned records are
// deep copies so callers can never alias or mutate the stored record.
type memoryRecoveryStore struct {
	mu sync.RWMutex

	// records maps ClientIDString to its recovery record.
	records map[string]*lock.V4ClientRecoveryRecord
}

// newMemoryRecoveryStore creates a new in-memory client recovery store.
func newMemoryRecoveryStore() *memoryRecoveryStore {
	return &memoryRecoveryStore{
		records: make(map[string]*lock.V4ClientRecoveryRecord),
	}
}

// cloneRecoveryRecord deep-copies a recovery record. BootVerifier is a value
// array so the struct copy duplicates it; there are no reference fields, but
// the explicit clone documents the no-aliasing contract.
func cloneRecoveryRecord(rec *lock.V4ClientRecoveryRecord) *lock.V4ClientRecoveryRecord {
	if rec == nil {
		return nil
	}
	cp := *rec
	return &cp
}

// PutClientRecovery stores or replaces the record for a confirmed client.
func (s *memoryRecoveryStore) PutClientRecovery(ctx context.Context, rec *lock.V4ClientRecoveryRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Store a clone so a later mutation of the caller's record does not
	// leak into the store.
	s.records[rec.ClientIDString] = cloneRecoveryRecord(rec)
	return nil
}

// DeleteClientRecovery removes the record for a client.
func (s *memoryRecoveryStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.records, clientIDString)
	return nil
}

// ListClientRecovery returns all stored records as deep copies.
func (s *memoryRecoveryStore) ListClientRecovery(ctx context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*lock.V4ClientRecoveryRecord, 0, len(s.records))
	for _, rec := range s.records {
		result = append(result, cloneRecoveryRecord(rec))
	}
	return result, nil
}

// RecordReclaimComplete marks the client's record as reclaim-complete.
func (s *memoryRecoveryStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.records[clientIDString]; ok {
		rec.ReclaimComplete = true
	}
	// No record => nothing to mark, not an error.
	return nil
}

// ============================================================================
// MemoryMetadataStore ClientRecoveryStore Integration
// ============================================================================

// Ensure MemoryMetadataStore implements ClientRecoveryStore.
var _ lock.ClientRecoveryStore = (*MemoryMetadataStore)(nil)

// initRecoveryStore ensures the recovery store is initialized.
// Must be called with the store's write lock held.
func (s *MemoryMetadataStore) initRecoveryStore() {
	if s.recoveryStore == nil {
		s.recoveryStore = newMemoryRecoveryStore()
	}
}

// PutClientRecovery stores or replaces a client recovery record.
func (s *MemoryMetadataStore) PutClientRecovery(ctx context.Context, rec *lock.V4ClientRecoveryRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initRecoveryStore()
	return s.recoveryStore.PutClientRecovery(ctx, rec)
}

// DeleteClientRecovery removes a client recovery record.
func (s *MemoryMetadataStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryStore == nil {
		return nil // Nothing to delete
	}
	return s.recoveryStore.DeleteClientRecovery(ctx, clientIDString)
}

// ListClientRecovery returns all stored client recovery records.
func (s *MemoryMetadataStore) ListClientRecovery(ctx context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.recoveryStore == nil {
		return []*lock.V4ClientRecoveryRecord{}, nil
	}
	return s.recoveryStore.ListClientRecovery(ctx)
}

// RecordReclaimComplete marks a client's recovery record reclaim-complete.
func (s *MemoryMetadataStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryStore == nil {
		return nil // Nothing to mark
	}
	return s.recoveryStore.RecordReclaimComplete(ctx, clientIDString)
}

// ClientRecoveryStore returns this store as a ClientRecoveryStore.
// This allows direct access to the interface for handler initialization.
func (s *MemoryMetadataStore) ClientRecoveryStore() lock.ClientRecoveryStore {
	return s
}
