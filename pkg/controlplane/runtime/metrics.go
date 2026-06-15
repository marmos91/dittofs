package runtime

import (
	"context"
	"strconv"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// MetricsSnapshot implements metrics.Provider: a read-through view of runtime
// state assembled at scrape time. Nothing here is cached — every call reflects
// live values pulled from the existing sub-services.
func (r *Runtime) MetricsSnapshot(ctx context.Context) metrics.Snapshot {
	var snap metrics.Snapshot

	for _, ps := range r.sharesSvc.MetricsBlockStats() {
		st := ps.Stats
		logical, _ := r.GetShareUsage(ps.ShareName)
		held, lastUnix := r.snapshotState(ctx, ps.ShareName)
		snap.Shares = append(snap.Shares, metrics.ShareSnapshot{
			Name:                ps.ShareName,
			DiskUsedBytes:       st.LocalDiskUsed,
			DiskMaxBytes:        st.LocalDiskMax,
			MemUsedBytes:        st.LocalMemUsed,
			MemMaxBytes:         st.LocalMemMax,
			UnsyncedBytes:       st.UnsyncedBytes,
			PendingUploads:      int64(st.PendingUploads),
			CompletedSyncs:      int64(st.CompletedSyncs),
			FailedSyncs:         int64(st.FailedSyncs),
			RemoteHealthy:       st.RemoteHealthy,
			HasRemote:           st.HasRemote,
			OutageSeconds:       st.OutageDurationSecs,
			OfflineReadsBlocked: st.OfflineReadsBlocked,
			LogicalBytes:        logical,
			FileCount:           int64(st.FileCount),
			SnapshotsHeld:       held,
			LastSnapshotUnix:    lastUnix,
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
