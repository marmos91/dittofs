package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestAuthCacheKey_DistinguishesPrincipalsSharingNumericTriple pins that the
// auth-context cache key covers SID and group SIDs, not just uid/gid/gids.
//
// Two Kerberos principals can resolve to the same numeric triple — a user
// record with no UID defaults to 1000 — while carrying different SIDs. If the
// key ignored the Windows half, the second principal would be served the
// first's cached identity and inherit its SID-keyed ACE grants. That fails
// open, unlike a missing SID, so the key must stay complete.
func TestAuthCacheKey_DistinguishesPrincipalsSharingNumericTriple(t *testing.T) {
	const (
		aliceSID = "S-1-5-21-3623811015-3361044348-30300820-1013"
		bobSID   = "S-1-5-21-3623811015-3361044348-30300820-1014"
	)

	uid := uint32(1000)
	gid := uint32(1000)

	mkCtx := func(sid string) *NFSHandlerContext {
		return &NFSHandlerContext{
			Context: gss.ContextWithIdentity(context.Background(), &metadata.Identity{
				UID:  &uid,
				GID:  &gid,
				GIDs: []uint32{1000},
				SID:  &sid,
			}),
			Share:      "/export",
			AuthFlavor: rpc.AuthRPCSECGSS,
			UID:        &uid,
			GID:        &gid,
			GIDs:       []uint32{1000},
		}
	}

	aliceKey := authCacheKey(mkCtx(aliceSID))
	bobKey := authCacheKey(mkCtx(bobSID))

	if aliceKey == bobKey {
		t.Fatalf("cache keys collide for distinct SIDs %q and %q: %q", aliceSID, bobSID, aliceKey)
	}

	// The same principal must still hit the same key, or the cache is useless.
	if again := authCacheKey(mkCtx(aliceSID)); again != aliceKey {
		t.Errorf("key for the same principal is not stable: %q != %q", again, aliceKey)
	}
}

// TestAuthCacheKey_GroupSIDsAffectKey covers the group half: a principal whose
// group SIDs differ must not share a cache entry even with an identical SID.
func TestAuthCacheKey_GroupSIDsAffectKey(t *testing.T) {
	uid := uint32(1000)
	gid := uint32(1000)
	sid := "S-1-5-21-3623811015-3361044348-30300820-1013"

	mkCtx := func(groupSIDs []string) *NFSHandlerContext {
		return &NFSHandlerContext{
			Context: gss.ContextWithIdentity(context.Background(), &metadata.Identity{
				UID:       &uid,
				GID:       &gid,
				GIDs:      []uint32{1000},
				SID:       &sid,
				GroupSIDs: groupSIDs,
			}),
			Share:      "/export",
			AuthFlavor: rpc.AuthRPCSECGSS,
			UID:        &uid,
			GID:        &gid,
			GIDs:       []uint32{1000},
		}
	}

	withGroup := authCacheKey(mkCtx([]string{"S-1-5-21-3623811015-3361044348-30300820-513"}))
	withoutGroup := authCacheKey(mkCtx(nil))

	if withGroup == withoutGroup {
		t.Fatalf("cache keys collide with and without group SIDs: %q", withGroup)
	}
}

// TestAuthCacheKey_AuthUnixUnaffected pins that a non-GSS request still keys on
// the numeric triple alone, so AUTH_UNIX caching behaviour is unchanged.
func TestAuthCacheKey_AuthUnixUnaffected(t *testing.T) {
	uid := uint32(1001)
	gid := uint32(1002)

	mkCtx := func() *NFSHandlerContext {
		return &NFSHandlerContext{
			Context:    context.Background(),
			Share:      "/export",
			AuthFlavor: rpc.AuthUnix,
			UID:        &uid,
			GID:        &gid,
			GIDs:       []uint32{1002},
		}
	}

	if a, b := authCacheKey(mkCtx()), authCacheKey(mkCtx()); a != b {
		t.Errorf("AUTH_UNIX key unstable: %q != %q", a, b)
	}
}
