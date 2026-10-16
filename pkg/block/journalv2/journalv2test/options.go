package journalv2test

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// WithSegmentCap makes the journal allocate segments of n bytes instead of
// the fixed segment size, so a test can build a journal of many segments in a
// small directory. Zero means the fixed size. It exists for tests only.
func WithSegmentCap(n int64) journalv2.Option {
	return func(*core.Config) {}
}
