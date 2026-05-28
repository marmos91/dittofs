// Handler-level coverage for SET_INFO BasicInformation propagation from an
// Alternate Data Stream handle onto the base file (smb2.streams.attributes2,
// source4/torture/smb2/streams.c::test_stream_attributes2).
//
// Per NTFS semantics, base and stream report identical DOS file attributes
// via QUERY_INFO. The handler enforces this by propagating the Hidden flag
// and the DOS attribute bits (Explicit/Archive/System/Readonly) from a
// stream SET_INFO onto the base file. The propagation only overlays DOS
// bits onto the base's existing mode — POSIX permission bits and the
// FSCTL-managed modeDOSCompressed bit must survive.
package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupADSAttrPropagationTest builds a memory-backed runtime with a single
// share that contains a base file `base.txt` and a stream `base.txt:s`. The
// base file's mode is preserved across the SET_INFO under test, so we let
// the caller pin its initial value via the basePOSIXMode parameter (the
// callers exercise both the default 0o644 and a non-default 0o700 to
// confirm POSIX bits survive).
//
// Returns the handler, auth context, both file metadata handles, and the
// OpenFile that represents the stream open. The OpenFile carries a colon
// in its FileName and a ParentHandle, which is the trigger the SET_INFO
// propagation path keys off.
func setupADSAttrPropagationTest(t *testing.T, basePOSIXMode uint32) (
	*Handler,
	*metadata.AuthContext,
	metadata.FileHandle, // base file handle
	metadata.FileHandle, // stream file handle
	*OpenFile, // open on the stream
) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ads-attr-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	const shareName = "/ads-attr"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "ads-attr-meta",
		Enabled:       true,
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
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

	// Base file with the requested POSIX mode plus modeDOSCompressed set so
	// the test can also assert that bit survives the SET_INFO propagation.
	baseFile, err := metaSvc.CreateFile(authCtx, rootHandle, "base.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: basePOSIXMode | modeDOSCompressed,
	})
	if err != nil {
		t.Fatalf("CreateFile base: %v", err)
	}
	baseHandle, err := metadata.EncodeFileHandle(baseFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle base: %v", err)
	}

	// Stream as a sibling entry under the parent directory, mirroring how
	// the CREATE handler stores ADS — name carries the colon, no special
	// metadata flag (the colon in the name is the marker).
	streamFile, err := metaSvc.CreateFile(authCtx, rootHandle, "base.txt:s", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile stream: %v", err)
	}
	streamHandle, err := metadata.EncodeFileHandle(streamFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle stream: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	streamOpen := &OpenFile{
		FileID:         [16]byte{0xAD, 0x5A, 0x77, 0x52, 0xC0, 0x01},
		MetadataHandle: streamHandle,
		ParentHandle:   rootHandle,
		FileName:       "base.txt:s",
		Path:           "base.txt:s",
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileWriteAttributes) | uint32(types.FileWriteData) | uint32(types.FileReadData),
	}
	h.StoreOpenFile(streamOpen)

	return h, authCtx, baseHandle, streamHandle, streamOpen
}

// TestSetInfo_ADS_PropagatesDosBitsToBase: SET_INFO BasicInformation on a
// stream handle with FileAttributes=HIDDEN must propagate HIDDEN onto the
// base file. Mirrors the "modify attributes via stream" step of
// smb2.streams.attributes2.
func TestSetInfo_ADS_PropagatesDosBitsToBase(t *testing.T) {
	h, authCtx, baseHandle, _, streamOpen := setupADSAttrPropagationTest(t, 0o644)
	metaSvc := h.Registry.GetMetadataService()

	// SET_INFO with FileAttributes = HIDDEN (0x02), all four FILETIME
	// fields zero (no time change).
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint32(buf[32:36], uint32(types.FileAttributeHidden))

	resp, err := h.setFileInfoFromStore(authCtx, streamOpen, types.FileBasicInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore on stream: err=%v status=%v", err, resp)
	}

	base, err := metaSvc.GetFile(authCtx.Context, baseHandle)
	if err != nil {
		t.Fatalf("GetFile(base): %v", err)
	}

	// The Hidden flag MUST be set on the base file.
	if !base.Hidden {
		t.Errorf("base.Hidden = false after stream SET_INFO HIDDEN; expected true")
	}
}

// TestSetInfo_ADS_PreservesBasePOSIXMode: SET_INFO BasicInformation on a
// stream handle MUST NOT clobber the base file's POSIX permission bits with
// the stream-derived default (0o644). A base file previously set to 0o700
// (e.g. via NFS chmod) must keep those bits after a stream SET_INFO.
func TestSetInfo_ADS_PreservesBasePOSIXMode(t *testing.T) {
	const basePOSIX = uint32(0o700)
	h, authCtx, baseHandle, _, streamOpen := setupADSAttrPropagationTest(t, basePOSIX)
	metaSvc := h.Registry.GetMetadataService()

	buf := make([]byte, 40)
	binary.LittleEndian.PutUint32(buf[32:36], uint32(types.FileAttributeHidden))

	resp, err := h.setFileInfoFromStore(authCtx, streamOpen, types.FileBasicInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore on stream: err=%v status=%v", err, resp)
	}

	base, err := metaSvc.GetFile(authCtx.Context, baseHandle)
	if err != nil {
		t.Fatalf("GetFile(base): %v", err)
	}

	// POSIX permission bits (the low 12 bits) must equal the original
	// 0o700 — the propagation must not have overwritten them with the
	// stream-derived 0o644 default that SMBModeFromAttrs would produce.
	gotPOSIX := base.Mode & 0o7777
	if gotPOSIX != basePOSIX {
		t.Errorf("base POSIX mode = 0o%o after stream SET_INFO; expected 0o%o (stream default 0o644 must NOT clobber)", gotPOSIX, basePOSIX)
	}

	// modeDOSCompressed (FSCTL-managed) must also survive the propagation.
	if base.Mode&modeDOSCompressed == 0 {
		t.Errorf("base.Mode lost modeDOSCompressed after stream SET_INFO; expected to survive")
	}
}

// TestSetInfo_ADS_TimestampOnlyDoesNotPropagateAttrs: SET_INFO
// BasicInformation with FileAttributes=0 (timestamp-only) must NOT touch
// the base file's Hidden field or DOS mode bits. Only an explicit
// FileAttributes value carries an attribute intent.
func TestSetInfo_ADS_TimestampOnlyDoesNotPropagateAttrs(t *testing.T) {
	h, authCtx, baseHandle, _, streamOpen := setupADSAttrPropagationTest(t, 0o644)
	metaSvc := h.Registry.GetMetadataService()

	// Seed: base file is NOT hidden.
	preBase, err := metaSvc.GetFile(authCtx.Context, baseHandle)
	if err != nil {
		t.Fatalf("GetFile(base) pre: %v", err)
	}
	if preBase.Hidden {
		t.Fatalf("seed: base.Hidden should be false, got true")
	}

	// SET_INFO with all-zero FILETIME and FileAttributes=0 — a no-op call.
	buf := make([]byte, 40)

	resp, err := h.setFileInfoFromStore(authCtx, streamOpen, types.FileBasicInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore on stream: err=%v status=%v", err, resp)
	}

	base, err := metaSvc.GetFile(authCtx.Context, baseHandle)
	if err != nil {
		t.Fatalf("GetFile(base): %v", err)
	}

	// Hidden must still be false — the zero-attributes SET_INFO must not
	// have flipped it.
	if base.Hidden {
		t.Errorf("base.Hidden flipped to true after zero-attributes SET_INFO on stream; expected false")
	}
}
