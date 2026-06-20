// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"fmt"
	"slices"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// implicitAuthenticatedGroupSIDs returns the canonical "automatic" groups
// every authenticated Windows principal belongs to, per MS-DTYP §2.4.2.4 and
// MS-LSAD §3.1.1.2.1 — Everyone (S-1-1-0) and Authenticated Users (S-1-5-11).
// DittoFS's local user model only persists the user's named groups, so a
// DACL ACE keyed on S-1-5-11 (e.g. the SD in smbtorture
// smb2.maximum_allowed.maximum_allowed) would never match without this
// implicit injection. Mirrors the SID set Windows LSA stamps onto every
// authenticated token at logon.
//
// Anonymous and guest sessions do not get S-1-5-11: per MS-DTYP §2.4.2.4
// "Authenticated Users" excludes anonymous logons and explicit guest accounts.
var implicitAuthenticatedGroupSIDs = []string{
	sid.FormatSID(sid.WellKnownEveryone),           // S-1-1-0
	sid.FormatSID(sid.WellKnownAuthenticatedUsers), // S-1-5-11
}

// Default UID/GID used when user has no UID/GID configured.
const (
	defaultUID = uint32(1000)
	defaultGID = uint32(1000)

	// nobodyUID/nobodyGID is the unprivileged identity that guest AND
	// null/anonymous SMB sessions map to. It MUST never be 0 (root): UID 0
	// trips the metadata UID==0 root short-circuit (auth_permissions.go) and
	// bypasses all POSIX bits / ACLs (audit #1132). Per MS-DTYP §2.4.2.4
	// anonymous and guest logons are excluded from "Authenticated Users", so
	// neither gets elevated authority.
	nobodyUID = uint32(65534)
	nobodyGID = uint32(65534)
)

// setUnprivilegedIdentity points the AuthContext identity at the nobody/nogroup
// (65534) UID/GID. Used for guest and null/anonymous sessions so per-file
// permission checks still apply. Centralised so the two call sites cannot drift
// back to a privileged mapping.
func setUnprivilegedIdentity(authCtx *metadata.AuthContext) {
	uid, gid := nobodyUID, nobodyGID
	authCtx.Identity.UID = &uid
	authCtx.Identity.GID = &gid
}

// BuildAuthContext creates a metadata.AuthContext from SMB handler context.
//
// This bridges the SMB authentication model to the protocol-agnostic
// metadata store authentication context. It maps:
//   - SMB session user → Unix UID (from User.UID field)
//   - SMB session user → Unix GID (from user's Group membership)
//   - SMB share permission → metadata store permission checks
//
// Identity Resolution:
// For authenticated users, UID comes from the User model and GID comes from
// the user's group membership (lowest GID is used for best permission matching).
// If not configured, falls back to default values (1000/1000).
func BuildAuthContext(ctx *SMBHandlerContext) (*metadata.AuthContext, error) {
	// Authenticated user - delegate to BuildAuthContextFromUser
	if ctx.User != nil {
		return BuildAuthContextFromUser(ctx, ctx.User), nil
	}

	authCtx := &metadata.AuthContext{
		Context:                ctx.Context,
		ClientAddr:             ctx.ClientAddr,
		LockClientID:           fmt.Sprintf("smb:%d", ctx.SessionID),
		Identity:               &metadata.Identity{},
		BypassTraverseChecking: true,
	}

	// Both guest AND null/anonymous sessions map to the unprivileged
	// nobody/nogroup identity. An anonymous SMB *connect* is spec-legal
	// (smb2.anon-signing / anon-encryption rely on it), but the resulting
	// principal MUST be non-privileged so per-file POSIX bits and NFSv4 ACLs
	// are still enforced. See nobodyUID for the UID=0 root-bypass rationale
	// (audit #1132).
	setUnprivilegedIdentity(authCtx)

	// Set share-level permission flags for guest/anonymous sessions
	authCtx.ShareWritable = HasWritePermission(ctx)
	authCtx.ShareReadOnly = ctx.Permission == models.PermissionRead

	return authCtx, nil
}

// getUserIdentity returns the UID/GID for a user.
// UID comes from user.UID field.
// GID comes from the user's group membership (lowest GID for root-level access).
// Falls back to defaults if not configured.
func getUserIdentity(user *models.User) (uid, gid uint32) {
	uid = defaultUID
	gid = defaultGID

	if user == nil {
		return uid, gid
	}

	// Get UID from user
	if user.UID != nil {
		uid = *user.UID
	} else {
		logger.Debug("User has no UID configured, using default",
			"username", user.Username, "uid", uid)
	}

	// Get GID from user's primary group (first group with a GID).
	// This follows Unix semantics where the primary group is used for new file creation.
	gidFound := false
	for _, group := range user.Groups {
		if group.GID != nil {
			gid = *group.GID
			gidFound = true
			break
		}
	}

	if !gidFound {
		logger.Debug("User has no group with GID configured, using default",
			"username", user.Username, "gid", gid)
	}

	return uid, gid
}

// BuildAuthContextFromUser creates an AuthContext from a User.
// This is useful when the handler has direct access to a User object.
//
// Identity Resolution:
// UID comes from User.UID, GID comes from user's group membership.
// Falls back to defaults (1000/1000) if not configured.
//
// Share Permission:
// ShareWritable is set based on the SMB context's share permission.
// This allows users with share-level write permission to bypass file-level
// Unix permission checks.
func BuildAuthContextFromUser(ctx *SMBHandlerContext, user *models.User) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:                ctx.Context,
		ClientAddr:             ctx.ClientAddr,
		LockClientID:           fmt.Sprintf("smb:%d", ctx.SessionID),
		Identity:               &metadata.Identity{},
		BypassTraverseChecking: true,
	}

	if user != nil {
		uid, gid := getUserIdentity(user)
		authCtx.Identity.UID = &uid
		authCtx.Identity.GID = &gid
		authCtx.Identity.Username = user.Username
		if user.SID != "" {
			userSID := user.SID
			authCtx.Identity.SID = &userSID
		}
		// Merge the user's persisted named group SIDs with any Kerberos PAC
		// group SIDs carried on this request's session. For an AD-issued ticket
		// the PAC delivers the DC-resolved transitive group set, so a DACL ACE
		// keyed on an AD group matches even when the local user model never
		// enumerated it. slices.Concat allocates a fresh slice (no aliasing of
		// user.GroupSIDs); mergeImplicitAuthSIDs then dedups (first occurrence wins).
		authCtx.Identity.GroupSIDs = mergeImplicitAuthSIDs(
			slices.Concat(user.GroupSIDs, ctx.PACGroupSIDs),
		)
	}

	// Set share-level permission flags
	// ShareWritable allows bypassing file-level permission checks for write operations
	authCtx.ShareWritable = HasWritePermission(ctx)
	authCtx.ShareReadOnly = ctx.Permission == models.PermissionRead

	return authCtx
}

// mergeImplicitAuthSIDs returns the union of an authenticated user's named
// group SIDs with the implicit "Everyone" + "Authenticated Users" group SIDs.
// Order is implicit-first then user-named (deduplicated), so a DACL ACE keyed
// against either S-1-1-0 or S-1-5-11 matches without depending on whether the
// local user model happened to enumerate the well-known groups. See
// implicitAuthenticatedGroupSIDs for the spec citation.
func mergeImplicitAuthSIDs(userGroupSIDs []string) []string {
	merged := make([]string, 0, len(implicitAuthenticatedGroupSIDs)+len(userGroupSIDs))
	merged = append(merged, implicitAuthenticatedGroupSIDs...)
	for _, s := range userGroupSIDs {
		if slices.Contains(merged, s) {
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// primeAuthContextFromOpenFile hand-offs the open's recorded session/tree
// identity onto ctx BEFORE BuildAuthContext is called (refs #603). Follow-up
// operations CREATE / READ / WRITE / QUERY_DIRECTORY (the four current
// callers of this helper) arrive keyed only by FileID — the SMB2 dispatcher
// has no user state to prefill ctx.User with. Without this hand-off
// BuildAuthContext takes the ctx.User==nil arm and synthesises an
// unprivileged nobody (65534) identity instead of the authenticated user's
// UID, causing follow-up ops to be authorized as nobody rather than the
// real opener.
//
// We also realign ctx.TreeID / ctx.SessionID onto the IDs the open was
// created against. Downstream gates (notably treeHasAccessBasedEnumeration
// in QueryDirectory) read ctx.TreeID directly; if the dispatcher left a
// stale or zero TreeID on ctx, ABE would be decided against the wrong tree.
//
// The sess.User nil-guard on the User assignment is load-bearing: GetSession(0)
// returns the manager's seeded anonymous pre-auth session with User=nil, and
// test fixtures often pre-populate ctx.User without registering a parallel
// session. In both cases clobbering with nil would re-introduce the bug.
// IsGuest, by contrast, MUST be propagated even when sess.User==nil: guest
// sessions are created with User=nil and IsGuest=true (see
// session.NewSession), and the BuildAuthContext guest arm is what maps them
// to UID/GID 65534 instead of root.
func (h *Handler) primeAuthContextFromOpenFile(ctx *SMBHandlerContext, openFile *OpenFile) {
	h.primeAuthContext(ctx, openFile.TreeID, openFile.SessionID)
}

// primeAuthContext is the same as primeAuthContextFromOpenFile but takes raw
// tree/session IDs. CREATE uses this with the dispatcher-provided ctx.TreeID /
// ctx.SessionID because there is no OpenFile yet.
func (h *Handler) primeAuthContext(ctx *SMBHandlerContext, treeID uint32, sessionID uint64) {
	ctx.TreeID = treeID
	ctx.SessionID = sessionID
	if tree, ok := h.GetTree(treeID); ok {
		ctx.ShareName = tree.ShareName
		ctx.Permission = tree.Permission
	}
	if sess, ok := h.GetSession(sessionID); ok && sess != nil {
		// Propagate guest-ness independent of User: guest sessions seed
		// User=nil + IsGuest=true and BuildAuthContext relies on IsGuest
		// to pick the nobody/nogroup (65534) arm instead of root.
		ctx.IsGuest = sess.IsGuest
		if sess.User != nil {
			ctx.User = sess.User
		}
		// Carry the session's Kerberos PAC group SIDs onto the request context
		// so BuildAuthContextFromUser can merge them into the identity. Follow-up
		// ops (CREATE/READ/WRITE/QUERY_DIRECTORY) arrive keyed only by FileID and
		// would otherwise lose the AD group set that was resolved at SESSION_SETUP.
		// PACIdentity copies under the session lock — safe against a concurrent
		// re-auth refreshing the set.
		ctx.PACGroupSIDs, _ = sess.PACIdentity()
	}
}

// CaptureOpenerIdentity records the SMB session's authenticated DittoFS user
// on the OpenFile at CREATE time so the handle's authorization identity is
// frozen against the user who actually opened it (MS-SMB2 §3.3.5.5.3:
// "Session.SecurityContext is fixed at the time of the open"). Subsequent
// SESSION_SETUP re-authentication on the same SessionID mutates Session.User
// in-place (see tryReauthUpdate); without this snapshot, a handle-bound op
// (e.g. SET_INFO SecurityDescriptor on h1 while the session has been re-authed
// to anonymous) would resolve identity to the NEW principal and fail the
// ownership gate in MetadataService.SetFileAttributes, even though the
// handle's GrantedAccess already authorized the call at CREATE time.
//
// Guest and null sessions have User=nil but carry IsGuest/IsNull on the
// session — the snapshot preserves those flags so the rebuilt opener
// AuthContext maps back to the same unprivileged nobody/65534 identity
// rather than re-resolving against the current session's user.
//
// smbtorture smb2.session.reauth4 / reauth5 gate on this — the tests open
// h1 / dh1 as U1, re-auth the session to anonymous, then SET_INFO SD on
// the U1-opened handle and expect SUCCESS.
func (h *Handler) CaptureOpenerIdentity(ctx *SMBHandlerContext, openFile *OpenFile) {
	if h == nil || ctx == nil || openFile == nil {
		return
	}
	openFile.OpenerUser = ctx.User
	openFile.OpenerIsGuest = ctx.IsGuest
	// IsNull mirrors the session-level "anonymous logon, not guest" state.
	// SMBHandlerContext doesn't carry it explicitly; re-resolve from the
	// session so a future re-auth to a real user doesn't lose the bit.
	// Guard against tests that hand-build a Handler without a SessionManager.
	if h.SessionManager == nil {
		return
	}
	if sess, ok := h.GetSession(ctx.SessionID); ok && sess != nil {
		openFile.OpenerIsNull = sess.IsNull
	}
}

// buildOpenerAuthContext returns an AuthContext built from the OpenFile's
// captured opener identity snapshot rather than the SMB context's current
// session. Used by handle-bound metadata calls that must remain anchored to
// the opener after SESSION_SETUP re-auth (notably SET_INFO SecurityDescriptor
// — MS-SMB2 §3.3.5.21.3 + §3.3.5.5.3). The handler-level WRITE_DAC /
// WRITE_OWNER / ACCESS_SYSTEM_SECURITY gate against OpenFile.GrantedAccess
// has already authorised the call by the time this runs; the metadata-layer
// ownership check that BuildAuthContext-from-session would trip is a NAT
// of the SMB authorization model onto POSIX semantics that doesn't apply
// here.
//
// Falls back to the session-current BuildAuthContext result when the opener
// snapshot wasn't captured (legacy code paths, restored durable handles
// pre-snapshot, tests). That preserves existing behaviour for everything
// that already worked while fixing the re-auth handle-binding gap.
//
// SET_INFO Security is the only current caller; its parent-dir-lease-break
// path (`breakParentDirLeasesForContentChange`) reads OpenFile directly and
// does not consult AuthContext, so this helper deliberately does NOT call
// `PropagateOpenFileParentLeaseKey`. Future callers that route through
// MetadataService.notifyDirChange (rename / hardlink / overwrite / unlink)
// MUST invoke PropagateOpenFileParentLeaseKey on the returned authCtx
// before issuing the metadata op — same contract as the BuildAuthContext
// callsites in close.go and set_info.go.
//
// Returns nil only when ctx is nil-equivalent for BuildAuthContext;
// callers MUST nil-check and may fall back to the session-current authCtx.
func (h *Handler) buildOpenerAuthContext(ctx *SMBHandlerContext, openFile *OpenFile) *metadata.AuthContext {
	if openFile == nil {
		ac, _ := BuildAuthContext(ctx)
		return ac
	}
	// No snapshot recorded (legacy or restored durable handle pre-binding):
	// use the session-current identity. This is also the path tests exercise
	// when they hand-build an OpenFile without going through CREATE.
	if openFile.OpenerUser == nil && !openFile.OpenerIsGuest && !openFile.OpenerIsNull {
		ac, _ := BuildAuthContext(ctx)
		return ac
	}

	if openFile.OpenerUser != nil {
		return BuildAuthContextFromUser(ctx, openFile.OpenerUser)
	}

	// Guest / null opener: synthesise the same identity BuildAuthContext
	// would for a User==nil session, but pinned to the captured opener
	// flags rather than the session's current state.
	authCtx := &metadata.AuthContext{
		Context:                ctx.Context,
		ClientAddr:             ctx.ClientAddr,
		LockClientID:           fmt.Sprintf("smb:%d", ctx.SessionID),
		Identity:               &metadata.Identity{},
		BypassTraverseChecking: true,
	}
	// Guest AND null/anonymous openers both map to the unprivileged
	// nobody/nogroup identity, matching the BuildAuthContext User==nil arm.
	// Never UID=0 — see nobodyUID for the root-bypass rationale.
	setUnprivilegedIdentity(authCtx)
	authCtx.ShareWritable = HasWritePermission(ctx)
	authCtx.ShareReadOnly = ctx.Permission == models.PermissionRead
	return authCtx
}

// PropagateOpenFileParentLeaseKey copies the OpenFile's RqLs parent-lease-key
// linkage (if any) onto an AuthContext. This is the hand-off that lets
// MetadataService.notifyDirChange and the dir-lease parent-key suppression
// rule (MS-SMB2 §3.3.4.20) skip the matching parent dir
// lease when the originating handle's CREATE carried
// LEASE_FLAG_PARENT_LEASE_KEY_SET. Safe to call with a nil OpenFile (no-op).
//
// For C2 (setinfo) the parent-dir-lease break runs through
// `breakParentDirLeasesForContentChange`, which reads OpenFile directly and
// does not depend on AuthContext — this helper is therefore not required on
// that path. C3+ (rename, hardlink, overwrite, unlink) route through
// MetadataService rename/remove/link/create, which calls notifyDirChange and
// reads `ctx.ParentLeaseKey` / `ctx.HasParentLeaseKey`. Callers should
// invoke this helper before issuing those metadata operations.
func PropagateOpenFileParentLeaseKey(authCtx *metadata.AuthContext, openFile *OpenFile) {
	if authCtx == nil || openFile == nil || !openFile.HasParentLeaseKey {
		return
	}
	authCtx.ParentLeaseKey = openFile.ParentLeaseKey
	authCtx.HasParentLeaseKey = true
}

// HasWritePermission checks if the SMB context has write permission for the share.
func HasWritePermission(ctx *SMBHandlerContext) bool {
	return ctx.Permission == models.PermissionReadWrite || ctx.Permission == models.PermissionAdmin
}
