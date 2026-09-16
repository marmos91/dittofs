package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// hasGranteeACE reports whether a root ACL carries an ACE naming the principal.
func hasGranteeACE(a *acl.ACL, who string) bool {
	if a == nil {
		return false
	}
	for _, ace := range a.ACEs {
		if ace.Who == who {
			return true
		}
	}
	return false
}

// TestUserRouteSharePermissionKeySpellings pins the share-name spellings the
// user route accepts to the ones the share permission routes accept.
//
// The share routes fold the path segment with metadata.NormalizeShareName, so
// "export" and "/export" both reach the share. The user route reads the share
// name from a JSON map key and, unfolded, an unslashed key misses the store
// lookup; the best-effort contract then skips the entry, and the endpoint
// answers 200 with the user body and no grant. Same administrative intent,
// different result by endpoint.
//
// The share root is owned by uid 0 mode 0755, so the grant only reaches the
// filesystem through the projected ACE — the assertion covers both halves.
func TestUserRouteSharePermissionKeySpellings(t *testing.T) {
	router, token, cpStore, rt := newRootACLTestRouter(t, "/export")
	seedUser(t, cpStore, "target")

	rec := doAuthedRequest(t, router, token, http.MethodPut, "/api/v1/users/target",
		`{"share_permissions":{"export":"read-write"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("user-route grant = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	perm, err := cpStore.GetUserSharePermission(context.Background(), "target", "/export")
	if err != nil {
		t.Fatalf("read back grant: %v", err)
	}
	if perm == nil || perm.Permission != string(models.PermissionReadWrite) {
		t.Fatalf("unslashed key %q did not reach the share: grant not persisted: %+v", "export", perm)
	}

	if !hasGranteeACE(rootACL(t, rt, "/export"), "4242@localdomain") {
		t.Errorf("grant persisted but never projected onto the root ACL of /export; got %+v", rootACL(t, rt, "/export"))
	}
}

// TestUserRouteReportsSkippedSharePermissions pins the report of a share
// permission entry the best-effort contract skipped.
//
// Skipping an unresolvable share is deliberate — a bad entry must not roll back
// a user create or update — but a 200 that silently drops a permission the
// caller asked for is indistinguishable from success. The skipped entry is
// named in the response, the user write still lands, and a request with nothing
// skipped carries no such field.
func TestUserRouteReportsSkippedSharePermissions(t *testing.T) {
	router, token, cpStore, _ := newRootACLTestRouter(t, "/export")
	seedUser(t, cpStore, "target")

	type userBody struct {
		Username string   `json:"username"`
		Warnings []string `json:"warnings,omitempty"`
	}

	rec := doAuthedRequest(t, router, token, http.MethodPut, "/api/v1/users/target",
		`{"share_permissions":{"nosuchshare":"read-write","/export":"bogus"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("user update = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body userBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if body.Username != "target" {
		t.Errorf("user body not returned: %+v", body)
	}
	for _, want := range []string{"nosuchshare", "bogus"} {
		if !strings.Contains(strings.Join(body.Warnings, "\n"), want) {
			t.Errorf("response does not report the skipped entry %q: warnings = %v", want, body.Warnings)
		}
	}

	// The best-effort contract still holds: the bad entries did not roll back
	// the user write, and the valid entry beside them still landed.
	user, err := cpStore.GetUser(context.Background(), "target")
	if err != nil {
		t.Fatalf("user write rolled back: %v", err)
	}
	if !user.Enabled {
		t.Errorf("user no longer enabled after a skipped share permission")
	}
	perm, err := cpStore.GetUserSharePermission(context.Background(), "target", "/export")
	if err != nil {
		t.Fatalf("read back grant: %v", err)
	}
	if perm != nil {
		t.Errorf("invalid permission %q was applied: %+v", "bogus", perm)
	}

	// Nothing skipped: the field stays absent, so a client cannot mistake a
	// clean response for one carrying a report.
	rec = doAuthedRequest(t, router, token, http.MethodPut, "/api/v1/users/target",
		`{"share_permissions":{"/export":"read"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("user update = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = userBody{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if len(body.Warnings) != 0 {
		t.Errorf("clean request reported warnings: %v", body.Warnings)
	}
}
