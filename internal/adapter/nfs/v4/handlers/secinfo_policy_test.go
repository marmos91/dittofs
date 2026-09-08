package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// secInfoEntry is one decoded secinfo4 entry. GSSService is zero for the
// non-GSS flavors, which carry no service level.
type secInfoEntry struct {
	Flavor     uint32
	GSSService uint32
}

// decodeSecInfoFlavors parses a SECINFO4res body into its flavor entries.
func decodeSecInfoFlavors(t *testing.T, data []byte) []secInfoEntry {
	t.Helper()

	reader := bytes.NewReader(data)
	status, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}
	count, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode array length: %v", err)
	}

	entries := make([]secInfoEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		flavor, err := xdr.DecodeUint32(reader)
		if err != nil {
			t.Fatalf("decode flavor[%d]: %v", i, err)
		}
		entry := secInfoEntry{Flavor: flavor}
		if flavor == authRPCSECGSS {
			if _, err := xdr.DecodeOpaque(reader); err != nil {
				t.Fatalf("decode oid[%d]: %v", i, err)
			}
			if _, err := xdr.DecodeUint32(reader); err != nil {
				t.Fatalf("decode qop[%d]: %v", i, err)
			}
			svc, err := xdr.DecodeUint32(reader)
			if err != nil {
				t.Fatalf("decode service[%d]: %v", i, err)
			}
			entry.GSSService = svc
		}
		entries = append(entries, entry)
	}
	if reader.Len() != 0 {
		t.Fatalf("SECINFO4res has %d trailing bytes", reader.Len())
	}
	return entries
}

// secInfoOnName runs SECINFO for name against the fixture's share root and
// returns the decoded flavor list.
func secInfoOnName(t *testing.T, fx *realFSTestFixture, current metadata.FileHandle, name string) []secInfoEntry {
	t.Helper()

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = append([]byte(nil), current...)

	result := fx.handler.handleSecInfo(ctx, bytes.NewReader(encodeSecInfoArgs(name)))
	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO(%q) status = %d, want NFS4_OK", name, result.Status)
	}
	return decodeSecInfoFlavors(t, result.Data)
}

func hasFlavor(entries []secInfoEntry, flavor uint32) bool {
	for _, e := range entries {
		if e.Flavor == flavor {
			return true
		}
	}
	return false
}

func hasGSSService(entries []secInfoEntry, service uint32) bool {
	for _, e := range entries {
		if e.Flavor == authRPCSECGSS && e.GSSService == service {
			return true
		}
	}
	return false
}

const (
	testAuthNoneFlavor = 0
	testAuthSysFlavor  = 1
)

// TestSecInfo_AllowAuthSysFalseDropsAuthSys pins the AllowAuthSys narrowing:
// buildV4AuthContext answers NFS4ERR_WRONGSEC on AUTH_SYS for such a share, so
// SECINFO must not offer it. AUTH_NONE is untouched by that field.
func TestSecInfo_AllowAuthSysFalseDropsAuthSys(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fx.createTestFile(t, fx.rootHandle, "hello.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	entries := secInfoOnName(t, fx, fx.rootHandle, "hello.txt")

	if hasFlavor(entries, testAuthSysFlavor) {
		t.Errorf("SECINFO offered AUTH_SYS on an allow_auth_sys=false share: %+v", entries)
	}
	if !hasFlavor(entries, testAuthNoneFlavor) {
		t.Errorf("SECINFO dropped AUTH_NONE, which allow_auth_sys=false does not refuse: %+v", entries)
	}
}

// TestSecInfo_RequireKerberosDropsNonGSS pins the RequireKerberos narrowing:
// the share refuses every non-GSS flavor, so neither AUTH_SYS nor AUTH_NONE may
// be advertised. Kerberos is enabled here, so the list is non-empty.
func TestSecInfo_RequireKerberosDropsNonGSS(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fx.handler.KerberosEnabled = true
	fx.createTestFile(t, fx.rootHandle, "hello.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	entries := secInfoOnName(t, fx, fx.rootHandle, "hello.txt")

	if hasFlavor(entries, testAuthSysFlavor) || hasFlavor(entries, testAuthNoneFlavor) {
		t.Errorf("SECINFO offered a non-GSS flavor on a require_kerberos share: %+v", entries)
	}
	if len(entries) != 3 {
		t.Errorf("SECINFO offered %d flavors, want the 3 Kerberos services: %+v", len(entries), entries)
	}
}

// TestSecInfo_MinKerberosLevelDropsWeakerServices pins the MinKerberosLevel
// floor: a krb5p share rejects a negotiated krb5 or krb5i session, so only the
// privacy entry may be advertised.
func TestSecInfo_MinKerberosLevelDropsWeakerServices(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fx.handler.KerberosEnabled = true
	fx.createTestFile(t, fx.rootHandle, "hello.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	if err := fx.rt.SetMinKerberosLevelForTesting("/export", models.KerberosLevelKrb5p); err != nil {
		t.Fatalf("SetMinKerberosLevelForTesting: %v", err)
	}

	entries := secInfoOnName(t, fx, fx.rootHandle, "hello.txt")

	if !hasGSSService(entries, gss.RPCGSSSvcPrivacy) {
		t.Errorf("SECINFO dropped krb5p on a min_kerberos_level=krb5p share: %+v", entries)
	}
	if hasGSSService(entries, gss.RPCGSSSvcIntegrity) {
		t.Errorf("SECINFO offered krb5i below the krb5p floor: %+v", entries)
	}
	if hasGSSService(entries, gss.RPCGSSSvcNone) {
		t.Errorf("SECINFO offered plain krb5 below the krb5p floor: %+v", entries)
	}
}

// TestSecInfo_JunctionReportsTargetPolicy pins that a name resolving across an
// export junction reports the target share's policy. The current filehandle is
// the pseudo-fs root, which has no policy of its own; the flavor list has to
// come from the share the name lands in.
func TestSecInfo_JunctionReportsTargetPolicy(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}
	// RegisterShareForTesting leaves RootHandle empty; the junction crossing
	// needs it to reach the share root the way a real export does.
	if err := fx.rt.SetRootHandleForTesting("/export", fx.rootHandle); err != nil {
		t.Fatalf("SetRootHandleForTesting: %v", err)
	}

	pseudoRoot := fx.handler.PseudoFS.GetRootHandle()
	entries := secInfoOnName(t, fx, pseudoRoot, "export")

	if hasFlavor(entries, testAuthSysFlavor) {
		t.Errorf("SECINFO across a junction offered AUTH_SYS, which the target share refuses: %+v", entries)
	}
	if !hasFlavor(entries, testAuthNoneFlavor) {
		t.Errorf("SECINFO across a junction dropped AUTH_NONE: %+v", entries)
	}
}

// TestSecInfoNoName_PseudoFSKeepsServerWideList pins that a pseudo-fs
// filehandle, which belongs to no share, is answered with the server-wide list
// even when every configured share narrows its own.
func TestSecInfoNoName_PseudoFSKeepsServerWideList(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", false, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = append([]byte(nil), fx.handler.PseudoFS.GetRootHandle()...)

	args := encodeSecInfoNoNameArgs(types.SECINFO_STYLE4_CURRENT_FH)

	result := fx.handler.handleSecInfoNoName(ctx, nil, bytes.NewReader(args))
	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO_NO_NAME status = %d, want NFS4_OK", result.Status)
	}

	entries := decodeSecInfoFlavors(t, result.Data)
	if !hasFlavor(entries, testAuthSysFlavor) || !hasFlavor(entries, testAuthNoneFlavor) {
		t.Errorf("SECINFO_NO_NAME on the pseudo-fs root narrowed to a share's policy: %+v", entries)
	}
}
