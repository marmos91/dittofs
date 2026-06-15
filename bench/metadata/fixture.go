// Package metadatabench drives read workloads directly against a
// metadata.Store backend — no protocol, no NFS/SMB client cache in the way —
// so per-backend GetFile / GetChild / ListChildren cost can be measured in
// isolation. It is the decisive signal for whether a server-side metadata
// read cache is worth building (see issue #1169): the e2e mount path is
// masked by client attribute caching, this path is not.
package metadatabench

import (
	"context"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// Backend names accepted by OpenStore.
const (
	BackendMemory   = "memory"
	BackendBadger   = "badger"
	BackendPostgres = "postgres"
)

// PGConfig carries the postgres connection parameters. Defaults match the CI
// service (.github/workflows/integration-tests.yml) so a bench reuses the same
// database the conformance suite runs against.
type PGConfig struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	SSLMode  string
	// Prepare toggles server-side prepared statements. Production defaults it
	// true; the store's ApplyDefaults leaves a zero-value bool false, so it is
	// set explicitly here. Measuring both isolates how much of the postgres
	// read cost is query re-planning vs the round-trip itself.
	Prepare bool
}

// OpenStore constructs the requested backend and returns it alongside a cleanup
// func that closes the store and removes any temp on-disk state. memory is pure
// RAM (the zero-cost floor a perfect cache would approach); badger is a local
// embedded LSM in a temp dir; postgres connects to an existing database and
// auto-migrates.
func OpenStore(ctx context.Context, backend string, pg PGConfig) (metadata.Store, func(), error) {
	switch backend {
	case BackendMemory:
		s := memory.NewMemoryMetadataStoreWithDefaults()
		return s, func() { _ = s.Close() }, nil

	case BackendBadger:
		dir, err := os.MkdirTemp("", "bench-metadata-badger-")
		if err != nil {
			return nil, nil, fmt.Errorf("temp dir: %w", err)
		}
		s, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, nil, err
		}
		return s, func() { _ = s.Close(); _ = os.RemoveAll(dir) }, nil

	case BackendPostgres:
		cfg := &postgres.PostgresMetadataStoreConfig{
			Host:              pg.Host,
			Port:              pg.Port,
			Database:          pg.Database,
			User:              pg.User,
			Password:          pg.Password,
			SSLMode:           pg.SSLMode,
			AutoMigrate:       true,
			PrepareStatements: pg.Prepare,
		}
		s, err := postgres.NewPostgresMetadataStore(ctx, cfg, defaultCaps())
		if err != nil {
			return nil, nil, err
		}
		return s, func() { _ = s.Close() }, nil

	default:
		return nil, nil, fmt.Errorf("unknown backend %q (want memory|badger|postgres)", backend)
	}
}

// defaultCaps mirrors the badger/postgres WithDefaults capability block. The
// postgres constructor requires explicit capabilities; the values are
// immaterial to read-latency measurement.
func defaultCaps() metadata.FilesystemCapabilities {
	return metadata.FilesystemCapabilities{
		MaxReadSize:         1048576,
		PreferredReadSize:   1048576,
		MaxWriteSize:        1048576,
		PreferredWriteSize:  1048576,
		MaxFileSize:         9223372036854775807,
		MaxFilenameLen:      255,
		MaxPathLen:          4096,
		MaxHardLinkCount:    32767,
		SupportsHardLinks:   true,
		SupportsSymlinks:    true,
		CaseSensitive:       true,
		CasePreserving:      true,
		TimestampResolution: 1,
	}
}

// tree is the seeded fixture: one share with Dirs subdirectories, each holding
// FilesPerDir regular files. The slices are the working sets the hot loop draws
// from — fileHandles for getattr, (lookupDir,lookupName) pairs for lookup,
// dirHandles for readdir.
type tree struct {
	dirHandles  []metadata.FileHandle
	fileHandles []metadata.FileHandle
	lookupDir   []metadata.FileHandle
	lookupName  []string
}

// resetter is implemented by backends that reuse a persistent database
// (postgres). Truncating before a seed keeps reruns idempotent — without it a
// second run collides on the share name.
type resetter interface {
	Reset(ctx context.Context) error
}

const benchShareName = "/bench"

// seedTree populates the store with Dirs × FilesPerDir entries and returns the
// working sets. Mirrors storetest's createTestShare/Dir/File helpers, minus the
// *testing.T coupling so it runs from the bench binary.
func seedTree(ctx context.Context, store metadata.Store, dirs, filesPerDir int) (*tree, error) {
	if r, ok := store.(resetter); ok {
		if err := r.Reset(ctx); err != nil {
			return nil, fmt.Errorf("reset: %w", err)
		}
	}

	rootHandle, err := createShare(ctx, store, benchShareName)
	if err != nil {
		return nil, err
	}

	t := &tree{}
	for d := 0; d < dirs; d++ {
		dirName := fmt.Sprintf("dir%06d", d)
		dirHandle, err := putChild(ctx, store, benchShareName, rootHandle, dirName, metadata.FileTypeDirectory, 0o755, 2)
		if err != nil {
			return nil, err
		}
		t.dirHandles = append(t.dirHandles, dirHandle)

		for f := 0; f < filesPerDir; f++ {
			fileName := fmt.Sprintf("file%06d", f)
			fileHandle, err := putChild(ctx, store, benchShareName, dirHandle, fileName, metadata.FileTypeRegular, 0o644, 1)
			if err != nil {
				return nil, err
			}
			t.fileHandles = append(t.fileHandles, fileHandle)
			t.lookupDir = append(t.lookupDir, dirHandle)
			t.lookupName = append(t.lookupName, fileName)
		}
	}
	return t, nil
}

// createShare creates a share + its root directory and returns the root handle.
func createShare(ctx context.Context, store metadata.Store, shareName string) (metadata.FileHandle, error) {
	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		return nil, fmt.Errorf("CreateShare(%q): %w", shareName, err)
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		return nil, fmt.Errorf("CreateRootDirectory(%q): %w", shareName, err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		return nil, fmt.Errorf("EncodeFileHandle: %w", err)
	}
	return rootHandle, nil
}

// putChild creates one directory or regular file under parentHandle and wires
// the parent/child/link-count edges, mirroring storetest.createTestFile/Dir.
func putChild(ctx context.Context, store metadata.Store, shareName string, parentHandle metadata.FileHandle, name string, ftype metadata.FileType, mode uint32, nlink uint32) (metadata.FileHandle, error) {
	fullPath, err := childFullPath(ctx, store, parentHandle, name)
	if err != nil {
		return nil, err
	}
	handle, err := store.GenerateHandle(ctx, shareName, fullPath)
	if err != nil {
		return nil, fmt.Errorf("GenerateHandle(%q): %w", fullPath, err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, fmt.Errorf("DecodeFileHandle: %w", err)
	}
	file := &metadata.File{
		ShareName: shareName,
		Path:      fullPath,
		FileAttr: metadata.FileAttr{
			Type: ftype,
			Mode: mode,
			UID:  1000,
			GID:  1000,
		},
	}
	file.ID = id
	if err := store.PutFile(ctx, file); err != nil {
		return nil, fmt.Errorf("PutFile(%q): %w", fullPath, err)
	}
	if err := store.SetParent(ctx, handle, parentHandle); err != nil {
		return nil, fmt.Errorf("SetParent(%q): %w", fullPath, err)
	}
	if err := store.SetChild(ctx, parentHandle, name, handle); err != nil {
		return nil, fmt.Errorf("SetChild(%q): %w", name, err)
	}
	if err := store.SetLinkCount(ctx, handle, nlink); err != nil {
		return nil, fmt.Errorf("SetLinkCount(%q): %w", fullPath, err)
	}
	return handle, nil
}

// childFullPath joins the parent directory's path with name so path-keyed
// backends (postgres) get a unique, non-empty path per entry.
func childFullPath(ctx context.Context, store metadata.Store, parentHandle metadata.FileHandle, name string) (string, error) {
	parent, err := store.GetFile(ctx, parentHandle)
	if err != nil {
		return "", fmt.Errorf("GetFile(parent): %w", err)
	}
	parentPath := parent.Path
	if parentPath == "" || parentPath == "/" {
		return "/" + name, nil
	}
	return parentPath + "/" + name, nil
}
