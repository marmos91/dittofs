package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestBuildAuthContextFromUser_PopulatesSID(t *testing.T) {
	uid := uint32(1001)
	user := &models.User{
		ID:        "user-1",
		Username:  "alice",
		UID:       &uid,
		SID:       "S-1-5-21-1-2-3-2001",
		GroupSIDs: []string{"S-1-5-21-1-2-3-513"},
	}
	ctx := &SMBHandlerContext{
		Context: context.Background(),
		User:    user,
	}

	authCtx := BuildAuthContextFromUser(ctx, user)
	if authCtx == nil || authCtx.Identity == nil {
		t.Fatal("nil Identity")
	}
	if authCtx.Identity.SID == nil {
		t.Fatalf("Identity.SID is nil, want %q", user.SID)
	}
	if got := *authCtx.Identity.SID; got != user.SID {
		t.Errorf("Identity.SID = %q, want %q", got, user.SID)
	}
	if len(authCtx.Identity.GroupSIDs) != 1 || authCtx.Identity.GroupSIDs[0] != user.GroupSIDs[0] {
		t.Errorf("Identity.GroupSIDs = %v, want %v", authCtx.Identity.GroupSIDs, user.GroupSIDs)
	}
}

func TestBuildAuthContextFromUser_NoSIDLeavesIdentityEmpty(t *testing.T) {
	uid := uint32(1001)
	user := &models.User{ID: "user-2", Username: "bob", UID: &uid}
	ctx := &SMBHandlerContext{Context: context.Background(), User: user}

	authCtx := BuildAuthContextFromUser(ctx, user)
	if authCtx.Identity.SID != nil {
		t.Errorf("Identity.SID = %v, want nil for user without SID", *authCtx.Identity.SID)
	}
	if len(authCtx.Identity.GroupSIDs) != 0 {
		t.Errorf("Identity.GroupSIDs = %v, want empty", authCtx.Identity.GroupSIDs)
	}
}
