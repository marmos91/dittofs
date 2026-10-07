package common

import (
	"context"
	"time"
)

// requestDeadline is the deadline a client operation gets at the payload
// choke points (ReadFromBlockStore, WriteToBlockStore, CommitBlockStore) when
// its request carries none. Every wait below it then ends within it: a cold
// read's fetch from an unanswering remote, which otherwise runs to the engine's
// own demand-fetch budget, and a write held at the local store's capacity,
// whose budget otherwise restarts whenever the remote drains a little. It is
// fixed rather than a setting; it is a variable only so tests can shorten it.
var requestDeadline = 30 * time.Second

// WithRequestDeadline returns ctx unchanged when it already carries a deadline,
// and otherwise a context that ends requestDeadline from now. The caller must
// call the returned cancel.
//
// decision: the default is installed where an operation reaches the block
// store, not when the request arrives at the protocol dispatcher, so the budget
// does not count the metadata work before it. Dispatch would be the earlier
// point, but SMB operations that go asynchronous — change notify, blocking
// byte-range locks, a request parked on a lease break — outlive their request
// under their own deadlines, and a dispatch-wide 30 s would cut them off. A
// handler that calls the choke points in a loop (QUERY_ALLOCATED_RANGES,
// SET_ZERO_DATA, READ_PLUS) takes one deadline up front with this function
// and passes it down, so the loop shares one budget instead of starting a new
// one per call. Revisit if a service layer with its own per-operation entry
// point appears between the adapters and the block store.
func WithRequestDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, requestDeadline)
}
