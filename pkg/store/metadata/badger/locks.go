package badger

import (
	"sort"
	"sync"
)

// lockFile acquires a per-file mutex to serialize writes to the same file.
// This prevents BadgerDB transaction conflicts when parallel requests modify the same file.
func (s *BadgerMetadataStore) lockFile(fileID string) *sync.Mutex {
	mu, _ := s.fileLocks.LoadOrStore(fileID, &sync.Mutex{})
	fileMu := mu.(*sync.Mutex)
	fileMu.Lock()
	return fileMu
}

// unlockFile releases the per-file mutex.
func (s *BadgerMetadataStore) unlockFile(_ string, mu *sync.Mutex) {
	mu.Unlock()
}

// lockDir acquires a per-directory mutex to serialize mutations to the same directory.
// This prevents BadgerDB transaction conflicts when parallel operations modify directory entries.
//
// Operations that should use this lock:
//   - Create (creates child entry in parent directory)
//   - Remove (removes child entry from parent directory)
//   - Move (modifies entries in source and/or destination directories)
//   - CreateSymlink, CreateSpecialFile, CreateHardLink (create entries in directories)
func (s *BadgerMetadataStore) lockDir(dirID string) *sync.Mutex {
	mu, _ := s.dirLocks.LoadOrStore(dirID, &sync.Mutex{})
	dirMu := mu.(*sync.Mutex)
	dirMu.Lock()
	return dirMu
}

// unlockDir releases the per-directory mutex.
func (s *BadgerMetadataStore) unlockDir(_ string, mu *sync.Mutex) {
	mu.Unlock()
}

// lockDirsOrdered acquires locks on multiple directories in consistent order.
// This prevents deadlock when operations touch multiple directories (e.g., Move).
//
// The locks are acquired in lexicographic order of directory IDs, ensuring that
// two concurrent operations that need locks on directories A and B will always
// acquire them in the same order (A then B), preventing circular wait.
//
// Returns the locks in the same order as the input dirIDs for proper unlocking.
func (s *BadgerMetadataStore) lockDirsOrdered(dirIDs ...string) []*sync.Mutex {
	if len(dirIDs) == 0 {
		return nil
	}

	// Remove duplicates (e.g., move within same directory)
	unique := make(map[string]bool)
	for _, id := range dirIDs {
		unique[id] = true
	}

	// Sort for consistent ordering
	sorted := make([]string, 0, len(unique))
	for id := range unique {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	// Acquire locks in sorted order
	locks := make([]*sync.Mutex, len(sorted))
	for i, id := range sorted {
		locks[i] = s.lockDir(id)
	}

	return locks
}

// unlockDirs releases multiple directory locks.
// The locks slice should be from lockDirsOrdered.
func (s *BadgerMetadataStore) unlockDirs(locks []*sync.Mutex) {
	for _, mu := range locks {
		if mu != nil {
			mu.Unlock()
		}
	}
}
