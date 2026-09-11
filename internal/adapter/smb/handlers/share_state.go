package handlers

import (
	"context"
	"encoding/binary"
	"strings"

	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Share state: cached shares, share security descriptors, durable-handle
// seeding, and the share-change callback.
func (h *Handler) notifyOpenFileModified(openFile *OpenFile, filter uint32) {
	name := openFile.Name()
	h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(name.Path), notifyStreamName(name.FileName), FileActionModified, filter)
}

// baseFileUUID returns the base file's UUID for an ADS path, or fallback for non-ADS.
// ADS streams share the base file's FileId so that all stream handles compare equal.

func (h *Handler) baseFileUUID(authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string, fallback [16]byte) [16]byte {
	if colonIdx := strings.Index(name, ":"); colonIdx > 0 && len(parentHandle) > 0 {
		metaSvc := h.Registry.GetMetadataService()
		if baseFile, _, err := metaSvc.LookupCaseInsensitive(authCtx, parentHandle, name[:colonIdx]); err == nil && baseFile != nil {
			return baseFile.ID
		}
	}
	return fallback
}

// SeedFromDurableHandles restores the in-memory state derived from persisted
// durable handles after a restart:
//
//   - the persistent-half FileID counter is bumped past the highest value
//     already recorded, so freshly minted FileIDs cannot collide with the
//     FileID of a still-reclaimable durable open (detailed below); and
//   - handles persisted in the disconnected state are counted, so the conflict
//     scans still see the handles a previous process left behind instead of
//     taking their fast path over them.
//
// Without this, the counter restarts at 1 every process start (see NewHandler;
// the first issued persistent half is 2, since GenerateFileID pre-increments),
// while durable-handle records in the metadata store retain the persistent
// halves issued before the restart. A post-restart CREATE would re-mint a
// persistent half equal to a persisted handle's, and a V1 reconnect (DHnC) —
// which matches on the persistent half alone, with no CreateGuid to
// disambiguate — could then reconnect or consume the wrong file
// (MS-SMB2 §3.3.5.9.7).
//
// Called once during adapter startup after the DurableStore is wired in.
// The persistent half is the little-endian uint64 in bytes 0..7 of FileID,
// matching the encoding GenerateFileID writes; the volatile half (bytes 8..15)
// is zeroed in persisted records and ignored here.

func (h *Handler) SeedFromDurableHandles(ctx context.Context, store lock.DurableHandleStore) {
	if store == nil {
		return
	}
	handles, err := store.ListDurableHandles(ctx)
	if err != nil {
		logger.Warn("SMB: could not seed FileID counter from durable handles; "+
			"using default start (FileID collision possible across restart)",
			"error", err)
		return
	}
	// Same lock the disconnect path holds around its count-then-persist, so a
	// concurrent scan's reconciliation cannot interleave with these counts.
	h.durablePurgeMu.Lock()
	var maxID uint64
	for _, dh := range handles {
		if !dh.DisconnectedAt.IsZero() {
			h.noteDisconnectedHandle(dh.MetadataHandle)
		}
		id := binary.LittleEndian.Uint64(dh.FileID[:8])
		if id > maxID {
			maxID = id
		}
	}
	h.durablePurgeMu.Unlock()
	if maxID == 0 {
		return
	}
	// Guard the overflow edge: if a persisted FileID already holds the maximum
	// uint64, seeding to it would make the next GenerateFileID's Add(1) wrap to
	// 0 (and the log line below wrap too), reintroducing collisions. Such a
	// value can't be exceeded anyway, so skip seeding and surface it.
	if maxID == ^uint64(0) {
		logger.Warn("SMB: persisted durable handle holds max FileID; cannot seed counter safely",
			"durable_handles", len(handles))
		return
	}
	// The counter holds the *last issued* persistent half — GenerateFileID
	// pre-increments via Add(1) — so storing maxID makes the next issued
	// FileID maxID+1, strictly above every persisted handle. Bump only
	// upward: a concurrent CREATE may already have advanced the counter at or
	// past maxID, so never lower it.
	for {
		cur := h.nextFileID.Load()
		if cur >= maxID {
			return
		}
		if h.nextFileID.CompareAndSwap(cur, maxID) {
			logger.Info("SMB: seeded FileID counter past persisted durable handles",
				"next_file_id", maxID+1, "durable_handles", len(handles))
			return
		}
	}
}

// GenerateFileID generates a new unique file ID

func (h *Handler) getCachedShares() []rpc.ShareInfo1 {
	h.sharesCacheMu.RLock()
	if h.sharesCacheValid {
		shares := h.cachedShares
		h.sharesCacheMu.RUnlock()
		return shares
	}
	h.sharesCacheMu.RUnlock()

	// Rebuild cache under write lock
	h.sharesCacheMu.Lock()
	defer h.sharesCacheMu.Unlock()

	// Double-check after acquiring write lock (another goroutine may have rebuilt)
	if h.sharesCacheValid {
		return h.cachedShares
	}

	if h.Registry == nil {
		return nil
	}

	shareNames := h.Registry.ListShares()
	shares := make([]rpc.ShareInfo1, 0, len(shareNames))
	for _, name := range shareNames {
		if strings.EqualFold(name, "/ipc$") {
			continue
		}
		displayName := strings.TrimPrefix(name, "/")
		shares = append(shares, rpc.ShareInfo1{
			Name:               displayName,
			Type:               rpc.STYPE_DISKTREE,
			Comment:            "DittoFS share",
			SecurityDescriptor: h.shareSecurityDescriptor(name),
		})
	}

	h.cachedShares = shares
	h.sharesCacheValid = true

	return shares
}

// shareSecurityDescriptor builds the self-relative security descriptor served
// for a share at srvsvc info level 502, so Windows Explorer's Advanced Sharing
// "Permissions" tab is populated. It mirrors the file Security tab: the share's
// control-plane grants are projected via ShareRootGrantACL and merged into a
// synthesized owner+SYSTEM descriptor, surfacing the AD/SID principals that
// govern the share. Best-effort — a lookup or build failure yields a nil
// descriptor (a level-502 reply with a null SD) rather than failing the pipe.

func (h *Handler) shareSecurityDescriptor(shareName string) []byte {
	if h.Registry == nil {
		return nil
	}

	var grantACEs []acl.ACE
	if grantACL, err := h.Registry.ShareRootGrantACL(context.Background(), shareName); err != nil {
		logger.Debug("share SD: grant ACL lookup failed (non-fatal)", "share", shareName, "error", err)
	} else if grantACL != nil {
		grantACEs = grantACL.ACEs
	}

	// The share root is owned by uid/gid 0 and carries no explicitly-stored
	// ACL, so BuildSecurityDescriptorWithGrants synthesizes the owner+SYSTEM
	// default and merges in the direct AD/SID grants (see buildDACL).
	root := &metadata.File{}
	sd, err := BuildSecurityDescriptorWithGrants(
		root,
		OwnerSecurityInformation|GroupSecurityInformation|DACLSecurityInformation,
		grantACEs,
	)
	if err != nil {
		logger.Debug("share SD: build failed (non-fatal)", "share", shareName, "error", err)
		return nil
	}
	return sd
}

// invalidateShareCache marks the share list cache as stale.
// Called by the Runtime share change callback.

func (h *Handler) invalidateShareCache() {
	h.sharesCacheMu.Lock()
	h.sharesCacheValid = false
	h.sharesCacheMu.Unlock()
}

// RegisterShareChangeCallback subscribes to share change events from the Runtime
// to invalidate the cached share list used by pipe CREATE operations.

func (h *Handler) RegisterShareChangeCallback() {
	if h.Registry == nil {
		return
	}
	h.Registry.OnShareChange(func(_ []string) {
		h.invalidateShareCache()
	})
}
