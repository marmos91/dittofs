package suite

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
)

func clients(t *testing.T, newService func(t *testing.T) vfs.Service) {
	// A client recovering from a revocation tests every reference it holds
	// in one call, and reads one answer for each.
	t.Run("TestStateAnswersEachReference", func(t *testing.T) {
		s := newService(t)
		refs := []vfs.StateRef{
			{Type: vfs.StateOpen, Shard: 1, Token: 1},
			{Type: vfs.StateLock, Shard: 1, Token: 2},
			{Type: vfs.StateGrant, Shard: 2, Token: 3},
		}
		got := s.TestState(t.Context(), vfs.Identity{}, 1, refs)
		if len(got) != len(refs) {
			t.Fatalf("TestState answered %d, want one per reference: %d", len(got), len(refs))
		}
		for i, err := range got {
			if !errors.Is(err, vfs.ErrState) {
				t.Errorf("reference %d, never issued, answered %v, want ErrState", i, err)
			}
		}
	})
}
