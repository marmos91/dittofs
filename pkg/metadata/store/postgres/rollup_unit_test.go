package postgres

import (
	"context"
	"math"
	"strings"
	"testing"
)

// TestPostgresSetRollupOffset_RejectsAboveMaxInt64 is the unit-level guard
// for FIX-14: SetRollupOffset must reject newOffset > math.MaxInt64 BEFORE
// touching the database, since the underlying rollup_offset column is
// BIGINT (signed int64) and a silent cast would land a negative offset on
// disk. Constructing the store with a nil pool is safe because the
// validation returns before any queryRow call.
func TestPostgresSetRollupOffset_RejectsAboveMaxInt64(t *testing.T) {
	s := &PostgresMetadataStore{}
	_, err := s.SetRollupOffset(context.Background(), "x", math.MaxUint64)
	if err == nil {
		t.Fatal("SetRollupOffset(MaxUint64): want non-nil error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds BIGINT range") {
		t.Fatalf("SetRollupOffset(MaxUint64): err=%q, expected mention of BIGINT range", err.Error())
	}
}
