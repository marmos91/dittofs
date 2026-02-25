package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// krb5OIDDER is the DER-encoded OID for the Kerberos 5 GSS-API mechanism.
// OID: 1.2.840.113554.1.2.2
// DER encoding: tag(0x06) + length(0x09) + OID value bytes.
//
// Per RFC 7530 Section 3.2.1, sec_oid4 is opaque<> containing the
// mechanism OID in DER format.
var krb5OIDDER = []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02}

// RPCSEC_GSS service levels for SECINFO responses.
// These match the rpc_gss_svc_t values from RFC 2203 Section 5.3.1.
const (
	rpcGSSSvcNone      uint32 = 1
	rpcGSSSvcIntegrity uint32 = 2
	rpcGSSSvcPrivacy   uint32 = 3
)

// RPCSEC_GSS auth flavor value per RFC 2203.
const authRPCSECGSS uint32 = 6

// handleSecInfo implements the SECINFO operation (RFC 7530 Section 16.31).
//
// SECINFO returns the security mechanisms available for a given name in
// a directory. When Kerberos is enabled, returns RPCSEC_GSS entries for
// krb5p (privacy), krb5i (integrity), and krb5 (auth only) in addition
// to AUTH_SYS and AUTH_NONE. When Kerberos is not enabled, returns only
// AUTH_SYS and AUTH_NONE.
//
// Wire format args:
//
//	name:  component4 (XDR string -- the filename to query security for)
//
// Wire format res:
//
//	nfsstat4:       uint32
//	SECINFO4resok:  array of secinfo4 entries
//	  each entry:   flavor (uint32) [+ rpcsec_gss_info if flavor == 6]
//	  rpcsec_gss_info:
//	    oid:        sec_oid4 (XDR opaque<> -- mechanism OID in DER)
//	    qop:        uint32 (quality of protection, 0 = default)
//	    service:    rpc_gss_svc_t (1=none, 2=integrity, 3=privacy)
//
// Per RFC 7530 Section 16.31.4: SECINFO consumes the current filehandle.
// After SECINFO, the current FH is cleared.
func (h *Handler) handleSecInfo(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_SECINFO,
			Data:   encodeStatusOnly(status),
		}
	}

	// Read the component name
	name, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SECINFO,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Per RFC 7530 Section 16.31.4: SECINFO consumes the current filehandle.
	ctx.CurrentFH = nil

	// Build response based on whether Kerberos is enabled
	const (
		authNoneFlavor = 0
		authSysFlavor  = 1
	)

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)

	if h.KerberosEnabled {
		// Return 5 entries: krb5p, krb5i, krb5, AUTH_SYS, AUTH_NONE
		// Ordered most secure first per RFC 7530 convention
		logger.Debug("SECINFO: returning RPCSEC_GSS (krb5p, krb5i, krb5) + AUTH_SYS + AUTH_NONE",
			"name", name,
			"client", ctx.ClientAddr)

		_ = xdr.WriteUint32(&buf, 5) // array length: 5 entries

		// 1. krb5p (privacy)
		encodeSecInfoGSSEntry(&buf, rpcGSSSvcPrivacy)
		// 2. krb5i (integrity)
		encodeSecInfoGSSEntry(&buf, rpcGSSSvcIntegrity)
		// 3. krb5 (auth only, no integrity/privacy)
		encodeSecInfoGSSEntry(&buf, rpcGSSSvcNone)
		// 4. AUTH_SYS fallback
		_ = xdr.WriteUint32(&buf, authSysFlavor)
		// 5. AUTH_NONE
		_ = xdr.WriteUint32(&buf, authNoneFlavor)
	} else {
		// No Kerberos: return AUTH_SYS + AUTH_NONE only
		logger.Debug("SECINFO: returning AUTH_SYS + AUTH_NONE",
			"name", name,
			"client", ctx.ClientAddr)

		_ = xdr.WriteUint32(&buf, 2) // array length: 2 entries
		_ = xdr.WriteUint32(&buf, authSysFlavor)
		_ = xdr.WriteUint32(&buf, authNoneFlavor)
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SECINFO,
		Data:   buf.Bytes(),
	}
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
