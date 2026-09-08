package handlers

import (
	"bytes"
	"io"
	"slices"

	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// krb5OIDDER is the DER-encoded OID for the Kerberos 5 GSS-API mechanism.
// OID: 1.2.840.113554.1.2.2
// DER encoding: tag(0x06) + length(0x09) + OID value bytes.
//
// Per RFC 7530 Section 3.2.1, sec_oid4 is opaque<> containing the
// mechanism OID in DER format.
var krb5OIDDER = []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02}

// RPCSEC_GSS auth flavor value per RFC 2203.
const authRPCSECGSS uint32 = 6

// The non-GSS auth flavors SECINFO can offer.
const (
	authNoneFlavor uint32 = 0
	authSysFlavor  uint32 = 1
)

// handleSecInfo implements the SECINFO operation (RFC 7530 Section 16.31).
// Resolves a name in the current directory, then returns the security
// mechanisms usable on what it resolved to -- AUTH_SYS, AUTH_NONE and the
// Kerberos services, less whatever the target share's export auth-flavor
// policy refuses.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_BADXDR, NFS4ERR_INVAL, NFS4ERR_NOTDIR,
// NFS4ERR_NOENT, NFS4ERR_ACCESS.
func (h *Handler) handleSecInfo(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return secInfoErr(types.OP_SECINFO, status)
	}

	// Read the component name
	name, err := xdr.DecodeString(reader)
	if err != nil {
		return secInfoErr(types.OP_SECINFO, types.NFS4ERR_BADXDR)
	}

	// Validate UTF-8 filename
	if status := types.ValidateUTF8Filename(name); status != types.NFS4_OK {
		return secInfoErr(types.OP_SECINFO, status)
	}

	// SECINFO reports on an existing directory entry, so the name has to
	// resolve before any flavor list is built. The resolved handle is what the
	// flavor list is about: a name that crosses an export junction lands in
	// another share, whose auth-flavor policy -- not the parent's -- is the one
	// the client will meet.
	status, target := h.secInfoLookupStatus(ctx, name)
	if status != types.NFS4_OK {
		logger.Debug("SECINFO: name did not resolve",
			"name", name,
			"status", status,
			"client", ctx.ClientAddr)
		return secInfoErr(types.OP_SECINFO, status)
	}

	// NFSv4.1 consumes the current filehandle here, so a following operation
	// finds none (RFC 8881 Section 2.6.3.1.1.8). NFSv4.0 retains it
	// (RFC 7530 Section 16.31.4), and that difference is deliberate in the
	// spec: 4.1 relies on it to resolve NFS4ERR_WRONGSEC.
	if ctx.MinorVersionAccepted && ctx.MinorVersion > 0 {
		ctx.CurrentFH = nil
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SECINFO,
		Data:   h.secInfoFlavorsForHandle(target),
	}
}

// secInfoLookupStatus resolves name under the current filehandle and reports
// the status of that resolution together with the handle it resolved to,
// leaving the current filehandle unchanged. The returned handle is the current
// filehandle itself when the resolution did not advance it.
//
// SECINFO applies the same access methodology as LOOKUP (RFC 7530
// Section 16.31.4): a non-directory current filehandle answers NFS4ERR_NOTDIR,
// a name that is not there NFS4ERR_NOENT, and a directory the caller may not
// search NFS4ERR_ACCESS. Reusing the LOOKUP path is what keeps the two in
// step; only LOOKUP's effect on the current filehandle is discarded.
func (h *Handler) secInfoLookupStatus(ctx *types.CompoundContext, name string) (uint32, []byte) {
	saved := ctx.CurrentFH
	defer func() { ctx.CurrentFH = saved }()

	var status uint32
	if pseudofs.IsPseudoFSHandle(saved) {
		status = h.lookupInPseudoFS(ctx, name).Status
	} else {
		status = h.lookupInRealFS(ctx, name).Status
	}
	target := ctx.CurrentFH

	// A WRONGSEC from the export's auth-flavor policy is the one status that
	// must not reach the caller: it is the answer to the question SECINFO was
	// asked, not a refusal to answer it. RFC 8881 Section 2.6.3.1.1.5 forbids
	// it outright from SECINFO and SECINFO_NO_NAME, and RFC 7530 Section 13.2
	// leaves it off SECINFO's error list. The flavor list goes back instead,
	// which is what lets the client pick a flavor it can actually use; whether
	// the entry exists goes unreported, and it is unreachable on this flavor
	// either way.
	if status == types.NFS4ERR_WRONGSEC {
		// The lookup never advanced the filehandle, so the policy reported is
		// the refusing share's own -- which is the one the client needs.
		status = types.NFS4_OK
	}
	return status, target
}

// secInfoErr builds a status-only result for SECINFO or SECINFO_NO_NAME.
func secInfoErr(opCode, status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: opCode,
		Data:   encodeStatusOnly(status),
	}
}

// secInfoFlavorsForHandle encodes the SECINFO4res flavor list for the object
// handle designates, narrowed to the export auth-flavor policy of the share
// owning it. A pseudo-filesystem handle belongs to no share and gets the
// server-wide list.
func (h *Handler) secInfoFlavorsForHandle(handle []byte) []byte {
	return encodeSecInfoFlavors(h.KerberosEnabled, h.shareForHandle(handle))
}

// shareForHandle resolves the share a real-filesystem filehandle belongs to.
// It returns nil for a pseudo-filesystem handle, an undecodable handle, or a
// share the registry no longer holds -- all cases where no per-share policy is
// known.
func (h *Handler) shareForHandle(handle []byte) *runtime.Share {
	if h.Registry == nil || pseudofs.IsPseudoFSHandle(handle) {
		return nil
	}
	shareName, _, err := metadata.DecodeFileHandle(metadata.FileHandle(handle))
	if err != nil {
		return nil
	}
	share, err := h.Registry.GetShare(shareName)
	if err != nil {
		return nil
	}
	return share
}

// encodeSecInfoFlavors encodes a SECINFO4res body: NFS4_OK followed by the
// secinfo4 array the server offers for one object, most preferred first
// (RFC 7530 Section 16.31.4). SECINFO_NO_NAME shares the result type
// (RFC 8881 Section 18.45.2) and so shares this encoding.
//
// The candidate set is server-wide -- it depends on whether Kerberos is
// configured -- but the answer is per-object, because the share carries the
// export auth-flavor policy buildV4AuthContext enforces on every real
// operation. Advertising a flavor that policy rejects is the exact failure
// SECINFO exists to prevent: the client picks a listed flavor, gets
// NFS4ERR_WRONGSEC, asks SECINFO again and is handed the same list. A nil
// share means no policy is known, so nothing is narrowed.
func encodeSecInfoFlavors(kerberosEnabled bool, share *runtime.Share) []byte {
	var gssServices []uint32
	if kerberosEnabled {
		gssServices = []uint32{gss.RPCGSSSvcPrivacy, gss.RPCGSSSvcIntegrity, gss.RPCGSSSvcNone}
	}
	rawFlavors := []uint32{authSysFlavor, authNoneFlavor}

	if share != nil {
		// RequireKerberos refuses every non-GSS flavor; AllowAuthSys=false
		// refuses only AUTH_SYS. Same two conditions, same order, as the
		// checks in buildV4AuthContext.
		switch {
		case share.RequireKerberos:
			rawFlavors = nil
		case !share.AllowAuthSys:
			rawFlavors = []uint32{authNoneFlavor}
		}
		// MinKerberosLevel is a floor on the negotiated GSS service, so any
		// weaker service is unusable on this share.
		gssServices = slices.DeleteFunc(gssServices, func(svc uint32) bool {
			return !auth.MeetsMinKerberosLevel(share.MinKerberosLevel, svc)
		})
	}

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteUint32(&buf, uint32(len(gssServices)+len(rawFlavors)))
	for _, svc := range gssServices {
		encodeSecInfoGSSEntry(&buf, svc)
	}
	for _, flavor := range rawFlavors {
		_ = xdr.WriteUint32(&buf, flavor)
	}
	return buf.Bytes()
}

// encodeSecInfoGSSEntry encodes a single RPCSEC_GSS secinfo4 entry.
//
// Per RFC 7530 Section 16.31, each RPCSEC_GSS entry consists of:
//   - flavor: uint32 = 6 (RPCSEC_GSS)
//   - oid: sec_oid4 (XDR opaque<> containing KRB5 OID in DER encoding)
//   - qop: uint32 = 0 (default quality of protection)
//   - service: rpc_gss_svc_t (1=none, 2=integrity, 3=privacy)
func encodeSecInfoGSSEntry(buf *bytes.Buffer, service uint32) {
	_ = xdr.WriteUint32(buf, authRPCSECGSS) // flavor = 6
	_ = xdr.WriteXDROpaque(buf, krb5OIDDER) // oid (KRB5 mechanism OID)
	_ = xdr.WriteUint32(buf, 0)             // qop = 0 (default)
	_ = xdr.WriteUint32(buf, service)       // service level
}
