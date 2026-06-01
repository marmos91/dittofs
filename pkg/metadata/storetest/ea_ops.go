package storetest

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runEAOpsTests asserts cross-backend parity for FileAttr.EAs: PutFile/GetFile
// round-trip of an EA map, case-insensitive name resolution with set-case
// preservation, zero-length values, deletion, persistence across reads, and
// deep-copy (aliasing) discipline on both the store-from-caller and
// return-to-caller paths.
//
// EAs (SMB FILE_FULL_EA_INFORMATION, MS-FSCC §2.4.15) ride on FileAttr the
// same way ACL does. The memory backend holds the map directly and must
// deep-copy it; Badger/Postgres round-trip through JSON. This suite pins that
// behaviour identically for every backend.
func runEAOpsTests(t *testing.T, factory StoreFactory) {
	t.Run("RoundTrip", func(t *testing.T) { testEARoundTrip(t, factory) })
	t.Run("ZeroLengthValue", func(t *testing.T) { testEAZeroLengthValue(t, factory) })
	t.Run("Delete", func(t *testing.T) { testEADelete(t, factory) })
	t.Run("CaseInsensitiveNamePreservesCase", func(t *testing.T) { testEACaseInsensitive(t, factory) })
	t.Run("PutDoesNotAliasCallerEAs", func(t *testing.T) { testEAPutNoAlias(t, factory) })
	t.Run("GetDoesNotAliasStoredEAs", func(t *testing.T) { testEAGetNoAlias(t, factory) })
	t.Run("PersistsAcrossReads", func(t *testing.T) { testEAPersistsAcrossReads(t, factory) })
}

// putFileWithEAs sets the supplied EA map on the file and stores it.
func putFileWithEAs(t *testing.T, store metadata.MetadataStore, handle metadata.FileHandle, eas map[string][]byte) {
	t.Helper()
	ctx := t.Context()
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	file.EAs = eas
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile with EAs: %v", err)
	}
}

// getEAs returns the stored EA map for a handle.
func getEAs(t *testing.T, store metadata.MetadataStore, handle metadata.FileHandle) map[string][]byte {
	t.Helper()
	file, err := store.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	return file.EAs
}

func testEARoundTrip(t *testing.T, factory StoreFactory) {
	store := factory(t)
	root := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", root, "ea-roundtrip.txt", 0o600)

	want := map[string][]byte{
		"EAONE":    []byte("first"),
		"SECONDEA": []byte("second value"),
		"NewEA":    []byte("testme"),
	}
	putFileWithEAs(t, store, handle, want)

	got := getEAs(t, store, handle)
	if len(got) != len(want) {
		t.Fatalf("EA count = %d, want %d (%v)", len(got), len(want), got)
	}
	for name, val := range want {
		gv, ok := got[name]
		if !ok {
			t.Fatalf("EA %q missing after round-trip", name)
		}
		if !bytes.Equal(gv, val) {
			t.Fatalf("EA %q = %q, want %q", name, gv, val)
		}
	}
}

func testEAZeroLengthValue(t *testing.T, factory StoreFactory) {
	store := factory(t)
	root := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", root, "ea-zero.txt", 0o600)

	putFileWithEAs(t, store, handle, map[string][]byte{
		"ZeroEA": {},
		"DataEA": []byte("x"),
	})

	got := getEAs(t, store, handle)
	zv, ok := got["ZeroEA"]
	if !ok {
		t.Fatalf("zero-length EA absent: a zero-length EA must round-trip as present, not deleted")
	}
	if len(zv) != 0 {
		t.Fatalf("ZeroEA value = %q, want empty", zv)
	}
}

func testEADelete(t *testing.T, factory StoreFactory) {
	store := factory(t)
	root := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", root, "ea-delete.txt", 0o600)

	putFileWithEAs(t, store, handle, map[string][]byte{
		"KEEP": []byte("keep"),
		"DROP": []byte("drop"),
	})

	// Drop one EA by re-putting the surviving map.
	putFileWithEAs(t, store, handle, map[string][]byte{
		"KEEP": []byte("keep"),
	})

	got := getEAs(t, store, handle)
	if _, ok := got["DROP"]; ok {
		t.Fatalf("DROP EA still present after deletion")
	}
	if _, ok := got["KEEP"]; !ok {
		t.Fatalf("KEEP EA lost during deletion of a sibling")
	}
}

// testEACaseInsensitive verifies the FileAttr helpers resolve EA names case-
// insensitively while preserving the casing the EA was stored under.
func testEACaseInsensitive(t *testing.T, factory StoreFactory) {
	store := factory(t)
	root := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", root, "ea-case.txt", 0o600)

	putFileWithEAs(t, store, handle, map[string][]byte{"MixedCaseEA": []byte("v")})

	file, err := store.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	for _, probe := range []string{"MixedCaseEA", "mixedcaseea", "MIXEDCASEEA"} {
		v, ok := file.LookupEA(probe)
		if !ok {
			t.Fatalf("LookupEA(%q) miss — names must resolve case-insensitively", probe)
		}
		if !bytes.Equal(v, []byte("v")) {
			t.Fatalf("LookupEA(%q) = %q, want %q", probe, v, "v")
		}
	}

	// ApplyEAMutations upsert under a different casing must update in place,
	// not create a second entry, and preserve the original stored casing.
	file.ApplyEAMutations([]metadata.EAMutation{{Name: "MIXEDCASEEA", Value: []byte("v2")}})
	if len(file.EAs) != 1 {
		t.Fatalf("case-different upsert created a duplicate entry: %v", file.EAs)
	}
	if _, ok := file.EAs["MixedCaseEA"]; !ok {
		t.Fatalf("case-different upsert did not preserve original casing: %v", file.EAs)
	}
}

func testEAPutNoAlias(t *testing.T, factory StoreFactory) {
	store := factory(t)
	root := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", root, "ea-put-alias.txt", 0o600)

	caller := map[string][]byte{"NewEA": []byte("testme")}
	putFileWithEAs(t, store, handle, caller)

	// Mutate the caller's map and its value bytes AFTER the store accepted it.
	caller["NewEA"][0] = 'X'
	caller["Injected"] = []byte("evil")

	got := getEAs(t, store, handle)
	if _, ok := got["Injected"]; ok {
		t.Fatalf("store aliased caller's EA map — a caller-side insert leaked into the stored row")
	}
	if !bytes.Equal(got["NewEA"], []byte("testme")) {
		t.Fatalf("store aliased caller's EA value — in-place byte edit corrupted the stored row: got %q", got["NewEA"])
	}
}

func testEAGetNoAlias(t *testing.T, factory StoreFactory) {
	store := factory(t)
	root := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", root, "ea-get-alias.txt", 0o600)

	putFileWithEAs(t, store, handle, map[string][]byte{"NewEA": []byte("testme")})

	first := getEAs(t, store, handle)
	first["NewEA"][0] = 'X'
	first["Injected"] = []byte("evil")

	second := getEAs(t, store, handle)
	if _, ok := second["Injected"]; ok {
		t.Fatalf("store handed back its backing EA map — a caller insert corrupted the store")
	}
	if !bytes.Equal(second["NewEA"], []byte("testme")) {
		t.Fatalf("store handed back backing EA value — a caller byte edit corrupted the store: got %q", second["NewEA"])
	}
}

// testEAPersistsAcrossReads asserts an EA written once survives an unrelated
// SetFileAttributes (a metadata-only write that re-puts the file) — the EA map
// must not be dropped by a mutation that does not touch it.
func testEAPersistsAcrossReads(t *testing.T, factory StoreFactory) {
	store := factory(t)
	root := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", root, "ea-persist.txt", 0o600)

	putFileWithEAs(t, store, handle, map[string][]byte{"EAONE": []byte("first")})

	// Re-Get then re-Put with an unrelated field changed (size grows). EAs were
	// hydrated on the Get, so they must survive the Put.
	file, err := store.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	file.Size = 4096
	if err := store.PutFile(t.Context(), file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	got := getEAs(t, store, handle)
	if !bytes.Equal(got["EAONE"], []byte("first")) {
		t.Fatalf("EA lost across an unrelated metadata write: got %v", got)
	}
}
