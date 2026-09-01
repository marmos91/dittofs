package storetest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// runStatfsOps pins that filesystem statistics are scoped to the share they
// were asked about.
//
// A statfs that reported the store-wide total would look entirely healthy on a
// single-share store, which is what most tests build — so the only way to see
// the bug is to put files in a second share and check the first one's answer
// did not move.
func runStatfsOps(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("CountsOnlyItsOwnShare", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		rootA := createTestShare(t, store, "/statfs-a")
		rootB := createTestShare(t, store, "/statfs-b")

		createTestFile(t, store, "/statfs-a", rootA, "only-file.txt", 0644)

		before, err := store.GetFilesystemStatistics(ctx, rootA)
		require.NoError(t, err)
		require.NotNil(t, before)

		// Fill the neighbouring share. Share A's numbers must not budge.
		for _, name := range []string{"b1.txt", "b2.txt", "b3.txt"} {
			createTestFile(t, store, "/statfs-b", rootB, name, 0644)
		}

		after, err := store.GetFilesystemStatistics(ctx, rootA)
		require.NoError(t, err)
		require.Equal(t, before.UsedFiles, after.UsedFiles,
			"share A's file count moved when share B was filled — statfs is not share-scoped")
		require.Equal(t, before.UsedBytes, after.UsedBytes,
			"share A's byte count moved when share B was filled — statfs is not share-scoped")
	})
}
