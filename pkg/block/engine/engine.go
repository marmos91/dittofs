// Package engine is the engine: it orders every content operation of the
// shares this node serves across the journals, one per device, and block
// metadata. The filesystem service is its only caller of the content methods;
// the node calls the lifecycle and share methods.
//
// Every byte is held in the journal: nothing is offloaded.
package engine

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// errUnbuilt answers every call whose behaviour is not built yet.
//
// decision: an unbuilt call answers vfserr.ErrUnavailable, wrapped so a test
// tells it from an outage, rather than an error of the engine's own: the
// engine returns only vfserr's values, and a caller maps this one already.
// It holds while nothing composes the engine; every call is built, and this
// removed, before the filesystem service runs over it.
var errUnbuilt = fmt.Errorf("engine: not built: %w", vfserr.ErrUnavailable)

// Engine is the engine of one node.
type Engine struct {
	d        Deps
	settings atomic.Pointer[Settings]
	shares   atomic.Pointer[bindings] // the share table; swapped whole, read without a lock
	files    *files                   // per-file state
	sample   func() vfserr.Verifier   // samples a write's verifier, before it is staged
}

// New checks d and builds the engine. It takes no settings, opens nothing and
// starts no goroutine. A missing member of d is refused with an error naming
// it.
func New(d Deps) (*Engine, error) {
	for _, m := range []struct {
		name    string
		missing bool
	}{
		{"Node", d.Node == (entity.NodeID{})},
		{"Devices", len(d.Devices) == 0},
		{"Open", d.Open == nil},
		{"Existence", d.Existence == nil},
		{"Content", d.Content == nil},
		{"Lifecycle", d.Lifecycle == nil},
		{"Holds", d.Holds == nil},
		{"Clock", d.Clock == nil},
		{"Logger", d.Logger == nil},
		{"Tracer", d.Tracer == nil},
	} {
		if m.missing {
			return nil, fmt.Errorf("engine: Deps.%s is missing", m.name)
		}
	}
	d.Logger = d.Logger.With("component", "engine")
	return &Engine{d: d, files: newFiles(d.Clock), sample: zeroVerifier}, nil
}

// Apply hands the engine its node-level settings: first before Start, then on
// every change. An invalid one is refused and the engine keeps its last.
func (e *Engine) Apply(s Settings) error {
	switch {
	case s.ExistenceAge <= 0:
		return fmt.Errorf("engine: existence_age %v is not positive", s.ExistenceAge)
	case s.GroupCommitMaxFiles <= 0:
		return fmt.Errorf("engine: group_commit.max_files %d is not positive", s.GroupCommitMaxFiles)
	case s.CloneMaxLen <= 0:
		return fmt.Errorf("engine: clone_max_len %d is not positive", s.CloneMaxLen)
	}
	e.settings.Store(&s)
	return nil
}

// Start opens each device's journal and attaches shares, all or nothing.
func (e *Engine) Start(ctx context.Context, shares []Share) error { return errUnbuilt }

// Close stops the engine and closes what it opened, in reverse.
func (e *Engine) Close(ctx context.Context) error { return errUnbuilt }

// Bind attaches a new share above the floor, or applies a change to one
// already bound.
func (e *Engine) Bind(ctx context.Context, s Share) error { return errUnbuilt }

// Unbind admits nothing new for the share and joins what it has in flight.
func (e *Engine) Unbind(ctx context.Context, id entity.ShareID, keepHolds bool) error {
	return errUnbuilt
}

// Drop closes the share's handles and forgets its tag in every journal.
func (e *Engine) Drop(ctx context.Context, id entity.ShareID) error { return errUnbuilt }

// Stats reports the engine's counters and conditions.
func (e *Engine) Stats() Stats { return Stats{} }
