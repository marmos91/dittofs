package session

import (
	"sync"
	"testing"
)

// TestSession_CryptoStateConcurrentReauthRace is the negative control for the
// data race on Session.CryptoState (audit #1132 MED). SESSION_SETUP re-auth
// swaps the whole SessionCryptoState (SetCryptoState / SetSigningKey /
// EnableSigning) on an already-published session while in-flight requests on
// the same session read it through ShouldSign / ShouldVerify / ShouldEncrypt /
// VerifyMessage / DecryptMessage and the response path.
//
// Before the fix CryptoState was a plain pointer field; the concurrent
// store/load is a data race under the Go memory model and `go test -race`
// flags it. After the fix it is an atomic.Pointer accessed only via
// GetCryptoState / SetCryptoState, so this test must run clean under -race.
//
// Run: go test -race ./internal/adapter/smb/session/ -run CryptoStateConcurrentReauthRace
func TestSession_CryptoStateConcurrentReauthRace(t *testing.T) {
	s := NewSession(1, "127.0.0.1:1", false, "alice", "WORKGROUP")

	const iters = 2000
	var wg sync.WaitGroup

	// Writer: re-auth swaps the crypto state repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		key := make([]byte, 16)
		for i := 0; i < iters; i++ {
			switch i % 3 {
			case 0:
				s.SetCryptoState(&SessionCryptoState{})
			case 1:
				s.SetSigningKey(key)
			case 2:
				s.EnableSigning(true)
			}
		}
	}()

	// Readers: in-flight requests inspect signing/encryption state.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := make([]byte, 64)
			for i := 0; i < iters; i++ {
				_ = s.ShouldSign()
				_ = s.ShouldVerify()
				_ = s.ShouldEncrypt()
				_ = s.VerifyMessage(msg)
				_ = s.GetCryptoState()
			}
		}()
	}

	wg.Wait()
}

// TestSession_NewlyCreatedConcurrentRace is the negative control for the data
// race on Session.NewlyCreated (audit #1132 LOW). The framing layer clears the
// flag (ClearNewlyCreated) on one request goroutine while another reads it
// (IsNewlyCreated) on a pipelined request during SESSION_SETUP completion.
//
// Before the fix NewlyCreated was a plain bool; the concurrent read+write is a
// data race. After the fix it is an atomic.Bool. Run under -race.
func TestSession_NewlyCreatedConcurrentRace(t *testing.T) {
	s := NewSession(1, "127.0.0.1:1", false, "alice", "WORKGROUP")
	if !s.IsNewlyCreated() {
		t.Fatal("freshly created session should report IsNewlyCreated()=true")
	}

	const iters = 5000
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			s.ClearNewlyCreated()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = s.IsNewlyCreated()
		}
	}()

	wg.Wait()

	if s.IsNewlyCreated() {
		t.Error("NewlyCreated should be cleared after ClearNewlyCreated()")
	}
}
