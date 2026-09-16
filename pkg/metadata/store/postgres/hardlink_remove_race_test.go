//go:build integration

package postgres_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// newRaceTestStore builds a PostgresMetadataStore from DITTOFS_TEST_POSTGRES_DSN.
func newRaceTestStore(t *testing.T) *postgres.PostgresMetadataStore {
	t.Helper()
	dsn := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL hard-link race test")
	}
	cfg := &postgres.PostgresMetadataStoreConfig{SSLMode: "disable", AutoMigrate: true}
	for _, kv := range strings.Fields(dsn) {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "host":
			cfg.Host = parts[1]
		case "port":
			p, err := strconv.Atoi(parts[1])
			if err != nil {
				t.Fatalf("parse port: %v", err)
			}
			cfg.Port = p
		case "user":
			cfg.User = parts[1]
		case "password":
			cfg.Password = parts[1]
		case "dbname", "database":
			cfg.Database = parts[1]
		case "sslmode", "ssl_mode":
			cfg.SSLMode = parts[1]
		}
	}
	caps := metadata.FilesystemCapabilities{
		MaxReadSize: 1048576, PreferredReadSize: 1048576,
		MaxWriteSize: 1048576, PreferredWriteSize: 1048576,
		MaxFileSize: 9223372036854775807, MaxFilenameLen: 255,
		MaxPathLen: 4096, MaxHardLinkCount: 32767,
		SupportsHardLinks: true, SupportsSymlinks: true,
		CaseSensitive: true, CasePreserving: true, TimestampResolution: 1,
	}
	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func rootAuthCtx() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "test",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
}

// TestRemoveFileHardLinkRace_PayloadFreedWhileLinked drives RemoveFile and
// CreateHardLink at the same inode concurrently and asserts that RemoveFile
// never reports the content free while a second name still references it.
//
// Both operations read the link count inside their transaction and write an
// absolute value back. Under READ COMMITTED with an unlocked read that is a
// lost update: the remove reads nlink=1, the link commits nlink=2, and the
// remove then writes 0 and hands the caller a non-empty PayloadID — telling it
// to delete blocks the surviving link still needs.
//
// A non-empty PayloadID together with a directory entry that still resolves to
// the inode is the failure; the link count is reported alongside it because a
// count of 1 after both operations is what the survivor is entitled to.
func TestRemoveFileHardLinkRace_PayloadFreedWhileLinked(t *testing.T) {
	store := newRaceTestStore(t)
	ctx := context.Background()

	const shareName = "/racetest"
	if err := store.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	svc := metadata.New()
	if err := svc.RegisterStoreForShare(shareName, store); err != nil {
		t.Fatalf("RegisterStoreForShare: %v", err)
	}

	actx := rootAuthCtx()

	// Roughly six in ten iterations lost the update before the fix, so fifty
	// rounds put a miss out of reach while keeping the test under a minute.
	const iterations = 50
	var losses int
	for i := 0; i < iterations; i++ {
		victim, _, err := svc.CreateFile(actx, rootHandle, "victim", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
		})
		if err != nil {
			t.Fatalf("iteration %d: CreateFile: %v", i, err)
		}
		victimHandle, err := metadata.EncodeFileHandle(victim)
		if err != nil {
			t.Fatalf("iteration %d: EncodeFileHandle: %v", i, err)
		}

		var (
			wg          sync.WaitGroup
			start       = make(chan struct{})
			removed     *metadata.File
			removeErr   error
			hardLinkErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			removed, _, removeErr = svc.RemoveFile(actx, rootHandle, "victim")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, hardLinkErr = svc.CreateHardLink(actx, rootHandle, "survivor", victimHandle)
		}()
		close(start)
		wg.Wait()

		// The interesting interleaving is the one where both succeeded: the
		// name "survivor" now points at the inode and the remove believed it
		// dropped the last link.
		if removeErr == nil && hardLinkErr == nil && removed != nil && removed.PayloadID != "" {
			count, cErr := store.GetLinkCount(ctx, victimHandle)
			if cErr != nil {
				t.Fatalf("iteration %d: GetLinkCount: %v", i, cErr)
			}
			losses++
			if losses <= 5 {
				t.Logf("iteration %d: RemoveFile returned PayloadID=%q while %q still links the inode (nlink=%d)",
					i, removed.PayloadID, "survivor", count)
			}
		}

		// Clean up whichever names survived so the next iteration starts fresh.
		if hardLinkErr == nil {
			if _, _, err := svc.RemoveFile(actx, rootHandle, "survivor"); err != nil {
				t.Fatalf("iteration %d: cleanup RemoveFile(survivor): %v", i, err)
			}
		}
		if removeErr != nil {
			if _, _, err := svc.RemoveFile(actx, rootHandle, "victim"); err != nil {
				t.Fatalf("iteration %d: cleanup RemoveFile(victim): %v", i, err)
			}
		}
	}

	if losses > 0 {
		t.Fatalf("RemoveFile reported the content free while a hard link still referenced it in %d of %d iterations",
			losses, iterations)
	}
}
