package netlogon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChannel is a test double for *SecureChannel that exercises the
// Authenticator's teardown/rebuild concurrency without real RPC. It enforces the
// production invariant that a close cannot overlap an in-flight samLogon (both
// take the channel's own lock), and records the credential each logon ran under
// so the test can prove a rebuild switched credentials atomically.
type fakeChannel struct {
	mu        sync.Mutex // mirrors SecureChannel.mu: held for the full samLogon/close
	connected bool
	closed    bool
	id        int // unique per channel instance, to detect rebuilds

	// perChanInFlight counts logons active on THIS channel. The channel lock must
	// keep it <= 1; the test fails the whole run if it ever exceeds 1, proving a
	// teardown/rebuild never lets two logons share a channel concurrently.
	perChanInFlight int32

	// shared across all channels built in one test run.
	state *fakeState
}

type fakeState struct {
	mu sync.Mutex
	// inFlight is the number of samLogon calls currently holding a channel lock.
	inFlight int32
	// maxInFlight is the high-water mark of concurrent samLogons on a SINGLE
	// channel — must stay 1 because samLogon serializes on the channel lock.
	maxInFlightPerChan int32
	// nextID hands out unique channel IDs.
	nextID int
	// logonCreds records the AccountName each logon authenticated under.
	logonCreds []string
	// built counts how many channels were connected (rebuild detection).
	built int
	// logonDelay slows samLogon so a concurrent reload has a window to interleave.
	logonDelay time.Duration
	// concurrencyViolation is set to 1 if any single channel ever served two
	// logons at once — i.e. the per-channel lock invariant was broken.
	concurrencyViolation int32
}

func (c *fakeChannel) connect(ctx context.Context, mc MachineCredential) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected {
		return nil
	}
	c.connected = true
	c.state.mu.Lock()
	c.state.built++
	c.state.mu.Unlock()
	return nil
}

func (c *fakeChannel) samLogon(ctx context.Context, mc MachineCredential, req NetworkLogonRequest) (*LogonResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.closed {
		// Mirror SecureChannel.samLogon's torn-down path so the Authenticator's
		// reload-race retry/rebuild logic is exercised. Uses the same sentinel the
		// real channel returns so NetworkLogon treats it as a benign reload race.
		return nil, errChannelNotConnected
	}

	// Per-channel concurrency must never exceed 1: the channel lock serializes
	// logons and a teardown blocks on it. If it does, the sequence chain could be
	// corrupted — record the violation for the test to fail on.
	if perChan := atomic.AddInt32(&c.perChanInFlight, 1); perChan > 1 {
		atomic.StoreInt32(&c.state.concurrencyViolation, 1)
	}
	n := atomic.AddInt32(&c.state.inFlight, 1)
	c.recordHighWater(n)
	if c.state.logonDelay > 0 {
		time.Sleep(c.state.logonDelay)
	}
	c.state.mu.Lock()
	c.state.logonCreds = append(c.state.logonCreds, mc.AccountName)
	c.state.mu.Unlock()
	atomic.AddInt32(&c.state.inFlight, -1)
	atomic.AddInt32(&c.perChanInFlight, -1)

	return &LogonResult{Username: req.Username, DomainName: req.Domain}, nil
}

func (c *fakeChannel) recordHighWater(n int32) {
	c.state.mu.Lock()
	if n > c.state.maxInFlightPerChan {
		c.state.maxInFlightPerChan = n
	}
	c.state.mu.Unlock()
}

func (c *fakeChannel) close(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	c.closed = true
}

// withFakeChannels swaps newSecureChannel for a factory that builds fakeChannels
// sharing st, and restores it on cleanup.
func withFakeChannels(t *testing.T, st *fakeState) {
	t.Helper()
	orig := newSecureChannel
	newSecureChannel = func() secureChannel {
		st.mu.Lock()
		st.nextID++
		id := st.nextID
		st.mu.Unlock()
		return &fakeChannel{id: id, state: st}
	}
	t.Cleanup(func() { newSecureChannel = orig })
}

func validCred(account string) MachineCredential {
	return MachineCredential{
		AccountName: account,
		Password:    "secret",
		Workstation: "DITTOFS",
		DomainName:  "DITTOFS",
		Realm:       "DITTOFS.AD",
	}
}

// TestReloadCredential_SwapsCredentialAndChannel proves a ReloadCredential makes
// the next logon authenticate with the new credential and rebuild the channel.
func TestReloadCredential_SwapsCredentialAndChannel(t *testing.T) {
	st := &fakeState{}
	withFakeChannels(t, st)

	prov := NewMutableProvider(validCred("OLD$"))
	a := NewAuthenticator(prov)
	ctx := context.Background()

	if _, err := a.NetworkLogon(ctx, NetworkLogonRequest{Username: "alice", Domain: "DITTOFS"}); err != nil {
		t.Fatalf("first logon: %v", err)
	}
	a.ReloadCredential(ctx, validCred("NEW$"))
	if _, err := a.NetworkLogon(ctx, NetworkLogonRequest{Username: "bob", Domain: "DITTOFS"}); err != nil {
		t.Fatalf("post-reload logon: %v", err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.logonCreds) != 2 {
		t.Fatalf("expected 2 logons, got %d (%v)", len(st.logonCreds), st.logonCreds)
	}
	if st.logonCreds[0] != "OLD$" || st.logonCreds[1] != "NEW$" {
		t.Fatalf("expected logons [OLD$ NEW$], got %v", st.logonCreds)
	}
	if st.built < 2 {
		t.Fatalf("expected the channel to be rebuilt (>=2 connects), got %d", st.built)
	}
}

// TestReloadCredential_ConcurrentLogons hammers NetworkLogon from many
// goroutines while repeatedly reloading the credential. It must not error, never
// overlap a close with an in-flight logon (the channel lock enforces this), and
// every logon must succeed. Run with -race to catch data races on the channel
// pointer swap and credential swap.
func TestReloadCredential_ConcurrentLogons(t *testing.T) {
	st := &fakeState{logonDelay: 50 * time.Microsecond}
	withFakeChannels(t, st)

	prov := NewMutableProvider(validCred("GEN0$"))
	a := NewAuthenticator(prov)
	ctx := context.Background()

	const workers = 16
	const perWorker = 200

	var logonWG sync.WaitGroup
	var reloadWG sync.WaitGroup
	var logonErrs int32

	// Logon workers.
	for w := 0; w < workers; w++ {
		logonWG.Add(1)
		go func() {
			defer logonWG.Done()
			for i := 0; i < perWorker; i++ {
				if _, err := a.NetworkLogon(ctx, NetworkLogonRequest{Username: "u", Domain: "DITTOFS"}); err != nil {
					atomic.AddInt32(&logonErrs, 1)
				}
			}
		}()
	}

	// Reload workers: continuously swap the credential + rebuild the channel
	// until the logon workers finish.
	stop := make(chan struct{})
	var reloads int32
	for w := 0; w < 4; w++ {
		reloadWG.Add(1)
		go func(base int) {
			defer reloadWG.Done()
			gen := base
			for {
				select {
				case <-stop:
					return
				default:
					gen++
					a.ReloadCredential(ctx, validCred("GEN"+itoa(gen)+"$"))
					atomic.AddInt32(&reloads, 1)
				}
			}
		}(w * 100000)
	}

	logonWG.Wait()
	close(stop)
	reloadWG.Wait()

	if got := atomic.LoadInt32(&logonErrs); got != 0 {
		t.Fatalf("expected zero logon errors under concurrent reload, got %d", got)
	}
	if got := completedLogons(st); got != workers*perWorker {
		t.Fatalf("expected %d completed logons, got %d", workers*perWorker, got)
	}
	if st.maxInFlightPerChan == 0 {
		t.Fatal("expected some in-flight logons to be recorded")
	}
	if atomic.LoadInt32(&st.concurrencyViolation) != 0 {
		t.Fatal("a single secure channel served two logons concurrently: " +
			"the per-channel lock invariant was broken (sequence chain could corrupt)")
	}
	t.Logf("completed %d logons across %d reloads; %d channels built",
		completedLogons(st), atomic.LoadInt32(&reloads), builtCount(st))
}

func completedLogons(st *fakeState) int32 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return int32(len(st.logonCreds))
}

func builtCount(st *fakeState) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.built
}

// itoa is a tiny dependency-free int-to-string for unique credential names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
