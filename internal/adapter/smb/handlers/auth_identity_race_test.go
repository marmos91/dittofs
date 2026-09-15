package handlers

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// principalOf names which of the two test principals an identity belongs to,
// answering separately from the user record and from the PAC group SIDs. A
// per-operation identity that reports one principal by its UID and the other by
// its group SIDs is a combination the session never held: the two halves were
// read a re-authentication apart.
func principalOf(t *testing.T, uid uint32) string {
	t.Helper()
	switch uid {
	case 1000:
		return "alice"
	case 1001:
		return "bob"
	}
	t.Fatalf("identity carries UID %d, which belongs to neither test principal", uid)
	return ""
}

// TestPrimeAuthContext_ConcurrentReauthIsRaceFree drives the per-operation
// authorization path against re-authentication on the same session.
//
// Every file operation resolves its identity through primeAuthContext, which
// reads the session's user record, its guest flag and its Kerberos PAC SIDs.
// SESSION_SETUP re-authentication replaces all three under the session mutex,
// so reading them off the struct is a data race — on the user pointer and on
// the PAC slice header alike — and reading them through separate accessors
// hands the operation a hybrid identity even when each read is individually
// safe. The race detector is the primary assertion; the identity check below
// pins the hybrid, which the detector alone would not name.
func TestPrimeAuthContext_ConcurrentReauthIsRaceFree(t *testing.T) {
	aliceUID, bobUID := uint32(1000), uint32(1001)
	alice := &models.User{ID: "u1", Username: "alice", UID: &aliceUID, Enabled: true}
	bob := &models.User{ID: "u2", Username: "bob", UID: &bobUID, Enabled: true}
	sidOf := map[string]string{
		"alice": "S-1-5-21-1-2-3-1000",
		"bob":   "S-1-5-21-1-2-3-1001",
	}

	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:12345", false, alice.Username, "")
	sess.UpdateIdentity(alice.Username, "", alice, false, false, []string{sidOf["alice"]}, sidOf["alice"])
	sessionID := sess.SessionID

	var wg sync.WaitGroup
	wg.Add(2)

	// The dispatch side: prime a fresh request context off the session and
	// build the AuthContext an ownership or group-ACL check would run against.
	mismatches := make(chan string, 200)
	go func() {
		defer wg.Done()
		for i := range 200 {
			ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:12345", sessionID, 0, uint64(i))
			h.primeAuthContext(ctx, 0, sessionID)
			authCtx, err := BuildAuthContext(ctx)
			if err != nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
				continue
			}
			byRecord := principalOf(t, *authCtx.Identity.UID)
			for name, sid := range sidOf {
				if name == byRecord {
					continue
				}
				for _, got := range authCtx.Identity.GroupSIDs {
					if got == sid {
						mismatches <- fmt.Sprintf("identity for %s carries %s's group SID %s", byRecord, name, sid)
					}
				}
			}
		}
	}()

	// The re-authentication side: SESSION_SETUP replacing the whole identity,
	// record and PAC together, the way kerberos_auth.go does.
	go func() {
		defer wg.Done()
		for i := range 200 {
			user, name := alice, "alice"
			if i%2 == 1 {
				user, name = bob, "bob"
			}
			sess.UpdateIdentity(user.Username, "", user, false, false, []string{sidOf[name]}, sidOf[name])
		}
	}()

	wg.Wait()
	close(mismatches)
	if msg, ok := <-mismatches; ok {
		t.Errorf("per-operation identity mixed two principals: %s", msg)
	}
}
