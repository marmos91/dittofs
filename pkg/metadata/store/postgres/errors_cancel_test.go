package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// A cancelled caller must not come back as an I/O fault. pgx surfaces the
// context error, and the server reports the statement it aborted as 57014, so
// both shapes have to be recognised or they fall through to ErrIOError and
// reach an NFS client as NFS3ERR_IO for a request the client itself abandoned.
func TestMapPgErrorCancellationIsNotIO(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"deadline exceeded", context.DeadlineExceeded},
		{"canceled", context.Canceled},
		{"wrapped canceled", fmt.Errorf("GetFile: %w", context.Canceled)},
		{"query_canceled", &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapPgError(tc.err, "GetFile", "")
			var se *metadata.StoreError
			if errors.As(got, &se) {
				t.Fatalf("mapped to StoreError %v (%q); a cancelled statement is not a store error", se.Code, se.Message)
			}
			if !errors.Is(got, context.Canceled) && !errors.Is(got, context.DeadlineExceeded) {
				t.Fatalf("mapPgError(%v) = %v, want a context error", tc.err, got)
			}
		})
	}
}
