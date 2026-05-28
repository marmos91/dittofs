package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupCreateWireTest stands up a Handler with a session + tree wired so the
// CREATE wire-validation gates near the top of Handler.Create can be exercised
// from a unit test. Returns (handler, smbCtx) ready to call h.Create on.
func setupCreateWireTest(t *testing.T) (*Handler, *SMBHandlerContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareName := "/wire-test"
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

	h := NewHandler()
	h.Registry = rt
	sess := h.CreateSession("127.0.0.1:0", true, "guest", "")

	tree := &TreeConnection{
		TreeID:    1,
		SessionID: sess.SessionID,
		ShareName: shareName,
	}
	h.StoreTree(tree)

	smbCtx := &SMBHandlerContext{
		SessionID: sess.SessionID,
		TreeID:    tree.TreeID,
		ShareName: shareName,
	}
	return h, smbCtx
}

// TestCreate_WireValidation_ImpersonationLevel verifies that an
// out-of-range ImpersonationLevel returns STATUS_BAD_IMPERSONATION_LEVEL
// per MS-SMB2 §2.2.13 (smbtorture smb2.create.impersonation). Only the
// failing-level path is exercised here — the four legal levels (0..3) pass
// the gate and continue into deeper CREATE logic which requires more
// scaffolding than this focused unit test provides.
func TestCreate_WireValidation_ImpersonationLevel(t *testing.T) {
	cases := []uint32{4, 0x12345678, 0xFFFFFFFF}
	for _, level := range cases {
		t.Run("OutOfRange", func(t *testing.T) {
			h, smbCtx := setupCreateWireTest(t)
			req := &CreateRequest{
				FileName:           "foo.txt",
				ImpersonationLevel: level,
				DesiredAccess:      0x120089,
				FileAttributes:     types.FileAttributeNormal,
				ShareAccess:        0x07,
				CreateDisposition:  types.FileOpenIf,
			}
			resp, err := h.Create(smbCtx, req)
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			if resp.Status != types.StatusBadImpersonationLevel {
				t.Errorf("level 0x%08x: status = %s (0x%08x), want STATUS_BAD_IMPERSONATION_LEVEL",
					level, resp.Status, uint32(resp.Status))
			}
		})
	}
}

// TestCreate_WireValidation_CreateOptionsReserved verifies that any non-zero
// bit in the upper byte (0xff000000) of CreateOptions returns
// STATUS_INVALID_PARAMETER per MS-SMB2 §2.2.13 (smbtorture smb2.create.gentest).
func TestCreate_WireValidation_CreateOptionsReserved(t *testing.T) {
	cases := []uint32{
		0x01000000, 0x02000000, 0x04000000, 0x08000000,
		0x10000000, 0x20000000, 0x40000000, 0x80000000,
		0xff000000,
	}
	for _, options := range cases {
		t.Run("Reserved", func(t *testing.T) {
			h, smbCtx := setupCreateWireTest(t)
			req := &CreateRequest{
				FileName:          "foo.txt",
				DesiredAccess:     0x120089,
				FileAttributes:    types.FileAttributeNormal,
				ShareAccess:       0x07,
				CreateDisposition: types.FileOpenIf,
				CreateOptions:     types.CreateOptions(options),
			}
			resp, err := h.Create(smbCtx, req)
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			if resp.Status != types.StatusInvalidParameter {
				t.Errorf("options 0x%08x: status = %s, want STATUS_INVALID_PARAMETER",
					options, resp.Status)
			}
		})
	}
}

// TestCreate_WireValidation_CreateOptionsUnsupported verifies that the three
// CreateOptions bits defined-but-unimplemented in DittoFS return
// STATUS_NOT_SUPPORTED. Samba sets `not_supported_mask = 0x00102080` for these
// probes (smbtorture smb2.create.gentest).
func TestCreate_WireValidation_CreateOptionsUnsupported(t *testing.T) {
	cases := []struct {
		name    string
		options uint32
	}{
		{"FILE_COMPLETE_IF_OPLOCKED", 0x00000080},
		{"FILE_OPEN_BY_FILE_ID", 0x00002000},
		{"FILE_OPEN_REQUIRING_OPLOCK", 0x00100000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, smbCtx := setupCreateWireTest(t)
			req := &CreateRequest{
				FileName:          "foo.txt",
				DesiredAccess:     0x120089,
				FileAttributes:    types.FileAttributeNormal,
				ShareAccess:       0x07,
				CreateDisposition: types.FileOpenIf,
				CreateOptions:     types.CreateOptions(tc.options),
			}
			resp, err := h.Create(smbCtx, req)
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			if resp.Status != types.StatusNotSupported {
				t.Errorf("options 0x%08x: status = %s, want STATUS_NOT_SUPPORTED",
					tc.options, resp.Status)
			}
		})
	}
}

// TestCreate_WireValidation_FileAttributesInvalid verifies that any bit outside
// the legal CREATE attribute mask (0x00007FB7) returns STATUS_INVALID_PARAMETER.
// In particular FILE_ATTRIBUTE_VOLUME (0x8) and FILE_ATTRIBUTE_DEVICE (0x40)
// are rejected (smbtorture smb2.create.gentest).
func TestCreate_WireValidation_FileAttributesInvalid(t *testing.T) {
	cases := []uint32{
		0x00000008, // FILE_ATTRIBUTE_VOLUME
		0x00000040, // FILE_ATTRIBUTE_DEVICE
		0x00010000, // reserved high bit
		0xffff8048, // Samba `invalid_parameter_mask` aggregate
	}
	for _, attrs := range cases {
		t.Run("Invalid", func(t *testing.T) {
			h, smbCtx := setupCreateWireTest(t)
			req := &CreateRequest{
				FileName:          "foo.txt",
				DesiredAccess:     0x120089,
				FileAttributes:    types.FileAttributes(attrs),
				ShareAccess:       0x07,
				CreateDisposition: types.FileOpenIf,
			}
			resp, err := h.Create(smbCtx, req)
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			if resp.Status != types.StatusInvalidParameter {
				t.Errorf("attrs 0x%08x: status = %s, want STATUS_INVALID_PARAMETER",
					attrs, resp.Status)
			}
		})
	}
}

// TestCreate_WireValidation_TWrpContext verifies that a TWrp
// (SMB2_CREATE_TIMEWARP_TOKEN) create context returns
// STATUS_OBJECT_NAME_NOT_FOUND — DittoFS has no VSS-style snapshot backend
// so any non-empty snapshot timestamp resolves to a non-existent view
// (smbtorture smb2.create.blob "Testing timewarp").
func TestCreate_WireValidation_TWrpContext(t *testing.T) {
	h, smbCtx := setupCreateWireTest(t)
	req := &CreateRequest{
		FileName:          "foo.txt",
		DesiredAccess:     0x120089,
		FileAttributes:    types.FileAttributeNormal,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpenIf,
		CreateContexts: []CreateContext{
			{Name: "TWrp", Data: []byte{0x10, 0x27, 0, 0, 0, 0, 0, 0}}, // FILETIME = 10000
		},
	}
	resp, err := h.Create(smbCtx, req)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if resp.Status != types.StatusObjectNameNotFound {
		t.Errorf("TWrp present: status = %s, want STATUS_OBJECT_NAME_NOT_FOUND", resp.Status)
	}
}
