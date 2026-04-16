package builtins_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/destination/builtins"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestRegisterBuiltins_BothKindsRegistered confirms the helper wires both
// built-in drivers into the registry under the typed BackupRepoKind
// constants, and that Kinds() returns them in deterministic order.
func TestRegisterBuiltins_BothKindsRegistered(t *testing.T) {
	destination.ResetRegistryForTest()
	t.Cleanup(destination.ResetRegistryForTest)

	builtins.RegisterBuiltins()

	_, ok := destination.Lookup(models.BackupRepoKindLocal)
	require.True(t, ok, "local factory should be registered")
	_, ok = destination.Lookup(models.BackupRepoKindS3)
	require.True(t, ok, "s3 factory should be registered")

	// Kinds() is deterministic (sorted lexicographically).
	require.Equal(t, []models.BackupRepoKind{
		models.BackupRepoKindLocal,
		models.BackupRepoKindS3,
	}, destination.Kinds())
}

// TestRegisterBuiltins_DuplicatePanics confirms calling RegisterBuiltins
// twice panics — matches destination.Register's duplicate-registration
// behavior. This is a programmer-error guard, not an operator-facing check.
func TestRegisterBuiltins_DuplicatePanics(t *testing.T) {
	destination.ResetRegistryForTest()
	t.Cleanup(destination.ResetRegistryForTest)

	builtins.RegisterBuiltins()
	defer func() {
		r := recover()
		require.NotNil(t, r, "duplicate RegisterBuiltins must panic")
	}()
	builtins.RegisterBuiltins()
}
