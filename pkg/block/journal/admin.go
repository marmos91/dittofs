package journal

import "context"

// This file holds the LocalStore admin surface the composition layer holds
// the local tier through: the disk cap probe, the durability report, and the
// liveness flag. The data plane lives in store.go/index.go; the flush seam in
// flush.go; eviction in reclaim.go.

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

// Closed reports whether the store has been closed and is no longer accepting
// reads or writes. It is the only failure mode an opened journal has that a
// caller can observe without doing IO, so it is the whole of journal's
// contribution to a host health report — the report itself is the caller's to
// shape, which is why this returns a bool and not a status type. Cheap: one
// atomic flag read, no IO.
func (s *Store) Closed() bool { return s.closed.Load() }
