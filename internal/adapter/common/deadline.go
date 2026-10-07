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

// withRequestDeadline returns ctx unchanged when it already carries a deadline,
// and otherwise a context that ends requestDeadline from now. The caller must
// call the returned cancel.
func withRequestDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, requestDeadline)
}
