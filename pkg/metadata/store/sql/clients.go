package sql

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ClientStore implements lock.ClientRegistrationStore for both dialects.
//
// It persists NSM registrations across restarts so a rebooting client can be
// notified. There is nothing dialect-specific in what these do — only in the
// placeholder syntax of the statements and in which sentinel the driver
// reports for an empty read, both of which the Dialect supplies.
type ClientStore struct {
	// X runs the statements. Never nil.
	X Executor
	// D supplies statement text and classifies driver errors. Never nil.
	D Dialect
}

// ClientQueries holds the NSM client-registration statements in one dialect's
// syntax.
type ClientQueries struct {
	// Put upserts one registration. Nine parameters, in the column order of
	// Get's result.
	Put string
	// Get selects one registration by client id. One parameter: the id.
	Get string
	// Delete removes one registration by client id. One parameter: the id.
	Delete string
	// List selects every registration, oldest registration first. No
	// parameters.
	List string
	// DeleteAll removes every registration. No parameters.
	DeleteAll string
	// DeleteByMonName removes every registration monitoring one host. One
	// parameter: the monitored name.
	DeleteByMonName string
}

func (s *ClientStore) putArgs(reg *lock.PersistedClientRegistration) []any {
	return []any{
		reg.ClientID,
		reg.MonName,
		reg.Priv[:], // the column is bytes; the field is a fixed array
		reg.CallbackHost,
		reg.CallbackProg,
		reg.CallbackVers,
		reg.CallbackProc,
		reg.RegisteredAt,
		reg.ServerEpoch,
	}
}

// scanClientRegistration reads one registration row. privBytes is scanned
// separately because the column is variable-length and the field is not; a
// row carrying anything other than 16 bytes leaves Priv zeroed rather than
// panicking on a short copy.
func scanClientRegistration(row Row) (*lock.PersistedClientRegistration, error) {
	var reg lock.PersistedClientRegistration
	var privBytes []byte

	if err := row.Scan(
		&reg.ClientID,
		&reg.MonName,
		&privBytes,
		&reg.CallbackHost,
		&reg.CallbackProg,
		&reg.CallbackVers,
		&reg.CallbackProc,
		&reg.RegisteredAt,
		&reg.ServerEpoch,
	); err != nil {
		return nil, err
	}
	if len(privBytes) == 16 {
		copy(reg.Priv[:], privBytes)
	}
	return &reg, nil
}

// PutClientRegistration stores or updates a client registration.
func (s *ClientStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	_, err := s.X.Exec(ctx, s.D.Clients().Put, s.putArgs(reg)...)
	return err
}

// GetClientRegistration retrieves a registration by client ID, reporting
// (nil, nil) when there is none.
func (s *ClientStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	reg, err := scanClientRegistration(s.X.QueryRow(ctx, s.D.Clients().Get, clientID))
	if s.D.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return reg, nil
}

// DeleteClientRegistration removes a registration by client ID.
func (s *ClientStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	_, err := s.X.Exec(ctx, s.D.Clients().Delete, clientID)
	return err
}

// ListClientRegistrations returns all stored registrations.
func (s *ClientStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	rows, err := s.X.Query(ctx, s.D.Clients().List)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*lock.PersistedClientRegistration
	for rows.Next() {
		reg, err := scanClientRegistration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, reg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteAllClientRegistrations removes all registrations, reporting how many
// went.
func (s *ClientStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	result, err := s.X.Exec(ctx, s.D.Clients().DeleteAll)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// DeleteClientRegistrationsByMonName removes every registration monitoring one
// host, reporting how many went.
func (s *ClientStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	result, err := s.X.Exec(ctx, s.D.Clients().DeleteByMonName, monName)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}
