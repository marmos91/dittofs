package journal

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// This file holds the LocalStore admin surface the composition layer holds
// the local tier through: the disk cap probe, the durability report, and the
// health check. The data plane lives in store.go/index.go; the carve seam in
// carve.go; eviction in reclaim.go.

// MaxLocalBytes reports the effective local on-disk cap eviction gates against
// (an explicit config value, or the free-space default Open derived). 0 means
// genuinely uncapped (the free-space probe itself failed at open).
func (s *Store) MaxLocalBytes() int64 { return s.cfg.MaxLocalBytes }

// Start is a no-op: Open launches the background loops itself, so there is
// nothing a caller must start. Retained on the interface for the lifecycle
// call sites.
func (s *Store) Start(context.Context) {}

// Durable reports crash survival: the journal fsyncs every record into segment
// files on the local device, so the substrate survives a process loss. Sees the
// SetDurable override (config["durable"]).
func (s *Store) Durable() bool { return s.durable.Load() }

// SetDurable overrides the durability report (config["durable"]). The default
// is true; an operator may flip it for a store on volatile media.
func (s *Store) SetDurable(v bool) { s.durable.Store(v) }

var _ interface {
	Durable() bool
	SetDurable(bool)
} = (*Store)(nil)

// Healthcheck reports the journal dir's status. The failure modes an open
// journal can have are: closed (no longer accepting reads or writes), or the
// caller's context canceled (StatusUnknown — the probe was indeterminate, not
// the store). Cheap: one mutex-guarded flag read, no IO.
func (s *Store) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()

	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}

	if s.closed.Load() {
		return health.NewUnhealthyReport("journal block store is closed", time.Since(start))
	}

	return health.NewHealthyReport(time.Since(start))
}

var _ interface {
	Start(context.Context)
	MaxLocalBytes() int64
	Healthcheck(context.Context) health.Report
} = (*Store)(nil)
