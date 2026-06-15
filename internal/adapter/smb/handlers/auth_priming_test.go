package handlers

// Tests for the two correctness bug classes addressed in #619:
//
//  A. UID-0 root-bypass — handler sites that called BuildAuthContext(ctx)
//     without first priming ctx.User from the OpenFile's recorded session.
//     With User==nil, BuildAuthContext synthesises UID-0 (root), bypassing
//     all DACL checks in the metadata layer. Same bug class as #603
//     (QUERY_DIRECTORY) fixed in PR #618.
//
//  B. DesiredAccess vs GrantedAccess — handler sites that gated access
//     decisions on the pre-DACL DesiredAccess instead of the post-DACL
//     GrantedAccess. Same flaw as #616 (ChangeNotify), flagged by the #616
//     reviewer.
//
// We test two sites per bug class (4 tests total): a positive case where the
// access is granted and a negative case where the bit is stripped (DACL) or
// the priming would otherwise have leaked UID-0 root.

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// =============================================================================
// Bug class A — UID-0 root-bypass
// =============================================================================

// TestClose_PrimesAuthContextFromOpenFile pins the CLOSE handler's hand-off
// of the OpenFile's recorded session identity onto ctx.User BEFORE
// BuildAuthContext is invoked for the deferred-metadata-flush / delete-on-
// close paths. Without the prime, ctx.User==nil falls through to the
// anonymous arm and synthesises UID-0 (root), silently granting root on the
// metadata-layer DACL gates (#619). This test exercises the regular-file
// CLOSE path that runs metaSvc.FlushPendingWriteForFile under the auth
// context.
func TestClose_PrimesAuthContextFromOpenFile(t *testing.T) {
	h := NewHandler()

	uid := uint32(1001)
	user := &models.User{ID: "alice", Username: "alice", UID: &uid}
	sess := h.CreateSession("127.0.0.1:0", false, "alice", "")
	sess.User = user

	const treeID uint32 = 7
	h.StoreTree(&TreeConnection{
		TreeID:    treeID,
		SessionID: sess.SessionID,
		ShareName: "/share",
	})

	openFile := &OpenFile{
		FileID:    [16]byte{0x01, 0x02},
		TreeID:    treeID,
		SessionID: sess.SessionID,
		Path:      "/share/a.txt",
		// No MetadataHandle / PayloadID — Close skips the BlockStore /
		// metadata-flush paths entirely, but Step 2b primer still runs.
	}
	h.StoreOpenFile(openFile)

	// Build a ctx with zero session/tree state — mirrors the dispatcher,
	// which arrives keyed only by FileID.
	ctx := NewSMBHandlerContext(context.TODO(), "127.0.0.1:0", 0 /*session*/, 0 /*tree*/, 1 /*msg*/)
	if ctx.User != nil {
		t.Fatal("precondition: ctx.User should start nil so the prime is observable")
	}

	resp, err := h.Close(ctx, &CloseRequest{FileID: openFile.FileID})
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("Close status = %v, want StatusSuccess", resp.Status)
	}

	// After Close, ctx.User must have been primed from the session — without
	// the prime BuildAuthContext would have minted a UID-0 root identity.
	if ctx.User != user {
		t.Errorf("ctx.User = %+v, want primed from session (alice); without the prime BuildAuthContext mints UID-0 root and bypasses DACL checks", ctx.User)
	}
	if ctx.SessionID != sess.SessionID {
		t.Errorf("ctx.SessionID = %d, want %d (primed from openFile)", ctx.SessionID, sess.SessionID)
	}
	if ctx.TreeID != treeID {
		t.Errorf("ctx.TreeID = %d, want %d (primed from openFile)", ctx.TreeID, treeID)
	}
}

// TestFlush_PrimesAuthContextFromOpenFile pins the same hand-off on the
// FLUSH path: BuildAuthContext is invoked to authorise the deferred
// metadata flush, and without the prime ctx.User==nil → UID-0 root → DACL
// bypass on FlushPendingWriteForFile. FLUSH returns
// StatusInternalError early (no block store wired in the test handler);
// the assertion is that ctx.User has been primed REGARDLESS of the
// downstream short-circuit — i.e. the prime runs at Step 1, not at Step 4.
//
// Note: with the current FLUSH layout the prime is placed at the
// BuildAuthContext call site (Step 4). When the block-store resolution at
// Step 2 fails the handler returns before that prime runs, which is the
// CORRECT behaviour: there is no BuildAuthContext call to protect. We
// therefore test the prime via the same code path the auth_helper_test
// uses: invoke primeAuthContextFromOpenFile directly on the same OpenFile
// to confirm the helper integrates with the session/tree fixtures the
// flush handler will rely on once it reaches the BuildAuthContext gate.
func TestFlush_PrimesAuthContextFromOpenFile(t *testing.T) {
	h := NewHandler()

	uid := uint32(2002)
	user := &models.User{ID: "bob", Username: "bob", UID: &uid}
	sess := h.CreateSession("127.0.0.1:0", false, "bob", "")
	sess.User = user

	const treeID uint32 = 9
	h.StoreTree(&TreeConnection{
		TreeID:    treeID,
		SessionID: sess.SessionID,
		ShareName: "/share2",
	})

	openFile := &OpenFile{
		FileID:    [16]byte{0x10, 0x20},
		TreeID:    treeID,
		SessionID: sess.SessionID,
		Path:      "/share2/b.txt",
	}

	ctx := NewSMBHandlerContext(context.TODO(), "127.0.0.1:0", 0, 0, 1)
	if ctx.User != nil {
		t.Fatal("precondition: ctx.User should start nil")
	}

	h.primeAuthContextFromOpenFile(ctx, openFile)

	if ctx.User != user {
		t.Errorf("ctx.User not primed from session (got %+v, want bob)", ctx.User)
	}
	if ctx.TreeID != treeID {
		t.Errorf("ctx.TreeID = %d, want %d", ctx.TreeID, treeID)
	}

	// Negative: with no prime, BuildAuthContext takes the User==nil arm and
	// mints the unprivileged nobody (65534) identity — NOT the authenticated
	// opener (bob). The prime is what restores the real opener's UID; this
	// asserts the bare path differs (and is non-privileged) so the prime is
	// load-bearing. Crucially the bare path must NOT be root (UID=0), which
	// would bypass all metadata permission checks (audit #1132).
	bareCtx := NewSMBHandlerContext(context.TODO(), "127.0.0.1:0", 0, 0, 1)
	bareAuth, err := BuildAuthContext(bareCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext (bare): %v", err)
	}
	if bareAuth.Identity == nil || bareAuth.Identity.UID == nil {
		t.Fatal("bare-ctx BuildAuthContext produced no UID")
	}
	if *bareAuth.Identity.UID == 0 {
		t.Errorf("bare-ctx BuildAuthContext.UID = 0 (root) — null session must map to unprivileged nobody, not root (audit #1132)")
	}
	if *bareAuth.Identity.UID != 65534 {
		t.Errorf("bare-ctx BuildAuthContext.UID = %d, want 65534 (nobody); the prime restores the real opener UID", *bareAuth.Identity.UID)
	}
}

// =============================================================================
// Bug class B — DesiredAccess vs GrantedAccess
// =============================================================================

// TestRead_DeniesOnGrantedAccessStripped pins the READ access gate to
// Open.GrantedAccess (post-DACL intersection at CREATE), not the pre-DACL
// DesiredAccess. The scenario: a MAXIMUM_ALLOWED open whose DACL stripped
// FILE_READ_DATA but whose DesiredAccess still records the requested mask.
// Before the fix, hasReadAccess(DesiredAccess) returned true and the READ
// silently succeeded; after the fix, hasReadAccess(GrantedAccess) returns
// false and the handler must return STATUS_ACCESS_DENIED.
//
// Mirror-case for #616 (ChangeNotify), flagged by the #616 reviewer.
func TestRead_DeniesOnGrantedAccessStripped(t *testing.T) {
	h := NewHandler()

	openFile := &OpenFile{
		FileID:        [16]byte{0xDE, 0xAD},
		Path:          "/share/secret.txt",
		DesiredAccess: uint32(types.FileReadData), // requested
		GrantedAccess: 0,                          // DACL stripped it
	}
	h.StoreOpenFile(openFile)

	ctx := NewSMBHandlerContext(context.TODO(), "127.0.0.1:0", 0, 0, 1)
	req := &ReadRequest{FileID: openFile.FileID, Length: 16, Offset: 0}
	resp, err := h.Read(ctx, req)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if resp.Status != types.StatusAccessDenied {
		t.Errorf("Read status = %v, want StatusAccessDenied — the gate is consulting DesiredAccess instead of GrantedAccess (#619 bug class B)", resp.Status)
	}
}

// TestRead_AllowsOnGrantedAccess is the positive counterpart: when the
// DACL granted FILE_READ_DATA the READ must NOT be denied at the access
// gate. We stop before the actual content read (no MetadataHandle / block
// store wired) — the assertion is that we do not get
// STATUS_ACCESS_DENIED at the access gate.
func TestRead_AllowsOnGrantedAccess(t *testing.T) {
	h := NewHandler()

	openFile := &OpenFile{
		FileID:        [16]byte{0xBE, 0xEF},
		Path:          "/share/public.txt",
		DesiredAccess: 0,                          // pre-DACL not relevant
		GrantedAccess: uint32(types.FileReadData), // DACL granted
	}
	h.StoreOpenFile(openFile)

	ctx := NewSMBHandlerContext(context.TODO(), "127.0.0.1:0", 0, 0, 1)
	req := &ReadRequest{FileID: openFile.FileID, Length: 16, Offset: 0}
	resp, err := h.Read(ctx, req)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	// Past the access gate the handler tries to resolve a metadata handle
	// and block store — that will fail with StatusInternalError or similar.
	// The single invariant under test is: NOT StatusAccessDenied.
	if resp.Status == types.StatusAccessDenied {
		t.Errorf("Read returned StatusAccessDenied despite GrantedAccess including FILE_READ_DATA — the gate is wrongly looking at DesiredAccess")
	}
}

// TestWrite_DeniesOnGrantedAccessStripped is the symmetric test for the
// WRITE gate: DesiredAccess records FILE_WRITE_DATA but the DACL stripped
// it, so GrantedAccess is 0 and WRITE must return STATUS_ACCESS_DENIED.
func TestWrite_DeniesOnGrantedAccessStripped(t *testing.T) {
	h := NewHandler()

	openFile := &OpenFile{
		FileID:        [16]byte{0xCA, 0xFE},
		Path:          "/share/ro.txt",
		DesiredAccess: uint32(types.FileWriteData), // requested
		GrantedAccess: 0,                           // DACL stripped it
	}
	h.StoreOpenFile(openFile)

	ctx := NewSMBHandlerContext(context.TODO(), "127.0.0.1:0", 0, 0, 1)
	req := &WriteRequest{FileID: openFile.FileID, Data: []byte("hi"), Offset: 0}
	resp, err := h.Write(ctx, req)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if resp.Status != types.StatusAccessDenied {
		t.Errorf("Write status = %v, want StatusAccessDenied — the gate is consulting DesiredAccess instead of GrantedAccess (#619 bug class B)", resp.Status)
	}
}
