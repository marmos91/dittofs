package handlers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// treeConnectFixedSize is the size of the TREE_CONNECT request fixed structure [MS-SMB2] 2.2.9
// StructureSize(2) + Reserved/Flags(2) + PathOffset(2) + PathLength(2) = 8 bytes
const treeConnectFixedSize = 8

// SMB2ShareFlagEncryptData indicates that the share requires encryption.
// When set in the ShareFlags of the TREE_CONNECT response, the client
// must encrypt all requests to this share.
// [MS-SMB2] Section 2.2.10
const SMB2ShareFlagEncryptData uint32 = 0x00008000

// SMB2ShareFlagAccessBasedDirectoryEnum advertises Windows access-based
// enumeration on the share. When set in the ShareFlags field of the
// TREE_CONNECT response, clients learn that QUERY_DIRECTORY hides entries
// the caller cannot read. [MS-SMB2] Section 2.2.10. Note: this lives in
// ShareFlags (0x0800), not Capabilities — the Capabilities value 0x80 is
// SMB2_SHARE_CAP_ASYMMETRIC, an unrelated dialect-3.0.2 feature.
const SMB2ShareFlagAccessBasedDirectoryEnum uint32 = 0x00000800

// SMB2ShareCapContinuousAvailability advertises continuous-availability on
// the share. Set in the Capabilities field (NOT ShareFlags) of the
// TREE_CONNECT response when the share is CA-enabled, it tells SMB3 clients
// the share supports persistent handles (DH2Q SMB2_DHANDLE_FLAG_PERSISTENT).
// [MS-SMB2] Section 2.2.10. smbtorture
// smb2.durable-v2-open.persistent-open-{oplock,lease} gate their persistent
// matrix on smb2cli_tcon_capabilities() & this bit (#739).
const SMB2ShareCapContinuousAvailability uint32 = 0x00000010

// ipcMaximalAccess defines the access rights for the IPC$ virtual share.
// [MS-SMB2] Section 2.2.10 - MaximalAccess is a bitmask of allowed operations.
// Value 0x1F grants the following SMB2 access rights for named pipe operations:
//   - FILE_READ_DATA   (0x01): Read data from the pipe
//   - FILE_WRITE_DATA  (0x02): Write data to the pipe
//   - FILE_APPEND_DATA (0x04): Append data to the pipe
//   - FILE_READ_EA     (0x08): Read extended attributes
//   - FILE_WRITE_EA    (0x10): Write extended attributes
//
// This is the minimum access required for RPC operations over named pipes.
const ipcMaximalAccess = 0x1F

// TreeConnect handles the SMB2 TREE_CONNECT command [MS-SMB2] 2.2.9, 2.2.10.
// It maps a client's UNC path (\\server\share) to a DittoFS share, resolves
// share-level permissions for the authenticated user, and creates a tree
// connection. The IPC$ virtual share is handled separately for named pipe
// operations. Returns the share type, flags, and MaximalAccess bitmask.
func (h *Handler) TreeConnect(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 9 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request
	r := smbenc.NewReader(body)
	r.Skip(4) // StructureSize(2) + Flags(2)
	pathOffset := r.ReadUint16()
	pathLength := r.ReadUint16()
	if r.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Path offset is relative to the start of the SMB2 header (64 bytes)
	// Since we receive body after the header, subtract 64 to get body offset
	adjustedOffset := int(pathOffset) - 64
	if adjustedOffset < treeConnectFixedSize {
		adjustedOffset = treeConnectFixedSize // Path starts after the fixed structure
	}

	// Extract path from body
	var sharePath string
	if pathLength > 0 && len(body) >= adjustedOffset+int(pathLength) {
		pathBytes := body[adjustedOffset : adjustedOffset+int(pathLength)]
		sharePath = decodeUTF16LE(pathBytes)
	}

	// Parse share path: \\server\share -> /share
	shareName := parseSharePath(sharePath)

	logger.Debug("TREE_CONNECT request",
		"pathOffset", pathOffset,
		"pathLength", pathLength,
		"adjustedOffset", adjustedOffset,
		"rawPath", sharePath,
		"parsedShareName", shareName,
		"bodyLen", len(body),
		"bodyHex", fmt.Sprintf("%x", body))

	// Handle IPC$ virtual share for named pipe operations (RPC, share enumeration)
	// IPC$ is always available and doesn't require registry configuration
	if strings.EqualFold(shareName, "/ipc$") {
		return h.handleIPCShare(ctx)
	}

	// Check if share exists in registry
	share, shareErr := h.Registry.GetShare(shareName)
	if shareErr != nil {
		logger.Debug("Share not found", "shareName", shareName)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// Refuse TREE_CONNECT on a disabled share. Matches
	// MS-SMB2 2.2.9 — STATUS_NETWORK_NAME_DELETED is the spec error for a
	// share that existed but is no longer available. The Enabled flag is
	// flipped by shares.Service.DisableShare before restore; clients see
	// the refusal and can re-TREE_CONNECT after re-enable.
	if !share.Enabled {
		logger.Warn("SMB TREE_CONNECT refused: share disabled",
			"share", share.Name, "sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusNetworkNameDeleted), nil
	}

	// Get session and resolve permissions
	sess, _ := h.SessionManager.GetSession(ctx.SessionID)
	defaultPerm := models.ParseSharePermission(share.DefaultPermission)

	// Resolve permission based on session type
	permission, user, generation := resolveSharePermission(ctx, sess, share, defaultPerm, h.Registry.GetUserStore())

	// Check for access denied
	if permission == models.PermissionNone {
		logger.Debug("Share access denied", "shareName", shareName, "user", user)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	logger.Debug("Permission resolved for tree connect",
		"shareName", shareName,
		"user", user,
		"permission", permission)

	permission = capReadOnlyShare(share, permission)

	// Encryption enforcement: in required mode, reject unencrypted sessions
	// connecting to encrypted shares.
	if shouldRejectUnencryptedTreeConnect(h.EncryptionConfig.Mode, share, sess) {
		logger.Info("TREE_CONNECT rejected: encrypted share requires encrypted session",
			"shareName", shareName,
			"encryptionMode", h.EncryptionConfig.Mode)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// Create tree connection with permission
	treeID := h.GenerateTreeID()
	tree := &TreeConnection{
		TreeID:                 treeID,
		SessionID:              ctx.SessionID,
		ShareName:              shareName,
		ShareType:              types.SMB2ShareTypeDisk,
		CreatedAt:              time.Now(),
		Permission:             permission,
		EncryptData:            share.EncryptData,
		AccessBasedEnumeration: share.AccessBasedEnumeration,
		ChangeNotifyDisabled:   share.ChangeNotifyDisabled,
		StreamsDisabled:        share.StreamsDisabled,
		ContinuousAvailability: share.ContinuousAvailability,
		AllowMFsymlink:         share.AllowMFsymlink,
	}
	// Re-check the revocation between resolving access and publishing the tree.
	// The dispatch gate ran before this handler did, so a re-check sweep that
	// revoked the session and removed its trees in the meantime would otherwise
	// find this one published behind it — and a later re-authentication, which
	// clears the revocation, would leave it standing on the permission resolved
	// for the user that was retired.
	if sess != nil && sess.IsExpiredOrRevoked() {
		logger.Warn("SMB TREE_CONNECT refused: session was revoked while access was being resolved",
			"share", shareName, "sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusNetworkSessionExpired), nil
	}
	// The permission above was resolved against one identity, and resolving it
	// takes two store round trips. A re-authentication landing inside that
	// window re-decides authorization for a principal this answer was not about,
	// and MS-SMB2 keeps tree connections across one — so publishing here would
	// pin the previous principal's access onto the new one for the life of the
	// connection. The client retries the TREE_CONNECT and gets an answer about
	// the identity it now holds. This is the check the authorization re-check
	// applies to its own tree updates, on the same generation.
	if sess != nil && sess.AuthGeneration() != generation {
		logger.Warn("SMB TREE_CONNECT refused: session re-authenticated while access was being resolved",
			"share", shareName, "sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusNetworkSessionExpired), nil
	}
	h.StoreTree(tree)

	// Published first, then checked again, because a teardown races this in the
	// one direction the check above cannot see. LOGOFF marks the session before
	// it removes the trees, so a teardown that had already started when the
	// check ran removes every tree except this one — it is not in the map yet —
	// and the client is handed a tree ID on a session that no longer exists.
	// Storing before the second look inverts that: either the teardown's sweep
	// finds this tree and takes it, or this check sees the mark and withdraws it.
	if sess != nil && (sess.LoggedOff.Load() || sess.IsExpiredOrRevoked()) {
		h.DeleteTree(treeID)
		logger.Warn("SMB TREE_CONNECT refused: session torn down while access was being resolved",
			"share", shareName, "sessionID", ctx.SessionID)
		if sess.LoggedOff.Load() {
			return NewErrorResult(types.StatusUserSessionDeleted), nil
		}
		return NewErrorResult(types.StatusNetworkSessionExpired), nil
	}

	ctx.TreeID = treeID
	ctx.ShareName = shareName

	// Calculate MaximalAccess based on effective permission
	maximalAccess := calculateMaximalAccess(permission)

	// Calculate ShareFlags
	var shareFlags uint32
	if share.EncryptData {
		shareFlags |= SMB2ShareFlagEncryptData
	}
	// Refs #549: SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM (MS-SMB2 §2.2.10)
	// tells the client we hide unreadable entries on enumeration. The bit
	// belongs in ShareFlags (0x0800), not Capabilities — matches Samba
	// source3/smbd/smb2_tcon.c and the smbtorture acls.ACCESSBASED check
	// at source4/torture/smb2/acls.c which reads smb2cli_tcon_flags().
	if share.AccessBasedEnumeration {
		shareFlags |= SMB2ShareFlagAccessBasedDirectoryEnum
	}

	// Capabilities — advertise continuous-availability when the share is
	// CA-enabled so SMB3 clients know persistent handles are supported
	// (MS-SMB2 §2.2.10; #739).
	var capabilities uint32
	if share.ContinuousAvailability {
		capabilities |= SMB2ShareCapContinuousAvailability
	}

	// Build response (16 bytes)
	w := smbenc.NewWriter(16)
	w.WriteUint16(16)                     // StructureSize
	w.WriteUint8(types.SMB2ShareTypeDisk) // ShareType
	w.WriteUint8(0)                       // Reserved
	w.WriteUint32(shareFlags)             // ShareFlags
	w.WriteUint32(capabilities)           // Capabilities
	w.WriteUint32(maximalAccess)          // MaximalAccess

	return NewResult(types.StatusSuccess, w.Bytes()), nil
}

// calculateMaximalAccess returns the SMB2 MaximalAccess mask based on share permission.
// [MS-SMB2] Section 2.2.10 - MaximalAccess is a bit mask of allowed operations.
func calculateMaximalAccess(perm models.SharePermission) uint32 {
	// SMB2 Access Mask values
	const (
		// Standard rights
		fileReadData        = 0x00000001
		fileWriteData       = 0x00000002
		fileAppendData      = 0x00000004
		fileReadEA          = 0x00000008
		fileWriteEA         = 0x00000010
		fileExecute         = 0x00000020
		fileDeleteChild     = 0x00000040
		fileReadAttributes  = 0x00000080
		fileWriteAttributes = 0x00000100
		delete_             = 0x00010000
		readControl         = 0x00020000
		writeDAC            = 0x00040000
		writeOwner          = 0x00080000
		synchronize         = 0x00100000

		// Generic read access
		genericRead = fileReadData | fileReadEA | fileReadAttributes | readControl | synchronize

		// Full access
		fullAccess = 0x001F01FF
	)

	switch perm {
	case models.PermissionAdmin:
		// Full access for admin users
		return fullAccess
	case models.PermissionReadWrite:
		// Full access for read-write users. macOS Finder checks MaximalAccess before
		// attempting file operations and refuses to create files if delete/ownership
		// bits are missing. Actual permission enforcement happens at operation time.
		return fullAccess
	case models.PermissionRead:
		// Read-only access
		return genericRead
	default:
		// No access (shouldn't reach here, access denied earlier)
		return 0
	}
}

// handleIPCShare handles TREE_CONNECT to the virtual IPC$ share.
// IPC$ is used for inter-process communication including:
// - Share enumeration via SRVSVC RPC
// - Remote registry access
// - Named pipe operations
// [MS-SMB2] Section 2.2.10 specifies ShareType 0x02 for pipe shares.
func (h *Handler) handleIPCShare(ctx *SMBHandlerContext) (*HandlerResult, error) {
	logger.Debug("TREE_CONNECT to virtual IPC$ share", "sessionID", ctx.SessionID)

	// Verify that a valid session exists before granting IPC$ access.
	// While IPC$ is a well-known share that should be accessible to authenticated clients,
	// we still require a valid session to have been established first.
	sess, found := h.SessionManager.GetSession(ctx.SessionID)
	if !found || sess == nil {
		logger.Debug("IPC$ access denied: no valid session", "sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusUserSessionDeleted), nil
	}

	// The dispatch gate refused this command only if the session had already
	// lost authorization when it arrived. A sweep that revokes in between would
	// otherwise leave this tree behind, and a later re-authentication clears the
	// revocation while the tree survives it.
	if sess.IsExpiredOrRevoked() {
		logger.Debug("IPC$ access denied: session authorization revoked", "sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusNetworkSessionExpired), nil
	}

	// Create tree connection for IPC$ with PIPE share type
	treeID := h.GenerateTreeID()
	tree := &TreeConnection{
		TreeID:     treeID,
		SessionID:  ctx.SessionID,
		ShareName:  "/ipc$",
		ShareType:  types.SMB2ShareTypePipe, // Named pipe share
		CreatedAt:  time.Now(),
		Permission: models.PermissionReadWrite,
	}
	h.StoreTree(tree)

	ctx.TreeID = treeID
	ctx.ShareName = "/ipc$"

	// Build response with PIPE share type
	// [MS-SMB2] Section 2.2.10 TREE_CONNECT Response
	w := smbenc.NewWriter(16)
	w.WriteUint16(16)                     // StructureSize
	w.WriteUint8(types.SMB2ShareTypePipe) // ShareType: Named pipe
	w.WriteUint8(0)                       // Reserved
	w.WriteUint32(0)                      // ShareFlags: none
	w.WriteUint32(0)                      // Capabilities: none
	w.WriteUint32(ipcMaximalAccess)       // MaximalAccess: basic read/write for IPC

	return NewResult(types.StatusSuccess, w.Bytes()), nil
}

// shouldRejectUnencryptedTreeConnect returns true if a TREE_CONNECT should be
// rejected because the share requires encryption but the session does not support it.
// This only applies when encryption_mode is "required" and the share has EncryptData=true.
// In "preferred" mode, unencrypted sessions are allowed (mixed model).
//
// EncryptData is an SMB 3.x share flag: an SMB 2.x client cannot negotiate
// encryption at all, so honoring the flag for it would hand the client a tree
// it can never send encrypted commands on (MS-SMB2 2.2.10 / 3.3.5.3 —
// encryption requires a dialect >= 3.0). SMB 2.x clients are rejected up front
// instead of receiving a dead tree.
func shouldRejectUnencryptedTreeConnect(encryptionMode string, share *runtime.Share, sess *session.Session) bool {
	if encryptionMode != "required" || share == nil || !share.EncryptData {
		return false
	}
	if sess == nil {
		return true
	}
	if sess.Dialect > 0 && sess.Dialect < types.Dialect0300 {
		return true
	}
	return !sess.ShouldEncrypt()
}

// parseSharePath parses \\server\share to /share or just share
// The share name is normalized to lowercase for case-insensitive matching.
func parseSharePath(path string) string {
	// Remove leading backslashes
	path = strings.TrimPrefix(path, "\\\\")

	// Split by backslash
	parts := strings.SplitN(path, "\\", 2)
	if len(parts) < 2 {
		// No server part, return as-is with lowercase normalization
		return "/" + strings.ToLower(strings.TrimPrefix(path, "/"))
	}

	// Return the share part, normalized to lowercase
	// Windows clients often send share names in uppercase (e.g., /EXPORT)
	// but our shares are typically configured in lowercase (e.g., /export)
	return "/" + strings.ToLower(parts[1])
}

// sidSharePermissionResolver is the subset of the control-plane store that
// resolves a share permission from a set of Windows SIDs (a login's PAC user +
// group SIDs). The concrete store implements it; a mock userStore that does not
// implement it simply skips the direct-AD-grant path (#1528).
type sidSharePermissionResolver interface {
	ResolveSharePermissionForSIDs(ctx context.Context, sids []string, shareName string) (models.SharePermission, error)
}

// resolveSharePermission determines the effective permission for a session on a share.
// Returns the permission level and a user identifier for logging.
//
// Permission resolution follows this order:
//  1. Root user bypass: If user has UID=0 and squash mode allows root access, grant PermissionAdmin
//  2. Local user/group permission from UserStore.ResolveSharePermission (if userStore available)
//  3. Direct AD/SID grants (#1528): the session's Kerberos PAC user + group SIDs
//     matched against the share's SID grants — applied even when the principal
//     has no local User object. The higher of (2) and (3) wins.
//  4. Default permission if no explicit permission found
//
// This mirrors the NFS behavior where root users get automatic admin access based on squash settings.
// capReadOnlyShare caps a resolved permission to read when the share itself is
// configured read-only, so no resolution hands back write access the share
// forbids. Every path that resolves a share permission goes through it —
// TREE_CONNECT and the authorization re-check alike — because a second copy of
// this rule is a copy that can drift.
func capReadOnlyShare(share *runtime.Share, permission models.SharePermission) models.SharePermission {
	if !share.ReadOnly || (permission != models.PermissionReadWrite && permission != models.PermissionAdmin) {
		return permission
	}
	logger.Debug("Share is read-only, capping permission to read",
		"shareName", share.Name, "originalPermission", permission)
	return models.PermissionRead
}

// It also reports the identity generation the answer belongs to, so the caller
// can refuse to publish a tree whose access was decided for a principal a
// re-authentication has since replaced.
func resolveSharePermission(
	ctx *SMBHandlerContext,
	sess *session.Session,
	share *runtime.Share,
	defaultPerm models.SharePermission,
	userStore models.UserStore,
) (models.SharePermission, string, uint64) {
	var snap session.AuthzIdentity
	if sess != nil {
		// Through the locked accessor: SESSION_SETUP re-authentication replaces
		// these fields under the session mutex, so reading them directly races a
		// concurrent re-auth and can resolve access from a half-published
		// identity.
		snap = sess.AuthzIdentity()
		snap.User = currentRecordFor(ctx.Context, snap.User, userStore)
	}
	perm, identifier, _ := resolveSharePermissionForIdentity(ctx, sess, snap, share, defaultPerm, userStore)
	return perm, identifier, snap.Generation
}

// currentRecordFor returns the persisted user record to resolve a fresh
// TREE_CONNECT against, given the one the session is carrying.
//
// A TREE_CONNECT is a new authorization decision, and permission resolution
// reads grants and group membership off the record it is handed rather than the
// database. Resolving against the session's snapshot therefore hands a
// reconnecting client exactly the grants that were withdrawn since it
// authenticated — and a client reconnects routinely, so a revocation that is
// only applied to the established trees survives no longer than the next
// TREE_CONNECT.
//
// decision: a lookup that fails, including one that reports the row gone,
// leaves the session's own record standing rather than becoming a decision. A
// store outage would otherwise revoke share access wholesale, and a deleted or
// disabled account is already refused outright by the dispatch gate, which is a
// stronger answer than anything decided here. The ceiling is that a deletion
// the store cannot currently confirm leaves this resolution on the older record
// until the store answers again. Withdraw the exemption for the missing-row
// case only if the dispatch gate ever stops covering it. The re-check applies
// the opposite policy to the same lookup — a missing row there revokes the
// session — which is why askPersistedRecord reports the outcome rather than
// deciding it.
func currentRecordFor(ctx context.Context, user *models.User, userStore models.UserStore) *models.User {
	current, asked, err := askPersistedRecord(ctx, user, userStore)
	if !asked {
		return user
	}
	if err != nil || current == nil {
		logger.Debug("TREE_CONNECT could not re-read the user record, resolving against the session's copy",
			"user", user.Username, "error", err)
		return user
	}
	return current
}

// askPersistedRecord re-reads the store's record for the user a session is
// carrying. It reports whether the store was asked at all, so a caller can tell
// "no answer available" from "the store says this user is gone" — the two mean
// opposite things to an authorization decision.
//
// decision: a principal resolved from the directory with no local account is
// never offered to the store. Its record was synthesized and never persisted,
// so it carries no primary key, and the store is keyed by name — a row that
// comes back under that name is a different principal, not a fresher copy of
// this one. The two callers would each be wrong in their own way without the
// guard: the re-check would read "missing" as deletion and retire every AD
// session on the next unrelated user edit, and TREE_CONNECT would resolve a
// domain principal's access from a local account that merely shares its
// sAMAccountName — the same substitution bindIdentityMatchesSession refuses on
// the channel-bind path. Withdraw the exemption only if such a principal ever
// gets a persisted row of its own, matched by SID rather than by name.
func askPersistedRecord(ctx context.Context, user *models.User, userStore models.UserStore) (*models.User, bool, error) {
	if user == nil || userStore == nil || user.ID == "" {
		return nil, false, nil
	}
	current, err := userStore.GetUser(ctx, user.Username)
	return current, true, err
}

// resolveSharePermissionForIdentity resolves against a supplied identity rather
// than reading the session again. An authorization re-check substitutes the
// record it has just read from the store into the snapshot, so the grants and
// group memberships consulted below are current ones rather than those captured
// when the session authenticated, while every other identity field still comes
// from the one read that produced the generation the re-check is pinned to.
func resolveSharePermissionForIdentity(
	ctx *SMBHandlerContext,
	sess *session.Session,
	snap session.AuthzIdentity,
	share *runtime.Share,
	defaultPerm models.SharePermission,
	userStore models.UserStore,
) (models.SharePermission, string, bool) {
	user := snap.User
	// No session at all — deny (the caller maps PermissionNone to
	// STATUS_ACCESS_DENIED).
	if sess == nil {
		return models.PermissionNone, "", true
	}

	// A disabled user keeps no access on any protocol. Every path that installs
	// a user on a session already requires Enabled, so this does not currently
	// refuse a request the earlier checks let through — it is the backstop that
	// keeps that true, and the one place a re-check can pass a record read after
	// the session was established. Ahead of the root bypass below because a
	// disabled root is still disabled. Mirrors the NFS resolver.
	if user != nil && !user.Enabled {
		logger.Debug("Share access denied (user disabled)",
			"shareName", share.Name, "user", user.Username)
		return models.PermissionNone, user.Username, true
	}

	// 1. Root user bypass: UID 0 with a squash mode that allows root access gets
	// admin regardless of grants. Mirrors resolveNFSSharePermission.
	if user != nil && isRootUser(user) && rootHasAdminAccess(share) {
		logger.Debug("Root user granted admin access via squash mode",
			"shareName", share.Name, "user", user.Username, "squash", share.Squash)
		return models.PermissionAdmin, user.Username, true
	}

	// 2. Local user/group resolution.

	localPerm := defaultPerm
	identifier := snap.Username
	// resolved reports whether the permission below is a decision the store
	// actually made — across both lookups, the local one here and the SID grant
	// further down. A failed lookup falls back to the share default, which is a
	// grant a caller must not mistake for a resolved one: TREE_CONNECT may hand
	// out the default on a fresh connect, but a re-check that writes the result
	// back would promote an explicitly restricted tree to it.
	resolved := true
	switch {
	case user != nil:
		identifier = user.Username
		if userStore != nil {
			if perm, err := userStore.ResolveSharePermission(ctx.Context, user, share.Name); err != nil {
				logger.Debug("Permission resolution failed, using default",
					"shareName", share.Name, "user", user.Username, "error", err, "default", defaultPerm)
				resolved = false
			} else {
				localPerm = perm
			}
		} else {
			logger.Debug("No userStore available, using default permission",
				"shareName", share.Name, "user", user.Username, "default", defaultPerm)
		}
	case snap.IsGuest:
		identifier = "guest"
	}

	// 3. Direct AD/SID grants (#1528). The Kerberos PAC user + group SIDs are on
	// the session independent of local user resolution, so this authorizes an AD
	// principal that has no local DittoFS account. SID grants are additive (like
	// group membership) EXCEPT when the user has an explicit per-user local grant
	// — including an explicit 'none' block — which is authoritative and must not
	// be overridden, mirroring the local resolver's "user-explicit wins" rule.
	effective := localPerm
	userExplicit := false
	if user != nil {
		_, userExplicit = user.GetExplicitSharePermission(share.Name)
	}
	if r, ok := userStore.(sidSharePermissionResolver); ok && !userExplicit {
		sids := snap.GroupSIDs
		if snap.UserSID != "" {
			sids = append(sids, snap.UserSID)
		}
		if len(sids) > 0 {
			if sidPerm, err := r.ResolveSharePermissionForSIDs(ctx.Context, sids, share.Name); err != nil {
				logger.Debug("SID permission resolution failed, ignoring",
					"shareName", share.Name, "error", err)
				// Unresolved for the same reason the local lookup is: an AD
				// principal can hold its whole grant here, so a failure leaves
				// the permission below resting on the share default rather than
				// on anything the store said.
				resolved = false
			} else if sidPerm.Level() > effective.Level() {
				logger.Debug("Direct AD/SID grant elevates share permission",
					"shareName", share.Name, "user", identifier, "from", effective, "to", sidPerm)
				effective = sidPerm
			}
		}
	}

	return effective, identifier, resolved
}

// isRootUser checks if the user has UID 0 (root).
func isRootUser(user *models.User) bool {
	return user != nil && user.UID != nil && *user.UID == 0
}

// rootHasAdminAccess checks if the share's squash mode allows root to have admin access.
// An empty/unset squash normalizes to DefaultSquashMode (root_to_guest), which
// does NOT grant root admin. Root has admin access only when squash mode is:
//   - SquashNone (no mapping)
//   - SquashRootToAdmin (root keeps admin)
//   - SquashAllToAdmin (everyone gets admin)
func rootHasAdminAccess(share *runtime.Share) bool {
	if share == nil {
		return false
	}
	switch share.Squash.OrDefault() {
	case models.SquashNone, models.SquashRootToAdmin, models.SquashAllToAdmin:
		return true
	default:
		return false
	}
}
