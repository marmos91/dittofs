package metadata_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateEntry_TypeMismatch pins that an attr.Type naming a different object
// than the entry point creates is refused rather than overwritten. The entry
// point owns the type; attr.Type is never read for it, so a caller that sets it
// to something else used to get an object of the wrong type back with no error
// and no log line — which is how table-driven cases asserting about directory
// behaviour ran against regular files and still passed.
func TestCreateEntry_TypeMismatch(t *testing.T) {
	t.Parallel()

	// Each entry point pins its own type, so each one has its own mismatch to
	// refuse. A guard on CreateFile alone leaves the siblings silent.
	cases := []struct {
		name    string
		create  func(fx *testFixture, attr *metadata.FileAttr) error
		asked   metadata.FileType
		created metadata.FileType
	}{
		{
			name: "directory asked of CreateFile",
			create: func(fx *testFixture, attr *metadata.FileAttr) error {
				_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "target", attr)
				return err
			},
			asked:   metadata.FileTypeDirectory,
			created: metadata.FileTypeRegular,
		},
		{
			name: "symlink asked of CreateDirectory",
			create: func(fx *testFixture, attr *metadata.FileAttr) error {
				_, _, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "target", attr)
				return err
			},
			asked:   metadata.FileTypeSymlink,
			created: metadata.FileTypeDirectory,
		},
		{
			name: "directory asked of CreateSymlink",
			create: func(fx *testFixture, attr *metadata.FileAttr) error {
				_, _, err := fx.service.CreateSymlink(fx.rootContext(), fx.rootHandle, "target", "../A", attr)
				return err
			},
			asked:   metadata.FileTypeDirectory,
			created: metadata.FileTypeSymlink,
		},
		{
			name: "directory asked of CreateSpecialFile",
			create: func(fx *testFixture, attr *metadata.FileAttr) error {
				_, _, err := fx.service.CreateSpecialFile(
					fx.rootContext(), fx.rootHandle, "target", metadata.FileTypeFIFO, attr, 0, 0)
				return err
			},
			asked:   metadata.FileTypeDirectory,
			created: metadata.FileTypeFIFO,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTestFixture(t)

			err := tc.create(fx, &metadata.FileAttr{Type: tc.asked, Mode: 0o700})
			require.Error(t, err, "asking for a %d from an entry point that creates a %d must fail", tc.asked, tc.created)
			var storeErr *metadata.StoreError
			require.True(t, errors.As(err, &storeErr), "want a StoreError, got %v", err)
			assert.Equal(t, metadata.ErrInvalidArgument, storeErr.Code,
				"a type mismatch is a caller error")

			// The refusal must also leave no entry behind: a create that reports
			// failure while still inserting the wrong-typed object is the same
			// trap one step further on.
			_, lookupErr := fx.store.GetChild(context.Background(), fx.rootHandle, "target")
			assert.True(t, metadata.IsNotFoundError(lookupErr),
				"a refused create must not leave an entry behind")
		})
	}

	// The matching case still works — the guard must not reject a caller that
	// passes the type the entry point already creates.
	t.Run("matching type is accepted", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "ok", &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644,
		})
		require.NoError(t, err)
	})

	// An unset Type is indistinguishable from FileTypeRegular, so a non-regular
	// entry point must still accept it rather than reading the zero value as a
	// deliberate request for a regular file.
	t.Run("unset type is accepted by a non-regular entry point", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, _, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "d", &metadata.FileAttr{
			Mode: 0o755,
		})
		require.NoError(t, err)
	})
}
