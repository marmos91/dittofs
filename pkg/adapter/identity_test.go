package adapter

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/identity"
)

func TestExtractRealm(t *testing.T) {
	for in, want := range map[string]string{
		"nfs/host@EXAMPLE.COM": "EXAMPLE.COM",
		"alice@EXAMPLE.COM":    "EXAMPLE.COM",
		"bare":                 "",
		"trailing@":            "",
	} {
		if got := ExtractRealm(in); got != want {
			t.Errorf("ExtractRealm(%q) = %q, want %q", in, got, want)
		}
	}
}

// newResolverRuntime builds a runtime over an in-memory control-plane store.
func newResolverRuntime(t *testing.T) *runtime.Runtime {
	t.Helper()
	cps, err := store.New(&store.Config{
		Type:   store.DatabaseTypeSQLite,
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("store.New(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = cps.Close() })
	return runtime.New(cps)
}

// BuildIdentityResolver has no unit coverage despite being on every Kerberos
// session path for both protocols; this exercises the userLookup it builds.
func TestBuildIdentityResolver_ResolvesLocalUser(t *testing.T) {
	rt := newResolverRuntime(t)
	uid, gid := uint32(4242), uint32(4343)
	if _, err := rt.Store().CreateUser(context.Background(), &models.User{
		Username: "alice", Enabled: true, UID: &uid, GID: &gid,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	resolver := BuildIdentityResolver(rt, "EXAMPLE.COM")
	got, err := resolver.Resolve(context.Background(), &identity.Credential{
		ExternalID: "alice@EXAMPLE.COM",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Found {
		t.Fatal("expected alice to resolve")
	}
	if got.UID != uid || got.GID != gid {
		t.Fatalf("uid/gid = %d/%d, want %d/%d", got.UID, got.GID, uid, gid)
	}
}

// decision: the resolver reports Found=true for a disabled user without
// checking Enabled, and no gate exists inside it. That is deliberate — the
// resolver answers "who is this principal", not "may they proceed" — and the
// authorization decision is made downstream, where auth.ResolveSharePermission
// refuses a disabled user's UID (covered in the nfs/auth package's
// share-permission tests). This test pins the division of labour: adding an
// Enabled gate here would deny at the wrong layer and must fail this test.
func TestBuildIdentityResolver_ResolvesDisabledUserWithoutGating(t *testing.T) {
	rt := newResolverRuntime(t)
	uid := uint32(4242)
	// Created enabled, then disabled through UpdateUser: the Enabled column's
	// `default:true` tag makes GORM overwrite an explicit false on insert, so a
	// create-with-false would leave the user enabled and prove nothing.
	id, err := rt.Store().CreateUser(context.Background(), &models.User{
		Username: "bob", Enabled: true, UID: &uid,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := rt.Store().UpdateUser(context.Background(), &models.User{
		ID: id, Username: "bob", Enabled: false, UID: &uid,
	}); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	resolver := BuildIdentityResolver(rt, "EXAMPLE.COM")
	got, err := resolver.Resolve(context.Background(), &identity.Credential{
		ExternalID: "bob@EXAMPLE.COM",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Found {
		t.Fatal("the resolver is not the Enabled gate; a disabled user must still resolve")
	}
}
