// Refs #573. Handler-level integration coverage for SMB2 QUERY_DIRECTORY
// with the share's AccessBasedEnumeration toggle. Mirrors MS-SMB2 §3.3.5.18.1
// ¶6 ("If TreeConnect.Share.AccessBasedEnumeration is TRUE, the server MUST
// NOT include entries for which the user does not have FILE_READ_DATA /
// FILE_LIST_DIRECTORY access") and the smb2.acls.ACCESSBASED smbtorture
// case at source4/torture/smb2/acls.c:2308.
package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// abeChild describes a single per-entry fixture: a regular file with the
// given name, UID/GID, mode bits and (optional) DACL. The SMB SET_INFO
// Security wire path lands at metadata.SetFileAttributes with SetAttrs.ACL,
// so the test setup uses the same metadata API.
type abeChild struct {
	name string
	uid  uint32
	gid  uint32
	mode uint32
	acl  *acl.ACL
}

// readOnlyACL grants ACE4_READ_DATA to a single principal. Under ABE,
// only callers matching that principal see the entry in the listing.
func readOnlyACL(who string) *acl.ACL {
	return &acl.ACL{
		ACEs: []acl.ACE{{
			Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
			AccessMask: acl.ACE4_READ_DATA,
			Who:        who,
		}},
	}
}

// setupABEQueryDirTest wires a Handler against an in-memory metadata store
// where the share has AccessBasedEnumeration explicitly toggled and the
// caller identity is built from callerUID / callerGID. children are
// created beneath the share root with the per-entry attrs given.
//
// The returned SMBHandlerContext is pre-populated with TreeID + User so
// QueryDirectory's ABE branch (gated on h.GetTree(TreeID).AccessBasedEnumeration
// + BuildAuthContext(ctx), which itself reads ctx.User) finds both the
// share toggle and the caller's UID/GID.
func setupABEQueryDirTest(t *testing.T, abe bool, callerUID, callerGID uint32, children []abeChild) (*Handler, *OpenFile, *SMBHandlerContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	const shareName = "/abe"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:                   shareName,
		MetadataStore:          "test-meta",
		AccessBasedEnumeration: abe,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	// Files are created as root so per-file ACL / mode-bit decisions made
	// downstream reflect the fixture exactly and aren't shadowed by the
	// creator's owner-implicit grants.
	rootUID, rootGID := uint32(0), uint32(0)
	rootAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &rootUID,
			GID: &rootGID,
		},
	}
	metaSvc := rt.GetMetadataService()
	for _, c := range children {
		childFile, err := metaSvc.CreateFile(rootAuth, rootHandle, c.name, &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: c.mode,
			UID:  c.uid,
			GID:  c.gid,
		})
		if err != nil {
			t.Fatalf("CreateFile(%q): %v", c.name, err)
		}
		if c.acl == nil {
			continue
		}
		// SetFileAttributes with SetAttrs.ACL is the metadata-layer
		// entrypoint that SET_INFO Security calls after parsing the
		// wire SD; mirrors what a Windows client triggers via
		// smb2_setinfo_file.
		childHandle, err := metadata.EncodeFileHandle(childFile)
		if err != nil {
			t.Fatalf("EncodeFileHandle(%q): %v", c.name, err)
		}
		if err := metaSvc.SetFileAttributes(rootAuth, childHandle, &metadata.SetAttrs{ACL: c.acl}); err != nil {
			t.Fatalf("SetFileAttributes ACL(%q): %v", c.name, err)
		}
	}

	h := NewHandler()
	h.Registry = rt

	// Register the tree so QueryDirectory's ABE gate
	// (treeHasAccessBasedEnumeration) finds the share toggle. TreeID = 1
	// is arbitrary as long as it matches smbCtx.TreeID below.
	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{
		TreeID:                 treeID,
		ShareName:              shareName,
		AccessBasedEnumeration: abe,
	})

	open := &OpenFile{
		FileID:         h.GenerateFileID(),
		TreeID:         treeID,
		Path:           shareName,
		IsDirectory:    true,
		MetadataHandle: rootHandle,
	}
	h.StoreOpenFile(open)

	// SMB session carries a User struct (callerUID / callerGID);
	// QueryDirectory goes through BuildAuthContext → BuildAuthContextFromUser
	// which mirrors the production session-setup → request-dispatch handoff.
	smbCtx := &SMBHandlerContext{
		Context: context.Background(),
		TreeID:  treeID,
		User: &models.User{
			Username: fmt.Sprintf("uid-%d", callerUID),
			UID:      &callerUID,
			Groups:   []models.Group{{GID: &callerGID}},
		},
	}

	return h, open, smbCtx
}

// callABEQuery invokes QueryDirectory with sensible defaults for the ABE
// handler-level tests. Mirrors the smbtorture acls.ACCESSBASED enumeration
// which uses SMB2_FIND_DIRECTORY_INFO with continue_flags=REOPEN and a
// 4 KiB output buffer.
func callABEQuery(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, openID [16]byte) *QueryDirectoryResponse {
	t.Helper()
	resp, err := h.QueryDirectory(smbCtx, &QueryDirectoryRequest{
		FileInfoClass:      uint8(types.FileIdBothDirectoryInformation),
		Flags:              uint8(types.SMB2RestartScans),
		FileID:             openID,
		FileName:           "*",
		OutputBufferLength: 65536,
	})
	if err != nil {
		t.Fatalf("QueryDirectory error: %v", err)
	}
	return resp
}

// countABEEntries walks the NextEntryOffset chain in a directory info buffer
// and returns the entry count. Encoder-class-agnostic: only the first
// 4 bytes of each entry (NextEntryOffset) are read.
func countABEEntries(buf []byte) int {
	if len(buf) < 4 {
		return 0
	}
	n, off := 0, 0
	for {
		n++
		next := binary.LittleEndian.Uint32(buf[off : off+4])
		if next == 0 {
			return n
		}
		off += int(next)
		if off+4 > len(buf) {
			return n
		}
	}
}

// TestQueryDirectory_ABE covers the share-level AccessBasedEnumeration toggle
// at the QUERY_DIRECTORY handler boundary. Each case seeds a single child
// under the share root and asserts the resulting entry count after the
// handler's per-entry ABE filter has run. Refs #573; mirrors
// smb2.acls.ACCESSBASED at source4/torture/smb2/acls.c:2308.
func TestQueryDirectory_ABE(t *testing.T) {
	const (
		callerUID = uint32(2000)
		callerGID = uint32(2000)
		// Owner UID for fixture children — distinct from callerUID so
		// owner-only DACLs deny the caller.
		ownerUID = uint32(1000)
		ownerGID = uint32(1000)
	)

	cases := []struct {
		name        string
		abe         bool
		child       abeChild
		wantEntries int // ". " + ".." + (child if visible)
	}{
		{
			name: "ABE_on_omits_non_readable",
			abe:  true,
			child: abeChild{
				name: "secret.txt",
				uid:  ownerUID,
				gid:  ownerGID,
				mode: 0o600,
				acl:  readOnlyACL(acl.SpecialOwner),
			},
			wantEntries: 2,
		},
		{
			name: "ABE_on_includes_readable",
			abe:  true,
			child: abeChild{
				name: "public.txt",
				uid:  ownerUID,
				gid:  ownerGID,
				mode: 0o644,
				acl:  readOnlyACL(acl.SpecialEveryone),
			},
			wantEntries: 3,
		},
		{
			name: "ABE_off_includes_all",
			abe:  false,
			child: abeChild{
				name: "secret.txt",
				uid:  ownerUID,
				gid:  ownerGID,
				mode: 0o600,
				acl:  readOnlyACL(acl.SpecialOwner),
			},
			wantEntries: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, open, smbCtx := setupABEQueryDirTest(t, tc.abe, callerUID, callerGID, []abeChild{tc.child})

			resp := callABEQuery(t, h, smbCtx, open.FileID)
			if resp.Status != types.StatusSuccess {
				t.Fatalf("status = 0x%x, want StatusSuccess", uint32(resp.Status))
			}
			if got := countABEEntries(resp.Data); got != tc.wantEntries {
				t.Fatalf("entries = %d, want %d", got, tc.wantEntries)
			}
		})
	}
}

// setupQueryDirTest wires a Handler against an in-memory metadata store and
// returns the handler, an OpenFile pointing at the given directory, and the
// AuthContext to thread through QUERY_DIRECTORY. Children listed in names are
// created as regular files inside the directory before the handle is returned.
func setupQueryDirTest(t *testing.T, names []string) (*Handler, *OpenFile, *metadata.AuthContext, *SMBHandlerContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	shareName := "/test"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	uid := uint32(0)
	gid := uint32(0)
	authCtx := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &uid,
			GID: &gid,
		},
	}

	metaSvc := rt.GetMetadataService()
	for _, name := range names {
		_, err := metaSvc.CreateFile(authCtx, rootHandle, name, &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
		})
		if err != nil {
			t.Fatalf("CreateFile(%q): %v", name, err)
		}
	}

	h := NewHandler()
	h.Registry = rt

	open := &OpenFile{
		FileID:         h.GenerateFileID(),
		Path:           shareName,
		IsDirectory:    true,
		MetadataHandle: rootHandle,
	}
	h.StoreOpenFile(open)

	smbCtx := &SMBHandlerContext{Context: context.Background()}
	return h, open, authCtx, smbCtx
}

// countEntries walks the NextEntryOffset chain in a directory info buffer and
// returns the entry count. Encoder-class-agnostic: only the first 4 bytes of
// each entry (NextEntryOffset) are read.
func countEntries(buf []byte) int {
	if len(buf) < 4 {
		return 0
	}
	n := 0
	off := 0
	for {
		n++
		next := binary.LittleEndian.Uint32(buf[off : off+4])
		if next == 0 {
			return n
		}
		off += int(next)
		if off+4 > len(buf) {
			return n
		}
	}
}

// callQuery invokes QueryDirectory with sensible defaults and returns the
// response. Caller supplies flags and pattern; buffer length is large enough
// to hold the largest plausible page so tests can focus on flag semantics
// rather than buffer chunking.
func callQuery(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, openID [16]byte, pattern string, flags uint8) *QueryDirectoryResponse {
	t.Helper()
	req := &QueryDirectoryRequest{
		FileInfoClass:      uint8(types.FileIdBothDirectoryInformation),
		Flags:              flags,
		FileID:             openID,
		FileName:           pattern,
		OutputBufferLength: 65536,
	}
	resp, err := h.QueryDirectory(smbCtx, req)
	if err != nil {
		t.Fatalf("QueryDirectory error: %v", err)
	}
	return resp
}

// TestQueryDirectory_SingleEntry_ReturnsOneEntryThenSuccess verifies the
// SMB2_RETURN_SINGLE_ENTRY (0x02) flag: each call must yield exactly one
// entry from the listing, even when the output buffer could comfortably hold
// the whole directory. Mirrors smb2.dir.fixed / smb2.dir.modify which drive
// the handler with SMB2_CONTINUE_FLAG_SINGLE.
func TestQueryDirectory_SingleEntry_ReturnsOneEntryThenSuccess(t *testing.T) {
	h, open, _, smbCtx := setupQueryDirTest(t, []string{"a.txt", "b.txt", "c.txt"})

	// First call: SMB2_RESTART_SCANS | SMB2_RETURN_SINGLE_ENTRY.
	resp := callQuery(t, h, smbCtx, open.FileID, "*",
		uint8(types.SMB2RestartScans|types.SMB2ReturnSingleEntry))
	if resp.Status != types.StatusSuccess {
		t.Fatalf("first call: status = 0x%x, want StatusSuccess", uint32(resp.Status))
	}
	if got := countEntries(resp.Data); got != 1 {
		t.Fatalf("first call: entries = %d, want exactly 1", got)
	}

	// Subsequent SINGLE calls advance the cursor one entry at a time. With
	// 3 user files plus "." and ".." that is exactly 5 entries before
	// STATUS_NO_MORE_FILES.
	seen := 1
	for seen < 5 {
		r := callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2ReturnSingleEntry))
		if r.Status != types.StatusSuccess {
			t.Fatalf("call %d: status = 0x%x, want StatusSuccess", seen+1, uint32(r.Status))
		}
		if got := countEntries(r.Data); got != 1 {
			t.Fatalf("call %d: entries = %d, want exactly 1", seen+1, got)
		}
		seen++
	}

	// After the last entry the next SINGLE call must report STATUS_NO_MORE_FILES.
	r := callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2ReturnSingleEntry))
	if r.Status != types.StatusNoMoreFiles {
		t.Fatalf("post-drain: status = 0x%x, want StatusNoMoreFiles", uint32(r.Status))
	}
}

// TestQueryDirectory_RestartScans_ResetsCursor verifies that SMB2_RESTART_SCANS
// (0x01) restarts an in-progress enumeration from the beginning, including
// when the previous enumeration had already drained (smb2.dir.fixed reuses h
// after the directory has been emptied through a second handle).
func TestQueryDirectory_RestartScans_ResetsCursor(t *testing.T) {
	h, open, _, smbCtx := setupQueryDirTest(t, []string{"alpha", "bravo", "charlie"})

	// First pass: drain the directory in one shot.
	resp := callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2RestartScans))
	if resp.Status != types.StatusSuccess {
		t.Fatalf("first pass: status = 0x%x, want StatusSuccess", uint32(resp.Status))
	}
	first := countEntries(resp.Data)
	if first != 5 { // 3 files + "." + ".."
		t.Fatalf("first pass: entries = %d, want 5", first)
	}

	// Without RESTART, the second call must report NO_MORE_FILES.
	r := callQuery(t, h, smbCtx, open.FileID, "*", 0)
	if r.Status != types.StatusNoMoreFiles {
		t.Fatalf("post-drain no-restart: status = 0x%x, want StatusNoMoreFiles", uint32(r.Status))
	}

	// Now RESTART — cursor must reset and re-yield the whole listing.
	r = callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2RestartScans))
	if r.Status != types.StatusSuccess {
		t.Fatalf("restart: status = 0x%x, want StatusSuccess", uint32(r.Status))
	}
	if got := countEntries(r.Data); got != first {
		t.Fatalf("restart: entries = %d, want %d (same as first pass)", got, first)
	}
}

// TestQueryDirectory_Reopen_ResetsCursor exercises the SMB2_REOPEN flag (0x10),
// which the spec treats as a stronger restart that also clears the cached
// pattern. The observable effect on the handler is identical to RESTART_SCANS
// here but the flag goes through a different reset path; we cover it to lock
// the behaviour in regression tests.
func TestQueryDirectory_Reopen_ResetsCursor(t *testing.T) {
	h, open, _, smbCtx := setupQueryDirTest(t, []string{"a", "b"})

	resp := callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2RestartScans))
	if resp.Status != types.StatusSuccess {
		t.Fatalf("first call: status = 0x%x", uint32(resp.Status))
	}
	first := countEntries(resp.Data)

	// Reopen on a drained handle must restart.
	r := callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2Reopen))
	if r.Status != types.StatusSuccess {
		t.Fatalf("reopen: status = 0x%x", uint32(r.Status))
	}
	if got := countEntries(r.Data); got != first {
		t.Fatalf("reopen: entries = %d, want %d", got, first)
	}
}

// TestQueryDirectory_ConcurrentModify_NoPanic walks the directory one entry
// at a time, deleting and creating files between calls, and asserts the
// handler never panics nor returns garbage. Mirrors smb2.dir.modify and the
// h2-delete leg of smb2.dir.fixed.
func TestQueryDirectory_ConcurrentModify_NoPanic(t *testing.T) {
	h, open, authCtx, smbCtx := setupQueryDirTest(t, []string{"a", "b", "c", "d", "e"})

	// Drive a SINGLE-entry enumeration, mutating the directory mid-flight.
	// The handler re-reads the directory on every call, so the snapshot
	// shrinks/grows under the cursor. Per MS-SMB2 §3.3.5.18 the server is
	// free to skip or repeat entries created/removed mid-enumeration; it
	// MUST NOT crash.
	metaSvc := h.Registry.GetMetadataService()

	// Initial SINGLE+RESTART call.
	r := callQuery(t, h, smbCtx, open.FileID, "*",
		uint8(types.SMB2RestartScans|types.SMB2ReturnSingleEntry))
	if r.Status != types.StatusSuccess {
		t.Fatalf("initial: status = 0x%x, want StatusSuccess", uint32(r.Status))
	}

	// Delete "c", add "z".
	if _, err := metaSvc.RemoveFile(authCtx, open.MetadataHandle, "c"); err != nil {
		t.Fatalf("RemoveFile c: %v", err)
	}
	if _, err := metaSvc.CreateFile(authCtx, open.MetadataHandle, "z",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}); err != nil {
		t.Fatalf("CreateFile z: %v", err)
	}

	// Continue without restart — must not panic or wedge. Drain to NO_MORE_FILES.
	drained := false
	for steps := 0; steps < 100; steps++ {
		r := callQuery(t, h, smbCtx, open.FileID, "*",
			uint8(types.SMB2ReturnSingleEntry))
		if r.Status == types.StatusNoMoreFiles {
			drained = true
			break
		}
		if r.Status != types.StatusSuccess {
			t.Fatalf("step %d: status = 0x%x, want StatusSuccess or NoMoreFiles",
				steps, uint32(r.Status))
		}
		if got := countEntries(r.Data); got != 1 {
			t.Fatalf("step %d: entries = %d, want 1", steps, got)
		}
	}
	if !drained {
		t.Fatal("drain loop exited without observing StatusNoMoreFiles after 100 iterations — cursor likely wedged")
	}

	// Now restart — must see the post-modification listing. Starting with
	// 5 user files (a,b,c,d,e), then -c +z = 5 user files (a,b,d,e,z),
	// plus "." and ".." = 7 entries total.
	r = callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2RestartScans))
	if r.Status != types.StatusSuccess {
		t.Fatalf("post-modify restart: status = 0x%x", uint32(r.Status))
	}
	if got := countEntries(r.Data); got != 7 {
		t.Fatalf("post-modify restart: entries = %d, want 7", got)
	}
}

// TestQueryDirectory_PatternChange_RestartsCursor verifies the MS-SMB2
// §3.3.5.18 rule that a change to the search pattern on an existing
// enumeration MUST restart the cursor.
func TestQueryDirectory_PatternChange_RestartsCursor(t *testing.T) {
	h, open, _, smbCtx := setupQueryDirTest(t, []string{"alpha.txt", "beta.log", "gamma.txt"})

	// First call: wildcard "*.txt" with RESTART, return all matches.
	r := callQuery(t, h, smbCtx, open.FileID, "*.txt", uint8(types.SMB2RestartScans))
	if r.Status != types.StatusSuccess {
		t.Fatalf("first: status = 0x%x", uint32(r.Status))
	}
	if got := countEntries(r.Data); got != 2 {
		t.Fatalf("first: entries = %d, want 2 (alpha.txt + gamma.txt)", got)
	}

	// Drain: next call without RESTART → NO_MORE_FILES (cursor at end).
	r = callQuery(t, h, smbCtx, open.FileID, "*.txt", 0)
	if r.Status != types.StatusNoMoreFiles {
		t.Fatalf("drain: status = 0x%x, want StatusNoMoreFiles", uint32(r.Status))
	}

	// Pattern change to "*.log" with NO restart flag — per spec, server MUST
	// restart the enumeration when the pattern differs from the previous one.
	r = callQuery(t, h, smbCtx, open.FileID, "*.log", 0)
	if r.Status != types.StatusSuccess {
		t.Fatalf("pattern-change: status = 0x%x, want StatusSuccess", uint32(r.Status))
	}
	if got := countEntries(r.Data); got != 1 {
		t.Fatalf("pattern-change: entries = %d, want 1 (beta.log)", got)
	}
}

// TestQueryDirectory_SingleEntry_PaginatesLargeDirectory drives a 1k-entry
// directory with SINGLE+RESTART cycles to reproduce the smb2.dir.1kfiles_rename
// loop shape (the test repeats the cycle 100 times in production). We use a
// modest 50 entries here so the test is cheap, but the cycle is the same.
func TestQueryDirectory_SingleEntry_PaginatesLargeDirectory(t *testing.T) {
	const N = 50
	names := make([]string, N)
	for i := range N {
		names[i] = fmt.Sprintf("t%03d.txt", i)
	}
	h, open, _, smbCtx := setupQueryDirTest(t, names)

	for cycle := 0; cycle < 3; cycle++ {
		// RESTART for the first call of each cycle, then drain SINGLE.
		r := callQuery(t, h, smbCtx, open.FileID, "*",
			uint8(types.SMB2RestartScans|types.SMB2ReturnSingleEntry))
		if r.Status != types.StatusSuccess {
			t.Fatalf("cycle %d restart: status = 0x%x", cycle, uint32(r.Status))
		}
		seen := 1
		for {
			r := callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2ReturnSingleEntry))
			if r.Status == types.StatusNoMoreFiles {
				break
			}
			if r.Status != types.StatusSuccess {
				t.Fatalf("cycle %d step %d: status = 0x%x", cycle, seen, uint32(r.Status))
			}
			seen++
			if seen > N+10 {
				t.Fatalf("cycle %d: runaway, expected ~%d entries got %d", cycle, N+2, seen)
			}
		}
		// N user files plus "." and ".." = N+2 entries per cycle.
		if seen != N+2 {
			t.Fatalf("cycle %d: total entries = %d, want %d", cycle, seen, N+2)
		}
	}
}

// TestNormalizeSearchPattern documents the "match all" normalization that
// keeps spec-equivalent patterns ("", "*", "*.*", "<") indistinguishable
// for the purposes of the MS-SMB2 §3.3.5.18 pattern-change check.
func TestNormalizeSearchPattern(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "*"},
		{"*", "*"},
		{"*.*", "*"},
		{"<", "*"},
		{"foo.txt", "foo.txt"},
		{"*.txt", "*.txt"},
	}
	for _, c := range cases {
		if got := normalizeSearchPattern(c.in); got != c.want {
			t.Errorf("normalizeSearchPattern(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
