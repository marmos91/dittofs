package journalv2

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

// The journal's clock, settings and descriptor budget, declared in
// internal/core because the internal packages read them. Each one's contract
// is documented at its declaration.
type (
	Clock            = core.Clock
	Settings         = core.Settings
	DescriptorBudget = core.DescriptorBudget
)

// SyncBound is the longest a record written under the Bounded policy waits to
// be synced.
const SyncBound = core.SyncBound

// NewDescriptorBudget returns a budget of limit open descriptors, shared by
// every journal opened with it. A limit of zero means a quarter of the
// process's open-file limit.
func NewDescriptorBudget(limit int) *DescriptorBudget {
	return core.NewDescriptorBudget(limit)
}

// Option configures Init, Open or Restamp. A zero Option is ignored.
//
// decision: options are appended to the calls' signatures as a variadic
// parameter, so a call written with only the parameters the journal's
// contract names still compiles. Every option but the clock has a default;
// Open refuses to run without a clock. The function an option applies is
// unexported, so only code inside the journal's tree can write an option.
type Option struct {
	apply func(*core.Config)
}

// WithClock makes the journal read time from c. Open requires it: a journal
// never falls back to the system clock on its own, so a test that forgets its
// clock fails at once instead of running on real time.
func WithClock(c Clock) Option {
	return Option{func(cfg *core.Config) { cfg.Clock = c }}
}

// WithStorage makes the journal reach its directory and files through fs
// instead of the operating system's. Its parameter type is internal, so only
// code inside the journal's tree, such as the crash-simulating seam its tests
// use, can build a value to pass. Without it the journal uses the operating
// system's files. The journal does no file I/O yet, so it does not read fs
// today.
func WithStorage(fs storage.FS) Option {
	return Option{func(cfg *core.Config) { cfg.Storage = fs }}
}

// WithSettings sets the journal's settings. Without it every setting takes its
// default.
func WithSettings(s Settings) Option {
	return Option{func(cfg *core.Config) { cfg.Settings = s }}
}

// WithDescriptorBudget makes the journal hold its segment descriptors under b,
// a budget shared with the other journals of the process. Without it the
// journal gets a budget of its own, at the default limit.
func WithDescriptorBudget(b *DescriptorBudget) Option {
	return Option{func(cfg *core.Config) { cfg.Budget = b }}
}
