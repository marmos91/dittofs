//go:build integration

package postgres_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// rootAuthCtx is the UID 0 caller every operation in this test runs as.
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
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL hard-link race test")
	}
	store := newPostgresStore(t)
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
