package metadata

import "context"

// txContextKey is the unexported type used as the context key for an
// active metadata.Transaction. Using a private type prevents key
// collisions across packages.
type txContextKey struct{}

// WithTx returns a context that carries the supplied Transaction. The
// transaction is retrievable via TxFromContext from anywhere in the
// call chain.
//
// Phase 12 CR-01 (review iteration 1): wired by common.CopyPayload
// (and any future caller that needs to bind engine-level RefCount
// mutations to its WithTransaction-owned tx). The per-share
// metadataCoordinator (pkg/controlplane/runtime/shares/coordinator.go)
// reads this on every IncrementRefCount / DecrementRefCount call so the
// underlying SQL/Badger UPDATE shares the caller's txn — without it
// every increment commits on a fresh pool connection and survives the
// caller's rollback (silent INV-02 leak).
//
// Memory and Badger backends are tolerant of the context-carried tx:
// memory's WithTransaction holds a single mutex (so the public-store
// methods would deadlock without this routing), badger uses
// db.Update which takes the same approach. Postgres is the only
// backend whose pool semantics make the difference observable in
// production data.
//
// The Transaction's lifetime is bounded by the surrounding
// WithTransaction call. Callers MUST NOT propagate the returned
// context past the WithTransaction body — using a Transaction after
// its WithTransaction has returned has undefined behavior (per the
// Transactor contract).
func WithTx(ctx context.Context, tx Transaction) context.Context {
	if tx == nil {
		return ctx
	}
	return context.WithValue(ctx, txContextKey{}, tx)
}

// TxFromContext returns the Transaction bound to ctx via WithTx, or
// nil if none is present. Backend implementations and engine-coordinator
// shims call this to route per-call SQL/KV operations through the
// active transaction instead of opening a fresh connection.
//
// Returning nil is the documented "no active txn" signal — callers
// should fall back to the public MetadataStore surface (which routes
// through the connection pool / global mutex / db.Update).
func TxFromContext(ctx context.Context) Transaction {
	if ctx == nil {
		return nil
	}
	v := ctx.Value(txContextKey{})
	if v == nil {
		return nil
	}
	tx, _ := v.(Transaction)
	return tx
}
