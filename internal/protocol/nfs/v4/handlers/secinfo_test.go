package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// SECINFO Tests - Without Kerberos (AUTH_SYS + AUTH_NONE only)
// ============================================================================

func TestHandleSecInfo_TwoFlavorsNoKerberos(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)
	// KerberosEnabled defaults to false

	rootHandle := pfs.GetRootHandle()
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(rootHandle)),
	}
	copy(ctx.CurrentFH, rootHandle)

	// Encode SECINFO args: component name
	var args bytes.Buffer
	_ = xdr.WriteXDRString(&args, "testfile")

	result := h.handleSecInfo(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO status = %d, want NFS4_OK", result.Status)
	}

	// Parse response: status + array length + flavors
	reader := bytes.NewReader(result.Data)

	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("SECINFO encoded status = %d, want NFS4_OK", status)
	}

	arrayLen, _ := xdr.DecodeUint32(reader)
	if arrayLen != 2 {
		t.Fatalf("SECINFO array length = %d, want 2", arrayLen)
	}

	flavor1, _ := xdr.DecodeUint32(reader)
	flavor2, _ := xdr.DecodeUint32(reader)

	// First flavor should be AUTH_SYS (1), second AUTH_NONE (0)
	if flavor1 != 1 {
		t.Errorf("SECINFO flavor[0] = %d, want AUTH_SYS (1)", flavor1)
	}
	if flavor2 != 0 {
		t.Errorf("SECINFO flavor[1] = %d, want AUTH_NONE (0)", flavor2)
	}
}

func TestHandleSecInfo_ClearsFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	rootHandle := pfs.GetRootHandle()
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(rootHandle)),
	}
	copy(ctx.CurrentFH, rootHandle)

	// Encode SECINFO args: component name
	var args bytes.Buffer
	_ = xdr.WriteXDRString(&args, "testfile")

	result := h.handleSecInfo(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO status = %d, want NFS4_OK", result.Status)
	}

	// Per RFC 7530 Section 16.31.4: CurrentFH should be cleared after SECINFO
	if ctx.CurrentFH != nil {
		t.Errorf("CurrentFH should be nil after SECINFO, got %q", string(ctx.CurrentFH))
	}
}

func TestHandleSecInfo_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  nil, // no filehandle
	}

	var args bytes.Buffer
	_ = xdr.WriteXDRString(&args, "testfile")

	result := h.handleSecInfo(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("SECINFO without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestHandleSecInfo_BadXDR(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	rootHandle := pfs.GetRootHandle()
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(rootHandle)),
	}
	copy(ctx.CurrentFH, rootHandle)

	// Send truncated args (can't decode string)
	result := h.handleSecInfo(ctx, bytes.NewReader([]byte{0x00}))

	if result.Status != types.NFS4ERR_BADXDR {
		t.Errorf("SECINFO with bad XDR status = %d, want NFS4ERR_BADXDR (%d)",
			result.Status, types.NFS4ERR_BADXDR)
	}
}

// ============================================================================
// SECINFO Tests - With Kerberos (RPCSEC_GSS + AUTH_SYS + AUTH_NONE)
// ============================================================================

func TestHandleSecInfo_KerberosEnabled_FiveEntries(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)
	h.KerberosEnabled = true

	rootHandle := pfs.GetRootHandle()
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(rootHandle)),
	}
	copy(ctx.CurrentFH, rootHandle)

	var args bytes.Buffer
	_ = xdr.WriteXDRString(&args, "testfile")

	result := h.handleSecInfo(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)

	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("SECINFO encoded status = %d, want NFS4_OK", status)
	}

	arrayLen, _ := xdr.DecodeUint32(reader)
	if arrayLen != 5 {
		t.Fatalf("SECINFO array length = %d, want 5 (3 GSS + AUTH_SYS + AUTH_NONE)", arrayLen)
	}

	// Entry 1: krb5p (flavor=6, oid=KRB5, qop=0, service=3)
	flavor1, _ := xdr.DecodeUint32(reader)
	if flavor1 != 6 {
		t.Fatalf("entry[0] flavor = %d, want RPCSEC_GSS (6)", flavor1)
	}
	oid1, _ := xdr.DecodeOpaque(reader)
	if !bytes.Equal(oid1, krb5OIDDER) {
		t.Fatalf("entry[0] OID mismatch")
	}
	qop1, _ := xdr.DecodeUint32(reader)
	if qop1 != 0 {
		t.Fatalf("entry[0] qop = %d, want 0", qop1)
	}
	svc1, _ := xdr.DecodeUint32(reader)
	if svc1 != 3 {
		t.Fatalf("entry[0] service = %d, want 3 (privacy)", svc1)
	}

	// Entry 2: krb5i (flavor=6, oid=KRB5, qop=0, service=2)
	flavor2, _ := xdr.DecodeUint32(reader)
	if flavor2 != 6 {
		t.Fatalf("entry[1] flavor = %d, want RPCSEC_GSS (6)", flavor2)
	}
	oid2, _ := xdr.DecodeOpaque(reader)
	if !bytes.Equal(oid2, krb5OIDDER) {
		t.Fatalf("entry[1] OID mismatch")
	}
	qop2, _ := xdr.DecodeUint32(reader)
	if qop2 != 0 {
		t.Fatalf("entry[1] qop = %d, want 0", qop2)
	}
	svc2, _ := xdr.DecodeUint32(reader)
	if svc2 != 2 {
		t.Fatalf("entry[1] service = %d, want 2 (integrity)", svc2)
	}

	// Entry 3: krb5 (flavor=6, oid=KRB5, qop=0, service=1)
	flavor3, _ := xdr.DecodeUint32(reader)
	if flavor3 != 6 {
		t.Fatalf("entry[2] flavor = %d, want RPCSEC_GSS (6)", flavor3)
	}
	oid3, _ := xdr.DecodeOpaque(reader)
	if !bytes.Equal(oid3, krb5OIDDER) {
		t.Fatalf("entry[2] OID mismatch")
	}
	qop3, _ := xdr.DecodeUint32(reader)
	if qop3 != 0 {
		t.Fatalf("entry[2] qop = %d, want 0", qop3)
	}
	svc3, _ := xdr.DecodeUint32(reader)
	if svc3 != 1 {
		t.Fatalf("entry[2] service = %d, want 1 (none/auth-only)", svc3)
	}

	// Entry 4: AUTH_SYS (flavor=1)
	flavor4, _ := xdr.DecodeUint32(reader)
	if flavor4 != 1 {
		t.Fatalf("entry[3] flavor = %d, want AUTH_SYS (1)", flavor4)
	}

	// Entry 5: AUTH_NONE (flavor=0)
	flavor5, _ := xdr.DecodeUint32(reader)
	if flavor5 != 0 {
		t.Fatalf("entry[4] flavor = %d, want AUTH_NONE (0)", flavor5)
	}
}

func TestHandleSecInfo_KerberosEnabled_ClearsFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)
	h.KerberosEnabled = true

	rootHandle := pfs.GetRootHandle()
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(rootHandle)),
	}
	copy(ctx.CurrentFH, rootHandle)

	var args bytes.Buffer
	_ = xdr.WriteXDRString(&args, "testfile")

	result := h.handleSecInfo(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO status = %d, want NFS4_OK", result.Status)
	}

	// Per RFC 7530 Section 16.31.4: CurrentFH should be cleared
	if ctx.CurrentFH != nil {
		t.Errorf("CurrentFH should be nil after SECINFO with Kerberos, got %q", string(ctx.CurrentFH))
	}
}

func TestHandleSecInfo_KerberosEnabled_SecurityOrder(t *testing.T) {
	// Verify the ordering is: krb5p > krb5i > krb5 > AUTH_SYS > AUTH_NONE
	// (most secure first per RFC 7530 convention)
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)
	h.KerberosEnabled = true

	rootHandle := pfs.GetRootHandle()
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(rootHandle)),
	}
	copy(ctx.CurrentFH, rootHandle)

	var args bytes.Buffer
	_ = xdr.WriteXDRString(&args, "testfile")

	result := h.handleSecInfo(ctx, bytes.NewReader(args.Bytes()))

	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status
	_, _ = xdr.DecodeUint32(reader) // array length

	// Collect service levels from RPCSEC_GSS entries
	services := make([]uint32, 0, 3)
	for i := 0; i < 3; i++ {
		flavor, _ := xdr.DecodeUint32(reader)
		if flavor != 6 {
			t.Fatalf("entry[%d] should be RPCSEC_GSS (6), got %d", i, flavor)
		}
		_, _ = xdr.DecodeOpaque(reader) // oid
		_, _ = xdr.DecodeUint32(reader)    // qop
		svc, _ := xdr.DecodeUint32(reader)
		services = append(services, svc)
	}

	// Verify order: privacy (3) > integrity (2) > none (1)
	if services[0] != 3 {
		t.Errorf("first GSS entry service = %d, want 3 (privacy)", services[0])
	}
	if services[1] != 2 {
		t.Errorf("second GSS entry service = %d, want 2 (integrity)", services[1])
	}
	if services[2] != 1 {
		t.Errorf("third GSS entry service = %d, want 1 (none)", services[2])
	}
}

func TestHandleSecInfo_KRB5OIDFormat(t *testing.T) {
	// Verify the KRB5 OID DER encoding is correct
	// OID 1.2.840.113554.1.2.2
	expected := []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02}

	if !bytes.Equal(krb5OIDDER, expected) {
		t.Fatalf("KRB5 OID DER = %x, want %x", krb5OIDDER, expected)
	}

	// Verify it starts with ASN.1 OID tag (0x06) and has correct length (0x09)
	if krb5OIDDER[0] != 0x06 {
		t.Errorf("OID tag = 0x%02x, want 0x06", krb5OIDDER[0])
	}
	if krb5OIDDER[1] != 0x09 {
		t.Errorf("OID length = %d, want 9", krb5OIDDER[1])
	}
}

// ============================================================================
// encodeSecInfoGSSEntry Unit Tests
// ============================================================================

func TestEncodeSecInfoGSSEntry_Privacy(t *testing.T) {
	var buf bytes.Buffer
	encodeSecInfoGSSEntry(&buf, rpcGSSSvcPrivacy)

	reader := bytes.NewReader(buf.Bytes())

	flavor, _ := xdr.DecodeUint32(reader)
	if flavor != 6 {
		t.Fatalf("flavor = %d, want 6", flavor)
	}

	oid, _ := xdr.DecodeOpaque(reader)
	if !bytes.Equal(oid, krb5OIDDER) {
		t.Fatalf("OID mismatch")
	}

	qop, _ := xdr.DecodeUint32(reader)
	if qop != 0 {
		t.Fatalf("qop = %d, want 0", qop)
	}

	svc, _ := xdr.DecodeUint32(reader)
	if svc != 3 {
		t.Fatalf("service = %d, want 3 (privacy)", svc)
	}
}

func TestEncodeSecInfoGSSEntry_Integrity(t *testing.T) {
	var buf bytes.Buffer
	encodeSecInfoGSSEntry(&buf, rpcGSSSvcIntegrity)

	reader := bytes.NewReader(buf.Bytes())

	flavor, _ := xdr.DecodeUint32(reader)
	if flavor != 6 {
		t.Fatalf("flavor = %d, want 6", flavor)
	}

	_, _ = xdr.DecodeOpaque(reader) // oid
	_, _ = xdr.DecodeUint32(reader)    // qop

	svc, _ := xdr.DecodeUint32(reader)
	if svc != 2 {
		t.Fatalf("service = %d, want 2 (integrity)", svc)
	}
}

func TestEncodeSecInfoGSSEntry_None(t *testing.T) {
	var buf bytes.Buffer
	encodeSecInfoGSSEntry(&buf, rpcGSSSvcNone)

	reader := bytes.NewReader(buf.Bytes())

	flavor, _ := xdr.DecodeUint32(reader)
	if flavor != 6 {
		t.Fatalf("flavor = %d, want 6", flavor)
	}

	_, _ = xdr.DecodeOpaque(reader) // oid
	_, _ = xdr.DecodeUint32(reader)    // qop

	svc, _ := xdr.DecodeUint32(reader)
	if svc != 1 {
		t.Fatalf("service = %d, want 1 (none)", svc)
	}
}
