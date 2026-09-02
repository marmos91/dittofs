package sql

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// RecoveryStore implements lock.ClientRecoveryStore for both dialects. As with
// ClientStore, nothing here is dialect-specific beyond the statement text.
type RecoveryStore struct {
	// X runs the statements. Never nil.
	X Executor
	// D supplies statement text and classifies driver errors. Never nil.
	D Dialect
}

// RecoveryQueries holds the v4 client-recovery statements in one dialect's
// syntax.
type RecoveryQueries struct {
	// Put upserts one record keyed by clientid_string. Seven parameters, in
	// the column order of List's result.
	Put string
	// Delete removes one record. One parameter: the clientid string.
	Delete string
	// List selects every record, earliest confirmation first. No parameters.
	List string
	// RecordReclaimComplete marks one record reclaim-complete. One parameter:
	// the clientid string.
	RecordReclaimComplete string
}

// scanRecoveryRecord reads one record, copying the variable-length
// boot_verifier column into the fixed array only when it is the expected
// width.
func scanRecoveryRecord(row Row) (*lock.V4ClientRecoveryRecord, error) {
	var rec lock.V4ClientRecoveryRecord
	var clientID, serverEpoch int64
	var bootVerifier []byte

	if err := row.Scan(
		&rec.ClientIDString,
		&clientID,
		&bootVerifier,
		&rec.Principal,
		&rec.ConfirmedAt,
		&serverEpoch,
		&rec.ReclaimComplete,
	); err != nil {
		return nil, err
	}

	rec.ClientID = uint64(clientID)
	rec.ServerEpoch = uint64(serverEpoch)
	if len(bootVerifier) == 8 {
		copy(rec.BootVerifier[:], bootVerifier)
	}
	return &rec, nil
}

// PutClientRecovery stores or replaces the record for a confirmed client.
//
// It replaces the whole record, reclaim_complete included, matching the memory
// and badger backends, which overwrite the whole struct. A re-confirm means the
// client's state is fresh, so reclaim starts over; callers persist reclaim
// completion through RecordReclaimComplete, not through this.
func (s *RecoveryStore) PutClientRecovery(ctx context.Context, rec *lock.V4ClientRecoveryRecord) error {
	_, err := s.X.Exec(ctx, s.D.Recovery().Put,
		rec.ClientIDString,
		int64(rec.ClientID),
		rec.BootVerifier[:], // the column is bytes; the field is a fixed array
		rec.Principal,
		rec.ConfirmedAt,
		int64(rec.ServerEpoch),
		rec.ReclaimComplete,
	)
	return err
}

// DeleteClientRecovery removes the record for a client.
func (s *RecoveryStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	_, err := s.X.Exec(ctx, s.D.Recovery().Delete, clientIDString)
	return err
}

// ListClientRecovery returns all stored records.
func (s *RecoveryStore) ListClientRecovery(ctx context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	rows, err := s.X.Query(ctx, s.D.Recovery().List)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]*lock.V4ClientRecoveryRecord, 0)
	for rows.Next() {
		rec, err := scanRecoveryRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// RecordReclaimComplete marks the client's record reclaim-complete. A missing
// record is a no-op, not an error.
func (s *RecoveryStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	_, err := s.X.Exec(ctx, s.D.Recovery().RecordReclaimComplete, clientIDString)
	return err
}
