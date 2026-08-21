package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// A cancelled caller must not come back as an I/O fault. database/sql aborts an
// in-flight statement through sqlite3_interrupt and the driver reports
// SQLITE_INTERRUPT, whose text carries no context error to match on, so it has
// to be recognised by name or it falls through to ErrIOError and reaches an NFS
// client as NFS3ERR_IO for a request the client itself abandoned.
func TestMapDBErrorCancellationIsNotIO(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"deadline exceeded", context.DeadlineExceeded},
		{"canceled", context.Canceled},
		{"wrapped deadline", fmt.Errorf("GetFile: %w", context.DeadlineExceeded)},
		// The driver's own text for a statement stopped mid-flight.
		{"driver interrupt", errors.New("interrupted (9)")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapDBError(tc.err, "GetFile", "")
			var se *metadata.StoreError
			if errors.As(got, &se) {
				t.Fatalf("mapped to StoreError %v (%q); a cancelled statement is not a store error", se.Code, se.Message)
			}
			if !errors.Is(got, context.Canceled) && !errors.Is(got, context.DeadlineExceeded) {
				t.Fatalf("mapDBError(%v) = %v, want a context error", tc.err, got)
			}
		})
	}
}
