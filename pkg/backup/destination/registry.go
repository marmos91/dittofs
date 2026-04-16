package destination

import (
	"context"
	"fmt"
	"sort"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// DestinationFactoryFromRepo looks up the factory registered for repo.Kind
// and invokes it. This is the single entrypoint callers (Phase 4 scheduler,
// Phase 5 restore orchestrator, Phase 6 CLI) use to construct a driver from
// a persisted BackupRepo row — they do NOT import fs/ or s3/ directly.
//
// Because both repo.Kind and the registry key are models.BackupRepoKind,
// Lookup(repo.Kind) compiles directly with no string conversion. Any caller
// that tries to pass a bare string will fail to compile — the intentional
// compile-time guarantee that prevents the "looked-up-wrong-kind" footgun.
//
// Returns ErrIncompatibleConfig wrapped with the unknown-kind message when
// repo.Kind has no registered factory; the error message lists every
// registered kind so operators can tell at a glance which drivers are wired.
func DestinationFactoryFromRepo(ctx context.Context, repo *models.BackupRepo) (Destination, error) {
	if repo == nil {
		return nil, fmt.Errorf("%w: repo is nil", ErrIncompatibleConfig)
	}
	if repo.Kind == "" {
		return nil, fmt.Errorf("%w: repo.Kind is empty", ErrIncompatibleConfig)
	}
	// repo.Kind is models.BackupRepoKind — matches registry key type directly.
	f, ok := Lookup(repo.Kind)
	if !ok {
		return nil, fmt.Errorf("%w: unknown destination kind %q (registered: %v)",
			ErrIncompatibleConfig, repo.Kind, Kinds())
	}
	return f(ctx, repo)
}

// Kinds returns a deterministic sorted list of registered destination kinds.
// Callers use this for CLI help output, error messages, and operator-facing
// introspection. The sort makes the output stable across runs.
func Kinds() []models.BackupRepoKind {
	out := make([]models.BackupRepoKind, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
