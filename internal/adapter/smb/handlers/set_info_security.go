package handlers

import (
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// SET_INFO security descriptors: SD parsing, DACL/SACL merge, and the
// security-access gate.
// parseSDOptsForShare resolves the Security Descriptor parse options for the
// share that owns openFile. Returns Windows-canonical defaults
// (CanonicalizeAutoInherited=true) when the share lookup fails — the safe
// fallback per MS-DTYP §2.5.3.4.2.
func (h *Handler) parseSDOptsForShare(shareName string) ParseSDOptions {
	opts := ParseSDOptions{CanonicalizeAutoInherited: true}
	if h.Registry == nil {
		return opts
	}
	share, err := h.Registry.GetShare(shareName)
	if err != nil {
		logger.Debug("SET_INFO: share lookup failed, defaulting to canonicalize",
			"share", shareName, "error", err)
		return opts
	}
	opts.CanonicalizeAutoInherited = share.AclFlagInheritedCanonicalization
	return opts
}

// setSecurityInfo handles SET_INFO for security descriptors.
//
// Parses the binary Security Descriptor from the client, extracts owner/group/ACL,
// and applies the changes to the file via MetadataService.SetFileAttributes.
//
// Per MS-SMB2 §3.3.5.21.3 and MS-FSA §2.1.5.17 ("Server Requests Setting of Security Information"), the access authorization for
// SET_INFO Security is performed against the OPEN'S granted access mask
// (captured at CREATE time), NOT against the file's current DACL. The new
// SD being installed is irrelevant to the authorization decision — installing
// a DACL that strips WRITE_DAC must still succeed if the handle was opened
// with WRITE_DAC. Section→bit mapping (mirrors Samba
// source3/smbd/smb2_setinfo.c::smbd_smb2_setinfo_security):
//
//	SECINFO_DACL  → SEC_STD_WRITE_DAC
//	SECINFO_OWNER → SEC_STD_WRITE_OWNER
//	SECINFO_GROUP → SEC_STD_WRITE_OWNER
//	SECINFO_SACL  → ACCESS_SYSTEM_SECURITY
//
// Each requested section is gated independently; the request is denied as a
// whole if any requested section lacks the corresponding bit on the handle.
// Refs #559.

func (h *Handler) setSecurityInfo(
	authCtx *metadata.AuthContext,
	openFile *OpenFile,
	additionalInfo uint32,
	buffer []byte,
) (*SetInfoResponse, error) {
	if len(buffer) == 0 {
		return setInfoStatus(types.StatusInvalidParameter), nil
	}

	// MS-SMB2 §3.3.5.21.3: authorize each requested SD section against the
	// open's GrantedAccess. The new SD is not consulted — the handle's mask
	// already captured the DACL-evaluated rights at CREATE time.
	if status, ok := checkSetInfoSecurityAccess(openFile.GrantedAccess, additionalInfo); !ok {
		logger.Debug("SET_INFO Security: handle lacks required access",
			"path", openFile.Name().Path,
			"additionalInfo", fmt.Sprintf("0x%x", additionalInfo),
			"grantedAccess", fmt.Sprintf("0x%x", openFile.GrantedAccess))
		return setInfoStatus(status), nil
	}

	// Per-share opt-out of MS-DTYP §2.5.3.4.2 canonicalization. Default true
	// matches Windows (and Samba's default). The toggle was populated onto
	// the runtime Share at AddShare time (refs #514 T1).
	opts := h.parseSDOptsForShare(openFile.ShareName)

	ownerUID, ownerGID, fileACL, err := ParseSecurityDescriptorWithOptions(buffer, opts)
	if err != nil {
		logger.Debug("SET_INFO: failed to parse security descriptor", "path", openFile.Name().Path, "error", err)
		return setInfoStatus(types.StatusInvalidParameter), nil
	}

	// ParseSecurityDescriptorWithOptions returns a nil ownerUID/ownerGID both
	// when the SD omits that SID section and when the section is present but its
	// SID could not be mapped to a local UID/GID. A requested OWNER/GROUP change
	// to a genuinely FOREIGN domain account (an S-1-5-21 SID from another
	// domain, resolvable only via AD/LDAP — #1231) must NOT silently no-op:
	// Windows would believe the owner changed when nothing did, so we reject it
	// with an explicit status. Refs #1228.
	//
	// Any other unmappable SID is accepted as a no-op success, matching real
	// servers. smbtorture smb2.acls.SDFLAGSVSCHOWN chowns the owner to the World
	// SID (S-1-1-0) and back, expecting NT_STATUS_OK each time; Samba resolves
	// well-known SIDs through idmap rather than failing. Well-known SIDs (World,
	// BUILTIN\*, NT AUTHORITY\*, CREATOR\*) and SIDs in our own machine domain
	// are therefore NOT rejected here even when they have no reverse UID/GID
	// mapping — only IsForeignDomainSID SIDs are. (A foreign SID set is a
	// no-op against local POSIX ownership too; we surface the error purely so a
	// client is not misled into thinking a foreign-principal chown succeeded.)
	if additionalInfo&(OwnerSecurityInformation|GroupSecurityInformation) != 0 {
		reqOwnerSID, reqGroupSID, hasOwnerSID, hasGroupSID := securityDescriptorOwnerGroupSIDs(buffer)
		mapper := GetSIDMapper()

		// A foreign owner/group SID the parse path could not reverse-map (nil
		// ownerUID/ownerGID) normally means an unmappable chown, which #1228
		// rejects. But #1617 emits an AD-owned file's owner/group AS a foreign AD
		// SID, and Windows echoes that exact SID back on unrelated DACL edits — so
		// a re-set of the file's CURRENT owner/group SID must be accepted as a
		// no-op even when the reverse directory lookup is momentarily unavailable
		// (a transient miss must not turn a no-op into StatusInvalidOwner). Fetch
		// the file's current uid/gid only when we might otherwise reject.
		ownerReject := (additionalInfo&OwnerSecurityInformation) != 0 && hasOwnerSID && ownerUID == nil &&
			mapper.IsForeignDomainSID(reqOwnerSID)
		groupReject := (additionalInfo&GroupSecurityInformation) != 0 && hasGroupSID && ownerGID == nil &&
			mapper.IsForeignDomainSID(reqGroupSID)
		if ownerReject || groupReject {
			var curUID, curGID uint32
			if cur, gerr := h.Registry.GetMetadataService().GetFile(authCtx.Context, openFile.MetadataHandle); gerr == nil && cur != nil {
				curUID, curGID = cur.UID, cur.GID
			}
			if ownerReject && !isCurrentOwnerSID(reqOwnerSID, curUID) {
				logger.Debug("SET_INFO Security: owner change requested with foreign-domain SID", "path", openFile.Name().Path)
				return setInfoStatus(types.StatusInvalidOwner), nil
			}
			if groupReject && !isCurrentGroupSID(reqGroupSID, curGID) {
				logger.Debug("SET_INFO Security: group change requested with foreign-domain SID", "path", openFile.Name().Path)
				return setInfoStatus(types.StatusNoneMapped), nil
			}
		}
	}

	metaSvc := h.Registry.GetMetadataService()

	// Build SetAttrs from parsed SD
	setAttrs := &metadata.SetAttrs{}
	changed := false

	// Only apply sections that were requested via AdditionalInfo
	if (additionalInfo&OwnerSecurityInformation) != 0 && ownerUID != nil {
		setAttrs.UID = ownerUID
		changed = true
	}

	if (additionalInfo&GroupSecurityInformation) != 0 && ownerGID != nil {
		setAttrs.GID = ownerGID
		changed = true
	}

	// DACL and SACL are stored on the same acl.ACL carrier (DACL in ACEs/flags,
	// SACL in the SACL slice). A SET_INFO may request either or both sections,
	// so build the installed ACL honoring exactly the requested sections while
	// preserving the unrequested portion from the file's current ACL. Without
	// this preserve step, a SACL-only SET would wipe the DACL (and vice versa).
	wantDACL := (additionalInfo & DACLSecurityInformation) != 0
	wantSACL := (additionalInfo & SACLSecurityInformation) != 0
	if wantDACL || wantSACL {
		// Fetch the current ACL so the unrequested section survives. A miss
		// (or nil ACL) leaves the section empty, matching today's full-replace
		// behavior for the requested section.
		var current *acl.ACL
		if !wantDACL || !wantSACL {
			metaSvc := h.Registry.GetMetadataService()
			if cur, gerr := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle); gerr == nil && cur != nil {
				current = cur.ACL
			}
		}

		merged := mergeSecurityACL(fileACL, current, wantDACL, wantSACL)
		setAttrs.ACL = merged
		changed = true
	}

	if !changed {
		if h.NotifyRegistry != nil {
			h.notifyOpenFileModified(openFile, FileNotifyChangeSecurity)
		}
		return setInfoStatus(types.StatusSuccess), nil
	}

	_, err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
	if err != nil {
		logger.Debug("SET_INFO: failed to set security info", "path", openFile.Name().Path, "error", err)
		return setInfoStatus(types.StatusForErr(err)), nil
	}

	if h.NotifyRegistry != nil {
		h.notifyOpenFileModified(openFile, FileNotifyChangeSecurity)
	}

	return setInfoStatus(types.StatusSuccess), nil
}

// parsedHasDACLBody reports whether a parsed SD carried an actual DACL section.
//
// ParseSecurityDescriptorWithOptions returns a non-nil carrier ACL whenever it
// found EITHER a DACL or a SACL, so "non-nil" alone does not imply a DACL is
// present: a SACL-only SD yields &acl.ACL{} (no Source, no NullDACL) with only
// the SACL slice populated. parseDACL always stamps Source=ACLSourceSMBExplicit
// (even for a 0-ACE explicit-empty DACL), and a wire null DACL sets NullDACL —
// those two markers are the only ways the DACL portion gets populated, so they
// are the reliable discriminator. Without this gate a DACL-info SET carrying a
// SACL-only SD would install an empty deny-all DACL instead of preserving the
// prior (or null-DACL) behavior.

func parsedHasDACLBody(parsed *acl.ACL) bool {
	return parsed != nil && (parsed.Source == acl.ACLSourceSMBExplicit || parsed.NullDACL)
}

// mergeSecurityACL builds the acl.ACL to install on a SET_INFO Security,
// combining the freshly-parsed SD (parsed) with the file's current ACL
// (current) according to which sections the request carried.
//
//   - DACL requested: take the DACL (ACEs + DACL-level flags + Source) from
//     parsed; if parsed carried no DACL body → null DACL.
//   - DACL not requested: preserve the current DACL unchanged.
//   - SACL requested: take the SACL from parsed (no/empty SACL → clear it).
//   - SACL not requested: preserve the current SACL unchanged.
//
// The result is a single carrier so the metadata store's whole-ACL replace
// installs both sections atomically without dropping the unrequested one.

func mergeSecurityACL(parsed, current *acl.ACL, wantDACL, wantSACL bool) *acl.ACL {
	out := &acl.ACL{}

	// DACL portion (ACEs + DACL-scoped control flags + Source).
	if wantDACL {
		if parsedHasDACLBody(parsed) {
			out.ACEs = parsed.ACEs
			out.Source = parsed.Source
			out.Protected = parsed.Protected
			out.AutoInherited = parsed.AutoInherited
			out.NullDACL = parsed.NullDACL
		} else {
			// DACL section requested but no DACL body in the SD → null DACL.
			out.NullDACL = true
		}
	} else if current != nil {
		out.ACEs = current.ACEs
		out.Source = current.Source
		out.Protected = current.Protected
		out.AutoInherited = current.AutoInherited
		out.NullDACL = current.NullDACL
	}

	// SACL portion. Normalize an empty (non-nil) slice to nil so "clear SACL"
	// stores the same shape as "no SACL" — json:"sacl,omitempty" only drops a
	// nil slice, so an empty slice would otherwise persist as "sacl":[].
	if wantSACL {
		if parsed != nil && len(parsed.SACL) > 0 {
			out.SACL = parsed.SACL
		}
		// parsed nil or empty SACL → leave out.SACL nil (cleared).
	} else if current != nil {
		out.SACL = current.SACL
	}

	return out
}

// checkSetInfoSecurityAccess maps requested SECURITY_INFORMATION sections to
// the access mask bits MS-SMB2 §3.3.5.21.3 / MS-FSA §2.1.5.17 ("Server Requests Setting of Security Information") require on the
// open's GrantedAccess, and verifies each requested section against the mask.
//
// Returns (StatusSuccess, true) when every requested section has the matching
// bit on the open; (StatusAccessDenied, false) otherwise. An additionalInfo
// of zero authorizes (no sections to gate).
//
// Mirrors Samba source3/smbd/smb2_setinfo.c::smbd_smb2_setinfo_security and
// source3/smbd/posix_acls.c::set_nt_acl — both consult `fsp->access_mask`
// (the equivalent of OpenFile.GrantedAccess), never re-evaluate against the
// file's current DACL. Refs #559.

func checkSetInfoSecurityAccess(grantedAccess, additionalInfo uint32) (types.Status, bool) {
	if additionalInfo&DACLSecurityInformation != 0 {
		if !hasAccessRight(grantedAccess, uint32(types.WriteDac)) {
			return types.StatusAccessDenied, false
		}
	}
	// SECINFO_OWNER and SECINFO_GROUP both require WRITE_OWNER per MS-DTYP
	// §2.5.3.3 (the algorithm folds owner+group under one privilege gate).
	if additionalInfo&(OwnerSecurityInformation|GroupSecurityInformation) != 0 {
		if !hasAccessRight(grantedAccess, uint32(types.WriteOwner)) {
			return types.StatusAccessDenied, false
		}
	}
	if additionalInfo&SACLSecurityInformation != 0 {
		if !hasAccessRight(grantedAccess, uint32(types.AccessSystemSecurity)) {
			return types.StatusAccessDenied, false
		}
	}
	return types.StatusSuccess, true
}

// breakParentDirLeases breaks leases on the parent directory when a child
// file's metadata or content changes (SET_INFO, WRITE, DELETE). Per MS-FSA 2.1.4.12 ("Algorithm to Check for an Oplock Break"):
//   - Handle caching is broken so clients revalidate cached directory handles
//   - Read caching is broken so clients see updated directory listing metadata
//     (timestamps, sizes, attributes visible in READDIR results)
//
// breakParentDirLeasesForContentChange breaks both Handle and Read leases on
// the parent directory when directory CONTENT changes (rename, delete). These
// operations affect what READDIR returns, invalidating Read caching.
//
// ctx may be nil — callers that don't carry a SMBHandlerContext (e.g. cross-
// protocol cleanup paths) fall back to inline dispatch. Carrying ctx lets the
// helper defer the dispatch via PostSend so the break notification arrives
// after the triggering request's response, matching Samba's tevent-cycle
// `send_break_to_none` semantics.

func isReservedACLXattrName(name string) bool {
	return strings.EqualFold(name, reservedACLXattrName)
}
