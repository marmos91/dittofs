package handlers

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// authStatusError wraps a buildV4AuthContext failure with the NFS4 status the
// COMPOUND operation should return. Without it every auth failure collapsed to
// NFS4ERR_SERVERFAULT, which mislabels an authorization denial (a krb5 export
// policy rejection or a default_permission=none denial) as an internal error.
type authStatusError struct {
	status uint32
	err    error
}

func (e *authStatusError) Error() string { return e.err.Error() }
func (e *authStatusError) Unwrap() error { return e.err }

// nfs4StatusForAuthError maps a buildV4AuthContext error to the NFS4 status a
// handler should return. Typed authStatusError values carry their own status
// (NFS4ERR_WRONGSEC for an export auth-flavor rejection, NFS4ERR_ACCESS for a
// share-permission denial); anything else is a genuine internal fault.
func nfs4StatusForAuthError(err error) uint32 {
	var ae *authStatusError
	if errors.As(err, &ae) {
		return ae.status
	}
	return types.NFS4ERR_SERVERFAULT
}

// buildV4AuthContext creates an AuthContext for NFSv4 real-FS operations.
//
// It extracts the share name from the file handle, builds an identity from
// the CompoundContext credentials, applies identity mapping rules, resolves
// permissions, and returns the effective AuthContext.
//
// Returns:
//   - *metadata.AuthContext: Auth context with effective (mapped) credentials
//   - string: The share name extracted from the handle
//   - error: If handle decoding, identity mapping, or permission resolution fails
func (h *Handler) buildV4AuthContext(ctx *types.CompoundContext, handle []byte) (*metadata.AuthContext, string, error) {
	// Decode file handle to extract share name
	shareName, _, err := metadata.DecodeFileHandle(metadata.FileHandle(handle))
	if err != nil {
		return nil, "", fmt.Errorf("decode file handle: %w", err)
	}

	// Map auth flavor to auth method string. RPCSEC_GSS (Kerberos) was
	// previously mislabeled "anonymous", which both confused audit logs and
	// blocked any per-share auth-flavor policy from telling krb5 apart from a
	// truly anonymous request.
	authMethod := "anonymous"
	switch ctx.AuthFlavor {
	case rpc.AuthUnix:
		authMethod = "unix"
	case rpc.AuthRPCSECGSS:
		authMethod = "kerberos"
	}

	// Build identity from Unix credentials (before mapping)
	originalIdentity := &metadata.Identity{
		UID:  ctx.UID,
		GID:  ctx.GID,
		GIDs: ctx.GIDs,
	}

	// Set username from UID if available (for logging/auditing)
	if originalIdentity.UID != nil {
		originalIdentity.Username = fmt.Sprintf("uid:%d", *originalIdentity.UID)
	}

	if h.Registry == nil {
		// No registry available -- return a basic auth context
		return &metadata.AuthContext{
			Context:    ctx.Context,
			ClientAddr: ctx.ClientAddr,
			AuthMethod: authMethod,
			Identity:   originalIdentity,
		}, shareName, nil
	}

	// Get share for the export-squash permission policy. On error GetShare
	// returns a nil share; ResolveSharePermission treats a nil share as "no
	// policy info" (allow with defaults) and the ApplyIdentityMapping step
	// below still fails closed if the share is genuinely gone.
	share, _ := h.Registry.GetShare(shareName)

	// Enforce the per-share export auth-flavor policy. NFSv4.1 has no MOUNT
	// call, so the RequireKerberos / AllowAuthSys checks the v3 MOUNT handler
	// applies (mount/handlers/mount.go) never ran on v4 — a share that requires
	// Kerberos was mountable over AUTH_SYS on v4.1, silently bypassing the
	// requirement. Mirror the v3 logic here, at the first real-FS op that
	// resolves the share handle, and surface the refusal as NFS4ERR_WRONGSEC so
	// the client retries with the correct flavor (SECINFO).
	if share != nil {
		if !share.AllowAuthSys && ctx.AuthFlavor == rpc.AuthUnix {
			return nil, "", &authStatusError{
				status: types.NFS4ERR_WRONGSEC,
				err:    fmt.Errorf("share %q does not allow AUTH_SYS", shareName),
			}
		}
		if share.RequireKerberos && ctx.AuthFlavor != rpc.AuthRPCSECGSS {
			return nil, "", &authStatusError{
				status: types.NFS4ERR_WRONGSEC,
				err:    fmt.Errorf("share %q requires Kerberos", shareName),
			}
		}
		// Enforce the per-share GSS protection floor (min_kerberos_level). A
		// share configured krb5i/krb5p must reject a GSS session negotiated at a
		// weaker service level (e.g. plain krb5 authentication-only on a krb5p
		// privacy share). The negotiated RPCSEC_GSS service level is carried in
		// the request context by the GSS DATA dispatch, which sets the session
		// info for every processed RPCSEC_GSS request. We enforce the floor only
		// when that session info is present: a real GSS session always carries
		// it, so its absence means the request was not processed as GSS (e.g.
		// Kerberos is not configured on the server) — applying the default
		// "krb5" floor there would spuriously deny. Non-GSS flavors are gated
		// above by AllowAuthSys / RequireKerberos.
		if ctx.AuthFlavor == rpc.AuthRPCSECGSS {
			if si := gss.SessionInfoFromContext(ctx.Context); si != nil {
				if !auth.MeetsMinKerberosLevel(share.MinKerberosLevel, si.Service) {
					return nil, "", &authStatusError{
						status: types.NFS4ERR_WRONGSEC,
						err: fmt.Errorf("share %q requires min kerberos level %q (negotiated service %d)",
							shareName, share.MinKerberosLevel, si.Service),
					}
				}
			}
		}
	}

	// Apply the export-squash permission policy (default_permission=none denial,
	// root→admin promotion, guest/known-user read-only coercion). This is the
	// SAME policy the v3 path applies via auth.ResolveSharePermission; v4
	// previously skipped it, so default_permission=none did not deny unknown
	// UIDs and read-only coercion never fired.
	permResult, err := auth.ResolveSharePermission(
		ctx.Context, h.Registry.GetIdentityStore(), share, shareName, ctx.ClientAddr, ctx.UID)
	if err != nil {
		// A share-permission denial (e.g. default_permission=none for an
		// unmapped principal — the krb5 machine-principal-maps-to-nobody case)
		// is an authorization decision, not an internal fault: surface it as
		// NFS4ERR_ACCESS so the client sees "permission denied", not a server
		// error.
		if errors.Is(err, auth.ErrShareAccessDenied) {
			return nil, "", &authStatusError{status: types.NFS4ERR_ACCESS, err: err}
		}
		return nil, "", err
	}
	if permResult.Username != "" {
		originalIdentity.Username = permResult.Username
	}

	// Apply share-level identity mapping (all_squash, root_squash).
	//
	// Fail closed on mapping failure (parity with the v3 path,
	// BuildAuthContextWithMapping). A mapping failure means the share could
	// not be resolved (e.g. a stale handle for a deleted/renamed share); the
	// previous behaviour of falling back to the original, UNMAPPED identity
	// silently bypassed squash rules (a root client would have stayed root).
	effectiveIdentity, err := h.Registry.ApplyIdentityMapping(shareName, originalIdentity)
	if err != nil {
		logger.Debug("NFSv4 identity mapping failed",
			"share", shareName,
			"error", err,
			"client", ctx.ClientAddr)
		return nil, "", fmt.Errorf("apply identity mapping: %w", err)
	}

	// Create auth context with the effective (mapped) identity
	authCtx := &metadata.AuthContext{
		Context:       ctx.Context,
		ClientAddr:    ctx.ClientAddr,
		AuthMethod:    authMethod,
		Identity:      effectiveIdentity,
		ShareReadOnly: permResult.ReadOnly,
	}

	return authCtx, shareName, nil
}

// getMetadataServiceForCtx returns the MetadataService from the handler's registry.
// Returns an error if the registry is nil.
func getMetadataServiceForCtx(h *Handler) (*metadata.Service, error) {
	if h.Registry == nil {
		return nil, fmt.Errorf("no registry configured")
	}
	return h.Registry.GetMetadataService(), nil
}

// encodeChangeInfo4 encodes a change_info4 structure into the buffer.
//
// change_info4 is used by CREATE, REMOVE, RENAME, and other operations
// to report directory change information.
//
// Wire format:
//
//	bool    atomic;      (true if before/after are atomic)
//	uint64  changeid_before;
//	uint64  changeid_after;
func encodeChangeInfo4(buf *bytes.Buffer, atomic bool, before, after uint64) {
	_ = xdr.WriteBool(buf, atomic)
	_ = xdr.WriteUint64(buf, before)
	_ = xdr.WriteUint64(buf, after)
}
