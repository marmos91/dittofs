package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// DurableHandleScavenger periodically checks for expired durable handles
// and performs full cleanup (release locks, flush caches, execute delete-on-close).
//
// Lifecycle: Created by the SMB adapter during Serve(), runs in a background
// goroutine, and stops when the serve context is cancelled.
//
// On first run it expires handles whose timeout elapsed during server downtime.
type DurableHandleScavenger struct {
	store     lock.DurableHandleStore
	handler   *Handler      // for cleanup operations (may be nil in tests)
	interval  time.Duration // scavenger tick interval
	timeoutMs uint32        // default timeout for handles
	startTime time.Time     // server start time for restart detection
}

// NewDurableHandleScavenger creates a new scavenger instance.
// handler may be nil for testing (cleanup steps are skipped).
func NewDurableHandleScavenger(
	store lock.DurableHandleStore,
	handler *Handler,
	interval time.Duration,
	timeoutMs uint32,
	startTime time.Time,
) *DurableHandleScavenger {
	return &DurableHandleScavenger{
		store:     store,
		handler:   handler,
		interval:  interval,
		timeoutMs: timeoutMs,
		startTime: startTime,
	}
}

// Run starts the scavenger loop. It blocks until ctx is cancelled.
//
// On first run, it expires handles whose timeout elapsed during server downtime.
// Then it enters a ticker loop, calling expireHandles on each tick.
func (s *DurableHandleScavenger) Run(ctx context.Context) {
	// Expire handles from a previous server instance whose timeout elapsed during downtime
	s.expireHandlesFromPreviousInstance(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireHandles(ctx)
		}
	}
}

// expireHandlesFromPreviousInstance expires persisted handles from a previous
// server instance whose timeout elapsed during downtime. No timeout fields are mutated.
func (s *DurableHandleScavenger) expireHandlesFromPreviousInstance(ctx context.Context) {
	handles, err := s.store.ListDurableHandles(ctx)
	if err != nil {
		logger.Warn("DurableHandleScavenger: failed to list handles for restart adjustment",
			"error", err)
		return
	}

	now := time.Now()
	var expired, adjusted int

	for _, h := range handles {
		// Only process handles from a previous server instance
		if h.ServerStartTime.Equal(s.startTime) {
			continue
		}

		elapsed := now.Sub(h.DisconnectedAt)
		timeout := time.Duration(h.TimeoutMs) * time.Millisecond

		if elapsed >= timeout {
			// Handle has already expired during downtime
			s.cleanupAndDelete(ctx, h)
			expired++
		} else {
			// Handle is still valid -- leave it for normal expiry
			adjusted++
		}
	}

	if expired > 0 || adjusted > 0 {
		logger.Info("DurableHandleScavenger: restart adjustment complete",
			"expired", expired,
			"adjusted", adjusted)
	}
}

// expireHandles iterates all handles, checks expiry, performs cleanup, and deletes.
//
// We iterate and check client-side rather than using DeleteExpiredDurableHandles
// because we need to perform full cleanup (locks, caches, delete-on-close) BEFORE
// deletion.
func (s *DurableHandleScavenger) expireHandles(ctx context.Context) {
	handles, err := s.store.ListDurableHandles(ctx)
	if err != nil {
		logger.Warn("DurableHandleScavenger: failed to list handles", "error", err)
		return
	}

	now := time.Now()
	expired := 0

	for _, h := range handles {
		expiresAt := h.DisconnectedAt.Add(time.Duration(h.TimeoutMs) * time.Millisecond)
		if !expiresAt.After(now) {
			s.cleanupAndDelete(ctx, h)
			expired++
		}
	}

	if expired > 0 {
		logger.Debug("DurableHandleScavenger: expired handles", "count", expired)
	}
}

// cleanupAndDelete performs full cleanup for a durable handle (release locks,
// flush caches), then deletes it from the store.
func (s *DurableHandleScavenger) cleanupAndDelete(ctx context.Context, h *lock.PersistedDurableHandle) {
	if s.handler != nil && s.handler.Registry != nil {
		metaSvc := s.handler.Registry.GetMetadataService()

		// Release byte-range locks
		if metaSvc != nil && len(h.MetadataHandle) > 0 {
			// Use session ID 0 to indicate scavenger cleanup (not tied to a session)
			if err := metaSvc.UnlockAllForSession(ctx, h.MetadataHandle, 0); err != nil {
				logger.Debug("DurableHandleScavenger: failed to release locks",
					"id", h.ID, "path", h.Path, "error", err)
			}
		}

		// Flush block cache
		if h.PayloadID != "" {
			if blockStore := s.handler.Registry.GetBlockStore(); blockStore != nil {
				if _, err := blockStore.Flush(ctx, h.PayloadID); err != nil {
					logger.Debug("DurableHandleScavenger: failed to flush cache",
						"id", h.ID, "path", h.Path, "error", err)
				}
			}
		}

		// Delete-on-close would be handled here if the PersistedDurableHandle
		// tracked a DeletePending flag. Currently this is handled at the OpenFile
		// level during normal close. For scavenger expiry, the file persists.
	}

	if err := s.store.DeleteDurableHandle(ctx, h.ID); err != nil {
		logger.Warn("DurableHandleScavenger: failed to delete expired handle",
			"id", h.ID, "error", err)
	} else {
		logger.Debug("DurableHandleScavenger: expired handle cleaned up",
			"id", h.ID, "path", h.Path, "share", h.ShareName)
	}
}

// ForceExpireDurableHandle immediately expires and cleans up a single handle.
// Used by the REST API force-close endpoint and conflict resolution.
//
// Returns an error if the handle does not exist.
func (s *DurableHandleScavenger) ForceExpireDurableHandle(ctx context.Context, handleID string) error {
	h, err := s.store.GetDurableHandle(ctx, handleID)
	if err != nil {
		return fmt.Errorf("failed to get durable handle %s: %w", handleID, err)
	}
	if h == nil {
		return fmt.Errorf("durable handle %s not found", handleID)
	}

	s.cleanupAndDelete(ctx, h)
	return nil
}

// HandleConflictingOpen force-expires all orphaned durable handles for a given
// file handle. This is called when a new open arrives for a file that has
// orphaned durable handles.
//
// Returns the number of handles that were force-expired.
func (s *DurableHandleScavenger) HandleConflictingOpen(ctx context.Context, fileHandle []byte) int {
	handles, err := s.store.GetDurableHandlesByFileHandle(ctx, fileHandle)
	if err != nil {
		logger.Warn("DurableHandleScavenger: failed to look up handles for conflicting open",
			"error", err)
		return 0
	}

	for _, h := range handles {
		s.cleanupAndDelete(ctx, h)
		logger.Debug("DurableHandleScavenger: force-expired handle for conflicting open",
			"id", h.ID, "path", h.Path)
	}

	return len(handles)
}
