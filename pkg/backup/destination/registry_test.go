package destination

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// stubDest is a minimal Destination used to confirm dispatch routes through
// the factory without touching any real backend. The full signatures are
// present so the file compiles without cross-file hunting.
type stubDest struct{ tag string }

func (s *stubDest) PutBackup(ctx context.Context, m *manifest.Manifest, r io.Reader) error {
	return nil
}
func (s *stubDest) GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
	return nil, nil, nil
}
func (s *stubDest) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
	return nil, nil
}
func (s *stubDest) List(ctx context.Context) ([]BackupDescriptor, error)           { return nil, nil }
func (s *stubDest) Stat(ctx context.Context, id string) (*BackupDescriptor, error) { return nil, nil }
func (s *stubDest) Delete(ctx context.Context, id string) error                    { return nil }
func (s *stubDest) ValidateConfig(ctx context.Context) error                       { return nil }
func (s *stubDest) Close() error                                                   { return nil }

// compile-time check that *stubDest satisfies Destination.
var _ Destination = (*stubDest)(nil)

func stubFactory(tag string) Factory {
	return func(ctx context.Context, repo *models.BackupRepo) (Destination, error) {
		return &stubDest{tag: tag}, nil
	}
}

func TestDestinationFactoryFromRepo_HappyPath(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)
	Register(models.BackupRepoKind("test-kind"), stubFactory("t1"))

	repo := &models.BackupRepo{ID: "r1", Kind: models.BackupRepoKind("test-kind")}
	d, err := DestinationFactoryFromRepo(context.Background(), repo)
	require.NoError(t, err)
	sd, ok := d.(*stubDest)
	require.True(t, ok)
	require.Equal(t, "t1", sd.tag)
}

func TestDestinationFactoryFromRepo_UnknownKind(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)
	Register(models.BackupRepoKind("foo"), stubFactory("f"))

	repo := &models.BackupRepo{ID: "r1", Kind: models.BackupRepoKind("bogus")}
	_, err := DestinationFactoryFromRepo(context.Background(), repo)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrIncompatibleConfig))
	require.Contains(t, err.Error(), "bogus")
	require.Contains(t, err.Error(), "foo") // Kinds() listing
}

func TestDestinationFactoryFromRepo_NilRepo(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)
	_, err := DestinationFactoryFromRepo(context.Background(), nil)
	require.ErrorIs(t, err, ErrIncompatibleConfig)
}

func TestDestinationFactoryFromRepo_EmptyKind(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)
	_, err := DestinationFactoryFromRepo(context.Background(), &models.BackupRepo{ID: "r", Kind: ""})
	require.ErrorIs(t, err, ErrIncompatibleConfig)
}

func TestKinds_Deterministic(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)
	Register(models.BackupRepoKind("zeta"), stubFactory("z"))
	Register(models.BackupRepoKind("alpha"), stubFactory("a"))
	Register(models.BackupRepoKind("mike"), stubFactory("m"))
	require.Equal(t, []models.BackupRepoKind{"alpha", "mike", "zeta"}, Kinds())
}

func TestKinds_Empty(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)
	require.Empty(t, Kinds())
}

// TestDestinationFactoryFromRepo_TypedConstants confirms that the built-in
// typed constants (models.BackupRepoKindLocal, models.BackupRepoKindS3) flow
// through DestinationFactoryFromRepo → Lookup without any string conversion.
// Because both repo.Kind and the registry key are models.BackupRepoKind, the
// dispatch compiles as Lookup(repo.Kind).
func TestDestinationFactoryFromRepo_TypedConstants(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)
	Register(models.BackupRepoKindLocal, stubFactory("local"))
	Register(models.BackupRepoKindS3, stubFactory("s3"))

	// Local.
	d, err := DestinationFactoryFromRepo(context.Background(), &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindLocal})
	require.NoError(t, err)
	require.Equal(t, "local", d.(*stubDest).tag)
	// S3.
	d2, err := DestinationFactoryFromRepo(context.Background(), &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindS3})
	require.NoError(t, err)
	require.Equal(t, "s3", d2.(*stubDest).tag)
}
