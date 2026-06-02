// Package trash implements the runtime recycle-bin service: listing, restoring,
// and emptying the per-share #recycle bin populated by the metadata layer when
// trash is enabled.
//
// Bin membership is defined by LOCATION: a node under a share's #recycle
// directory carrying a non-nil FileAttr.DeletedAt is a recycled root. The
// metadata layer recycles a deletion by moving the victim (a file, or a whole
// directory subtree as one entry) under #recycle and stamping DeletedAt /
// OriginalPath / DeletedBy on it. This service is the inverse: it walks the bin
// to enumerate those roots, moves an entry back out and clears its stamp on
// restore, and permanently destroys entries (freeing their CAS blocks) on empty.
package trash

import (
	"context"
	stderrors "errors"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// defaultReapInterval is the reaper cadence used when New is given a zero
// interval. The reaper goroutine (see reaper.go) ticks at this interval to
// enforce per-share retention and max-size policy.
const defaultReapInterval = time.Hour

// Entry describes one recycled root in a share's #recycle bin.
type Entry struct {
	// BinPath is the entry's path under #recycle (e.g. "doc.txt" or "a/b/c.txt").
	BinPath string `json:"bin_path"`
	// OriginalPath is the share-relative path the node occupied before deletion.
	OriginalPath string `json:"original_path"`
	// DeletedBy is the principal that recycled the node (display only).
	DeletedBy string `json:"deleted_by"`
	// DeletedAt is when the node was recycled.
	DeletedAt time.Time `json:"deleted_at"`
	// Size is the file size in bytes (0 for directories).
	Size uint64 `json:"size"`
	// IsDir reports whether the entry is a directory subtree.
	IsDir bool `json:"is_dir"`
}

// Config is the per-share recycle-bin policy the service operates under. The
// runtime derives it from share configuration; the service uses it for
// retention/size reaping (later tasks) and to gate listing on Enabled.
type Config struct {
	Enabled         bool
	RetentionDays   int
	RestrictToAdmin bool
	MaxBytes        int64
	ExcludePatterns []string
}

// Deps is the narrow runtime surface the service needs, kept as an interface so
// the service is testable without a full Runtime.
type Deps interface {
	// MetadataServiceForShare resolves the per-share metadata service and its
	// root handle. ok=false when the share is unknown.
	MetadataServiceForShare(shareName string) (svc *metadata.Service, root metadata.FileHandle, ok bool)
	// TrashConfigForShare returns the trash policy for the share. ok=false when
	// the share is unknown.
	TrashConfigForShare(shareName string) (Config, bool)
	// EnabledTrashShares lists the shares with trash enabled.
	EnabledTrashShares() []string
	// FreeBlocks frees the CAS blocks for a permanently-deleted file (the
	// payloadID and BlockRef list RemoveFile returned). Implemented by the
	// runtime via GetBlockStoreForHandle + blockStore.Delete. The blocks list is
	// required: blockStore.Delete only decrements per-block CAS RefCounts (so the
	// GC can reclaim now-unreferenced chunks) when it is given the file's blocks
	// — passing nil leaks the refcounts. A no-op when payloadID is empty.
	FreeBlocks(ctx context.Context, shareName string, root metadata.FileHandle, payloadID string, blocks []block.BlockRef) error
}

// Service lists, restores, and empties per-share recycle bins.
type Service struct {
	deps     Deps
	interval time.Duration
	stopCh   chan struct{}
}

// New constructs a trash Service. A zero reapInterval defaults to one hour. The
// stopCh backs Stop; call Start to launch the background reaper.
func New(deps Deps, reapInterval time.Duration) *Service {
	if reapInterval <= 0 {
		reapInterval = defaultReapInterval
	}
	return &Service{
		deps:     deps,
		interval: reapInterval,
		stopCh:   make(chan struct{}),
	}
}

// resolve returns the share's metadata service and root handle, or a NotFound
// StoreError when the share is unknown to the runtime.
func (s *Service) resolve(shareName string) (*metadata.Service, metadata.FileHandle, error) {
	svc, root, ok := s.deps.MetadataServiceForShare(shareName)
	if !ok {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "unknown share",
			Path:    shareName,
		}
	}
	return svc, root, nil
}

// List walks the share's #recycle bin and returns its recycled roots: nodes
// whose DeletedAt is set. A recycled directory is reported once as a subtree
// root; its children are not listed as separate entries.
func (s *Service) List(ctx *metadata.AuthContext, shareName string) ([]Entry, error) {
	svc, root, err := s.resolve(shareName)
	if err != nil {
		return nil, err
	}

	binHandle, err := svc.GetChild(ctx.Context, root, metadata.RecycleDirName)
	if err != nil {
		// No bin yet means nothing has ever been recycled: an empty list, not
		// an error.
		if metadata.IsNotFoundError(err) {
			return nil, nil
		}
		return nil, err
	}

	var entries []Entry
	if err := s.walkBin(ctx, svc, binHandle, "", &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// walkBin recursively descends the bin starting at dirHandle (whose path under
// #recycle is binPrefix). It appends an Entry for every recycled root it finds
// (a node with DeletedAt set) and, crucially, does NOT descend into a recycled
// subtree: the root is listed once and its children stay hidden. Non-recycled
// intermediary directories (the recreated original parent chain, e.g. the "a/b"
// under #recycle/a/b/c.txt) are descended into but not themselves listed.
func (s *Service) walkBin(ctx *metadata.AuthContext, svc *metadata.Service, dirHandle metadata.FileHandle, binPrefix string, out *[]Entry) error {
	var cookie uint64
	for {
		page, err := svc.ReadDirectory(ctx, dirHandle, cookie, 0)
		if err != nil {
			return err
		}
		for i := range page.Entries {
			e := &page.Entries[i]
			attr, err := s.entryAttr(ctx, svc, dirHandle, e)
			if err != nil {
				return err
			}
			childBinPath := joinBin(binPrefix, e.Name)
			if attr.DeletedAt != nil {
				// Recycled root: list it once, do not descend.
				*out = append(*out, Entry{
					BinPath:      childBinPath,
					OriginalPath: attr.OriginalPath,
					DeletedBy:    attr.DeletedBy,
					DeletedAt:    attr.DeletedAt.UTC(),
					Size:         attr.Size,
					IsDir:        attr.Type == metadata.FileTypeDirectory,
				})
				continue
			}
			// Non-recycled node. Descend into intermediary directories (the
			// recreated original parent chain) looking for deeper roots.
			if attr.Type == metadata.FileTypeDirectory {
				childHandle, err := svc.GetChild(ctx.Context, dirHandle, e.Name)
				if err != nil {
					return err
				}
				if err := s.walkBin(ctx, svc, childHandle, childBinPath, out); err != nil {
					return err
				}
			}
		}
		if !page.HasMore {
			return nil
		}
		cookie = page.NextCookie
	}
}

// entryAttr returns a directory entry's FileAttr, using the READDIRPLUS-style
// inline attrs when present and falling back to GetFile otherwise.
func (s *Service) entryAttr(ctx *metadata.AuthContext, svc *metadata.Service, dirHandle metadata.FileHandle, e *metadata.DirEntry) (*metadata.FileAttr, error) {
	if e.Attr != nil {
		return e.Attr, nil
	}
	handle := e.Handle
	if len(handle) == 0 {
		h, err := svc.GetChild(ctx.Context, dirHandle, e.Name)
		if err != nil {
			return nil, err
		}
		handle = h
	}
	file, err := svc.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}
	return &file.FileAttr, nil
}

// Status summarizes a share's recycle bin: whether trash is enabled, the
// number of recycled roots, their total size in bytes, and the oldest
// deletion time (nil when the bin is empty). It is a thin read-only roll-up
// over List and the share's trash config; it deliberately does NOT report
// reaper run-state.
func (s *Service) Status(ctx *metadata.AuthContext, shareName string) (*Status, error) {
	cfg, ok := s.deps.TrashConfigForShare(shareName)
	if !ok {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "unknown share",
			Path:    shareName,
		}
	}

	entries, err := s.List(ctx, shareName)
	if err != nil {
		return nil, err
	}

	status := &Status{
		Enabled:   cfg.Enabled,
		ItemCount: len(entries),
	}
	for i := range entries {
		status.TotalBytes += entries[i].Size
		t := entries[i].DeletedAt
		if status.Oldest == nil || t.Before(*status.Oldest) {
			oldest := t
			status.Oldest = &oldest
		}
	}
	return status, nil
}

// Status is the read-only roll-up returned by Service.Status.
type Status struct {
	// Enabled reports whether the share's trash policy is enabled.
	Enabled bool `json:"enabled"`
	// ItemCount is the number of recycled roots in the bin.
	ItemCount int `json:"item_count"`
	// TotalBytes is the summed Size of every recycled root.
	TotalBytes uint64 `json:"total_bytes"`
	// Oldest is the earliest DeletedAt across the bin, nil when empty.
	Oldest *time.Time `json:"oldest,omitempty"`
}

// Restore moves the bin entry at binPath back to dest (defaulting to the
// entry's OriginalPath when dest is empty) and clears its deletion metadata.
// Returns an AlreadyExists StoreError when the destination is already occupied;
// recreates the destination's parent chain when missing.
func (s *Service) Restore(ctx *metadata.AuthContext, shareName, binPath, dest string) error {
	svc, root, err := s.resolve(shareName)
	if err != nil {
		return err
	}

	binHandle, err := svc.GetChild(ctx.Context, root, metadata.RecycleDirName)
	if err != nil {
		return err
	}

	// Locate the entry's parent directory and leaf name within the bin.
	binSrcParent, binSrcName, err := resolveParent(ctx, svc, binHandle, binPath)
	if err != nil {
		return err
	}

	// The node must actually be a recycled root.
	srcHandle, err := svc.GetChild(ctx.Context, binSrcParent, binSrcName)
	if err != nil {
		return err
	}
	srcFile, err := svc.GetFile(ctx.Context, srcHandle)
	if err != nil {
		return err
	}
	if srcFile.DeletedAt == nil {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "not a recycled entry",
			Path:    binPath,
		}
	}

	// Default the restore destination to the entry's original location.
	relDest := strings.TrimPrefix(dest, "/")
	if relDest == "" {
		relDest = strings.TrimPrefix(srcFile.OriginalPath, "/")
	}
	if relDest == "" {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "no restore destination and entry has no original path",
			Path:    binPath,
		}
	}

	// Recreate the destination parent chain under the share root and reject a
	// restore onto an existing node rather than clobbering it.
	destParent, destName, err := ensureParent(ctx, svc, root, relDest)
	if err != nil {
		return err
	}
	if _, err := svc.GetChild(ctx.Context, destParent, destName); err == nil {
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "restore destination already exists",
			Path:    relDest,
		}
	} else if !metadata.IsNotFoundError(err) {
		return err
	}

	// Move the entry out of the bin, then clear its deletion stamp in place.
	if _, err := svc.Move(ctx, binSrcParent, binSrcName, destParent, destName); err != nil {
		return err
	}
	return clearStamp(ctx, svc, shareName, destParent, destName)
}

// clearStamp loads the restored node and nils its DeletedAt / OriginalPath /
// DeletedBy so it reads as live again. It writes through the share's metadata
// store directly: these three fields are not exposed by SetFileAttributes.
func clearStamp(ctx *metadata.AuthContext, svc *metadata.Service, shareName string, parent metadata.FileHandle, name string) error {
	handle, err := svc.GetChild(ctx.Context, parent, name)
	if err != nil {
		return err
	}
	store, err := svc.GetStoreForShare(shareName)
	if err != nil {
		return err
	}
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}
	file.DeletedAt = nil
	file.OriginalPath = ""
	file.DeletedBy = ""
	return store.PutFile(ctx.Context, file)
}

// Empty permanently removes every recycled root from the share's bin, freeing
// each removed file's CAS blocks, and returns the count of top-level entries
// removed. force is accepted for API symmetry but carries no behavior here: the
// REST layer enforces the admin gate, this layer just executes.
func (s *Service) Empty(ctx *metadata.AuthContext, shareName string, force bool) (int, error) {
	_ = force // advisory; admin gate is enforced at the REST layer.

	svc, root, err := s.resolve(shareName)
	if err != nil {
		return 0, err
	}

	binHandle, err := svc.GetChild(ctx.Context, root, metadata.RecycleDirName)
	if err != nil {
		if metadata.IsNotFoundError(err) {
			return 0, nil
		}
		return 0, err
	}

	// Collect the recycled roots first so we are not enumerating the bin while
	// mutating it.
	var roots []Entry
	if err := s.walkBin(ctx, svc, binHandle, "", &roots); err != nil {
		return 0, err
	}

	for _, e := range roots {
		parent, name, err := resolveParent(ctx, svc, binHandle, e.BinPath)
		if err != nil {
			return 0, err
		}
		if err := s.purgeEntry(ctx, svc, shareName, root, parent, name); err != nil {
			return 0, err
		}
	}

	// Purging recycled roots leaves behind the recreated original parent chain
	// (e.g. the "a/b" under #recycle/a/b/c.txt). Sweep those now-empty
	// intermediary directories out depth-first. The #recycle root itself is
	// left in place — only OnDisable removes it.
	if err := s.pruneEmptyDirs(ctx, svc, binHandle); err != nil {
		return 0, err
	}
	return len(roots), nil
}

// OnDisable is invoked when a share's trash policy transitions to disabled. It
// permanently empties the bin (freeing every recycled file's CAS blocks) and
// then removes the #recycle root directory from the share root entirely, so a
// re-enable starts from a clean slate. A share that never used trash (no
// #recycle dir) is a no-op.
//
// Empty's prune pass leaves #recycle empty, so RemoveDirectory on the bin root
// succeeds. Because the bin path "#recycle" satisfies metadata.inRecycle, that
// RemoveDirectory is a PERMANENT delete (the metadata layer does not re-recycle
// a deletion that is already inside the bin) rather than recycling the bin into
// itself.
func (s *Service) OnDisable(ctx *metadata.AuthContext, shareName string) error {
	if _, err := s.Empty(ctx, shareName, true); err != nil {
		return err
	}

	svc, root, err := s.resolve(shareName)
	if err != nil {
		return err
	}

	if _, err := svc.GetChild(ctx.Context, root, metadata.RecycleDirName); err != nil {
		// No bin means trash was never used: nothing to remove.
		if metadata.IsNotFoundError(err) {
			return nil
		}
		return err
	}

	// Empty left the bin logically empty; remove the now-empty #recycle root.
	// A surviving non-empty / other error is surfaced, not swallowed.
	_, err = svc.RemoveDirectory(ctx, root, metadata.RecycleDirName)
	return err
}

// pruneEmptyDirs removes now-empty intermediary directories under dirHandle
// depth-first, returning whether dirHandle itself is empty afterwards. The
// caller removes a returned-empty child; dirHandle (the #recycle root passed by
// Empty) is never removed here. Not-empty / already-gone removals are ignored
// so a concurrent recycle racing the sweep cannot fail an Empty.
func (s *Service) pruneEmptyDirs(ctx *metadata.AuthContext, svc *metadata.Service, dirHandle metadata.FileHandle) error {
	page, err := svc.ReadDirectory(ctx, dirHandle, 0, 0)
	if err != nil {
		return err
	}
	for i := range page.Entries {
		e := &page.Entries[i]
		attr, err := s.entryAttr(ctx, svc, dirHandle, e)
		if err != nil {
			return err
		}
		if attr.Type != metadata.FileTypeDirectory {
			continue
		}
		childHandle, err := svc.GetChild(ctx.Context, dirHandle, e.Name)
		if err != nil {
			if metadata.IsNotFoundError(err) {
				continue
			}
			return err
		}
		// Recurse first so the deepest empties are removed before their parent
		// is reconsidered.
		if err := s.pruneEmptyDirs(ctx, svc, childHandle); err != nil {
			return err
		}
		// Attempt removal; a non-empty or already-gone directory is fine.
		if _, err := svc.RemoveDirectory(ctx, dirHandle, e.Name); err != nil &&
			!metadata.IsNotFoundError(err) && !isNotEmpty(err) {
			return err
		}
	}
	return nil
}

// isNotEmpty reports whether err is a StoreError for a non-empty directory.
func isNotEmpty(err error) bool {
	var se *metadata.StoreError
	return stderrors.As(err, &se) && se.Code == metadata.ErrNotEmpty
}

// purgeEntry permanently deletes a single bin entry (a file or a whole subtree)
// from under parent, freeing CAS blocks for every removed file. A file is
// RemoveFile'd directly (it lives inside #recycle, so the metadata layer does a
// real delete returning the file's PayloadID). A directory is emptied
// depth-first — RemoveDirectory refuses a non-empty directory — then itself
// removed.
func (s *Service) purgeEntry(ctx *metadata.AuthContext, svc *metadata.Service, shareName string, root, parent metadata.FileHandle, name string) error {
	handle, err := svc.GetChild(ctx.Context, parent, name)
	if err != nil {
		return err
	}
	file, err := svc.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	if file.Type == metadata.FileTypeDirectory {
		if err := s.purgeChildren(ctx, svc, shareName, root, handle); err != nil {
			return err
		}
		_, err := svc.RemoveDirectory(ctx, parent, name)
		return err
	}

	removed, _, err := svc.RemoveFile(ctx, parent, name)
	if err != nil {
		return err
	}
	// Pass the removed file's BlockRef list, not just its payloadID: Delete only
	// decrements per-block CAS RefCounts (freeing now-unreferenced chunks for GC)
	// when given the blocks. Dropping them here would leak the refcounts (#832).
	return s.deps.FreeBlocks(ctx.Context, shareName, root, string(removed.PayloadID), removed.Blocks)
}

// purgeChildren empties a directory inside the bin by recursively purging every
// child, leaving the directory empty so its caller can RemoveDirectory it.
func (s *Service) purgeChildren(ctx *metadata.AuthContext, svc *metadata.Service, shareName string, root, dirHandle metadata.FileHandle) error {
	for {
		page, err := svc.ReadDirectory(ctx, dirHandle, 0, 0)
		if err != nil {
			return err
		}
		if len(page.Entries) == 0 {
			return nil
		}
		for i := range page.Entries {
			name := page.Entries[i].Name
			if err := s.purgeEntry(ctx, svc, shareName, root, dirHandle, name); err != nil {
				return err
			}
		}
		// Always restart from the beginning: removals invalidate cookies, and the
		// directory shrinks each pass until ReadDirectory returns empty.
	}
}

// resolveParent walks a bin-relative path (e.g. "a/b/c.txt") under binHandle and
// returns the handle of the leaf's parent directory plus the leaf name.
func resolveParent(ctx *metadata.AuthContext, svc *metadata.Service, binHandle metadata.FileHandle, relPath string) (metadata.FileHandle, string, error) {
	parts := splitPath(relPath)
	if len(parts) == 0 {
		return nil, "", &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "empty bin path",
		}
	}
	parent := binHandle
	for _, dir := range parts[:len(parts)-1] {
		child, err := svc.GetChild(ctx.Context, parent, dir)
		if err != nil {
			return nil, "", err
		}
		parent = child
	}
	return parent, parts[len(parts)-1], nil
}

// ensureParent walks a share-relative destination path under root, creating any
// missing intermediary directories, and returns the leaf's parent handle plus
// the leaf name.
func ensureParent(ctx *metadata.AuthContext, svc *metadata.Service, root metadata.FileHandle, relPath string) (metadata.FileHandle, string, error) {
	parts := splitPath(relPath)
	if len(parts) == 0 {
		return nil, "", &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "empty restore destination",
		}
	}
	parent := root
	for _, dir := range parts[:len(parts)-1] {
		child, err := svc.GetChild(ctx.Context, parent, dir)
		if err != nil {
			if !metadata.IsNotFoundError(err) {
				return nil, "", err
			}
			created, _, cErr := svc.CreateDirectory(ctx, parent, dir, &metadata.FileAttr{
				Type: metadata.FileTypeDirectory,
				Mode: 0o755,
			})
			if cErr != nil {
				// A racing creator may have won between the GetChild miss and
				// our CreateDirectory; treat AlreadyExists as success and
				// re-resolve, mirroring metadata.ensureChildDir.
				var se *metadata.StoreError
				if !stderrors.As(cErr, &se) || se.Code != metadata.ErrAlreadyExists {
					return nil, "", cErr
				}
				child, cErr = svc.GetChild(ctx.Context, parent, dir)
				if cErr != nil {
					return nil, "", cErr
				}
				parent = child
				continue
			}
			child, cErr = metadata.EncodeFileHandle(created)
			if cErr != nil {
				return nil, "", cErr
			}
		}
		parent = child
	}
	return parent, parts[len(parts)-1], nil
}

// splitPath splits a slash-separated relative path into non-empty components.
func splitPath(rel string) []string {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return nil
	}
	return strings.Split(rel, "/")
}

// joinBin joins a bin path prefix with a child name.
func joinBin(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}
