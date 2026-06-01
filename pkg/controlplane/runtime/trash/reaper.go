package trash

import (
	"context"
	"sort"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Start launches the background reaper goroutine. It enforces each enabled
// share's retention and max-size policy on the configured interval until ctx is
// cancelled or Stop is called.
func (s *Service) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.reapAll(ctx)
			}
		}
	}()
}

// Stop signals the reaper goroutine to exit. It is idempotent: a second call is
// a no-op rather than a panic on a closed channel.
func (s *Service) Stop() {
	select {
	case <-s.stopCh:
		// Already stopped.
	default:
		close(s.stopCh)
	}
}

// reapAll runs one reap pass over every trash-enabled share, applying retention
// (RetentionDays) then max-size (MaxBytes) eviction. Per-share errors are
// swallowed so one bad share cannot stall the others; the next tick retries.
func (s *Service) reapAll(ctx context.Context) {
	now := time.Now().UTC()
	for _, share := range s.deps.EnabledTrashShares() {
		cfg, ok := s.deps.TrashConfigForShare(share)
		if !ok || !cfg.Enabled {
			continue
		}
		actx := metadata.NewSystemAuthContext(ctx)
		if cfg.RetentionDays > 0 {
			_, _ = s.reapShareAt(actx, share, cfg, now)
		}
		if cfg.MaxBytes > 0 {
			_, _ = s.evictToCap(actx, share, cfg.MaxBytes)
		}
	}
}

// reapShareAt permanently removes every bin entry whose DeletedAt predates the
// retention cutoff (now - RetentionDays), freeing each removed file's CAS
// blocks. now is explicit so tests can inject a fixed clock. Returns the number
// of top-level entries removed.
func (s *Service) reapShareAt(ctx *metadata.AuthContext, share string, cfg Config, now time.Time) (int, error) {
	svc, root, err := s.resolve(share)
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

	entries, err := s.List(ctx, share)
	if err != nil {
		return 0, err
	}

	cutoff := now.Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)
	removed := 0
	for _, e := range entries {
		if !e.DeletedAt.Before(cutoff) {
			continue
		}
		parent, name, err := resolveParent(ctx, svc, binHandle, e.BinPath)
		if err != nil {
			return removed, err
		}
		if err := s.purgeEntry(ctx, svc, share, root, parent, name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// evictToCap enforces the per-share max-size policy: while the bin's total size
// exceeds capBytes, it permanently removes the oldest entry (smallest
// DeletedAt), freeing its CAS blocks. Returns the number of entries removed.
func (s *Service) evictToCap(ctx *metadata.AuthContext, share string, capBytes int64) (int, error) {
	svc, root, err := s.resolve(share)
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

	entries, err := s.List(ctx, share)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, e := range entries {
		total += int64(e.Size)
	}
	if total <= capBytes {
		return 0, nil
	}

	// Oldest first: evict the entries deleted longest ago until under cap.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].DeletedAt.Before(entries[j].DeletedAt)
	})

	removed := 0
	for _, e := range entries {
		if total <= capBytes {
			break
		}
		parent, name, err := resolveParent(ctx, svc, binHandle, e.BinPath)
		if err != nil {
			return removed, err
		}
		if err := s.purgeEntry(ctx, svc, share, root, parent, name); err != nil {
			return removed, err
		}
		total -= int64(e.Size)
		removed++
	}
	return removed, nil
}
