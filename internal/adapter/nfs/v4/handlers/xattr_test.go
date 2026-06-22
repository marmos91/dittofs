package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// realHandle is a valid-format real-FS handle ("shareName:uuid") accepted by
// buildV4AuthContext with a nil registry (basic auth context).
var realHandle = []byte("/export:00000000-0000-0000-0000-000000000001")

// fakeXattrBackend is an in-memory XattrBackend for the NFSv4.2 xattr handler
// tests. It keys on the store-canonical (user.-prefixed) name.
type fakeXattrBackend struct {
	store map[string][]byte
	// forceErr, when set, is returned by every method (to exercise error mapping).
	forceErr error
	// setErr / removeErr, when set, are returned only by SetXattr / RemoveXattr
	// (to exercise per-op error mapping without tripping the GetXattr pre-check).
	setErr    error
	removeErr error
}

func newFakeXattrBackend() *fakeXattrBackend {
	return &fakeXattrBackend{store: make(map[string][]byte)}
}

func (f *fakeXattrBackend) GetXattr(_ *metadata.AuthContext, _ metadata.FileHandle, name string) ([]byte, bool, error) {
	if f.forceErr != nil {
		return nil, false, f.forceErr
	}
	v, ok := f.store[name]
	return v, ok, nil
}

func (f *fakeXattrBackend) SetXattr(_ *metadata.AuthContext, _ metadata.FileHandle, name string, value []byte) error {
	if f.forceErr != nil {
		return f.forceErr
	}
	if f.setErr != nil {
		return f.setErr
	}
	f.store[name] = value
	return nil
}

func (f *fakeXattrBackend) RemoveXattr(_ *metadata.AuthContext, _ metadata.FileHandle, name string) error {
	if f.forceErr != nil {
		return f.forceErr
	}
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.store, name)
	return nil
}

func (f *fakeXattrBackend) ListXattr(_ *metadata.AuthContext, _ metadata.FileHandle) ([]string, error) {
	if f.forceErr != nil {
		return nil, f.forceErr
	}
	names := make([]string, 0, len(f.store))
	for k := range f.store {
		names = append(names, k)
	}
	return names, nil
}

// xattrTestHandler builds a handler wired with the given fake backend and a
// pseudo-fs containing /export. Registry is nil, so buildV4AuthContext returns
// a basic auth context and xattrChangeID is a best-effort no-op.
func xattrTestHandler(t *testing.T, backend XattrBackend) *Handler {
	t.Helper()
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)
	h.SetXattrBackend(backend)
	return h
}

func xattrCtx(handle []byte) *types.CompoundContext {
	uid := uint32(1000)
	gid := uint32(1000)
	return &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.1:1234",
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &uid,
		GID:        &gid,
		CurrentFH:  handle,
	}
}

// --- arg encoders (mirror the RFC 8276 wire formats) ---

func encGetXattrArgs(name string) io.Reader {
	var buf bytes.Buffer
	_ = xdr.WriteXDRString(&buf, name)
	return bytes.NewReader(buf.Bytes())
}

func encSetXattrArgs(option uint32, name string, value []byte) io.Reader {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, option)
	_ = xdr.WriteXDRString(&buf, name)
	_ = xdr.WriteXDROpaque(&buf, value)
	return bytes.NewReader(buf.Bytes())
}

func encListXattrsArgs(cookie uint64, maxcount uint32) io.Reader {
	var buf bytes.Buffer
	_ = xdr.WriteUint64(&buf, cookie)
	_ = xdr.WriteUint32(&buf, maxcount)
	return bytes.NewReader(buf.Bytes())
}

func encRemoveXattrArgs(name string) io.Reader {
	var buf bytes.Buffer
	_ = xdr.WriteXDRString(&buf, name)
	return bytes.NewReader(buf.Bytes())
}

// --- GETXATTR ---

func TestHandleGetXattr(t *testing.T) {
	t.Run("success strips and round-trips value", func(t *testing.T) {
		b := newFakeXattrBackend()
		b.store["user.foo"] = []byte("bar")
		h := xattrTestHandler(t, b)

		res := h.handleGetXattr(xattrCtx(realHandle), encGetXattrArgs("foo"))
		if res.Status != types.NFS4_OK {
			t.Fatalf("status = %d, want NFS4_OK", res.Status)
		}
		// res.Data = status(4) + opaque value
		r := bytes.NewReader(res.Data)
		st, _ := xdr.DecodeUint32(r)
		if st != types.NFS4_OK {
			t.Fatalf("encoded status = %d", st)
		}
		val, err := xdr.DecodeOpaque(r)
		if err != nil {
			t.Fatalf("decode value: %v", err)
		}
		if string(val) != "bar" {
			t.Errorf("value = %q, want %q", val, "bar")
		}
	})

	t.Run("missing -> NOXATTR", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		res := h.handleGetXattr(xattrCtx(realHandle), encGetXattrArgs("nope"))
		if res.Status != types.NFS4ERR_NOXATTR {
			t.Fatalf("status = %d, want NOXATTR", res.Status)
		}
	})

	t.Run("non-user namespace rejected -> NOXATTR", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		res := h.handleGetXattr(xattrCtx(realHandle), encGetXattrArgs("system.posix_acl_access"))
		if res.Status != types.NFS4ERR_NOXATTR {
			t.Fatalf("status = %d, want NOXATTR for system.* namespace", res.Status)
		}
	})

	t.Run("pseudo-fs -> NOTSUPP", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		ctx := xattrCtx(h.PseudoFS.GetRootHandle())
		res := h.handleGetXattr(ctx, encGetXattrArgs("foo"))
		if res.Status != types.NFS4ERR_NOTSUPP {
			t.Fatalf("status = %d, want NOTSUPP on pseudo-fs", res.Status)
		}
	})

	t.Run("no current FH -> NOFILEHANDLE", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		ctx := xattrCtx(nil)
		res := h.handleGetXattr(ctx, encGetXattrArgs("foo"))
		if res.Status != types.NFS4ERR_NOFILEHANDLE {
			t.Fatalf("status = %d, want NOFILEHANDLE", res.Status)
		}
	})
}

// --- SETXATTR ---

func TestHandleSetXattr(t *testing.T) {
	t.Run("EITHER creates and canonicalizes to user.", func(t *testing.T) {
		b := newFakeXattrBackend()
		h := xattrTestHandler(t, b)
		res := h.handleSetXattr(xattrCtx(realHandle), encSetXattrArgs(types.SETXATTR4_EITHER, "foo", []byte("v1")))
		if res.Status != types.NFS4_OK {
			t.Fatalf("status = %d, want NFS4_OK", res.Status)
		}
		if got := string(b.store["user.foo"]); got != "v1" {
			t.Errorf("store[user.foo] = %q, want v1", got)
		}
		// res.Data = status(4) + change_info4 (20 bytes)
		if len(res.Data) != 4+20 {
			t.Errorf("res.Data len = %d, want 24", len(res.Data))
		}
	})

	t.Run("CREATE on existing -> EXIST", func(t *testing.T) {
		b := newFakeXattrBackend()
		b.store["user.foo"] = []byte("x")
		h := xattrTestHandler(t, b)
		res := h.handleSetXattr(xattrCtx(realHandle), encSetXattrArgs(types.SETXATTR4_CREATE, "foo", []byte("y")))
		if res.Status != types.NFS4ERR_EXIST {
			t.Fatalf("status = %d, want EXIST", res.Status)
		}
	})

	t.Run("REPLACE on missing -> NOXATTR", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		res := h.handleSetXattr(xattrCtx(realHandle), encSetXattrArgs(types.SETXATTR4_REPLACE, "foo", []byte("y")))
		if res.Status != types.NFS4ERR_NOXATTR {
			t.Fatalf("status = %d, want NOXATTR", res.Status)
		}
	})

	t.Run("CREATE on missing succeeds", func(t *testing.T) {
		b := newFakeXattrBackend()
		h := xattrTestHandler(t, b)
		res := h.handleSetXattr(xattrCtx(realHandle), encSetXattrArgs(types.SETXATTR4_CREATE, "foo", []byte("y")))
		if res.Status != types.NFS4_OK {
			t.Fatalf("status = %d, want NFS4_OK", res.Status)
		}
	})

	t.Run("REPLACE on existing succeeds", func(t *testing.T) {
		b := newFakeXattrBackend()
		b.store["user.foo"] = []byte("x")
		h := xattrTestHandler(t, b)
		res := h.handleSetXattr(xattrCtx(realHandle), encSetXattrArgs(types.SETXATTR4_REPLACE, "foo", []byte("y")))
		if res.Status != types.NFS4_OK {
			t.Fatalf("status = %d, want NFS4_OK", res.Status)
		}
		if got := string(b.store["user.foo"]); got != "y" {
			t.Errorf("store[user.foo] = %q, want y", got)
		}
	})

	t.Run("invalid option -> INVAL", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		res := h.handleSetXattr(xattrCtx(realHandle), encSetXattrArgs(99, "foo", []byte("y")))
		if res.Status != types.NFS4ERR_INVAL {
			t.Fatalf("status = %d, want INVAL", res.Status)
		}
	})

	t.Run("pseudo-fs -> ROFS", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		ctx := xattrCtx(h.PseudoFS.GetRootHandle())
		res := h.handleSetXattr(ctx, encSetXattrArgs(types.SETXATTR4_EITHER, "foo", []byte("y")))
		if res.Status != types.NFS4ERR_ROFS {
			t.Fatalf("status = %d, want ROFS on pseudo-fs", res.Status)
		}
	})

	t.Run("oversized value -> XATTR2BIG (RFC 8276 §11.2)", func(t *testing.T) {
		b := newFakeXattrBackend()
		// EITHER skips the GetXattr pre-check, so setErr surfaces directly.
		b.setErr = metadata.ErrXattrTooLarge
		h := xattrTestHandler(t, b)
		res := h.handleSetXattr(xattrCtx(realHandle), encSetXattrArgs(types.SETXATTR4_EITHER, "foo", []byte("big")))
		if res.Status != types.NFS4ERR_XATTR2BIG {
			t.Fatalf("status = %d, want XATTR2BIG (not the generic INVAL)", res.Status)
		}
	})
}

// --- LISTXATTRS ---

func TestHandleListXattrs(t *testing.T) {
	t.Run("lists user names stripped, sorted, eof", func(t *testing.T) {
		b := newFakeXattrBackend()
		b.store["user.bbb"] = []byte("1")
		b.store["user.aaa"] = []byte("2")
		b.store["security.NTACL"] = []byte("hidden") // non-user: must be hidden
		h := xattrTestHandler(t, b)

		res := h.handleListXattrs(xattrCtx(realHandle), encListXattrsArgs(0, 1<<16))
		if res.Status != types.NFS4_OK {
			t.Fatalf("status = %d, want NFS4_OK", res.Status)
		}
		r := bytes.NewReader(res.Data)
		_, _ = xdr.DecodeUint32(r) // status
		cookie, _ := xdr.DecodeUint64(r)
		count, _ := xdr.DecodeUint32(r)
		if count != 2 {
			t.Fatalf("name count = %d, want 2 (security.* hidden)", count)
		}
		names := make([]string, count)
		for i := range names {
			s, _ := xdr.DecodeString(r)
			names[i] = s
		}
		eof, _ := xdr.DecodeBool(r)
		if names[0] != "aaa" || names[1] != "bbb" {
			t.Errorf("names = %v, want [aaa bbb]", names)
		}
		if cookie != 2 {
			t.Errorf("cookie = %d, want 2", cookie)
		}
		if !eof {
			t.Error("eof = false, want true")
		}
	})

	t.Run("empty list eof true", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		res := h.handleListXattrs(xattrCtx(realHandle), encListXattrsArgs(0, 1<<16))
		if res.Status != types.NFS4_OK {
			t.Fatalf("status = %d, want NFS4_OK", res.Status)
		}
		r := bytes.NewReader(res.Data)
		_, _ = xdr.DecodeUint32(r)
		_, _ = xdr.DecodeUint64(r)
		count, _ := xdr.DecodeUint32(r)
		eof, _ := xdr.DecodeBool(r)
		if count != 0 || !eof {
			t.Errorf("count=%d eof=%v, want 0/true", count, eof)
		}
	})

	t.Run("paging via cookie", func(t *testing.T) {
		b := newFakeXattrBackend()
		b.store["user.a"] = []byte("1")
		b.store["user.b"] = []byte("2")
		b.store["user.c"] = []byte("3")
		h := xattrTestHandler(t, b)

		// maxcount budget for exactly one short name per call:
		// fixed 16 + one entry (4 + 1 byte name + 3 pad = 8) = 24.
		res := h.handleListXattrs(xattrCtx(realHandle), encListXattrsArgs(0, 24))
		r := bytes.NewReader(res.Data)
		_, _ = xdr.DecodeUint32(r)
		cookie, _ := xdr.DecodeUint64(r)
		count, _ := xdr.DecodeUint32(r)
		if count != 1 {
			t.Fatalf("first page count = %d, want 1", count)
		}
		name0, _ := xdr.DecodeString(r)
		eof, _ := xdr.DecodeBool(r)
		if name0 != "a" || cookie != 1 || eof {
			t.Fatalf("page1: name=%q cookie=%d eof=%v", name0, cookie, eof)
		}

		// Second page from cookie=1.
		res2 := h.handleListXattrs(xattrCtx(realHandle), encListXattrsArgs(cookie, 24))
		r2 := bytes.NewReader(res2.Data)
		_, _ = xdr.DecodeUint32(r2)
		_, _ = xdr.DecodeUint64(r2)
		c2, _ := xdr.DecodeUint32(r2)
		n2, _ := xdr.DecodeString(r2)
		if c2 != 1 || n2 != "b" {
			t.Fatalf("page2: count=%d name=%q", c2, n2)
		}
	})

	t.Run("stale cookie -> BAD_COOKIE", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		res := h.handleListXattrs(xattrCtx(realHandle), encListXattrsArgs(5, 1<<16))
		if res.Status != types.NFS4ERR_BAD_COOKIE {
			t.Fatalf("status = %d, want BAD_COOKIE", res.Status)
		}
	})

	t.Run("maxcount too small -> TOOSMALL", func(t *testing.T) {
		b := newFakeXattrBackend()
		b.store["user.longname"] = []byte("1")
		h := xattrTestHandler(t, b)
		res := h.handleListXattrs(xattrCtx(realHandle), encListXattrsArgs(0, 16))
		if res.Status != types.NFS4ERR_TOOSMALL {
			t.Fatalf("status = %d, want TOOSMALL", res.Status)
		}
	})

	t.Run("pseudo-fs -> NOTSUPP", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		ctx := xattrCtx(h.PseudoFS.GetRootHandle())
		res := h.handleListXattrs(ctx, encListXattrsArgs(0, 1<<16))
		if res.Status != types.NFS4ERR_NOTSUPP {
			t.Fatalf("status = %d, want NOTSUPP on pseudo-fs", res.Status)
		}
	})
}

// --- REMOVEXATTR ---

func TestHandleRemoveXattr(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		b := newFakeXattrBackend()
		b.store["user.foo"] = []byte("v")
		h := xattrTestHandler(t, b)
		res := h.handleRemoveXattr(xattrCtx(realHandle), encRemoveXattrArgs("foo"))
		if res.Status != types.NFS4_OK {
			t.Fatalf("status = %d, want NFS4_OK", res.Status)
		}
		if _, ok := b.store["user.foo"]; ok {
			t.Error("user.foo still present after remove")
		}
		if len(res.Data) != 4+20 {
			t.Errorf("res.Data len = %d, want 24 (status + change_info4)", len(res.Data))
		}
	})

	t.Run("missing -> NOXATTR", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		res := h.handleRemoveXattr(xattrCtx(realHandle), encRemoveXattrArgs("foo"))
		if res.Status != types.NFS4ERR_NOXATTR {
			t.Fatalf("status = %d, want NOXATTR", res.Status)
		}
	})

	t.Run("non-user namespace -> NOXATTR", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		res := h.handleRemoveXattr(xattrCtx(realHandle), encRemoveXattrArgs("trusted.foo"))
		if res.Status != types.NFS4ERR_NOXATTR {
			t.Fatalf("status = %d, want NOXATTR", res.Status)
		}
	})

	t.Run("stream-backed (pre-check passes, remove not-found) -> NOXATTR", func(t *testing.T) {
		// Models a name that exists via the stream backing (GetXattr pre-check
		// finds it) but whose RemoveXattr returns ErrNotFound because PR1 does
		// not delete stream entities. RFC 8276 §11.2: surface NOXATTR, not the
		// generic NOENT the store error maps to.
		b := newFakeXattrBackend()
		b.store["user.foo"] = []byte("v") // pre-check sees it
		b.removeErr = &metadata.StoreError{Code: metadata.ErrNotFound, Message: "xattr not found"}
		h := xattrTestHandler(t, b)
		res := h.handleRemoveXattr(xattrCtx(realHandle), encRemoveXattrArgs("foo"))
		if res.Status != types.NFS4ERR_NOXATTR {
			t.Fatalf("status = %d, want NOXATTR (not NOENT)", res.Status)
		}
	})

	t.Run("pseudo-fs -> ROFS", func(t *testing.T) {
		h := xattrTestHandler(t, newFakeXattrBackend())
		ctx := xattrCtx(h.PseudoFS.GetRootHandle())
		res := h.handleRemoveXattr(ctx, encRemoveXattrArgs("foo"))
		if res.Status != types.NFS4ERR_ROFS {
			t.Fatalf("status = %d, want ROFS on pseudo-fs", res.Status)
		}
	})
}

// --- name canonicalization ---

func TestCanonicalizeXattrName(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"foo", "user.foo", true},
		{"user.foo", "user.foo", true},
		{"user.", "", false}, // bare prefix, no key -> invalid
		{"system.posix_acl_access", "", false},
		{"trusted.x", "", false},
		{"security.NTACL", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := canonicalizeXattrName(c.in)
		if ok != c.wantOK || got != c.wantName {
			t.Errorf("canonicalizeXattrName(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.wantName, c.wantOK)
		}
	}
}

func TestStripXattrPrefix(t *testing.T) {
	if got := stripXattrPrefix("user.foo"); got != "foo" {
		t.Errorf("stripXattrPrefix(user.foo) = %q, want foo", got)
	}
	if got := stripXattrPrefix("foo"); got != "foo" {
		t.Errorf("stripXattrPrefix(foo) = %q, want foo", got)
	}
}

// --- v4.2 op gating in dispatchOne ---

// TestXattrOpsGatedToV42 verifies that the RFC 8276 xattr ops are dispatched
// only under v4.2 (isV42=true) and return NFS4ERR_NOTSUPP under v4.0/v4.1.
func TestXattrOpsGatedToV42(t *testing.T) {
	b := newFakeXattrBackend()
	b.store["user.foo"] = []byte("bar")
	h := xattrTestHandler(t, b)

	for _, op := range []uint32{types.OP_GETXATTR, types.OP_SETXATTR, types.OP_LISTXATTRS, types.OP_REMOVEXATTR} {
		// Under v4.1 (isV42=false): NOTSUPP, args not consumed.
		ctx := xattrCtx(realHandle)
		res := h.dispatchOne(ctx, nil, op, bytes.NewReader(nil), true, false)
		if res.Status != types.NFS4ERR_NOTSUPP {
			t.Errorf("%s under v4.1: status = %d, want NOTSUPP", types.OpName(op), res.Status)
		}
	}

	// Under v4.2 (isV42=true): GETXATTR with valid args reaches the handler.
	ctx := xattrCtx(realHandle)
	res := h.dispatchOne(ctx, nil, types.OP_GETXATTR, encGetXattrArgs("foo"), true, true)
	if res.Status != types.NFS4_OK {
		t.Errorf("GETXATTR under v4.2: status = %d, want NFS4_OK", res.Status)
	}
}

// --- backend error mapping ---

func TestXattrBackendErrorMapping(t *testing.T) {
	b := newFakeXattrBackend()
	b.forceErr = errors.New("boom")
	h := xattrTestHandler(t, b)
	res := h.handleGetXattr(xattrCtx(realHandle), encGetXattrArgs("foo"))
	if res.Status == types.NFS4_OK {
		t.Fatalf("expected non-OK status on backend error, got OK")
	}
}
