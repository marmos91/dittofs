package badger

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Key prefixes for durable handle storage in BadgerDB.
const (
	// Primary key: dh:id:{uuid} -> JSON(PersistedDurableHandle)
	prefixDHID = "dh:id:"

	// Index by CreateGuid: dh:cguid:{hex} -> id (string)
	prefixDHCreateGuid = "dh:cguid:"

	// Index by AppInstanceId: dh:appid:{hex}:{id} -> id (string)
	prefixDHAppInstanceId = "dh:appid:"

	// Index by FileID: dh:fid:{hex} -> id (string)
	prefixDHFileID = "dh:fid:"

	// Index by FileHandle: dh:fh:{hex}:{id} -> id (string)
	prefixDHFileHandle = "dh:fh:"

	// Index by Share: dh:share:{name}:{id} -> id (string)
	prefixDHShare = "dh:share:"
)

// badgerDurableStore implements lock.DurableHandleStore using BadgerDB.
// Primary storage: dh:id:{uuid} -> JSON(PersistedDurableHandle).
// Secondary indices enable efficient lookups by FileID, CreateGuid,
// AppInstanceId, FileHandle, and ShareName.
type badgerDurableStore struct {
	db *badgerdb.DB
}

func newBadgerDurableStore(db *badgerdb.DB) *badgerDurableStore {
	return &badgerDurableStore{
		db: db,
	}
}

func hexEncode16(b [16]byte) string {
	return hex.EncodeToString(b[:])
}

func hexEncodeBytes(b []byte) string {
	return hex.EncodeToString(b)
}

var zeroGUID [16]byte

func (s *badgerDurableStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		// If updating an existing handle, first clean up old indices
		existingItem, err := txn.Get([]byte(prefixDHID + handle.ID))
		if err == nil {
			// Handle exists -- remove old indices before writing new ones
			var existing lock.PersistedDurableHandle
			err = existingItem.Value(func(val []byte) error {
				return json.Unmarshal(val, &existing)
			})
			if err != nil {
				return fmt.Errorf("failed to unmarshal existing durable handle: %w", err)
			}
			if err := s.deleteIndicesTx(txn, &existing); err != nil {
				return err
			}
		} else if err != badgerdb.ErrKeyNotFound {
			return err
		}

		return s.putDurableHandleTx(txn, handle)
	})
}

func (s *badgerDurableStore) putDurableHandleTx(txn *badgerdb.Txn, handle *lock.PersistedDurableHandle) error {
	data, err := json.Marshal(handle)
	if err != nil {
		return fmt.Errorf("failed to marshal durable handle: %w", err)
	}

	primaryKey := []byte(prefixDHID + handle.ID)
	if err := txn.Set(primaryKey, data); err != nil {
		return err
	}

	fidKey := []byte(prefixDHFileID + hexEncode16(handle.FileID))
	if err := txn.Set(fidKey, []byte(handle.ID)); err != nil {
		return err
	}

	if handle.CreateGuid != zeroGUID {
		cguidKey := []byte(prefixDHCreateGuid + hexEncode16(handle.CreateGuid))
		if err := txn.Set(cguidKey, []byte(handle.ID)); err != nil {
			return err
		}
	}

	if handle.AppInstanceId != zeroGUID {
		appIdKey := []byte(prefixDHAppInstanceId + hexEncode16(handle.AppInstanceId) + ":" + handle.ID)
		if err := txn.Set(appIdKey, []byte(handle.ID)); err != nil {
			return err
		}
	}

	if len(handle.MetadataHandle) > 0 {
		fhKey := []byte(prefixDHFileHandle + hexEncodeBytes(handle.MetadataHandle) + ":" + handle.ID)
		if err := txn.Set(fhKey, []byte(handle.ID)); err != nil {
			return err
		}
	}

	shareKey := []byte(prefixDHShare + handle.ShareName + ":" + handle.ID)
	if err := txn.Set(shareKey, []byte(handle.ID)); err != nil {
		return err
	}

	return nil
}

func (s *badgerDurableStore) deleteIndicesTx(txn *badgerdb.Txn, handle *lock.PersistedDurableHandle) error {
	fidKey := []byte(prefixDHFileID + hexEncode16(handle.FileID))
	if err := txn.Delete(fidKey); err != nil && err != badgerdb.ErrKeyNotFound {
		return err
	}

	if handle.CreateGuid != zeroGUID {
		cguidKey := []byte(prefixDHCreateGuid + hexEncode16(handle.CreateGuid))
		if err := txn.Delete(cguidKey); err != nil && err != badgerdb.ErrKeyNotFound {
			return err
		}
	}

	if handle.AppInstanceId != zeroGUID {
		appIdKey := []byte(prefixDHAppInstanceId + hexEncode16(handle.AppInstanceId) + ":" + handle.ID)
		if err := txn.Delete(appIdKey); err != nil && err != badgerdb.ErrKeyNotFound {
			return err
		}
	}

	if len(handle.MetadataHandle) > 0 {
		fhKey := []byte(prefixDHFileHandle + hexEncodeBytes(handle.MetadataHandle) + ":" + handle.ID)
		if err := txn.Delete(fhKey); err != nil && err != badgerdb.ErrKeyNotFound {
			return err
		}
	}

	shareKey := []byte(prefixDHShare + handle.ShareName + ":" + handle.ID)
	if err := txn.Delete(shareKey); err != nil && err != badgerdb.ErrKeyNotFound {
		return err
	}

	return nil
}

func (s *badgerDurableStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var handle *lock.PersistedDurableHandle

	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get([]byte(prefixDHID + id))
		if err == badgerdb.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			handle = &lock.PersistedDurableHandle{}
			return json.Unmarshal(val, handle)
		})
	})

	if err != nil {
		return nil, err
	}
	return handle, nil
}

func (s *badgerDurableStore) getHandleByIndex(txn *badgerdb.Txn, indexKey []byte) (*lock.PersistedDurableHandle, error) {
	item, err := txn.Get(indexKey)
	if err == badgerdb.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var id string
	err = item.Value(func(val []byte) error {
		id = string(val)
		return nil
	})
	if err != nil {
		return nil, err
	}

	primaryItem, err := txn.Get([]byte(prefixDHID + id))
	if err == badgerdb.ErrKeyNotFound {
		return nil, nil // Index points to deleted record (stale index)
	}
	if err != nil {
		return nil, err
	}

	var handle lock.PersistedDurableHandle
	err = primaryItem.Value(func(val []byte) error {
		return json.Unmarshal(val, &handle)
	})
	if err != nil {
		return nil, err
	}

	return &handle, nil
}

func (s *badgerDurableStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var handle *lock.PersistedDurableHandle

	err := s.db.View(func(txn *badgerdb.Txn) error {
		var err error
		handle, err = s.getHandleByIndex(txn, []byte(prefixDHFileID+hexEncode16(fileID)))
		return err
	})

	if err != nil {
		return nil, err
	}
	return handle, nil
}

func (s *badgerDurableStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var handle *lock.PersistedDurableHandle

	err := s.db.View(func(txn *badgerdb.Txn) error {
		var err error
		handle, err = s.getHandleByIndex(txn, []byte(prefixDHCreateGuid+hexEncode16(createGuid)))
		return err
	})

	if err != nil {
		return nil, err
	}
	return handle, nil
}

// getHandlesByPrefix scans a secondary index prefix and resolves each entry to
// its primary record. Stale index entries are silently skipped.
func (s *badgerDurableStore) getHandlesByPrefix(txn *badgerdb.Txn, prefix []byte) ([]*lock.PersistedDurableHandle, error) {
	opts := badgerdb.DefaultIteratorOptions
	opts.Prefix = prefix
	opts.PrefetchValues = true

	it := txn.NewIterator(opts)
	defer it.Close()

	var result []*lock.PersistedDurableHandle
	for it.Rewind(); it.Valid(); it.Next() {
		var id string
		err := it.Item().Value(func(val []byte) error {
			id = string(val)
			return nil
		})
		if err != nil {
			return nil, err
		}

		primaryItem, err := txn.Get([]byte(prefixDHID + id))
		if err == badgerdb.ErrKeyNotFound {
			continue // Stale index
		}
		if err != nil {
			return nil, err
		}

		var handle lock.PersistedDurableHandle
		err = primaryItem.Value(func(val []byte) error {
			return json.Unmarshal(val, &handle)
		})
		if err != nil {
			return nil, err
		}

		result = append(result, &handle)
	}

	return result, nil
}

func (s *badgerDurableStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result []*lock.PersistedDurableHandle
	err := s.db.View(func(txn *badgerdb.Txn) error {
		var err error
		result, err = s.getHandlesByPrefix(txn, []byte(prefixDHAppInstanceId+hexEncode16(appInstanceId)+":"))
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *badgerDurableStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result []*lock.PersistedDurableHandle
	err := s.db.View(func(txn *badgerdb.Txn) error {
		var err error
		result, err = s.getHandlesByPrefix(txn, []byte(prefixDHFileHandle+hexEncodeBytes(fileHandle)+":"))
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *badgerDurableStore) DeleteDurableHandle(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		return s.deleteDurableHandleTx(txn, id)
	})
}

func (s *badgerDurableStore) deleteDurableHandleTx(txn *badgerdb.Txn, id string) error {
	item, err := txn.Get([]byte(prefixDHID + id))
	if err == badgerdb.ErrKeyNotFound {
		return nil // Already gone
	}
	if err != nil {
		return err
	}

	var handle lock.PersistedDurableHandle
	err = item.Value(func(val []byte) error {
		return json.Unmarshal(val, &handle)
	})
	if err != nil {
		return err
	}

	if err := s.deleteIndicesTx(txn, &handle); err != nil {
		return err
	}

	return txn.Delete([]byte(prefixDHID + id))
}

func (s *badgerDurableStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result []*lock.PersistedDurableHandle

	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixDHID)

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()

			err := item.Value(func(val []byte) error {
				handle := &lock.PersistedDurableHandle{}
				if err := json.Unmarshal(val, handle); err != nil {
					return err
				}
				result = append(result, handle)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *badgerDurableStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result []*lock.PersistedDurableHandle
	err := s.db.View(func(txn *badgerdb.Txn) error {
		var err error
		result, err = s.getHandlesByPrefix(txn, []byte(prefixDHShare+shareName+":"))
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *badgerDurableStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var expiredIDs []string
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixDHID)

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()

			err := item.Value(func(val []byte) error {
				var handle lock.PersistedDurableHandle
				if err := json.Unmarshal(val, &handle); err != nil {
					return err
				}

				expiresAt := handle.DisconnectedAt.Add(time.Duration(handle.TimeoutMs) * time.Millisecond)
				if !expiresAt.After(now) { // expiresAt <= now means expired
					key := item.KeyCopy(nil)
					id := strings.TrimPrefix(string(key), prefixDHID)
					expiredIDs = append(expiredIDs, id)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	if len(expiredIDs) == 0 {
		return 0, nil
	}

	err = s.db.Update(func(txn *badgerdb.Txn) error {
		for _, id := range expiredIDs {
			if err := s.deleteDurableHandleTx(txn, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	return len(expiredIDs), nil
}

// BadgerMetadataStore DurableHandleStore delegation

var _ lock.DurableHandleStore = (*BadgerMetadataStore)(nil)

func (s *BadgerMetadataStore) getDurableStore() *badgerDurableStore {
	s.durableStoreMu.Lock()
	defer s.durableStoreMu.Unlock()
	if s.durableStore == nil {
		s.durableStore = newBadgerDurableStore(s.db)
	}
	return s.durableStore
}

func (s *BadgerMetadataStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	return s.getDurableStore().PutDurableHandle(ctx, handle)
}

func (s *BadgerMetadataStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandle(ctx, id)
}

func (s *BadgerMetadataStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandleByFileID(ctx, fileID)
}

func (s *BadgerMetadataStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandleByCreateGuid(ctx, createGuid)
}

func (s *BadgerMetadataStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandlesByAppInstanceId(ctx, appInstanceId)
}

func (s *BadgerMetadataStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandlesByFileHandle(ctx, fileHandle)
}

func (s *BadgerMetadataStore) DeleteDurableHandle(ctx context.Context, id string) error {
	return s.getDurableStore().DeleteDurableHandle(ctx, id)
}

func (s *BadgerMetadataStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().ListDurableHandles(ctx)
}

func (s *BadgerMetadataStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().ListDurableHandlesByShare(ctx, shareName)
}

func (s *BadgerMetadataStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	return s.getDurableStore().DeleteExpiredDurableHandles(ctx, now)
}

// DurableHandleStore returns this store as a DurableHandleStore.
func (s *BadgerMetadataStore) DurableHandleStore() lock.DurableHandleStore {
	return s
}
