package metadata_test

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// The EA total bound exempts a chain that only deletes. These pin both halves of
// why that exemption is safe, because an exemption nobody tests is
// indistinguishable from a hole: a delete must still be able to shrink a set that
// is already over the bound, and no delete-only chain may grow one.

// oversizedEASet builds the set a build without the bound would have left behind:
// over XattrTotalMaxBytes, and still perfectly decodable.
func oversizedEASet(t *testing.T) *metadata.FileAttr {
	t.Helper()
	attr := &metadata.FileAttr{EAs: map[string][]byte{}}
	value := make([]byte, metadata.XattrInlineMaxBytes)
	for i := range 24 {
		attr.EAs[fmt.Sprintf("v%02d", i)] = value
	}
	return attr
}

// TestEATotal_DeleteTrimsAnAlreadyOversizedSet is the reason deletes are exempt.
// A set recorded before the bound existed keeps decoding, so a file can hold one;
// if the bound refused its deletes too, that file would be stuck over the bound
// with no operation left that could bring it back under.
func TestEATotal_DeleteTrimsAnAlreadyOversizedSet(t *testing.T) {
	t.Parallel()
	attr := oversizedEASet(t)

	// A set on such a file is refused, and changes nothing.
	err := attr.ApplyEAMutations([]metadata.EAMutation{{Name: "new", Value: []byte("x")}})
	require.ErrorIs(t, err, metadata.ErrXattrTooLarge)
	require.Len(t, attr.EAs, 24, "a refused set must leave the oversized set exactly as it was")

	// Deletes are accepted all the way down.
	for i := range 22 {
		require.NoError(t, attr.ApplyEAMutations([]metadata.EAMutation{{Name: fmt.Sprintf("v%02d", i), Delete: true}}),
			"a delete must be accepted on a set that is over the bound, or the file can never be repaired")
	}
	require.Len(t, attr.EAs, 2)

	// And once the set is back under the bound, writes work again.
	require.NoError(t, attr.ApplyEAMutations([]metadata.EAMutation{{Name: "new", Value: []byte("x")}}))
}

// TestEATotal_NoDeleteOnlyChainGrowsTheSet is the other half: the exemption is
// only sound while "delete-only" really cannot add bytes. Each chain here is
// classified delete-only and is shaped to sneak a key past that classification —
// a name that is not present, names that collide case-insensitively with each
// other and with the stored one, and a Delete carrying a non-empty Value.
func TestEATotal_NoDeleteOnlyChainGrowsTheSet(t *testing.T) {
	t.Parallel()
	chains := map[string][]metadata.EAMutation{
		"absent name":             {{Name: "absent", Delete: true}},
		"case-folded duplicates":  {{Name: "KEEP", Delete: true}, {Name: "keep", Delete: true}},
		"delete carrying a value": {{Name: "other", Value: []byte("payload"), Delete: true}},
	}
	for name, chain := range chains {
		t.Run(name, func(t *testing.T) {
			attr := &metadata.FileAttr{EAs: map[string][]byte{"Keep": []byte("v")}}
			require.NoError(t, attr.ApplyEAMutations(chain))
			require.LessOrEqual(t, len(attr.EAs), 1,
				"a delete-only chain must never add an entry, got %v", attr.EAs)
		})
	}
}
