package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// =============================================================================
// effectiveAllocationSize / allocReservationFor Tests
// =============================================================================

func TestEffectiveAllocationSize(t *testing.T) {
	cases := []struct {
		name      string
		size      uint64
		requested uint64
		want      uint64
	}{
		{"empty file no request", 0, 0, 0},
		// An empty file opened with a requested allocation must report a
		// non-zero, cluster-aligned AllocationSize (smb2.durable-open.alloc-size
		// sets in.alloc_size=0x1000 and asserts out.alloc_size != 0).
		{"empty file requested 0x1000", 0, 0x1000, 0x1000},
		{"requested rounds up to cluster", 0, 0x1001, 0x2000},
		{"file size exceeds request", 0x3000, 0x1000, 0x3000},
		{"request exceeds file size", 0x1000, 0x5000, 0x5000},
		// A client-controlled requested allocation within clusterSize-1 of the
		// uint64 max must saturate, not wrap to a small value.
		{"requested near-max saturates", 0, ^uint64(0), 0xFFFFFFFFFFFFF000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveAllocationSize(tc.size, tc.requested); got != tc.want {
				t.Errorf("effectiveAllocationSize(%d, %d) = %d, want %d",
					tc.size, tc.requested, got, tc.want)
			}
		})
	}
}

func TestAllocReservationFor(t *testing.T) {
	// Directories ignore the requested reservation (smb2.create.dir-alloc-size
	// sets a 1 GiB request on a directory and asserts the reported allocation
	// stays small); regular files keep it.
	if got := allocReservationFor(true, 1<<30); got != 0 {
		t.Errorf("directory reservation = %d, want 0", got)
	}
	if got := allocReservationFor(false, 0x1000); got != 0x1000 {
		t.Errorf("regular-file reservation = %d, want 0x1000", got)
	}
}

// =============================================================================
// FileAttrToFileStandardInfo Tests
// =============================================================================

func TestFileAttrToFileStandardInfo_NumberOfLinks(t *testing.T) {
	tests := []struct {
		name     string
		nlink    uint32
		expected uint32
	}{
		{"normal file with one link", 1, 1},
		{"hard linked file with three links", 3, 3},
		{"uninitialized zero defaults to one", 0, 1},
		{"directory with dot and dotdot", 2, 2},
		{"large link count", 100, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attr := &metadata.FileAttr{
				Type:  metadata.FileTypeRegular,
				Nlink: tt.nlink,
			}
			info := FileAttrToFileStandardInfo(attr, false)
			if info.NumberOfLinks != tt.expected {
				t.Errorf("NumberOfLinks = %d, expected %d", info.NumberOfLinks, tt.expected)
			}
		})
	}
}

func TestFileAttrToFileStandardInfo_Directory(t *testing.T) {
	attr := &metadata.FileAttr{
		Type:  metadata.FileTypeDirectory,
		Nlink: 2, // standard . and ..
	}
	info := FileAttrToFileStandardInfo(attr, false)

	if !info.Directory {
		t.Error("Directory should be true for FileTypeDirectory")
	}
	if info.NumberOfLinks != 2 {
		t.Errorf("NumberOfLinks = %d, expected 2 for directory", info.NumberOfLinks)
	}
}

func TestFileAttrToFileStandardInfo_DeletePending(t *testing.T) {
	attr := &metadata.FileAttr{
		Type:  metadata.FileTypeRegular,
		Nlink: 1,
	}

	t.Run("not delete pending", func(t *testing.T) {
		info := FileAttrToFileStandardInfo(attr, false)
		if info.DeletePending {
			t.Error("DeletePending should be false")
		}
	})

	t.Run("delete pending", func(t *testing.T) {
		info := FileAttrToFileStandardInfo(attr, true)
		if !info.DeletePending {
			t.Error("DeletePending should be true")
		}
	})

	// smbtorture smb2.setinfo (setinfo.c:229) asserts NumberOfLinks tracks the
	// delete-on-close disposition: 0 when delete_pending, the real count
	// (>= 1) otherwise.
	t.Run("nlink zero when delete pending", func(t *testing.T) {
		info := FileAttrToFileStandardInfo(attr, true)
		if info.NumberOfLinks != 0 {
			t.Errorf("NumberOfLinks = %d, want 0 when delete pending", info.NumberOfLinks)
		}
	})
	t.Run("nlink one when not delete pending", func(t *testing.T) {
		info := FileAttrToFileStandardInfo(attr, false)
		if info.NumberOfLinks != 1 {
			t.Errorf("NumberOfLinks = %d, want 1 when not delete pending", info.NumberOfLinks)
		}
	})
}

func TestFileAttrToFileStandardInfo_Sizes(t *testing.T) {
	attr := &metadata.FileAttr{
		Type:  metadata.FileTypeRegular,
		Size:  5000,
		Nlink: 1,
	}
	info := FileAttrToFileStandardInfo(attr, false)

	if info.EndOfFile != 5000 {
		t.Errorf("EndOfFile = %d, expected 5000", info.EndOfFile)
	}

	// AllocationSize should be cluster-aligned (4096-byte clusters)
	expectedAlloc := uint64(8192) // 5000 rounded up to next 4096 boundary
	if info.AllocationSize != expectedAlloc {
		t.Errorf("AllocationSize = %d, expected %d", info.AllocationSize, expectedAlloc)
	}
}

// =============================================================================
// FileAttrToSMBAttributes Tests
// =============================================================================

func TestFileAttrToSMBAttributes_RegularFile(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular}
	attrs := FileAttrToSMBAttributes(attr)

	// Regular files should have ARCHIVE attribute
	if attrs&0x20 == 0 { // FileAttributeArchive = 0x20
		t.Error("Regular file should have Archive attribute")
	}
}

func TestFileAttrToSMBAttributes_Directory(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeDirectory}
	attrs := FileAttrToSMBAttributes(attr)

	if attrs&0x10 == 0 { // FileAttributeDirectory = 0x10
		t.Error("Directory should have Directory attribute")
	}
}

func TestFileAttrToSMBAttributes_SystemBitRoundTrip(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeSystem|types.FileAttributeArchive, false)
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode}
	got := FileAttrToSMBAttributes(attr)
	if got&types.FileAttributeSystem == 0 {
		t.Errorf("SYSTEM missing after round-trip: attrs=0x%x mode=0x%x", got, mode)
	}
}

func TestFileAttrToSMBAttributesWithName_HiddenByMetadata(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Hidden: true}
	got := FileAttrToSMBAttributesWithName(attr, "normalname")
	if got&types.FileAttributeHidden == 0 {
		t.Errorf("HIDDEN missing when attr.Hidden=true, attrs=0x%x", got)
	}
}

func TestFileAttrToSMBAttributesWithName_HiddenByDotPrefix(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Hidden: false}
	got := FileAttrToSMBAttributesWithName(attr, ".dotfile")
	if got&types.FileAttributeHidden == 0 {
		t.Errorf("HIDDEN missing for dot-prefix file, attrs=0x%x", got)
	}
}

// TestSMBModeFromAttrs_OverwriteForcesArchive locks down the contract that
// overwriteFile relies on: per MS-FSA 2.1.5.1.2.1, OVERWRITE/SUPERSEDE always
// sets FILE_ATTRIBUTE_ARCHIVE on the post-overwrite metadata, so the mode
// produced from the request attrs OR'd with ARCHIVE must round-trip to a
// FileAttr whose SMB attrs include ARCHIVE — even when the client only sent
// FILE_ATTRIBUTE_NORMAL (the case smbtorture breaking2/breaking5 exercises).
func TestSMBModeFromAttrs_OverwriteForcesArchive(t *testing.T) {
	cases := []struct {
		name    string
		reqAttr types.FileAttributes
	}{
		{"client sent zero", 0},
		{"client sent NORMAL", types.FileAttributeNormal},
		{"client sent HIDDEN", types.FileAttributeHidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forced := tc.reqAttr | types.FileAttributeArchive
			mode := SMBModeFromAttrs(forced, false)
			attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode}
			got := FileAttrToSMBAttributes(attr)
			if got&types.FileAttributeArchive == 0 {
				t.Errorf("ARCHIVE missing from round-trip attrs 0x%x (mode 0x%x)", got, mode)
			}
		})
	}
}

// TestSMBModeFromAttrs_ReadonlyDoesNotStripWriteBit pins the contract that
// applying READONLY via SET_INFO does NOT mutate POSIX owner-write — the
// READONLY bit is tracked in modeDOSReadonly instead. Stripping owner-write
// would change the mode-synthesized DACL, breaking smb2.winattr
// (source4/torture/smb2/attr.c:365) which captures the DACL before any
// SET_INFO and asserts ACEs are unchanged across attribute round-trips.
func TestSMBModeFromAttrs_ReadonlyDoesNotStripWriteBit(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeReadonly, false)
	if mode&0o200 == 0 {
		t.Errorf("SMBModeFromAttrs(READONLY) stripped owner-write: mode=0o%o (want 0o200 preserved)", mode)
	}
	if mode&modeDOSReadonly == 0 {
		t.Errorf("SMBModeFromAttrs(READONLY) did not set modeDOSReadonly: mode=0x%x", mode)
	}
}

// TestFileAttrToSMBAttributes_ReadonlyRoundTrip verifies SET_INFO -> QUERY_INFO
// round-trip via the high-bit storage path.
func TestFileAttrToSMBAttributes_ReadonlyRoundTrip(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeReadonly, false)
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode}
	got := FileAttrToSMBAttributes(attr)
	if got&types.FileAttributeReadonly == 0 {
		t.Errorf("READONLY missing after round-trip: attrs=0x%x mode=0x%x", got, mode)
	}
	if got&types.FileAttributeArchive != 0 {
		t.Errorf("ARCHIVE leaked into READONLY-only round-trip: attrs=0x%x", got)
	}
}

// TestFileAttrToSMBAttributes_ReadonlyLegacyMode covers files whose POSIX
// owner-write was stripped out-of-band (e.g., NFS chmod or shell chmod with no
// DOS bits set). They must still report READONLY to SMB clients per MS-FSCC
// §2.6, matching Samba dosmode.c::dos_mode_from_sbuf.
func TestFileAttrToSMBAttributes_ReadonlyLegacyMode(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o444}
	got := FileAttrToSMBAttributes(attr)
	if got&types.FileAttributeReadonly == 0 {
		t.Errorf("READONLY missing for owner-write-stripped legacy mode 0o444: attrs=0x%x", got)
	}
}

// TestFileAttrToSMBAttributes_DirectoryArchiveRoundTrip verifies the
// smb2.winattr directory loop (source4/torture/smb2/attr.c:439): SET_INFO of
// ARCHIVE on a directory must round-trip as DIRECTORY|ARCHIVE.
func TestFileAttrToSMBAttributes_DirectoryArchiveRoundTrip(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeArchive, true)
	attr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: mode}
	got := FileAttrToSMBAttributes(attr)
	want := types.FileAttributeDirectory | types.FileAttributeArchive
	if got != want {
		t.Errorf("DIR+ARCHIVE round-trip: got 0x%x, want 0x%x (mode 0x%x)", got, want, mode)
	}
}

// TestFileAttrToSMBAttributes_DirectoryReadonlyRoundTrip verifies READONLY
// round-trip on directories — same winattr loop, j=2.
func TestFileAttrToSMBAttributes_DirectoryReadonlyRoundTrip(t *testing.T) {
	mode := SMBModeFromAttrs(types.FileAttributeReadonly, true)
	attr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: mode}
	got := FileAttrToSMBAttributes(attr)
	want := types.FileAttributeDirectory | types.FileAttributeReadonly
	if got != want {
		t.Errorf("DIR+READONLY round-trip: got 0x%x, want 0x%x (mode 0x%x)", got, want, mode)
	}
}

// TestFileAttrToSMBAttributes_AllDOSBitsRoundTrip covers the winattr open_attrs
// combinations (ARCHIVE | READONLY | HIDDEN | SYSTEM) — every combination must
// round-trip exactly when stored via SMBModeFromAttrs.
func TestFileAttrToSMBAttributes_AllDOSBitsRoundTrip(t *testing.T) {
	cases := []types.FileAttributes{
		types.FileAttributeArchive,
		types.FileAttributeReadonly,
		types.FileAttributeHidden,
		types.FileAttributeSystem,
		types.FileAttributeArchive | types.FileAttributeReadonly,
		types.FileAttributeArchive | types.FileAttributeHidden,
		types.FileAttributeArchive | types.FileAttributeSystem,
		types.FileAttributeArchive | types.FileAttributeReadonly | types.FileAttributeHidden,
		types.FileAttributeArchive | types.FileAttributeReadonly | types.FileAttributeSystem,
		types.FileAttributeArchive | types.FileAttributeHidden | types.FileAttributeSystem,
		types.FileAttributeReadonly | types.FileAttributeHidden,
		types.FileAttributeReadonly | types.FileAttributeSystem,
		types.FileAttributeReadonly | types.FileAttributeHidden | types.FileAttributeSystem,
	}
	for _, in := range cases {
		mode := SMBModeFromAttrs(in, false)
		attr := &metadata.FileAttr{
			Type:   metadata.FileTypeRegular,
			Mode:   mode,
			Hidden: in&types.FileAttributeHidden != 0,
		}
		got := FileAttrToSMBAttributes(attr)
		if got != in {
			t.Errorf("attrs round-trip: in=0x%x got=0x%x (mode=0x%x)", in, got, mode)
		}
	}
}

// TestFileAttrToFileBasicInfoWithName_DotPrefixHidden ensures the name-aware
// variant marks dot-prefixed files HIDDEN even when attr.Hidden is false.
// Required by smbtorture smb2.dosmode (dosmode.c:158).
func TestFileAttrToFileBasicInfoWithName_DotPrefixHidden(t *testing.T) {
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}
	info := FileAttrToFileBasicInfoWithName(attr, ".dotfile")
	if info.FileAttributes&types.FileAttributeHidden == 0 {
		t.Errorf("dot-prefix file missing HIDDEN: attrs=0x%x", info.FileAttributes)
	}
}

// =============================================================================
// UTF-16LE surrogate-safe conversion (smb2.charset.Testing, #740)
// =============================================================================

// le builds the UTF-16LE byte stream for a sequence of raw 16-bit code units.
func le(units ...uint16) []byte {
	w := smbenc.NewWriter(len(units) * 2)
	for _, u := range units {
		w.WriteUint16(u)
	}
	return w.Bytes()
}

// TestUTF16LE_RoundTrip asserts decode->encode reproduces the exact input bytes
// for a spread of well-formed and malformed UTF-16, including the surrogate
// edge cases the Samba charset suite exercises (source4/torture/smb2/charset.c).
func TestUTF16LE_RoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		units []uint16
	}{
		{"ascii", []uint16{'t', 'e', 's', 't'}},
		{"composed", []uint16{0x61, 0x308}},    // a + combining umlaut
		{"precomposed", []uint16{0xE4}},        // ä
		{"naked_diacritical", []uint16{0x308}}, // lone combining umlaut
		{"double_diacritical", []uint16{0x308, 0x308}},
		{"lone_high_surrogate", []uint16{0xD800}},         // unpaired high
		{"lone_low_surrogate", []uint16{0xDC00}},          // unpaired low
		{"full_surrogate_pair", []uint16{0xD800, 0xDC00}}, // U+10000
		{"high_then_bmp", []uint16{0xD800, 0x0041}},       // unpaired high, then 'A'
		{"bmp_then_low", []uint16{0x0041, 0xDC00}},        // 'A', then unpaired low
		{"two_high_surrogates", []uint16{0xD800, 0xD801}},
		{"wide_a_upper", []uint16{0xFF21}}, // fullwidth A
		{"wide_a_lower", []uint16{0xFF41}}, // fullwidth a
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := le(tc.units...)
			s := decodeUTF16LESurrogateSafe(in)
			out := encodeUTF16LESurrogateSafe(s)
			if string(out) != string(in) {
				t.Fatalf("round-trip mismatch for %v:\n in  = % x\n out = % x", tc.units, in, out)
			}
		})
	}
}

// TestUTF16LE_LoneSurrogatesAreDistinct is the crux of smb2.charset.Testing's
// test_surrogate: the unpaired surrogates {0xD800} and {0xDC00} are different
// filenames and must NOT collapse to the same string (the stdlib utf16.Decode
// maps both to U+FFFD, which made the server reject the second CREATE as a
// spurious name collision).
func TestUTF16LE_LoneSurrogatesAreDistinct(t *testing.T) {
	d800 := decodeUTF16LESurrogateSafe(le(0xD800))
	dc00 := decodeUTF16LESurrogateSafe(le(0xDC00))
	pair := decodeUTF16LESurrogateSafe(le(0xD800, 0xDC00))

	if d800 == dc00 {
		t.Fatalf("lone high (0xD800) and lone low (0xDC00) collapsed to the same name %q", d800)
	}
	if d800 == pair || dc00 == pair {
		t.Fatalf("lone surrogate collided with the well-formed pair: d800=%q dc00=%q pair=%q", d800, dc00, pair)
	}
	// The lone surrogates must be preserved as WTF-8, not collapsed to the
	// U+FFFD replacement character ("\xef\xbf\xbd"). Inspect raw bytes: ranging
	// the string with for/range would itself decode WTF-8 to U+FFFD.
	const replacement = "\xef\xbf\xbd"
	if strings.Contains(d800, replacement) || strings.Contains(dc00, replacement) {
		t.Fatalf("lone surrogate was lossily replaced with U+FFFD: d800=% x dc00=% x", d800, dc00)
	}
	// The well-formed pair must decode to a single supplementary rune (U+10000).
	if r := []rune(pair); len(r) != 1 || r[0] != 0x10000 {
		t.Fatalf("surrogate pair did not combine into U+10000: got %#v", r)
	}
}

// TestEncodeUTF16LE_DelegatesSurrogateSafe pins that the live filename codec
// in encoding.go (the one CREATE/QUERY_DIRECTORY/SET_INFO actually call) is the
// surrogate-safe one, not the lossy stdlib path. Before the wiring these
// helpers collapsed both lone surrogates to U+FFFD.
func TestEncodeUTF16LE_DelegatesSurrogateSafe(t *testing.T) {
	for _, units := range [][]uint16{
		{0xD800}, {0xDC00}, {0xD800, 0xDC00}, {'t', 'e', 's', 't'},
	} {
		in := le(units...)
		if got := decodeUTF16LE(in); got != decodeUTF16LESurrogateSafe(in) {
			t.Fatalf("decodeUTF16LE diverged from surrogate-safe for %v", units)
		}
		s := decodeUTF16LE(in)
		if string(encodeUTF16LE(s)) != string(in) {
			t.Fatalf("encodeUTF16LE not byte-exact round-trip for %v", units)
		}
	}
	if decodeUTF16LE(le(0xD800)) == decodeUTF16LE(le(0xDC00)) {
		t.Fatal("live decodeUTF16LE collapsed the two lone surrogates")
	}
}

// TestLoneSurrogateNames_CreateQueryRoundTrip drives the real
// CREATE -> QUERY_DIRECTORY filename path that smb2.charset.Testing exercises:
// the two distinct lone surrogates {0xD800} and {0xDC00} are decoded through the
// live filename codec, stored as separate files, resolved back via the
// case-insensitive lookup CREATE uses for collision detection (must NOT fold
// them together), then re-encoded for the directory listing byte-for-byte.
func TestLoneSurrogateNames_CreateQueryRoundTrip(t *testing.T) {
	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareName := "/test"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0755},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	metaSvc := rt.GetMetadataService()

	// Decode the three charset.Testing names exactly as the wire CREATE handler
	// does (decodeUTF16LE over the UTF-16LE name bytes).
	hiBytes := le(0xD800)
	loBytes := le(0xDC00)
	pairBytes := le(0xD800, 0xDC00)
	hiName := decodeUTF16LE(hiBytes)
	loName := decodeUTF16LE(loBytes)
	pairName := decodeUTF16LE(pairBytes)

	if hiName == loName {
		t.Fatalf("decoded lone surrogates collapsed: %q == %q", hiName, loName)
	}

	fileAttr := func() *metadata.FileAttr {
		return &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0644}
	}
	for _, name := range []string{hiName, loName, pairName} {
		if _, err := metaSvc.CreateFile(authCtx, rootHandle, name, fileAttr()); err != nil {
			t.Fatalf("CreateFile(% x) failed (spurious collision?): %v", name, err)
		}
	}

	// Each name must resolve back to a DISTINCT file via the case-insensitive
	// path CREATE uses — the lone surrogates must not fold together.
	resolve := func(name string) string {
		f, _, lookupErr := metaSvc.LookupCaseInsensitive(authCtx, rootHandle, name)
		if lookupErr != nil || f == nil {
			t.Fatalf("LookupCaseInsensitive(% x) failed: file=%v err=%v", name, f, lookupErr)
		}
		return f.ID.String()
	}
	hiID, loID, pairID := resolve(hiName), resolve(loName), resolve(pairName)
	if hiID == loID || hiID == pairID || loID == pairID {
		t.Fatalf("lone-surrogate names resolved to the same file: hi=%s lo=%s pair=%s", hiID, loID, pairID)
	}

	// QUERY_DIRECTORY path: every stored name must re-encode to its original
	// UTF-16LE bytes through the live filename codec.
	want := map[string][]byte{hiName: hiBytes, loName: loBytes, pairName: pairBytes}
	page, err := metaSvc.ReadDirectory(authCtx, rootHandle, 0, 1<<20)
	if err != nil {
		t.Fatalf("ReadDirectory: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range page.Entries {
		if entry.Name == "." || entry.Name == ".." {
			continue
		}
		exp, ok := want[entry.Name]
		if !ok {
			t.Fatalf("unexpected directory entry % x", entry.Name)
		}
		if got := encodeUTF16LE(entry.Name); string(got) != string(exp) {
			t.Fatalf("entry % x re-encoded to % x, want % x", entry.Name, got, exp)
		}
		seen[entry.Name] = true
	}
	for name := range want {
		if !seen[name] {
			t.Fatalf("directory listing missing name % x", name)
		}
	}
}

// TestUTF16LE_WideACaseFold documents the test_widea expectation: name matching
// is case-insensitive via the metadata layer's strings.EqualFold, which folds
// fullwidth 'Ａ' (U+FF21) to fullwidth 'ａ' (U+FF41) — so they collide — while
// neither folds to ASCII 'a'. converters.go does not special-case this; the
// test pins the stdlib behavior the collision detection relies on.
func TestUTF16LE_WideACaseFold(t *testing.T) {
	wideUpper := decodeUTF16LESurrogateSafe(le(0xFF21))
	wideLower := decodeUTF16LESurrogateSafe(le(0xFF41))
	ascii := decodeUTF16LESurrogateSafe(le('a'))

	if !strings.EqualFold(wideUpper, wideLower) {
		t.Fatalf("fullwidth A/a must case-fold equal (collision): %q vs %q", wideUpper, wideLower)
	}
	if strings.EqualFold(ascii, wideLower) {
		t.Fatalf("ASCII 'a' must NOT fold to fullwidth 'ａ': %q vs %q", ascii, wideLower)
	}
}
