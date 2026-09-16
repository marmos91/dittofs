package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestUserUIDChangeReprojectsRootACL pins the root-ACL projection to the
// identity mutation that moves the key it is written under.
//
// A share grant projects onto the share root directory as an ACE keyed on the
// grantee's Unix id ("{uid}@localdomain"). A user's UID is rewritten by
// PUT /api/v1/users/{name}, which writes no grant row — so a completion hung
// off the grant write never runs, the ACE keeps naming the old id, and the
// grantee is refused at a root directory owned by uid 0 mode 0755 while holding
// a live grant.
func TestUserUIDChangeReprojectsRootACL(t *testing.T) {
	router, token, cpStore, rt := newRootACLTestRouter(t, "/export")
	seedUser(t, cpStore, "target")

	// Grant through the share route so the projection is known-good first.
	if rec := doAuthedRequest(t, router, token, http.MethodPut,
		"/api/v1/shares/export/permissions/users/target", `{"level":"read-write"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("grant = %d, want %d (body=%q)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if !hasGranteeACE(rootACL(t, rt, "/export"), "4242@localdomain") {
		t.Fatalf("grant did not project onto the root ACL; got %+v", rootACL(t, rt, "/export"))
	}

	if rec := doAuthedRequest(t, router, token, http.MethodPut,
		"/api/v1/users/target", `{"uid":7777}`); rec.Code != http.StatusOK {
		t.Fatalf("uid change = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	acl := rootACL(t, rt, "/export")
	if hasGranteeACE(acl, "4242@localdomain") {
		t.Errorf("root ACL still names the old uid after the change; got %+v", acl)
	}
	if !hasGranteeACE(acl, "7777@localdomain") {
		t.Errorf("root ACL does not name the new uid, so the grantee is refused at the share root; got %+v", acl)
	}

	// The grant row is untouched by a UID change, so the projection must follow
	// the identity, not the row.
	perm, err := cpStore.GetUserSharePermission(context.Background(), "target", "/export")
	if err != nil {
		t.Fatalf("read back grant: %v", err)
	}
	if perm == nil || perm.Permission != string(models.PermissionReadWrite) {
		t.Fatalf("grant row changed by a uid update: %+v", perm)
	}
}

// TestGroupGIDChangeReprojectsRootACL is the group-grant twin of the user case:
// a group's GID is the key its grants project under, and PUT /api/v1/groups/{name}
// writes no grant row either.
func TestGroupGIDChangeReprojectsRootACL(t *testing.T) {
	router, token, cpStore, rt := newRootACLTestRouter(t, "/export")
	seedGroup(t, cpStore, "staff")

	if rec := doAuthedRequest(t, router, token, http.MethodPut,
		"/api/v1/shares/export/permissions/groups/staff", `{"level":"read"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("group grant = %d, want %d (body=%q)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if !hasGranteeACE(rootACL(t, rt, "/export"), "9001@localdomain") {
		t.Fatalf("group grant did not project onto the root ACL; got %+v", rootACL(t, rt, "/export"))
	}

	if rec := doAuthedRequest(t, router, token, http.MethodPut,
		"/api/v1/groups/staff", `{"gid":9002}`); rec.Code != http.StatusOK {
		t.Fatalf("gid change = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	acl := rootACL(t, rt, "/export")
	if hasGranteeACE(acl, "9001@localdomain") {
		t.Errorf("root ACL still names the old gid after the change; got %+v", acl)
	}
	if !hasGranteeACE(acl, "9002@localdomain") {
		t.Errorf("root ACL does not name the new gid; got %+v", acl)
	}
}
