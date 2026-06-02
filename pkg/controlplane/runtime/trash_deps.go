package runtime

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/trash"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// trashReapInterval is the cadence of the recycle-bin reaper (retention +
// max-size enforcement). One hour is frequent enough for day-granularity
// retention and cheap: a pass over a trash-disabled fleet is a no-op.
const trashReapInterval = time.Hour

// Trash returns the runtime recycle-bin service (list / restore / empty +
// background reaper). Constructed lazily on first call so a Runtime that never
// uses trash carries no extra state; the reaper is launched/stopped by the
// lifecycle Serve/shutdown path.
func (r *Runtime) Trash() *trash.Service {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.trashSvc == nil {
		r.trashSvc = trash.New(&trashDeps{rt: r}, trashReapInterval)
	}
	return r.trashSvc
}

// trashDeps adapts the Runtime to the narrow trash.Deps surface. It resolves
// per-share metadata services / root handles, maps the shares-service trash
// policy snapshot into the trash package's Config, and frees CAS blocks for
// permanently-deleted files via the per-share block store.
type trashDeps struct {
	rt *Runtime
}

var _ trash.Deps = (*trashDeps)(nil)

// MetadataServiceForShare resolves the share's metadata service and root
// handle. The Runtime owns a single *metadata.Service keyed by share
// name (per-share stores are registered into it by AddShare), so the service
// pointer is the runtime's shared one and the root handle comes from the
// shares registry. ok=false when the share is unknown to the registry.
func (d *trashDeps) MetadataServiceForShare(shareName string) (*metadata.Service, metadata.FileHandle, bool) {
	root, err := d.rt.sharesSvc.GetRootHandle(shareName)
	if err != nil {
		return nil, nil, false
	}
	return d.rt.metadataService, root, true
}

// TrashConfigForShare maps the shares service's locked TrashSettings snapshot
// into the trash package's Config. ok passes through the registry lookup.
func (d *trashDeps) TrashConfigForShare(shareName string) (trash.Config, bool) {
	s, ok := d.rt.sharesSvc.TrashSettingsForShare(shareName)
	if !ok {
		return trash.Config{}, false
	}
	return trash.Config{
		Enabled:         s.Enabled,
		RetentionDays:   s.RetentionDays,
		RestrictToAdmin: s.RestrictToAdmin,
		MaxBytes:        s.MaxBytes,
		ExcludePatterns: s.ExcludePatterns,
	}, true
}

// EnabledTrashShares lists the shares with trash enabled.
func (d *trashDeps) EnabledTrashShares() []string {
	return d.rt.sharesSvc.EnabledTrashShares()
}

// FreeBlocks frees the CAS blocks backing a permanently-deleted file. payloadID
// and blocks are the values RemoveFile returned for the purged node; an empty
// payloadID (hard links still reference the content, or a directory) is a
// no-op. The per-share block store is resolved from the share root handle, and
// Delete is invoked WITH the file's BlockRef list so the engine decrements each
// block's CAS RefCount and lets the GC reclaim chunks no other file references.
// Passing nil here would skip the per-block decrement and leak the refcounts
// (#832); unlike the NFS v3 / SMB close path (which deletes a still-live file
// whose blocks it does not have on hand), the reaper holds the removed file's
// blocks and must thread them through.
func (d *trashDeps) FreeBlocks(ctx context.Context, shareName string, root metadata.FileHandle, payloadID string, blocks []block.BlockRef) error {
	if payloadID == "" {
		return nil
	}
	bs, err := d.rt.GetBlockStoreForHandle(ctx, root)
	if err != nil {
		return err
	}
	return bs.Delete(ctx, payloadID, blocks)
}

// trashPolicy adapts the shares service into a metadata.TrashPolicy. A single
// instance serves every share: TrashConfigForShare is share-aware, so the same
// pointer can be installed on the shared MetadataService and route by name.
type trashPolicy struct {
	sharesSvc *shares.Service
}

var _ metadata.TrashPolicy = (*trashPolicy)(nil)

// TrashConfigForShare maps the shares service's TrashSettings snapshot into the
// metadata layer's TrashConfig (Enabled + ExcludePatterns drive the recycle
// decision on delete). ok=false when the share is unknown.
func (p *trashPolicy) TrashConfigForShare(shareName string) (metadata.TrashConfig, bool) {
	s, ok := p.sharesSvc.TrashSettingsForShare(shareName)
	if !ok {
		return metadata.TrashConfig{}, false
	}
	return metadata.TrashConfig{
		Enabled:         s.Enabled,
		ExcludePatterns: s.ExcludePatterns,
	}, true
}
