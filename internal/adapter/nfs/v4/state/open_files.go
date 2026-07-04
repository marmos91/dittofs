package state

import "context"

// EnumerateOpenFiles streams the metadata file handle of every file that
// currently has at least one live NFSv4 open state. It implements the
// runtime's OpenFileEnumerator interface (the state manager is registered as
// the "nfs" adapter provider), feeding the block-GC open-handle hold (#1448):
// an open-but-unlinked file's blocks must survive GC until the last CLOSE
// (or client lease expiry, which purges the open states and thereby releases
// the hold).
//
// Handles are copied out under the read lock and emitted afterwards so fn may
// perform blocking work (metadata-store reads) without stalling state-machine
// operations.
func (sm *StateManager) EnumerateOpenFiles(_ context.Context, fn func(fileHandle []byte) error) error {
	sm.mu.RLock()
	handles := make([][]byte, 0, len(sm.openStateByFile))
	for key, opens := range sm.openStateByFile {
		if len(opens) == 0 {
			continue
		}
		handles = append(handles, []byte(key))
	}
	sm.mu.RUnlock()

	for _, h := range handles {
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}
