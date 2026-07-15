package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func benchAuthUser() *models.User {
	gid := func(v uint32) *uint32 { return &v }
	uid := uint32(1000)
	return &models.User{
		Username:  "alice",
		UID:       &uid,
		GID:       gid(1000),
		SID:       "S-1-5-21-1-2-3-1001",
		GroupSIDs: []string{"S-1-5-21-1-2-3-513", "S-1-5-21-1-2-3-514", "S-1-5-21-1-2-3-515"},
		Groups: []models.Group{
			{Name: "g1", GID: gid(1000)},
			{Name: "g2", GID: gid(1001)},
			{Name: "g3", GID: gid(1002)},
			{Name: "g4", GID: gid(1003)},
		},
	}
}

func benchAuthCtx(user *models.User, sess *session.Session) *SMBHandlerContext {
	return &SMBHandlerContext{
		Context:      context.Background(),
		ClientAddr:   "10.0.0.1:445",
		SessionID:    1,
		User:         user,
		PACGroupSIDs: []string{"S-1-5-21-9-9-9-1", "S-1-5-21-9-9-9-2"},
		Permission:   models.PermissionReadWrite,
		session:      sess,
	}
}

// BenchmarkBuildAuthContextFromUser_Uncached measures the per-op cost with no
// session cache: the identity (UID/GID lookup, GID slice, group-SID concat +
// dedup) is rebuilt on every call — the pre-C5 behaviour.
func BenchmarkBuildAuthContextFromUser_Uncached(b *testing.B) {
	user := benchAuthUser()
	ctx := benchAuthCtx(user, nil) // nil session => never caches, always rebuilds
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildAuthContextFromUser(ctx, user)
	}
}

// BenchmarkBuildAuthContextFromUser_Cached measures the per-op cost once the
// session has memoized the identity (the C5 fast path).
func BenchmarkBuildAuthContextFromUser_Cached(b *testing.B) {
	user := benchAuthUser()
	sess := session.NewSessionWithUser(1, "10.0.0.1:445", user, "")
	ctx := benchAuthCtx(user, sess)
	_ = BuildAuthContextFromUser(ctx, user) // prime the cache
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildAuthContextFromUser(ctx, user)
	}
}
