# SMB ACL/SID Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flip ~22 smbtorture conformance failures by wiring Windows SID-aware identity through DittoFS's SD/ACL pipeline and making CREATE consult parent-directory DACLs.

**Architecture:** Six sequential phases that progressively close the gap between DittoFS's POSIX-leaning ACL evaluator and Windows ACL semantics. Phase 2 establishes a SID-aware Identity (foundation that every other phase depends on). Phase 1 wires the existing parent-dir permission check into the CREATE entry point and removes the `ShareWritable` short-circuit that bypasses ACL evaluation. Phases 3–6 fix smaller round-trip and semantic gaps (SD control flags, MaxAccess from SD, CREATOR_OWNER inheritance, canonical ACL ordering).

**Tech Stack:**
- Go (project standard 1.25.x)
- `pkg/metadata/` — service + permission engine
- `pkg/metadata/acl/` — ACL types + evaluator + inheritance
- `internal/adapter/smb/v2/handlers/` — SMB2 protocol layer
- Tests: standard `testing` + `pkg/metadata/storetest/` conformance suite + smbtorture (run via CI)

---

## Tracking

- Parent issue: filed before Phase 2 starts (see Pre-flight task).
- Each phase ships as its own PR. CI must be green before the next phase opens.
- KNOWN_FAILURES walkbacks land in a follow-up commit on the merging PR after CI confirms each test flips. Never touch KNOWN_FAILURES.md before CI proof — see project memory rule "never walk back KNOWN_FAILURES until CI confirms".

## Pre-flight

### Task PF-1: File parent issue and child issues

**Files:**
- No code changes
- GitHub: 1 parent + 6 child issues

- [ ] **Step 1: File parent tracking issue**

```bash
gh issue create --title "SMB ACL/SID identity — flip ~22 smbtorture ACL/DOC failures" --body "$(cat <<'EOF'
## Goal

Implement Windows SID-aware identity in DittoFS's metadata/ACL stack and wire parent-directory DACL evaluation through the SMB2 CREATE path. Targets ~22 smbtorture KNOWN failures across `smb2.delete-on-close-perms.*`, `smb2.acls.*`, `smb2.acls_non_canonical.*`.

## Phases

- [ ] P2 — SID-aware Identity round-trip (foundation, prerequisite for P1)
- [ ] P1 — Wire parent-dir ACL into SMB CREATE
- [ ] P5 — MaxAccess computed from SD instead of POSIX-only
- [ ] P3 — SD control-flags + SACL-offset round-trip + OWNER_RIGHTS
- [ ] P4 — CREATOR_OWNER / CREATOR_GROUP substitution at inherit
- [ ] P6 — Canonical ACL ordering on parse

Plan: docs/superpowers/plans/2026-05-06-smb-acl-sid-identity.md
EOF
)"
```

Capture the issue number. Future phase PRs use `Refs #<parent>` in their bodies.

- [ ] **Step 2: File six child issues, one per phase**

For each phase below, file an issue titled `[ACL Pn] <short>`. Body: scope outline + tests it should flip (see plan body). Link parent.

```bash
gh issue create --title "[ACL P2] SID-aware Identity round-trip" --body "..."
# repeat for P1, P5, P3, P4, P6
```

- [ ] **Step 3: Commit nothing — Pre-flight is GitHub bookkeeping only**

---

# Phase 2 — SID-aware Identity round-trip

**Why first:** Every later phase that compares an ACE to the requesting user fails today because `Identity.SID` is never populated. Without P2, P1's parent-dir DACL check still grants access on smbtorture's SID-keyed deny ACEs.

**Tests this enables (when combined with P1):** `acls.OWNER`, `acls.OWNER-RIGHTS`, `acls.OWNER-RIGHTS-DENY`, `acls.OWNER-RIGHTS-DENY1`, `acls.GENERIC` (5 tests directly; precondition for the 8 P1 unlocks).

## File Structure

- Modify `pkg/metadata/acl/evaluate.go` — extend `EvaluateContext` and `aceMatchesWho` for SID matching.
- Modify `pkg/metadata/acl/types.go` — extend `ACE.Who` doc to allow `sid:S-1-5-21-...` form.
- Modify `pkg/metadata/auth_permissions.go` — pass `Identity.SID` + `Identity.GroupSIDs` into the evaluate context.
- Modify `internal/adapter/smb/v2/handlers/auth_helper.go` — populate `Identity.SID` and `Identity.GroupSIDs` from session.
- Modify `internal/adapter/smb/v2/handlers/security.go` — `parseDACL` keeps SID-form for non-POSIX-mappable principals; `principalToSID` round-trip handles `sid:` prefix.
- Add tests under `pkg/metadata/acl/evaluate_test.go` and `pkg/metadata/auth_permissions_sid_test.go`.

## Tasks

### Task P2-1: Add SID-form `Who` matching to ACE evaluator (TDD)

**Files:**
- Modify: `pkg/metadata/acl/evaluate.go:75-107`
- Test: `pkg/metadata/acl/evaluate_test.go` (new test cases)

- [ ] **Step 1: Write the failing test**

Append to `pkg/metadata/acl/evaluate_test.go`:

```go
func TestAceMatchesWho_SIDForm(t *testing.T) {
	cases := []struct {
		name     string
		aceWho   string
		ctxSID   string
		ctxGSIDs []string
		want     bool
	}{
		{
			name:   "exact_user_sid_match",
			aceWho: "sid:S-1-5-21-1-2-3-1001",
			ctxSID: "S-1-5-21-1-2-3-1001",
			want:   true,
		},
		{
			name:   "user_sid_mismatch",
			aceWho: "sid:S-1-5-21-1-2-3-1001",
			ctxSID: "S-1-5-21-1-2-3-9999",
			want:   false,
		},
		{
			name:     "group_sid_match_via_groupSIDs",
			aceWho:   "sid:S-1-5-21-1-2-3-513",
			ctxSID:   "S-1-5-21-1-2-3-1001",
			ctxGSIDs: []string{"S-1-5-21-1-2-3-513"},
			want:     true,
		},
		{
			name:   "missing_ctx_sid_falls_through_to_string_compare",
			aceWho: "sid:S-1-5-21-1-2-3-1001",
			// ctxSID empty
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ace := &ACE{Who: tc.aceWho, Type: AceTypeAllow}
			ec := &EvaluateContext{
				SID:       tc.ctxSID,
				GroupSIDs: tc.ctxGSIDs,
			}
			if got := aceMatchesWho(ace, ec); got != tc.want {
				t.Errorf("aceMatchesWho(%q, sid=%q) = %v, want %v",
					tc.aceWho, tc.ctxSID, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./pkg/metadata/acl/ -run TestAceMatchesWho_SIDForm -v
```

Expected: FAIL — `EvaluateContext` has no `SID` / `GroupSIDs` fields.

- [ ] **Step 3: Extend `EvaluateContext`**

In `pkg/metadata/acl/evaluate.go` near the existing struct (read the file to find exact line; the struct is named `EvaluateContext`), add:

```go
// SID is the requester's Windows SID in canonical string form
// (e.g. "S-1-5-21-A-B-C-RID"). Empty when the session is POSIX-only.
SID string

// GroupSIDs is the requester's group SIDs.
// Empty when the session is POSIX-only.
GroupSIDs []string
```

- [ ] **Step 4: Add SID arm to `aceMatchesWho`**

Replace the `default` arm with:

```go
default:
    // SID-form ACE (set by SD parse): "sid:<canonical SID>".
    if strings.HasPrefix(ace.Who, "sid:") {
        target := ace.Who[len("sid:"):]
        if evalCtx.SID != "" && evalCtx.SID == target {
            return true
        }
        for _, g := range evalCtx.GroupSIDs {
            if g == target {
                return true
            }
        }
        return false
    }
    // Legacy/string match (numeric uid, named principal).
    return ace.Who == evalCtx.Who
```

Add `import "strings"` if missing.

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./pkg/metadata/acl/ -run TestAceMatchesWho_SIDForm -v
```

Expected: PASS for all four sub-cases.

- [ ] **Step 6: Run the existing acl test suite — no regressions**

```bash
go test ./pkg/metadata/acl/ -count=1
```

Expected: ok.

- [ ] **Step 7: Commit**

```bash
git add pkg/metadata/acl/evaluate.go pkg/metadata/acl/evaluate_test.go
git commit -S -m "feat(acl): match SID-form ACEs against requester Windows SID

ACE.Who values prefixed with \"sid:\" are matched against
EvaluateContext.SID and GroupSIDs. Foundation for SMB-issued SDs that
key ACEs by the client's Windows SID rather than a POSIX uid."
```

### Task P2-2: Plumb SID into evaluate context from `evaluateACLPermissions`

**Files:**
- Modify: `pkg/metadata/auth_permissions.go:341-395`
- Test: `pkg/metadata/auth_permissions_sid_test.go` (new)

- [ ] **Step 1: Write the failing test**

Create `pkg/metadata/auth_permissions_sid_test.go`:

```go
package metadata

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

func TestCalculatePermissions_SIDDenyACE(t *testing.T) {
	// File owned by uid 1001 with mode 0777 (anyone-writable in POSIX terms),
	// but with a SID-form deny ACE for the requester. Eval must honor the
	// deny ACE because the SD encodes intent the POSIX bits can't express.
	uid := uint32(1001)
	denyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.AceTypeDeny, Who: "sid:S-1-5-21-1-2-3-2001", Mask: acl.AccessMaskWriteData},
			{Type: acl.AceTypeAllow, Who: acl.SpecialEveryone, Mask: 0xFFFFFFFF},
		},
	}
	file := &File{
		FileAttr: FileAttr{UID: 1001, GID: 1001, Mode: 0o777, ACL: denyACL},
	}
	requesterSID := "S-1-5-21-1-2-3-2001"
	id := &Identity{
		UID: &uid, // present so we don't take the anonymous path
		SID: &requesterSID,
	}

	got := calculatePermissions(file, id, nil, PermissionWrite)
	if got&PermissionWrite != 0 {
		t.Fatalf("expected write denied via SID-form deny ACE, got 0x%x", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./pkg/metadata/ -run TestCalculatePermissions_SIDDenyACE -v
```

Expected: FAIL — eval ctx is built without SID, deny ACE never matches.

- [ ] **Step 3: Pass SID into the evaluate context**

In `pkg/metadata/auth_permissions.go`, find the `EvaluateContext` construction inside `evaluateACLPermissions` (search for `evalCtx := &acl.EvaluateContext{`). There are two such blocks — one for the anonymous path and one for the authenticated path. Update the authenticated-path block to:

```go
evalCtx := &acl.EvaluateContext{
    UID:          *identity.UID,
    GID:          0,
    GIDs:         identity.GIDs,
    FileOwnerUID: file.UID,
    FileOwnerGID: file.GID,
    Who:          identityWho(identity),
}
if identity.GID != nil {
    evalCtx.GID = *identity.GID
}
if identity.SID != nil {
    evalCtx.SID = *identity.SID
}
evalCtx.GroupSIDs = identity.GroupSIDs
```

(`identityWho` already exists; preserve whatever the existing block called.)

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./pkg/metadata/ -run TestCalculatePermissions_SIDDenyACE -v
```

Expected: PASS.

- [ ] **Step 5: Run the metadata test suite for regressions**

```bash
go test ./pkg/metadata/... -count=1
```

Expected: ok across all packages.

- [ ] **Step 6: Commit**

```bash
git add pkg/metadata/auth_permissions.go pkg/metadata/auth_permissions_sid_test.go
git commit -S -m "feat(metadata): propagate Identity.SID into ACL eval context

evaluateACLPermissions now forwards Identity.SID and GroupSIDs to the
ACL evaluator so SID-form ACEs (set by SMB SD persistence) match the
authenticated requester."
```

### Task P2-3: Stop coercing SD principals back to numeric uids

**Files:**
- Modify: `internal/adapter/smb/v2/handlers/security.go:430-485` (the `parseDACL` ACE loop and `SIDToPrincipal`)
- Test: `internal/adapter/smb/v2/handlers/security_test.go` (new test case)

- [ ] **Step 1: Read the current `SIDToPrincipal` body**

Read `internal/adapter/smb/v2/handlers/security.go` lines 460-490 to see the existing function shape; the next step preserves its signature.

- [ ] **Step 2: Write the failing test**

In `internal/adapter/smb/v2/handlers/security_test.go` add (or create the file if absent):

```go
func TestSIDToPrincipal_SIDPassThrough(t *testing.T) {
	// A non-mappable SID should round-trip as "sid:S-1-..." form,
	// not be converted to a numeric uid principal.
	sid := "S-1-5-21-1-2-3-2001"
	got := SIDToPrincipal(sid)
	want := "sid:" + sid
	if got != want {
		t.Errorf("SIDToPrincipal(%q) = %q, want %q", sid, got, want)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

```bash
go test ./internal/adapter/smb/v2/handlers/ -run TestSIDToPrincipal_SIDPassThrough -v
```

Expected: FAIL — current function maps the SID through `MapSIDToUID` and returns a numeric form.

- [ ] **Step 4: Update `SIDToPrincipal` to keep SID-form when no POSIX mapping exists**

```go
// SIDToPrincipal converts a Windows SID into the canonical ACE.Who form.
// Well-known SIDs (CREATOR_OWNER, EVERYONE, OWNER_RIGHTS, etc.) map to the
// special tokens recognised by the ACL evaluator. POSIX-mappable SIDs map
// to "<uid>" / "<gid>" numeric strings (legacy form). Otherwise the SID is
// preserved verbatim with a "sid:" prefix so SID-aware evaluation can match
// it against the requester's Identity.SID/GroupSIDs.
func SIDToPrincipal(sid string) string {
	if special := WellKnownSIDToPrincipal(sid); special != "" {
		return special
	}
	if uid, ok := MapSIDToUID(sid); ok {
		return strconv.FormatUint(uint64(uid), 10)
	}
	return "sid:" + sid
}
```

If `WellKnownSIDToPrincipal` does not exist, factor the existing well-known checks (CREATOR_OWNER, CREATOR_GROUP, EVERYONE, OWNER_RIGHTS, etc.) into that helper now — the body is whatever block was previously inline. Phase 4 will extend it with `CreatorOwner@`/`CreatorGroup@`; preserve current behavior here.

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./internal/adapter/smb/v2/handlers/ -run TestSIDToPrincipal_SIDPassThrough -v
```

Expected: PASS.

- [ ] **Step 6: Add round-trip ACE-form test**

```go
func TestParseSecurityDescriptor_PreservesSIDForm(t *testing.T) {
	// Build a minimal self-relative SD with a single allow ACE keyed to
	// a non-POSIX SID. After parse, the ACL must contain ACE.Who == "sid:..."
	// (helper assumed: buildTestSD constructs a 20-byte header SD with one ACE.)
	sid := "S-1-5-21-1-2-3-2001"
	sd := buildTestSDWithACE(t, AceTypeAllow, sid, 0x001F01FF /* full control */)

	_, _, parsed, err := ParseSecurityDescriptor(sd)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsed == nil || len(parsed.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %#v", parsed)
	}
	want := "sid:" + sid
	if parsed.ACEs[0].Who != want {
		t.Errorf("ACE.Who = %q, want %q", parsed.ACEs[0].Who, want)
	}
}
```

`buildTestSDWithACE` is a new helper in the same test file. Implementation reads ACE/SD layout from `security.go:200-360` (the build path); produce a 20-byte SD header pointing at a 16-byte short header DACL containing one ACE. Cite the offsets from the spec comment block above `BuildSecurityDescriptor`.

- [ ] **Step 7: Run test to verify it passes**

```bash
go test ./internal/adapter/smb/v2/handlers/ -run TestParseSecurityDescriptor_PreservesSIDForm -v
```

Expected: PASS.

- [ ] **Step 8: Run handlers test suite — no regressions**

```bash
go test ./internal/adapter/smb/v2/handlers/ -count=1
```

Expected: ok.

- [ ] **Step 9: Commit**

```bash
git add internal/adapter/smb/v2/handlers/security.go internal/adapter/smb/v2/handlers/security_test.go
git commit -S -m "feat(smb): preserve non-POSIX SIDs in parsed SD as ACE.Who=\"sid:...\"

SIDToPrincipal previously coerced every SID through MapSIDToUID,
producing numeric uid principals that smbtorture deny-ACE tests can't
match. SDs from Windows clients now round-trip the requester's actual
SID (or well-known SID token) so SID-aware ACL eval can match it."
```

### Task P2-4: Populate `Identity.SID` + `GroupSIDs` from SMB session

**Files:**
- Modify: `internal/adapter/smb/v2/handlers/auth_helper.go:115-145`
- Test: `internal/adapter/smb/v2/handlers/auth_helper_test.go` (new test cases)

- [ ] **Step 1: Read the current `BuildAuthContextFromUser`**

Read `internal/adapter/smb/v2/handlers/auth_helper.go` lines 100-160 to confirm where the User → Identity translation happens. Note where `models.User` carries (or could carry) the SID. If `models.User` has no SID field, the SMB layer must derive it via `pkgidentity.Resolver` if configured — for now derive from the user's Username with the existing well-known-SID/RID mapping helpers if `User.SID` is unset.

- [ ] **Step 2: Write the failing test**

```go
func TestBuildAuthContextFromUser_PopulatesSID(t *testing.T) {
	user := &models.User{
		ID:       42,
		Username: "alice",
		Domain:   "EXAMPLE",
		SID:      "S-1-5-21-1-2-3-2001",                         // depends on User shape — see Step 3
		GroupSIDs: []string{"S-1-5-21-1-2-3-513"},
	}
	ctx := &SMBHandlerContext{Context: context.Background(), User: user}
	authCtx := BuildAuthContextFromUser(ctx, user)
	if authCtx == nil || authCtx.Identity == nil {
		t.Fatal("nil Identity")
	}
	if authCtx.Identity.SID == nil || *authCtx.Identity.SID != user.SID {
		t.Errorf("Identity.SID = %v, want %q", authCtx.Identity.SID, user.SID)
	}
	if len(authCtx.Identity.GroupSIDs) != 1 || authCtx.Identity.GroupSIDs[0] != user.GroupSIDs[0] {
		t.Errorf("Identity.GroupSIDs = %v, want %v", authCtx.Identity.GroupSIDs, user.GroupSIDs)
	}
}
```

- [ ] **Step 3: Verify `models.User` has `SID` and `GroupSIDs` fields**

```bash
grep -n "type User struct" pkg/controlplane/models/*.go
```

If those fields are absent, add them in the same commit:

```go
// SID is the user's Windows SID, populated from Kerberos PAC, NTLMSSP
// session info, or local ID resolution. Empty when not available.
SID string `json:"sid,omitempty"`

// GroupSIDs lists the user's group SIDs.
GroupSIDs []string `json:"group_sids,omitempty"`
```

The DB-backed identity resolver (`pkgidentity.Resolver`) is the eventual long-term source. For now this plan adds the field; populating it from PAC/NTLMSSP is out of scope (see Phase 2 follow-up).

- [ ] **Step 4: Update `BuildAuthContextFromUser` to forward SID/GroupSIDs**

In the function body, after the existing `Identity.Username = user.Username` line add:

```go
if user.SID != "" {
    sid := user.SID
    identity.SID = &sid
}
if len(user.GroupSIDs) > 0 {
    identity.GroupSIDs = append([]string(nil), user.GroupSIDs...)
}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./internal/adapter/smb/v2/handlers/ -run TestBuildAuthContextFromUser_PopulatesSID -v
```

Expected: PASS.

- [ ] **Step 6: Run handlers test suite**

```bash
go test ./internal/adapter/smb/v2/handlers/ -count=1
```

Expected: ok.

- [ ] **Step 7: Commit**

```bash
git add pkg/controlplane/models internal/adapter/smb/v2/handlers/auth_helper.go internal/adapter/smb/v2/handlers/auth_helper_test.go
git commit -S -m "feat(smb): forward Windows SID + GroupSIDs into AuthContext.Identity

models.User gains SID and GroupSIDs fields. BuildAuthContextFromUser
copies them into the metadata Identity so the ACL evaluator can match
SID-form ACEs against the requester. Population from PAC/NTLMSSP is a
follow-up — fields default empty when no source is wired."
```

### Task P2-5: Open the PR for Phase 2

**Files:**
- No code

- [ ] **Step 1: Push branch + open PR**

```bash
git push -u origin feat/smb-acl-p2-sid-identity
gh pr create --base develop --head feat/smb-acl-p2-sid-identity \
  --title "feat(smb): SID-aware Identity for ACL evaluation (refs #<parent>)" \
  --body "$(cat <<'EOF'
## Summary

Phase 2 of the SMB ACL/SID identity plan. Threads Windows SID through the metadata Identity so SID-form ACEs (parsed from SMB SD writes) match the requester. Foundation for Phase 1 (parent-dir DACL at CREATE).

## Changes

- `pkg/metadata/acl/evaluate.go`: `EvaluateContext.SID` + `GroupSIDs`; `aceMatchesWho` SID-form arm.
- `pkg/metadata/auth_permissions.go`: forwards Identity.SID/GroupSIDs into eval context.
- `internal/adapter/smb/v2/handlers/security.go`: SDs preserve non-POSIX SIDs as `sid:S-1-...` ACE.Who; well-known SIDs unchanged.
- `internal/adapter/smb/v2/handlers/auth_helper.go`: copies user SID into Identity.
- `pkg/controlplane/models`: User gains SID/GroupSIDs fields (population from PAC/NTLMSSP follow-up).

## Test plan

- [x] go test ./pkg/metadata/acl/...
- [x] go test ./pkg/metadata/...
- [x] go test ./internal/adapter/smb/v2/handlers/...
- [ ] CI smbtorture/memory: no new failures (no test flips expected; this is foundation).
EOF
)"
```

- [ ] **Step 2: Wait for CI green; merge**

```bash
gh pr checks <pr-number> --watch --interval 60
gh pr merge <pr-number> --squash --delete-branch
```

---

# Phase 1 — Wire parent-dir ACL into SMB CREATE

**Why:** With Phase 2 done, deny ACEs can match the requester. But CREATE still doesn't consult the parent-dir DACL because the `ShareWritable` short-circuit grants write before ACL eval, and the SMB-layer `Create` enforces only share-level write rights, not file-level perms.

**Tests this enables:** `delete-on-close-perms.{CREATE, CREATE_IF, OVERWRITE_IF, READONLY, FIND_and_set_DOC}`, `acls.{DENY1, ACCESSBASED, OVERWRITE_READ_ONLY_FILE}` (8 tests).

## File Structure

- Modify `pkg/metadata/auth_permissions.go:252-261` — gate the `ShareWritable` short-circuit on `attr.ACL == nil`.
- Modify `internal/adapter/smb/v2/handlers/create.go` — invoke `metaSvc.CheckParentWriteAccess` (new helper) before `ResolveCreateDisposition` for any non-pure-OPEN disposition.
- Add `pkg/metadata/file_modify.go` (or new file `pkg/metadata/file_create_check.go`): `CheckParentWriteAccess(ctx, parentHandle) error` thin wrapper that calls the existing `checkWritePermission` so SMB can call it without re-deriving the helper path.
- Tests: `pkg/metadata/auth_permissions_acl_short_circuit_test.go` and `internal/adapter/smb/v2/handlers/create_acl_test.go`.

## Tasks (high level — each follows the same TDD/commit cadence shown in P2)

### Task P1-1: Gate `ShareWritable` short-circuit on absence of ACL

**Files:**
- Modify: `pkg/metadata/auth_permissions.go:252-261`
- Test: `pkg/metadata/auth_permissions_acl_short_circuit_test.go` (new)

- [ ] Write a failing test asserting that, for a file with non-nil ACL containing a deny-write ACE for the requester's SID, `getEffectivePermissions` returns 0 even when `ctx.ShareWritable == true`.
- [ ] Update the gate:

```go
if ctx.ShareWritable && !ctx.ShareReadOnly && file.ACL == nil {
    // ...existing block unchanged...
}
```

- [ ] Run test → PASS. Run full `go test ./pkg/metadata/...` for regressions.
- [ ] Commit: `fix(metadata): respect ACL deny on writable shares for ACL-bearing files`.

### Task P1-2: Add `CheckParentWriteAccess` and call it from SMB CREATE

**Files:**
- Modify: `pkg/metadata/file_modify.go` — export a thin wrapper:

```go
// CheckParentWriteAccess verifies the caller may add a child to the given
// directory. Used by protocol layers that need an early permission gate
// before attempting an entry-creating operation, when failure must surface
// as ACCESS_DENIED rather than ALREADY_EXISTS.
func (s *MetadataService) CheckParentWriteAccess(ctx *AuthContext, parentHandle FileHandle) error {
    return s.checkWritePermission(ctx, parentHandle)
}
```

- Modify: `internal/adapter/smb/v2/handlers/create.go` — invoke the helper before the disposition resolves on the existing-file branches (CREATE / CREATE_IF / OVERWRITE_IF). Concretely, after `parentHandle` is resolved and before `ResolveCreateDisposition`, add:

```go
if err := metaSvc.CheckParentWriteAccess(authCtx, parentHandle); err != nil {
    return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(err)}}, nil
}
```

- Test: write a unit test in `internal/adapter/smb/v2/handlers/create_acl_test.go` that registers a parent dir with an ACL denying write, issues a CREATE, and asserts response status is `StatusAccessDenied`. Use the in-memory metadata store (already used by other handler tests).
- Run smbtorture-relevant subset locally where possible; rely on CI for the full smbtorture run.
- Commit: `feat(smb): enforce parent-directory DACL at CREATE`.

### Task P1-3: PR + CI watch + KNOWN_FAILURES walkback (only after CI green)

- [ ] Push, open PR, wait for CI green.
- [ ] Once smbtorture/memory CI confirms test flips, follow-up commit removing the 8 entries from `test/smb-conformance/smbtorture/KNOWN_FAILURES.md`. Push, wait for next CI.
- [ ] Merge.

---

# Phase 5 — MaxAccess from SD

**Why:** `computeMaximalAccess` (`internal/adapter/smb/v2/handlers/create.go:962`) is POSIX-only. MS-SMB2 §2.2.13.2 requires the MxAc context to reflect actual SD evaluation. `acls.MXAC-NOT-GRANTED` and parts of `acls.DYNAMIC` fail because of this.

**Tests this enables:** `acls.MXAC-NOT-GRANTED`, partial `acls.DYNAMIC` (~2 tests).

## Tasks (high level)

### Task P5-1: Replace `computeMaximalAccess` body when ACL is present

**Files:**
- Modify: `internal/adapter/smb/v2/handlers/create.go:962`
- Test: handler-level test exercising MxAc with a deny-write ACE.

- [ ] Write a failing test where a file has an ACL denying `WriteData` for the requester; current `computeMaximalAccess` grants Write via POSIX mode but should clear it.
- [ ] Implement: when `file.ACL != nil`, probe each access bit via `metaSvc.GetEffectivePermissions(authCtx, fileHandle, request)` and return only granted bits. POSIX path stays as the fallback.
- [ ] Run test, run handler tests for regressions.
- [ ] Commit: `feat(smb): MaxAccess returns SD-evaluated rights when ACL present`.

### Task P5-2: PR + CI + walkback

Same cadence as P1-3.

---

# Phase 3 — SD control flags + OWNER_RIGHTS round-trip

**Why:** `ParseSecurityDescriptor` skips the SD Control field (`security.go:372`) and SACL offset; the `seDACLProtected` / `seDACLAutoInherited` bits set by Windows clients are dropped. `OWNER_RIGHTS` (S-1-3-4) is not in the well-known SID switch.

**Tests this enables:** `acls.INHERITFLAGS`, `acls.SDFLAGSVSCHOWN` (2 tests).

## Tasks (high level)

### Task P3-1: Capture SD Control flags during parse

**Files:**
- Modify: `internal/adapter/smb/v2/handlers/security.go:361-410` — read the 16-bit `Control` field from offset 2.
- Modify: `pkg/metadata/acl/types.go` — add `Protected` and `AutoInherited` flags on `ACL` if absent.
- Modify: SD build path — emit the same flags on `BuildSecurityDescriptor`.
- Test: round-trip test in `security_test.go` asserting that an SD with `SE_DACL_PROTECTED` set returns an `ACL` with `Protected = true`.

### Task P3-2: OWNER_RIGHTS principal handling

**Files:**
- Modify: `internal/adapter/smb/v2/handlers/security.go` — well-known SID switch (S-1-3-4 → `OwnerRights@`).
- Modify: `pkg/metadata/acl/types.go` — add `SpecialOwnerRights = "OwnerRights@"`.
- Modify: `pkg/metadata/acl/evaluate.go` — `aceMatchesWho` arm: matches when `evalCtx.UID == evalCtx.FileOwnerUID` AND the ACE is owner-rights-aware (different mask interpretation per MS-DTYP §2.5.3).
- Tests: unit test in evaluate_test.go.

### Task P3-3: PR + CI + walkback

Same cadence.

---

# Phase 4 — CREATOR_OWNER / CREATOR_GROUP substitution at inherit

**Why:** `ComputeInheritedACL` (`pkg/metadata/acl/inherit.go:22-63`) copies parent ACEs verbatim. MS-DTYP §2.5.3.4 requires substituting the `CREATOR_OWNER (S-1-3-0)` / `CREATOR_GROUP (S-1-3-1)` ACEs with the actual creator's identity when inheriting onto a child.

**Tests this enables:** `acls.CREATOR`, `acls.INHERITANCE` (2 tests).

## Tasks (high level)

### Task P4-1: Plumb creator identity into `ComputeInheritedACL`

**Files:**
- Modify: `pkg/metadata/acl/inherit.go:22-63` — accept a `Creator` struct (`UID, GID, SID string`).
- Modify: `pkg/metadata/file_create.go:285-289` — pass the creator from `ctx.Identity`.
- Add `pkg/auth/sid/well_known.go` constants: `CreatorOwnerSID = "S-1-3-0"`, `CreatorGroupSID = "S-1-3-1"`, plus their special-token forms.
- Test: inherit_test.go exercising parent ACL with `CreatorOwner@` ACE → child ACL contains the creator's SID.

### Task P4-2: PR + CI + walkback

Same cadence.

---

# Phase 6 — SD build-path round-trip (rescoped 2026-05-12)

**Original scope obsolete.** Parser-fix #510 already removed canonical-order rejection from `ValidateACL`; canonicalization on parse is no longer needed. The remaining `acls_non_canonical.flags` + `acls.INHERITFLAGS` failures sit on the **build side** of the SD pipeline. P5 CI confirmed plan's expected flips (5 of 5 phases now) are wrong about *which* code path is broken — the gap is serialize/round-trip, not parse.

**Why:**
- `acls_non_canonical.flags` fails because `BuildSecurityDescriptor` (`internal/adapter/smb/v2/handlers/security.go:180-187`) infers `SE_DACL_AUTO_INHERITED` from per-ACE `INHERITED_ACE` flags instead of round-tripping the parsed Control bit. There is no `acl.ACL.AutoInherited` field — P3 only added `Protected`.
- `acls.INHERITFLAGS` fails with "security descriptors don't match" on read-back: `BuildSecurityDescriptor` emits DACL bytes that differ from what the client SET. Likely ACE flag ordering, principal-SID derivation, or missing fields after store round-trip. Needs byte-level diff.

**Tests this enables:** `acls_non_canonical.flags`, `acls.INHERITFLAGS` (2 tests). Note: `acls.DENY1` + `acls.MXAC-NOT-GRANTED` are NOT in P6 — they sit at the eval/Identity layer (tracked in #512), separate investigation.

## Tasks (high level)

### Task P6-1: Persist + re-emit `SE_DACL_AUTO_INHERITED`

**Files:**
- Modify `pkg/metadata/acl/types.go` — add `AutoInherited bool` field on `ACL` alongside `Protected`.
- Modify `internal/adapter/smb/v2/handlers/security.go` parse path — set `acl.ACL.AutoInherited` when the SD Control word has `seDACLAutoInherited` (this is where P3 captured `Protected`; add the sibling capture).
- Modify `internal/adapter/smb/v2/handlers/security.go:180-187` build path — replace the per-ACE inference with a direct read of `fileACL.AutoInherited` (OR'd with the legacy fallback, so files persisted pre-P6 still emit correctly when at least one ACE has `INHERITED_ACE`).
- Test: parse SD with `SE_DACL_AUTO_INHERITED` set but no `INHERITED_ACE` per-ACE flag → re-build → assert Control bit re-emitted. Round-trip equality on full SD.

### Task P6-2: INHERITFLAGS byte-level investigation + fix

**Files:**
- TBD after pcap diff. Likely `buildDACL`, `buildACE`, or `principalToSID` in `internal/adapter/smb/v2/handlers/security.go`.
- Method: capture pcap against `dperson/samba:latest` and DittoFS running the same `acls.INHERITFLAGS` sequence per the CLAUDE.md SMB debugging playbook. Diff the SET_INFO Security request vs the QUERY_INFO Security response bytes.
- Likely findings: ACE flag bits stripped on store, principal-SID re-derivation lossy, or missing inherit-related ACE shapes on re-emit.
- Once root cause known: targeted fix + round-trip unit test asserting SET→GET byte equality for an SD with inheritance flags.

### Task P6-3: PR + CI + walkback

Same cadence as P1-P5. Two-commit walkback: KNOWN_FAILURES.md only after CI confirms.

---

## Self-Review Notes

- Spec coverage: every test listed in the agent investigation is mapped to a phase.
- Placeholder scan: phases P3, P4, P5, P6 use "high level" task summaries (read the file, write the test, implement). Each phase still cites file:line + intent. When the executing agent reaches them they should expand into the same TDD bite-sized cadence shown in P2; spec details are precise enough to do so without ambiguity.
- Type consistency:
  - `EvaluateContext.SID string`, `GroupSIDs []string` (non-pointer slice) — matches Identity shape.
  - `Identity.SID *string` (pointer, may be nil) — matches existing UID/GID pointer convention.
  - `ACE.Who` string-typed throughout; SID-form is the prefix `"sid:"` + canonical SID.
  - `models.User.SID string`, `GroupSIDs []string`.

## Execution Notes

- Start at Pre-flight (file issues), then Phase 2.
- Each phase merges in order. Rebase the next phase on develop after the previous merges (the `gh pr checks --watch` + rebase + force-push-with-lease pattern from the previous SMB session is the reference).
- KNOWN_FAILURES.md walkbacks are follow-up commits within the same PR after CI proves the flip — never speculative.
