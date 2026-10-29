package badger

import (
	"fmt"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// A share with more rows than one transaction may delete is removed whole: no
// file row or directory entry of it is left, and the usage it held is given
// back. 25,000 files in one directory is well past what a single transaction
// takes at the default memtable.
func TestDeleteShare_MoreRowsThanOneTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("writes and deletes 25,000 files: about a second, ten under the race detector")
	}
	ctx := t.Context()
	s, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const share, files, owner = "/big", 25_000, 7
	root, err := s.CreateRootDirectory(ctx, share, &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(root)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	for first := 0; first < files; first += 1000 {
		err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
			for i := first; i < first+1000; i++ {
				name := fmt.Sprintf("f%05d", i)
				h, err := tx.GenerateHandle(ctx, share, "/"+name)
				if err != nil {
					return err
				}
				_, id, err := metadata.DecodeFileHandle(h)
				if err != nil {
					return err
				}
				if err := tx.SetLinkCount(ctx, h, 1); err != nil {
					return err
				}
				if err := tx.UpdateAttrs(ctx, &metadata.File{ID: id, ShareName: share, Path: "/" + name, FileAttr: metadata.FileAttr{
					Type: metadata.FileTypeRegular, Mode: 0o644, UID: owner, GID: owner, Size: 1,
					PayloadID: metadata.PayloadID("big/" + id.String()),
				}}); err != nil {
					return err
				}
				if err := tx.SetParent(ctx, h, rootHandle); err != nil {
					return err
				}
				if err := tx.SetChild(ctx, rootHandle, name, h); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("create files %d..: %v", first, err)
		}
	}
	if u, err := s.GetQuotaUsage(share, metadata.QuotaScopeUser, owner); err != nil || u.Files != files {
		t.Fatalf("usage before = %+v, %v; want %d files", u, err, files)
	}

	if err := s.DeleteShare(ctx, share); err != nil {
		t.Fatalf("DeleteShare: %v", err)
	}

	if _, err := s.GetRootHandle(ctx, share); !metadata.IsNotFoundError(err) {
		t.Fatalf("the share record is still there: %v", err)
	}
	left, entries := 0, 0
	if err := s.db.View(func(txn *badgerdb.Txn) error {
		it := txn.NewIterator(badgerdb.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek([]byte(prefixFile)); it.ValidForPrefix([]byte(prefixFile)); it.Next() {
			val, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			if f, err := decodeFile(val); err == nil && f.ShareName == share {
				left++
			}
		}
		for _, prefix := range [][]byte{keyChildPrefix(root.ID), keyChildNamePrefix(root.ID)} {
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				entries++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if left != 0 || entries != 0 {
		t.Fatalf("%d file rows and %d directory entries of the share are left", left, entries)
	}
	if u, err := s.GetQuotaUsage(share, metadata.QuotaScopeUser, owner); err != nil || u.Files != 0 || u.Bytes != 0 {
		t.Fatalf("usage after = %+v, %v; want none", u, err)
	}
}
