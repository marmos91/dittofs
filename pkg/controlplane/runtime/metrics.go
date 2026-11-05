package runtime

import (
	"context"
	"strconv"

	"github.com/cubbit/dittofs/pkg/controlplane/models"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/cubbit/dittofs/pkg/metrics"
)

// MetricsSnapshot implements metrics.Provider: a read-through view of runtime
// state assembled at scrape time. Nothing here is cached — every call reflects
// live values pulled from the existing sub-services.
func (r *Runtime) MetricsSnapshot(ctx context.Context) metrics.Snapshot {
	var snap metrics.Snapshot

	for _, name := range r.sharesSvc.ListShares() {
		// Logical bytes come straight from the metadata store's per-share
		// counter. No content store is wired (block.ErrNoContentStore), so the
		// content-store fields stay zero.
		var logical int64
		if ms, err := r.metadataService.GetStoreForShare(name); err == nil {
			logical, _ = ms.GetUsedBytesForShare(ctx, name)
		}
		held, lastUnix := r.snapshotState(ctx, name)
		snap.Shares = append(snap.Shares, metrics.ShareSnapshot{
			Name:             name,
			LogicalBytes:     logical,
			SnapshotsHeld:    held,
			LastSnapshotUnix: lastUnix,
			Integrity:        integritySnapshot(r.ShareIntegrity(name)),
		})
	}

	if ms := r.metadataService; ms != nil {
		for _, cq := range ms.ListIdentityQuotas() {
			scope := models.QuotaScopeUser
			if cq.Quota.Scope == metadata.QuotaScopeGroup {
				scope = models.QuotaScopeGroup
			}
			id := cq.Quota.ID
			usedBytes, usedFiles := r.GetIdentityQuotaUsage(cq.Share, scope, &id)
			snap.Quotas = append(snap.Quotas, metrics.QuotaSnapshot{
				Scope:       scope,
				Principal:   quotaPrincipalLabel(id),
				Share:       cq.Share,
				UsedBytes:   usedBytes,
				LimitBytes:  cq.Quota.LimitBytes,
				UsedInodes:  usedFiles,
				LimitInodes: cq.Quota.LimitFiles,
			})
		}
	}

	if reg := r.Clients(); reg != nil {
		snap.Clients.NFS = int64(len(reg.ListByProtocol("nfs")))
		snap.Clients.SMB = int64(len(reg.ListByProtocol("smb")))
	}

	return snap
}

// snapshotState returns the count of ready (held) snapshots and the unix time
// of the most recent ready snapshot (0 = none) for a share.
func (r *Runtime) snapshotState(ctx context.Context, share string) (held int64, lastUnix int64) {
	snaps, err := r.store.ListSnapshots(ctx, share)
	if err != nil {
		return 0, 0
	}
	for _, s := range snaps {
		if s.State != models.StateReady {
			continue
		}
		held++
		if u := s.CreatedAt.Unix(); u > lastUnix {
			lastUnix = u
		}
	}
	return held, lastUnix
}

// quotaPrincipalLabel renders a quota principal id as a label value. The
// default-user sentinel is rendered as "default" rather than its numeric
// MaxUint32 form.
func quotaPrincipalLabel(id uint32) string {
	if id == metadata.DefaultUserID {
		return "default"
	}
	return strconv.FormatUint(uint64(id), 10)
}

// integritySnapshot renders a recorded manifest-scan outcome for the metrics
// surface. A share never scanned in this process, or whose last scan failed,
// reports zeros: a zero last-scan timestamp is what an alert on scan age is
// meant to fire on, and a failed scan produced no counts worth publishing.
// The failed case is flagged rather than left to look like the never-scanned
// one, so a scanner erroring every tick does not read as one that was never
// switched on.
func integritySnapshot(s *health.IntegrityStatus) metrics.IntegrityScanSnapshot {
	if s == nil {
		return metrics.IntegrityScanSnapshot{}
	}
	if s.Error != "" {
		return metrics.IntegrityScanSnapshot{LastScanFailed: true}
	}
	return metrics.IntegrityScanSnapshot{
		LastScanUnix:           s.LastRunAt.Unix(),
		DurationSeconds:        float64(s.DurationMS) / 1000,
		FilesScanned:           int64(s.FilesScanned),
		PayloadsWithFindings:   int64(s.PayloadsWithFindings),
		DamagedPayloads:        int64(s.DamagedPayloads),
		ClaimedUncoveredRanges: int64(s.ClaimedUncoveredRanges),
		UnplaceableRows:        int64(s.UnplaceableRows),
		UnknownHashRows:        int64(s.UnknownHashRows),
	}
}
