// Handler-level coverage for the per-share StreamsDisabled gate
// (smb2.create_no_streams.no_stream, source4/torture/smb2/create.c:3960).
//
// The CREATE handler rejects any reference to a data stream — named ADS,
// stream-with-type suffix, arbitrary stream-type suffix, and the explicit
// default `::$DATA` syntax — with STATUS_OBJECT_NAME_INVALID on a tree
// whose share has StreamsDisabled=true. Non-stream paths and the
// directory-index syntaxes (`::$INDEX_ALLOCATION`, `:$I30:$INDEX_ALLOCATION`)
// remain accepted because the stream-syntax extractor normalizes those away
// before the gate.
//
// These tests drive `h.Create` end-to-end so the gate, the upstream
// stream-syntax extractor, and the StreamsDisabled plumbing on
// TreeConnection are exercised together.
package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupStreamsDisabledShare wires a handler + runtime + memory store with a
// single share whose StreamsDisabled flag is set per the streamsDisabled
// argument. Returns the handler, an SMBHandlerContext primed with a session
// and tree, and the share's root handle (the latter is unused by these tests
// but matches the pattern used by other handler-level integration tests in
// this package).
func setupStreamsDisabledShare(t *testing.T, streamsDisabled bool) (*Handler, *SMBHandlerContext, metadata.FileHandle) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("streams-test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	const shareName = "/streams-test"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:            shareName,
		MetadataStore:   "streams-test-meta",
		Enabled:         true,
		StreamsDisabled: streamsDisabled,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	const callerUID, callerGID uint32 = 1000, 1000
	uid, gid := callerUID, callerGID
	sess := h.CreateSession("127.0.0.1:12345", false, "test-user", "")
	sess.User = &models.User{
		Username: "test-user",
		UID:      &uid,
		Groups:   []models.Group{{GID: &gid}},
	}

	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{
		TreeID:          treeID,
		SessionID:       sess.SessionID,
		ShareName:       shareName,
		Permission:      models.PermissionReadWrite,
		StreamsDisabled: streamsDisabled,
	})

	smbCtx := &SMBHandlerContext{
		Context:   context.Background(),
		TreeID:    treeID,
		SessionID: sess.SessionID,
		ShareName: shareName,
	}

	return h, smbCtx, rootHandle
}

// streamCreateRequest builds a minimal CREATE request for the given path.
// DesiredAccess SEC_FILE_ALL + share access RWD + disposition OPEN match the
// smbtorture sequence in source4/torture/smb2/create.c::test_no_stream.
func streamCreateRequest(fname string) *CreateRequest {
	return &CreateRequest{
		FileName:          fname,
		DesiredAccess:     0x02000000, // SEC_FLAG_MAXIMUM_ALLOWED
		FileAttributes:    types.FileAttributeNormal,
		ShareAccess:       0x07, // R|W|D
		CreateDisposition: types.FileOpen,
		CreateOptions:     0,
	}
}

// TestCreate_StreamsDisabled_RejectsStreamSyntax mirrors the four
// stream-reference forms exercised by smbtorture
// smb2.create_no_streams.no_stream. Each must return
// STATUS_OBJECT_NAME_INVALID when the tree's share has StreamsDisabled=true.
func TestCreate_StreamsDisabled_RejectsStreamSyntax(t *testing.T) {
	h, smbCtx, _ := setupStreamsDisabledShare(t, true)

	cases := []struct {
		name  string
		fname string
	}{
		{"NamedADS", "test_no_stream:stream"},
		{"NamedADSWithDataType", "test_no_stream:stream:$DATA"},
		{"ArbitraryStreamTypeSuffix", "test_no_stream::foooooooooooo"},
		{"ExplicitDefaultDataStream", "test_no_stream::$DATA"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.Create(smbCtx, streamCreateRequest(tc.fname))
			if err != nil {
				t.Fatalf("Create(%q): unexpected error: %v", tc.fname, err)
			}
			if resp.Status != types.StatusObjectNameInvalid {
				t.Fatalf("Create(%q): status = 0x%08x, expected STATUS_OBJECT_NAME_INVALID (0x%08x)",
					tc.fname, uint32(resp.Status), uint32(types.StatusObjectNameInvalid))
			}
		})
	}
}

// TestCreate_StreamsDisabled_AllowsNonStreamPaths confirms the gate is
// scoped to stream syntaxes — a plain file path on a streams-disabled share
// must not be rejected by this code path. We use FileOpen disposition on a
// path that doesn't exist; the expected outcome is NOT
// STATUS_OBJECT_NAME_INVALID (the gate doesn't fire). The actual status here
// is whatever the downstream lookup returns (typically
// STATUS_OBJECT_NAME_NOT_FOUND) — we assert only that the gate didn't fire.
func TestCreate_StreamsDisabled_AllowsNonStreamPaths(t *testing.T) {
	h, smbCtx, _ := setupStreamsDisabledShare(t, true)

	resp, err := h.Create(smbCtx, streamCreateRequest("plain_file"))
	if err != nil {
		t.Fatalf("Create(plain_file): unexpected error: %v", err)
	}
	if resp.Status == types.StatusObjectNameInvalid {
		t.Fatalf("Create(plain_file): unexpected STATUS_OBJECT_NAME_INVALID — gate fired on non-stream path")
	}
}

// TestCreate_StreamsNotDisabled_AcceptsStreamSyntax confirms the gate is
// genuinely scoped to StreamsDisabled=true shares. On a streams-enabled
// share, a stream-named CREATE must NOT return STATUS_OBJECT_NAME_INVALID
// from this code path (the request flows past the gate and either
// auto-creates the base file or surfaces a downstream status — anything
// except the gate's status is acceptable here).
func TestCreate_StreamsNotDisabled_AcceptsStreamSyntax(t *testing.T) {
	h, smbCtx, _ := setupStreamsDisabledShare(t, false)

	resp, err := h.Create(smbCtx, streamCreateRequest("test_no_stream:stream"))
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if resp.Status == types.StatusObjectNameInvalid {
		t.Fatalf("Create on streams-enabled share: STATUS_OBJECT_NAME_INVALID returned — gate fired on a streams-enabled share")
	}
}

// TestQueryInfo_FsAttrs_NamedStreamsBit pins the FileFsAttributeInformation
// FileSystemAttributes round-trip: streams-enabled shares advertise
// FILE_NAMED_STREAMS (0x00040000), streams-disabled shares strip it. This
// is the QUERY_INFO half of the StreamsDisabled contract (the CREATE half
// is covered by TestCreate_StreamsDisabled_RejectsStreamSyntax above).
func TestQueryInfo_FsAttrs_NamedStreamsBit(t *testing.T) {
	const fileNamedStreams uint32 = 0x00040000

	cases := []struct {
		name            string
		streamsDisabled bool
		wantStreams     bool
	}{
		{"StreamsEnabled_AdvertisesNamedStreams", false, true},
		{"StreamsDisabled_StripsNamedStreams", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, smbCtx, rootHandle := setupStreamsDisabledShare(t, tc.streamsDisabled)
			metaSvc := h.Registry.GetMetadataService()

			// buildFilesystemInfo needs an OpenFile to source the handle.
			// The streams-disabled gate is applied based on tree state,
			// which is what the call site (handler) reads — mirror that.
			info, err := h.buildFilesystemInfo(smbCtx.Context, 5, metaSvc, rootHandle, tc.streamsDisabled, nil)
			if err != nil {
				t.Fatalf("buildFilesystemInfo: %v", err)
			}
			if len(info) < 4 {
				t.Fatalf("FileFsAttributeInformation buffer too short: %d", len(info))
			}
			fsAttrs := binary.LittleEndian.Uint32(info[0:4])
			gotStreams := fsAttrs&fileNamedStreams != 0
			if gotStreams != tc.wantStreams {
				t.Fatalf("FILE_NAMED_STREAMS bit: got=%v want=%v (fsAttrs=0x%08x)", gotStreams, tc.wantStreams, fsAttrs)
			}
		})
	}
}
