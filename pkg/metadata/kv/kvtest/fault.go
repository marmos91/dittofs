package kvtest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The operation names a FaultKV hands its fault, as a sim.Rule's Op matches
// them.
const (
	OpUpdate     = "update"
	OpView       = "view"
	OpGet        = "get"
	OpScan       = "scan"
	OpBefore     = "before"
	OpVersions   = "versions"
	OpSet        = "set"
	OpDelete     = "delete"
	OpGuard      = "guard"
	OpGuardRange = "guard_range"
)

// FaultKV is a kv.KV over another that consults a sim.Fault before each
// operation, so a test can stall the store, fail its calls, or fail one
// file's existence commit, from a seeded schedule read from a test clock.
//
// Composed with kv.Counting, the order decides what is counted.
// NewFaultKV(&kv.Counting{KV: inner}) counts only what reached the store: a
// read or a write the wrapper refused is not counted. &kv.Counting{KV:
// faultKV} also counts the reads and writes the wrapper refused inside a
// transaction, since Counting counts each operation before calling it. A Scan
// is the exception: Counting counts only the records it yields without an
// error, so a refused scan is counted in neither order. An Update or a View
// the wrapper refused never runs fn, so it adds no key in either order. An
// outer Counting still counts a refused Update in TakeUpdates, and an inner
// one does not.
type FaultKV struct {
	inner kv.KV

	mu    sync.Mutex
	fault sim.Fault // nil passes every call
}

var (
	_ kv.KV     = (*FaultKV)(nil)
	_ kv.Txn    = (*faultTxn)(nil)
	_ kv.Reader = (*faultReader)(nil)
)

// NewFaultKV returns a FaultKV over inner that passes every call until
// SetFault gives it a fault.
func NewFaultKV(inner kv.KV) *FaultKV {
	return &FaultKV{
		inner: inner,
	}
}

// SetFault makes the wrapper consult f before every operation, and apply the
// results it returns. A nil f, the default, passes every call. It may be
// called while other calls run.
//
// Each operation is named by one of the Op constants, and its key is the key
// it acts on, rendered by keys.Describe: the key for get, versions, set,
// delete and guard; the prefix for scan and guard_range; the key argument,
// not the prefix, for before; and an empty key for update and view. GetMany
// consults get once per key and Versions consults versions once per key, so
// a rule on one key fires whether that key is read alone or in a batch. The
// reads inside a View consult under the same names as inside an Update.
// Update and view consult once per call, before the inner call; every other
// op consults inside the transaction, before the inner op, so it consults
// again on each attempt the backend retries. A scan consults when its
// iteration starts, not when Scan is called. Now, Limits and Close consult
// nothing.
//
// How the wrapper applies the results:
//   - Zero, or a delay alone: performs the operation. The delay is one f has
//     already waited out, so the wrapper applies it as passed and waits on
//     nothing itself. It does not watch ctx meanwhile. f is called without
//     the wrapper's lock held, so a delay stalls only its own call.
//   - err on update or view: returns err at once; fn never runs and nothing
//     is committed.
//   - err on any other op: that op returns err and the inner op is not
//     called. A kv.ErrConflict on an op inside an Update is returned as is,
//     so the backend's retry runs fn again.
//
// A tear or a loss on any op, and a kv.ErrConflict on update, on view or on
// a read inside a View, panics, naming the op: it is a mistake in the test,
// not a store fault.
//
// Describe shows the first 4 bytes of an ID, so two IDs render alike with
// probability 2^-32 per pair. A rule meant for one file's record uses the
// record key's exact Describe text, F‖share=…‖file=…, with no trailing '*':
// a trailing '*' also matches the file's sub-keys (fx, rm and the others),
// and an error on any of those fails the whole existence commit, not the one
// file. A get rule on a file's record also fires on that file's reads outside
// the existence commit, such as Existence.Applied, Existence.Overwrites and
// attribute reads, so a test that wants only the commit counts its calls
// with From and Count.
//
// A delay on an op inside a transaction holds that transaction open. A
// backend aborts a transaction older than its Limits().Age, measured in real
// time, as a conflict, and retries it, and the rule then counts the retried
// call again; so a long stall goes on update, outside the transaction.
func (f *FaultKV) SetFault(fault sim.Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.fault = fault
}

// consult asks the wrapper's fault what to do with op on key and returns the
// error to fail the op with, or nil to perform it. It panics on a result the
// wrapper cannot apply. bareConflict is whether the kv contract lets op
// return kv.ErrConflict to its caller: true only for an op inside an Update.
func (f *FaultKV) consult(op string, key []byte, bareConflict bool) error {
	f.mu.Lock()
	fault := f.fault
	f.mu.Unlock()

	if fault == nil {
		return nil
	}

	// The delay At returns it has already waited out.
	_, tearAfter, err := fault.At(op, []byte(keys.Describe(key)))

	// decision: a key-value store has no torn and no lost write: a commit
	// lands whole or not at all, and a lost commit reply is a setting of its
	// own, never a call's fault (RFC 16 §6.4). A seam refuses a tear or a loss
	// it cannot apply as a mistake in the check (RFC 8 §2.1), so a tear or a
	// loss on any op panics rather than being applied in some invented way.
	// Revisit if the kv contract ever gives a call a partial outcome.
	switch {
	case tearAfter > 0:
		panic(fmt.Sprintf("kvtest: a tear on %s is not supported: no store write is partial", op))
	case tearAfter < 0:
		panic(fmt.Sprintf("kvtest: a loss on %s is not supported: no store write is lost", op))
	}

	// decision: a conflict never reaches a caller bare: it is retried within
	// the caller's deadline. kv.KV's contract says ErrConflict "reaches a
	// caller only joined with ctx's error", and RFC 16, "What this RFC
	// promises", says a serialisation conflict is "never returned as an
	// error". Only an op inside an Update may return one, because Update's
	// retry is what turns it into a new attempt. Overturn this only for a
	// backend whose contract lets a conflict reach the caller of a View or of
	// an Update.
	if !bareConflict && errors.Is(err, kv.ErrConflict) {
		panic(fmt.Sprintf("kvtest: a conflict on %s is not supported: it is never returned", op))
	}

	return err
}

func (f *FaultKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if err := f.consult(OpUpdate, nil, false); err != nil {
		return err
	}

	return f.inner.Update(ctx, func(tx kv.Txn) error {
		return fn(&faultTxn{
			faultReader: faultReader{
				f:            f,
				r:            tx,
				bareConflict: true,
			},
			tx: tx,
		})
	})
}

func (f *FaultKV) View(ctx context.Context, fn func(kv.Reader) error) error {
	if err := f.consult(OpView, nil, false); err != nil {
		return err
	}

	return f.inner.View(ctx, func(r kv.Reader) error {
		return fn(&faultReader{
			f: f,
			r: r,
		})
	})
}

func (f *FaultKV) Limits() kv.TxnLimits {
	return f.inner.Limits()
}

func (f *FaultKV) Close() error {
	return f.inner.Close()
}

// faultReader consults the wrapper's fault before each read of r.
type faultReader struct {
	f            *FaultKV
	r            kv.Reader
	bareConflict bool // r belongs to an Update, whose retry handles a conflict
}

func (r *faultReader) consult(op string, key []byte) error {
	return r.f.consult(op, key, r.bareConflict)
}

func (r *faultReader) Get(key []byte) ([]byte, error) {
	if err := r.consult(OpGet, key); err != nil {
		return nil, err
	}

	return r.r.Get(key)
}

func (r *faultReader) GetMany(ks [][]byte) ([][]byte, error) {
	for _, k := range ks {
		if err := r.consult(OpGet, k); err != nil {
			return nil, err
		}
	}

	return r.r.GetMany(ks)
}

func (r *faultReader) Scan(prefix, after []byte) iter.Seq2[kv.KeyValue, error] {
	return func(yield func(kv.KeyValue, error) bool) {
		if err := r.consult(OpScan, prefix); err != nil {
			yield(kv.KeyValue{}, err)

			return
		}

		for kvp, err := range r.r.Scan(prefix, after) {
			if !yield(kvp, err) {
				return
			}
		}
	}
}

func (r *faultReader) Before(prefix, key []byte) (kv.KeyValue, bool, error) {
	if err := r.consult(OpBefore, key); err != nil {
		return kv.KeyValue{}, false, err
	}

	return r.r.Before(prefix, key)
}

func (r *faultReader) Versions(ks [][]byte) ([]uint64, error) {
	for _, k := range ks {
		if err := r.consult(OpVersions, k); err != nil {
			return nil, err
		}
	}

	return r.r.Versions(ks)
}

// faultTxn consults the wrapper's fault before each operation of tx. It
// embeds no kv.Txn, so a method kv.Txn gains fails to compile here until it
// is given its own consult.
type faultTxn struct {
	faultReader
	tx kv.Txn
}

func (t *faultTxn) Set(key, value []byte) error {
	if err := t.consult(OpSet, key); err != nil {
		return err
	}

	return t.tx.Set(key, value)
}

func (t *faultTxn) Delete(key []byte) error {
	if err := t.consult(OpDelete, key); err != nil {
		return err
	}

	return t.tx.Delete(key)
}

func (t *faultTxn) Guard(key []byte) error {
	if err := t.consult(OpGuard, key); err != nil {
		return err
	}

	return t.tx.Guard(key)
}

func (t *faultTxn) GuardRange(prefix []byte) error {
	if err := t.consult(OpGuardRange, prefix); err != nil {
		return err
	}

	return t.tx.GuardRange(prefix)
}

func (t *faultTxn) Now() time.Time {
	return t.tx.Now()
}
