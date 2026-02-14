package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

func TestBuildV4AuthContext_ValidHandle(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs) // nil registry -- basic auth context

	uid := uint32(1000)
	gid := uint32(1000)
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "192.168.1.100:9999",
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &uid,
		GID:        &gid,
		GIDs:       []uint32{100, 200},
	}

	// Use a valid file handle format: "shareName:uuid"
	handle := []byte("/export:00000000-0000-0000-0000-000000000001")

	authCtx, shareName, err := h.buildV4AuthContext(ctx, handle)
	if err != nil {
		t.Fatalf("buildV4AuthContext error: %v", err)
	}

	if shareName != "/export" {
		t.Errorf("shareName = %q, want %q", shareName, "/export")
	}

	if authCtx == nil {
		t.Fatal("authCtx is nil")
	}

	if authCtx.ClientAddr != "192.168.1.100:9999" {
		t.Errorf("ClientAddr = %q, want %q", authCtx.ClientAddr, "192.168.1.100:9999")
	}

	if authCtx.AuthMethod != "unix" {
		t.Errorf("AuthMethod = %q, want %q", authCtx.AuthMethod, "unix")
	}

	if authCtx.Identity == nil {
		t.Fatal("Identity is nil")
	}

	if authCtx.Identity.UID == nil || *authCtx.Identity.UID != 1000 {
		t.Errorf("UID = %v, want 1000", authCtx.Identity.UID)
	}

	if authCtx.Identity.Username != "uid:1000" {
		t.Errorf("Username = %q, want %q", authCtx.Identity.Username, "uid:1000")
	}
}

func TestBuildV4AuthContext_InvalidHandle(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	// Invalid handle (no colon separator)
	handle := []byte("invalid-handle-no-colon")

	_, _, err := h.buildV4AuthContext(ctx, handle)
	if err == nil {
		t.Fatal("expected error for invalid handle, got nil")
	}
}

func TestBuildV4AuthContext_NilRegistry(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs) // nil registry

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		AuthFlavor: 0, // AUTH_NULL
	}

	handle := []byte("/export:00000000-0000-0000-0000-000000000001")

	authCtx, shareName, err := h.buildV4AuthContext(ctx, handle)
	if err != nil {
		t.Fatalf("expected no error with nil registry, got: %v", err)
	}

	if shareName != "/export" {
		t.Errorf("shareName = %q, want %q", shareName, "/export")
	}

	if authCtx.AuthMethod != "anonymous" {
		t.Errorf("AuthMethod = %q, want %q for AUTH_NULL", authCtx.AuthMethod, "anonymous")
	}

	// With nil registry, ShareReadOnly defaults to false
	if authCtx.ShareReadOnly {
		t.Error("ShareReadOnly should be false with nil registry")
	}
}

func TestEncodeChangeInfo4(t *testing.T) {
	t.Run("atomic true", func(t *testing.T) {
		var buf bytes.Buffer
		encodeChangeInfo4(&buf, true, 100, 200)

		reader := bytes.NewReader(buf.Bytes())

		atomic, err := xdr.DecodeUint32(reader)
		if err != nil {
			t.Fatalf("decode atomic: %v", err)
		}
		if atomic != 1 {
			t.Errorf("atomic = %d, want 1", atomic)
		}

		before, err := xdr.DecodeUint64(reader)
		if err != nil {
			t.Fatalf("decode before: %v", err)
		}
		if before != 100 {
			t.Errorf("before = %d, want 100", before)
		}

		after, err := xdr.DecodeUint64(reader)
		if err != nil {
			t.Fatalf("decode after: %v", err)
		}
		if after != 200 {
			t.Errorf("after = %d, want 200", after)
		}
	})

	t.Run("atomic false", func(t *testing.T) {
		var buf bytes.Buffer
		encodeChangeInfo4(&buf, false, 50, 75)

		reader := bytes.NewReader(buf.Bytes())

		atomic, _ := xdr.DecodeUint32(reader)
		if atomic != 0 {
			t.Errorf("atomic = %d, want 0", atomic)
		}

		before, _ := xdr.DecodeUint64(reader)
		if before != 50 {
			t.Errorf("before = %d, want 50", before)
		}

		after, _ := xdr.DecodeUint64(reader)
		if after != 75 {
			t.Errorf("after = %d, want 75", after)
		}
	})

	t.Run("encoding size", func(t *testing.T) {
		var buf bytes.Buffer
		encodeChangeInfo4(&buf, true, 0, 0)

		// Expected: 4 bytes (bool) + 8 bytes (before) + 8 bytes (after) = 20 bytes
		if buf.Len() != 20 {
			t.Errorf("encoded size = %d, want 20", buf.Len())
		}
	})
}
