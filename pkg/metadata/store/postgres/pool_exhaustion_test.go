//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// poolAcquireTimeout mirrors the unexported poolConnectionAcquireTimeout the
// store applies to every checkout. If that constant changes, this test starts
// failing on its deadline rather than silently passing for the wrong reason.
const poolAcquireTimeout = 10 * time.Second

// TestPostgresLockPath_ExhaustedPoolFailsRatherThanBlocking pins what the lock
// path does when the pool has no connection to give.
//
// The lock store used to hold the pgxpool directly, where a checkout is bounded
// only by the caller's context — and the NFS handler context has no deadline,
// so a saturated pool parked NLM waiters indefinitely. It now goes through the
// same helper as every other query in the package, which bounds the checkout.
//
// That is a behaviour change, and this is the case that decides whether it is
// the right one: under saturation sustained past the timeout, an NLM lock
// acquisition now FAILS with a message naming pool exhaustion instead of
// waiting for a pool that may never drain. Failing is the intended answer, but
// nothing exercised the state, so nothing said so.
//
// Note how this fails if the bound is ever removed: the call never returns and
// the deadline below reports it. That is deliberate — the alternative to
// failing fast is not a wrong error, it is silence.
func TestPostgresLockPath_ExhaustedPoolFailsRatherThanBlocking(t *testing.T) {
	cfg, caps := postgresTestConfig()
	// One connection, so a single open transaction is total saturation. MinConns
	// has to come down with it or the config validator rejects the pair.
	cfg.MaxConns = 1
	cfg.MinConns = 1

	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore() failed: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Hold the only connection for longer than the acquire timeout by parking a
	// transaction on it. release closes the hold; held reports that it is up.
	release := make(chan struct{})
	held := make(chan struct{})
	txDone := make(chan error, 1)
	go func() {
		txDone <- store.WithTransaction(context.Background(), func(metadata.Transaction) error {
			close(held)
			<-release
			return nil
		})
	}()

	select {
	case <-held:
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatal("could not open the transaction that saturates the pool")
	}

	// With the single connection held, this checkout cannot succeed. A bounded
	// one gives up; an unbounded one waits for the release that never comes
	// before the deadline.
	type result struct {
		err     error
		elapsed time.Duration
	}
	got := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := store.GetLock(context.Background(), "no-such-lock")
		got <- result{err: err, elapsed: time.Since(start)}
	}()

	select {
	case r := <-got:
		close(release)
		if r.err == nil {
			t.Fatalf("GetLock succeeded against a fully saturated pool after %v", r.elapsed)
		}
		if !strings.Contains(r.err.Error(), "pool may be exhausted") {
			t.Fatalf("GetLock failed after %v, but the error does not name the cause: %v", r.elapsed, r.err)
		}
		// The bound is what produced this, not some faster unrelated failure:
		// a checkout that gave up well before the timeout would mean something
		// else refused the query and this test would not be pinning the guard.
		if r.elapsed < poolAcquireTimeout/2 {
			t.Fatalf("GetLock failed after only %v, far short of the %v acquire timeout — "+
				"something other than the pool bound refused it, so this test is not "+
				"exercising what it claims", r.elapsed, poolAcquireTimeout)
		}
		t.Logf("lock read gave up after %v with: %v", r.elapsed, r.err)
	case <-time.After(poolAcquireTimeout + 20*time.Second):
		close(release)
		t.Fatalf("GetLock did not return within %v of a saturated pool: the acquire bound is "+
			"gone and an NLM lock request would wait on the pool indefinitely",
			poolAcquireTimeout+20*time.Second)
	}

	if err := <-txDone; err != nil {
		t.Fatalf("the saturating transaction failed: %v", err)
	}
}
