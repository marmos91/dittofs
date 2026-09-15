package handlers

import "context"

// EnumerateOpenFiles streams the metadata file handle of every file with a
// live SMB open. It implements the runtime's OpenFileEnumerator interface
// (the handler is registered as the "smb_open_files" adapter provider),
// feeding the block-GC open-handle hold (#1448): a file that is unlinked
// (e.g. over NFS) while an SMB client still holds it open must keep its
// blocks until the last SMB close. Session teardown removes the entries and
// thereby releases the hold.
//
// Handles are collected during the Range and emitted afterwards so fn may
// perform blocking work (metadata-store reads) without holding the map's
// internal locks.
func (h *Handler) EnumerateOpenFiles(_ context.Context, fn func(fileHandle []byte) error) error {
	var handles [][]byte
	h.files.Range(func(_, value any) bool {
		of, ok := value.(*OpenFile)
		if !ok || of == nil {
			return true
		}
		// Under the handle's lock: SET_REPARSE_POINT repoints a live handle's
		// MetadataHandle, so a bare read can observe the slice header mid-swap.
		fh := of.GetMetadataHandle()
		if len(fh) == 0 {
			return true
		}
		handles = append(handles, []byte(fh))
		return true
	})
	for _, fh := range handles {
		if err := fn(fh); err != nil {
			return err
		}
	}
	return nil
}
