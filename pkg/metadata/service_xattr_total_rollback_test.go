package metadata_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// TestSetFileAttributes_RefusedEATotal_RollsBackTheWholeCall asserts that an EA
// chain refused for exceeding XattrTotalMaxBytes takes the rest of its SetAttrs
// down with it. One call carries many fields, the bound is checked inside the
// transaction that writes them, and a mode change left committed beside a
// refused EA write would be a partial application of an operation the caller was
// told had failed.
//
// Asserted through the service rather than at ApplyEAMutations: the function
// returning an error says nothing about what the surrounding transaction did with
// the fields it had already folded onto the row.
func TestSetFileAttributes_RefusedEATotal_RollsBackTheWholeCall(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "ea.txt", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	handle, err := fx.store.GetChild(ctx, fx.rootHandle, "ea.txt")
	require.NoError(t, err)

	// Fill the file to the bound with values at the per-value ceiling, so the
	// next chain is refused on the total rather than on any single value.
	atLimit := make([]byte, metadata.XattrInlineMaxBytes)
	accepted := 0
	var refusal error
	for i := range 24 {
		refusal = fx.store.SetXattr(ctx, handle, fmt.Sprintf("v%02d", i), atLimit)
		if refusal != nil {
			break
		}
		accepted++
	}
	require.ErrorIs(t, refusal, metadata.ErrXattrTooLarge)
	require.Positive(t, accepted, "the bound must admit at least one value at the per-value ceiling")

	before, err := fx.store.GetFile(ctx, handle)
	require.NoError(t, err)

	// One call: a mode the store would happily take, and an EA chain it must not.
	mode := uint32(0o600)
	_, err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{
		Mode:        &mode,
		EAMutations: []metadata.EAMutation{{Name: "one-too-many", Value: atLimit}},
	})
	require.ErrorIs(t, err, metadata.ErrXattrTooLarge)

	after, err := fx.store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.Equal(t, before.Mode, after.Mode,
		"the mode in a refused call must not be committed; the whole call fails or none of it does")
	require.Equal(t, before.Ctime, after.Ctime, "a refused call must not stamp ctime either")
	require.Len(t, after.EAs, accepted, "the refused chain must leave the stored set exactly as it was")

	// The control, without which the assertions above are vacuous: the same call
	// carrying a chain the bound accepts must commit the mode. If it did not, an
	// unchanged mode after the refusal would be evidence of nothing.
	_, err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{
		Mode:        &mode,
		EAMutations: []metadata.EAMutation{{Name: "small", Value: []byte("v")}},
	})
	require.NoError(t, err)
	committed, err := fx.store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.Equal(t, mode, committed.Mode&0o7777,
		"a call whose EA chain fits must commit the mode, or the rollback assertion above tests nothing")
	require.Len(t, committed.EAs, accepted+1)
}
