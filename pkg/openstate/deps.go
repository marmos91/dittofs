package openstate

import (
	"io"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// Deps is what the table is built over.
type Deps struct {
	// Records is the store whose open-state views hold the durable records:
	// the Clients view and NextID. It is the concrete store, since its
	// open-state views have no interface of their own.
	Records *kvstore.Store
	Clock   Clock
	Rand    io.Reader // every random bit of a token or confirm; crypto/rand in production
}

// Clock is the part of the node's clock open state reads. Every lease and
// deadline is an Elapsed reading, never a wall time, so a wall clock's step
// neither expires a client nor keeps one.
type Clock interface {
	Elapsed() time.Duration                // monotonic, from an origin the clock chose
	After(d time.Duration) <-chan struct{} // fires after d on the monotonic clock
}
