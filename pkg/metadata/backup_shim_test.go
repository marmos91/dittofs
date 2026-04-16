package metadata_test

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/metadata"

	"github.com/stretchr/testify/require"
)

// TestShimPreservesSentinelIdentity asserts that the compat shim re-exports
// the SAME sentinel values (not copies). If someone ever replaces the
// `var X = backup.X` aliases with `errors.New(...)` calls, errors.Is would
// return false across the module boundary and callers that migrate
// partially would silently stop matching — this test catches that regression.
func TestShimPreservesSentinelIdentity(t *testing.T) {
	type pair struct {
		name  string
		shim  error
		canon error
	}
	pairs := []pair{
		{"ErrBackupUnsupported", metadata.ErrBackupUnsupported, backup.ErrBackupUnsupported},
		{"ErrRestoreDestinationNotEmpty", metadata.ErrRestoreDestinationNotEmpty, backup.ErrRestoreDestinationNotEmpty},
		{"ErrRestoreCorrupt", metadata.ErrRestoreCorrupt, backup.ErrRestoreCorrupt},
		{"ErrSchemaVersionMismatch", metadata.ErrSchemaVersionMismatch, backup.ErrSchemaVersionMismatch},
		{"ErrBackupAborted", metadata.ErrBackupAborted, backup.ErrBackupAborted},
	}
	for _, p := range pairs {
		p := p
		t.Run(p.name, func(t *testing.T) {
			require.Truef(t, errors.Is(p.shim, p.canon),
				"metadata.%s must be identical to backup.%s (shim must use `var X = backup.X`, not a new errors.New)",
				p.name, p.name)
			require.Truef(t, errors.Is(p.canon, p.shim),
				"backup.%s must be identical to metadata.%s (symmetry)",
				p.name, p.name)
		})
	}
}

// TestShimTypeAliasIdentity confirms metadata.Backupable and backup.Backupable
// are the identical type, not two conversion-compatible types. Two-way
// assignment without a cast only compiles when the types are alias-identical.
func TestShimTypeAliasIdentity(t *testing.T) {
	var m metadata.Backupable
	var b backup.Backupable
	m = b
	b = m
	_ = m
	_ = b
}

// TestShimPayloadIDSetAliasIdentity confirms the map alias is preserved.
// A conversion call compiles for both alias-identical types and distinct
// map types with identical element types, but passing `shim` to a function
// that REQUIRES backup.PayloadIDSet tests the same path without tripping
// staticcheck's "merge var + assignment" rule.
func TestShimPayloadIDSetAliasIdentity(t *testing.T) {
	shim := metadata.NewPayloadIDSet()
	shim.Add("x")
	require.True(t, acceptsCanonical(shim))
}

func acceptsCanonical(s backup.PayloadIDSet) bool { return s.Contains("x") }
