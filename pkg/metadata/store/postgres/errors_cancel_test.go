package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// A caller that went away must not come back as a store I/O error, or the
// request looks like a store failure instead of the cancellation the adapters
// already know how to drop.
func TestMapPgErrorCancellationIsNotIO(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"deadline exceeded", context.DeadlineExceeded},
		{"canceled", context.Canceled},
		{"wrapped canceled", fmt.Errorf("GetFile: %w", context.Canceled)},
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

// A server-side statement_timeout also arrives as 57014 while the caller's
// context is still live and its client still waiting, so it must stay a store
// error: reported as a cancellation the dispatcher would drop the reply and
// leave that client waiting for one that never comes.
func TestMapPgErrorStatementTimeoutStaysStoreError(t *testing.T) {
	got := mapPgError(&pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, "GetFile", "")
	var se *metadata.StoreError
	if !errors.As(got, &se) {
		t.Fatalf("mapPgError(57014) = %v (%T), want a StoreError", got, got)
	}
	if errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("mapPgError(57014) = %v, must not claim the caller cancelled", got)
	}
}
