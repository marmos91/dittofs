package metadata

import "context"

// namespaceAccess pins a directory while an operation changes its children.
// Removing or reparenting that directory requires exclusive access instead.
type namespaceAccess struct {
	handle    FileHandle
	exclusive bool
}

type namespaceSlot struct {
	index     uint32
	exclusive bool
}

type namespaceGuard struct {
	service *Service
	slots   [4]namespaceSlot
	count   int
}

// namespaceShard hashes the decoded identity: alternate UUID spellings accepted
// by the stores must not acquire independent lifecycle guards for one inode.
func namespaceShard(handle FileHandle) (uint32, error) {
	share, id, err := DecodeFileHandle(handle)
	if err != nil {
		return 0, err
	}
	h := uint32(2166136261)
	for i := 0; i < len(share); i++ {
		h = (h ^ uint32(share[i])) * 16777619
	}
	h *= 16777619
	for _, b := range id {
		h = (h ^ uint32(b)) * 16777619
	}
	return h % parentLinkShardCount, nil
}

// lockNamespace takes the complete set before any name, parent-link or pending
// write lock. Sorting and promoting colliding slots avoids both lock inversion
// and recursive RWMutex acquisition. Four identities suffice for a rename's
// two parents, source directory and overwritten directory.
//
// decision: these guards coordinate one Service's namespace operations. Direct
// backend mutations and a second Service sharing its store do not participate.
func (s *Service) lockNamespace(accesses ...namespaceAccess) (namespaceGuard, error) {
	g := namespaceGuard{service: s}
	for _, access := range accesses {
		index, err := namespaceShard(access.handle)
		if err != nil {
			return namespaceGuard{}, err
		}
		found := false
		for i := 0; i < g.count; i++ {
			if g.slots[i].index == index {
				g.slots[i].exclusive = g.slots[i].exclusive || access.exclusive
				found = true
				break
			}
		}
		if found {
			continue
		}
		i := g.count
		for i > 0 && g.slots[i-1].index > index {
			g.slots[i] = g.slots[i-1]
			i--
		}
		g.slots[i] = namespaceSlot{index: index, exclusive: access.exclusive}
		g.count++
	}
	for i := 0; i < g.count; i++ {
		mu := &s.namespaceShards[g.slots[i].index]
		if g.slots[i].exclusive {
			mu.Lock()
		} else {
			mu.RLock()
		}
	}
	return g, nil
}

// unlock is safe to call before notification and again from deferred cleanup.
func (g *namespaceGuard) unlock() {
	for g.count > 0 {
		g.count--
		slot := g.slots[g.count]
		mu := &g.service.namespaceShards[slot.index]
		if slot.exclusive {
			mu.Unlock()
		} else {
			mu.RUnlock()
		}
	}
}

func transactionDirectory(ctx context.Context, tx Transaction, handle FileHandle) (*File, error) {
	dir, err := tx.GetFile(ctx, handle)
	if err != nil {
		return nil, err
	}
	if dir == nil {
		return nil, &StoreError{Code: ErrNotFound, Message: "parent directory no longer exists"}
	}
	if dir.Type != FileTypeDirectory {
		return nil, &StoreError{Code: ErrNotDirectory, Message: "parent is not a directory"}
	}
	return dir, nil
}

func requireChildIdentity(ctx context.Context, tx Transaction, parent FileHandle, name string, expected FileHandle) error {
	actual, err := tx.GetChild(ctx, parent, name)
	if err != nil {
		return err
	}
	if string(actual) != string(expected) {
		return &StoreError{Code: ErrConflict, Message: "directory entry changed during removal", Path: name}
	}
	return nil
}

func requireEmptyDirectory(ctx context.Context, tx Transaction, handle FileHandle, name string) error {
	entries, _, err := tx.ListChildren(ctx, handle, "", 1, NamesOnly)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return &StoreError{Code: ErrNotEmpty, Message: "directory not empty", Path: name}
	}
	return nil
}
