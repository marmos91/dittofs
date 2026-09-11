package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// CREATE completion after a lease/oplock break: the post-break completion
// state machine (recheck gates, re-encode, respond).
func (h *Handler) completeCreateAfterBreak(ctx *SMBHandlerContext, d *createDraft) *CreateResponse {
	req := d.req
	tree := d.tree
	authCtx := d.authCtx
	filename := d.filename
	baseName := d.baseName
	parentHandle := d.parentHandle
	existingFile := d.existingFile
	fileExists := d.fileExists
	createAction := d.createAction
	excludeOwner := d.excludeOwner

	// Compute the effective access mask once for both the share-mode conflict
	// check below and the DACL access-rights check downstream. Mirrors Samba's
	// `open_access_mask` (source3/smbd/open.c::open_file_ntcreate) which folds
	// FILE_WRITE_DATA into the mask for O_TRUNC dispositions and then uses the
	// augmented mask for BOTH checks. See effectiveAccessForOpen for spec refs.
	effectiveAccess := effectiveAccessForOpen(req.DesiredAccess, req.CreateDisposition)

	// Share-mode recheck + DACL access gate + delete-on-close (read-only)
	// checks, evaluated against the existing-file draft view after any lease
	// breaks have drained. Lifted into a helper so the race-recovery branch
	// (ErrAlreadyExists resync below) can replay the SAME gates against the
	// winner's view before the overwrite/open path proceeds (#765).
	metaSvc := h.Registry.GetMetadataService()
	grantedAccess, grantedComputed, failResp := h.recheckExistingFileGates(d, effectiveAccess)
	if failResp != nil {
		return failResp
	}

	// Step 6e: Break parent directory leases on create/overwrite/supersede
	// BEFORE the actual file mutation so the metadata-layer notifyDirChange
	// (fire-and-forget) finds the dir lease already broken and does not
	// mark the recently-broken cache — which would block the test's lease
	// rearm (test_rearm_dirlease). Mirrors Samba's delay_for_oplock_fn
	// running before the actual create.
	//
	// Parent-key suppression (Samba `dirlease_should_break`):
	// if this CREATE carried an RqLs with
	// LEASE_FLAG_PARENT_LEASE_KEY_SET, extract the ParentLeaseKey from the
	// incoming request so the matching dir-lease is NOT broken.
	//
	// Single break-to-None per Samba do_dirlease_break_to_none
	// (source3/smbd/smb2_oplock.c): a directory-content change emits ONE
	// LEASE_BREAK to None per dir-lease holder, not the two-step strip-H /
	// strip-R pattern used for file leases. Applies uniformly to CREATE,
	// OVERWRITE, and SUPERSEDE — WPTS BVT_DirectoryLeasing_ReadWriteHandleCaching
	// (#454) asserts the break notification carries NewLeaseState=None.
	if (createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded) && h.LeaseManager != nil {
		parentLockHandle := lock.FileHandle(parentHandle)
		// Per Samba dirlease_should_break: no ClientID exclusion for parent
		// dir lease breaks. Same-client CREATEs break the parent dir lease
		// unless the ParentLeaseKey matches (smb2.dirlease.v2_request,
		// smb2.dirlease.leases). Only ParentLeaseKey suppression applies.
		var excludeParentKey [16]byte
		var hasExcludeKey bool
		if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
			if leaseReq, decodeErr := DecodeLeaseCreateContext(leaseCtx.Data); decodeErr == nil &&
				leaseReq.Flags&smbenc.LeaseResponseFlagParentKeySet != 0 {
				excludeParentKey = leaseReq.ParentLeaseKey
				hasExcludeKey = true
			}
		}
		// Dispatch the break fire-and-forget, WITHOUT waiting for the holder's
		// LEASE_BREAK_ACK inline (#1768). A new-file CREATE never async-parks, so
		// waiting here would block the CREATE reply on an ACK the creating client
		// can only send over the same credit-limited connection — under sustained
		// fio create load the SMB credit window collapses and Linux cifs.ko returns
		// EDEADLK. The break notification is still sent (the returned dispatch
		// runs inline here); the holder acks on its own schedule. Mirrors Samba
		// contend_dirleases → send_break_to_none.
		h.LeaseManager.PrepareParentDirLeaseBreakOnContentChange(parentLockHandle, tree.ShareName, "", excludeParentKey, hasExcludeKey)()
	}

	// Step 7: Perform create/open.
	var file *metadata.File
	var fileHandle metadata.FileHandle
	switch createAction {
	case types.FileOpened:
		file = existingFile
		var err error
		fileHandle, err = metadata.EncodeFileHandle(file)
		if err != nil {
			logger.Warn("CREATE: failed to encode handle", "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}
		}
	case types.FileCreated:
		var err error
		file, fileHandle, err = h.createNewFile(authCtx, parentHandle, d.parentFile, baseName, req, d.isDirectoryRequest)
		if err != nil {
			// Concurrent-create race: another opener won between our
			// pre-create lookup and the metadata-store transaction. For
			// dispositions that accept an existing target (OPEN_IF,
			// OVERWRITE_IF, SUPERSEDE), fall back to the existing-file branch
			// rather than surfacing the race as OBJECT_NAME_COLLISION. MS-FSA
			// has no concurrency model and does not specify this; it is
			// required by smbtorture
			// smb2.create.mkdir-dup (two parallel OPEN_IF on the same
			// directory name MUST yield 1 CREATED + 1 EXISTED). Strict-CREATE
			// (FILE_CREATE / FILE_OVERWRITE) still surfaces the collision per
			// MS-FSA, matching Samba's open_file_ntcreate fallthrough.
			var storeErr *metadata.StoreError
			if errors.As(err, &storeErr) && storeErr.Code == metadata.ErrAlreadyExists {
				switch req.CreateDisposition {
				case types.FileOpenIf, types.FileOverwriteIf, types.FileSupersede:
					winner, _, lookupErr := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, baseName)
					if lookupErr == nil && winner != nil {
						// Resync draft state to the winner so downstream
						// share-mode + DACL gates and the lease/open
						// bookkeeping run against the real file. The
						// original pre-break share-mode recheck ran on
						// our stale (nil) view; rerun it now. The local
						// existingFile snapshot is not touched: from here the
						// race branch drives file/fileHandle directly and the
						// replayed gates read the winner from d.existingFile.
						fileExists = true
						d.existingFile = winner
						d.fileExists = true
						if enc, encErr := metadata.EncodeFileHandle(winner); encErr == nil {
							d.existingHandle = enc
						}
						// Re-resolve createAction: with the winner in
						// place, OPEN_IF → Opened; OVERWRITE_IF /
						// SUPERSEDE → Overwritten / Superseded.
						newAction, dispErr := ResolveCreateDisposition(req.CreateDisposition, true)
						if dispErr != nil {
							return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(dispErr)}}
						}
						createAction = newAction
						d.createAction = newAction
						// Replay the share-mode + DACL + delete-on-close
						// gates against the winner BEFORE the overwrite/open
						// proceeds. The first invocation (top of
						// completeCreateAfterBreak) ran against the stale nil
						// draft, so a winner with an incompatible share-mode
						// or denying DACL — surfaced only under real path
						// contention on OVERWRITE_IF / SUPERSEDE — would
						// otherwise slip past every access gate (#765). On the
						// OPEN_IF dir-mkdir-dup path the winner is a fresh
						// share-compatible directory, so this is a no-op there.
						// effectiveAccess is immutable across the resync
						// (DesiredAccess / CreateDisposition don't change), so
						// the value computed at the top still applies.
						rg, rc, raceFail := h.recheckExistingFileGates(d, effectiveAccess)
						if raceFail != nil {
							return raceFail
						}
						grantedAccess = rg
						grantedComputed = rc
						// Break the winner's leases/oplocks before the
						// overwrite/open proceeds. The pre-break break ran
						// against the stale (nil) view, so the winner's
						// holders never saw a break: without this, the race
						// branch truncates a file whose holder still holds a
						// write lease and a cached read state. Mirrors the
						// inline-wait arm of breakAndMaybeParkCreate: dispatch
						// the break on the winner's handle, then wait for the
						// delay-mask bits to drain (the pre-break wait cannot
						// be reused — its handle was the nil view).
						raceReason := lock.BreakReasonDefault
						if isDestructiveDisposition(req.CreateDisposition) {
							raceReason = lock.BreakReasonDestructive
						} else if d.req.CreateOptions&types.FileDeleteOnClose != 0 {
							raceReason = lock.BreakReasonSharingViolation
						}
						raceMask := lock.LeaseStateWrite
						if raceReason == lock.BreakReasonSharingViolation {
							raceMask = lock.LeaseStateHandle
						}
						if h.LeaseManager != nil {
							raceHandle := lock.FileHandle(d.existingHandle)
							var raceExceptKey [16]byte
							if d.excludeOwner != nil {
								raceExceptKey = d.excludeOwner.ExcludeLeaseKey
							}
							if err := h.LeaseManager.BreakHandleLeasesOnOpenAsync(raceHandle, d.tree.ShareName, raceReason, d.excludeOwner); err != nil {
								logger.Debug("CREATE: race-recovery winner lease break failed", "error", err)
							}
							if h.LeaseManager.AnyHolderHasLeaseBits(raceHandle, d.tree.ShareName, raceExceptKey, raceMask) {
								releaseResponseOrder(ctx)
								raceWaitCtx, cancelRaceWait := context.WithTimeout(authCtx.Context, lease.AsyncCreateBreakWaitTimeout)
								if err := h.LeaseManager.WaitForOtherKeyBreaks(raceWaitCtx, raceHandle, d.tree.ShareName, raceExceptKey); err != nil {
									logger.Debug("CREATE: race-recovery winner break wait completed", "error", err)
								}
								cancelRaceWait()
							}
						}
						if createAction == types.FileOpened {
							file = winner
							fileHandle = d.existingHandle
						} else {
							var owErr error
							file, fileHandle, owErr = h.overwriteFile(authCtx, winner, req)
							if owErr != nil {
								logger.Warn("CREATE: overwrite-after-create-race failed", "name", baseName, "error", owErr)
								return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(owErr)}}
							}
						}
						break // exits inner CreateDisposition switch (race handled)
					}
				}
			}
			if file == nil {
				logger.Warn("CREATE: failed to create file", "name", baseName, "error", err)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(err)}}
			}
		}
	case types.FileOverwritten, types.FileSuperseded:
		var err error
		file, fileHandle, err = h.overwriteFile(authCtx, existingFile, req)
		if err != nil {
			logger.Warn("CREATE: failed to overwrite file", "name", baseName, "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(err)}}
		}
	}

	// Apply security descriptor from SMB2_CREATE_SD_BUFFER create context.
	// MS-SMB2 §2.2.13.2.2: client supplies an initial SD at CREATE time.
	if createAction == types.FileCreated {
		if sdCtx := FindCreateContext(req.CreateContexts, SDBufferCreateContextTag); sdCtx != nil && len(sdCtx.Data) > 0 {
			opts := h.parseSDOptsForShare(d.tree.ShareName)
			ownerUID, ownerGID, fileACL, parseErr := ParseSecurityDescriptorWithOptions(sdCtx.Data, opts)
			if parseErr != nil {
				logger.Debug("CREATE: failed to parse SD_BUFFER", "path", baseName, "error", parseErr)
			} else {
				setAttrs := &metadata.SetAttrs{}
				apply := false
				if ownerUID != nil {
					setAttrs.UID = ownerUID
					apply = true
				}
				if ownerGID != nil {
					setAttrs.GID = ownerGID
					apply = true
				}
				if fileACL != nil {
					setAttrs.ACL = fileACL
					apply = true
				}
				if apply {
					metaSvc := h.Registry.GetMetadataService()
					if _, setErr := metaSvc.SetFileAttributes(authCtx, fileHandle, setAttrs); setErr != nil {
						logger.Debug("CREATE: failed to apply SD_BUFFER", "path", baseName, "error", setErr)
					} else {
						if updated, getErr := metaSvc.GetFile(authCtx.Context, fileHandle); getErr == nil {
							file = updated
						}
					}
				}
			}
		}
	}

	// Apply extended attributes from an SMB2_CREATE_EA_BUFFER ("ExtA") create
	// context. MS-SMB2 §2.2.13.2.1: the client may attach a
	// FILE_FULL_EA_INFORMATION chain (MS-FSCC §2.4.16 ("FileFullEaInformation")) at CREATE so the file is
	// born with EAs (smbtorture smb2.setinfo opens its test file this way, then
	// asserts the pre-existing EAs survive a later SET_INFO). Only applied on a
	// freshly created file; reopening an existing file does not re-seed EAs.
	if createAction == types.FileCreated {
		if eaCtx := FindCreateContext(req.CreateContexts, CreateContextTagExtendedAttributes); eaCtx != nil && len(eaCtx.Data) > 0 {
			entries, decErr := decodeFullEaEntries(eaCtx.Data)
			if decErr != nil {
				logger.Debug("CREATE: failed to decode EA_BUFFER", "path", baseName, "error", decErr)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}
			}
			// The reserved ACL-xattr slot is server-private and cannot be set
			// through the EA channel (parity with SET_INFO EA).
			reserved := false
			for _, e := range entries {
				if isReservedACLXattrName(e.name) {
					reserved = true
					break
				}
			}
			if reserved {
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}
			}
			if muts := eaMutationsFromEntries(entries); len(muts) > 0 {
				metaSvc := h.Registry.GetMetadataService()
				if _, setErr := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{EAMutations: muts}); setErr != nil {
					logger.Debug("CREATE: failed to apply EA_BUFFER", "path", baseName, "error", setErr)
				} else if updated, getErr := metaSvc.GetFile(authCtx.Context, fileHandle); getErr == nil {
					file = updated
				}
			}
		}
	}

	// Compute GrantedAccess for new/overwritten files. The fileExists branch
	// above already populated grantedAccess from CheckFileAccess on the
	// existing file's DACL (the open-time gate).
	//
	// For FileCreated the file is brand-new: Windows/Samba grant the creator
	// the resolved DesiredAccess as-is and never re-check it against the
	// inherited DACL. The inherited DACL governs subsequent opens by other
	// principals — it does not narrow the creator's handle. This matches
	// Samba `source3/smbd/open.c::open_file_ntcreate`, which only runs the
	// DACL check on existing-file paths, and MS-FSA §2.1.5.1.1 ("Creation of a New File") CreateFile
	// (the DesiredAccess check is gated by the parent DACL, not the new
	// child's inherited DACL). Re-checking would surface the smbtorture
	// failure in smb2.acls.INHERITANCE/INHERITFLAGS/SDFLAGSVSCHOWN, where
	// a parent DACL grants the creator only WRITE_DATA|WRITE_DAC
	// inheritably but the test still expects the new handle to carry
	// SEC_RIGHTS_FILE_ALL (per Samba behavior).
	//
	// For FileOverwritten/Superseded the prior DACL is preserved and the
	// open-time gate at step 6c-bis already validated DesiredAccess against
	// it; grantedAccess was set there. If grantedComputed is false in that
	// branch (defensive — should not happen in practice because
	// fileExists==true for overwrite/supersede) we fall back to the
	// resolved DesiredAccess to avoid under-granting.
	if !grantedComputed {
		grantedAccess = resolveAccessFlags(req.DesiredAccess)
	}

	// Step 7a: Restore frozen timestamps on parent directory (MS-FSA §2.1.5.15.2 ("FileBasicInformation")).
	if createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded {
		h.restoreParentDirFrozenTimestamps(authCtx, parentHandle)
	}

	// Step 7b: Update base object ChangeTime for ADS operations (MS-FSA / NTFS).
	if colonIdx := strings.Index(baseName, ":"); colonIdx > 0 && (createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded) {
		h.updateBaseObjectCtime(authCtx, metaSvc, parentHandle, baseName[:colonIdx])
	}

	// Step 8: Generate FileID.
	smbFileID := h.GenerateFileID()

	// Step 8a: Break conflicting oplocks/leases on existing files for no-oplock opens.
	if fileExists && h.LeaseManager != nil && file.Type != metadata.FileTypeDirectory &&
		req.OplockLevel == OplockLevelNone && !isStatOnlyOpen(req.DesiredAccess) {
		lockFileHandle := lock.FileHandle(fileHandle)
		if breakErr := h.LeaseManager.BreakConflictingOplocksOnOpen(lockFileHandle, tree.ShareName, excludeOwner); breakErr != nil {
			logger.Debug("CREATE: oplock break on open failed", "error", breakErr)
		}
	}

	// Step 8a-bis: disconnected-handle preservation/purge. Preservation is
	// MS-SMB2 §3.3.7.1 ("Handling Loss of a Connection"); the break that can
	// knock a preserved handle below H is §3.3.4.7 ("Object Store Indicates a
	// Lease Break").
	//
	// Evaluate any disconnected durable handles on this metadata handle
	// against the new open's lease/share-mode and purge those that the new
	// open's required break_to would knock below H caching. Runs after the
	// live-lease break (8a) so the disconnected-side check sees the same
	// post-break view, and before lease grant (8b) so the granted state
	// reflects only handles that survive the purge. See
	// disconnected_state_machine.go for the predicate.
	if fileExists && h.DurableStore != nil && len(fileHandle) > 0 {
		var newLeaseState uint32
		var newLeaseKey [16]byte
		if req.OplockLevel == OplockLevelLease {
			if lc := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); lc != nil {
				if parsed, decErr := DecodeLeaseCreateContext(lc.Data); decErr == nil && parsed != nil {
					newLeaseState = parsed.LeaseState
					newLeaseKey = parsed.LeaseKey
				}
			}
		}
		// This callsite is the FRESH-CREATE path; durable reconnect early-returns
		// in create.go before reaching here (see create.go::handleCreate, the
		// ProcessDurableReconnectContext branch). The predicate's contract
		// relies on this — see disconnectedConflictOnNewOpen doc.
		if purged := h.purgeConflictingDisconnectedHandlesForOpen(
			authCtx.Context,
			fileHandle,
			newLeaseState,
			newLeaseKey,
			req.ShareAccess,
			req.DesiredAccess,
		); purged > 0 {
			logger.Debug("CREATE: purged disconnected handles on conflicting open",
				"path", filename,
				"count", purged)
		}
	}

	// Step 8b: Request oplock or lease if applicable.
	var grantedOplock uint8
	var leaseResponse *LeaseResponseContext
	var syntheticLeaseKey [16]byte

	if req.OplockLevel == OplockLevelLease && h.LeaseManager != nil {
		if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
			lockFileHandle := lock.FileHandle(fileHandle)
			var err error
			// Samba `disallow_write_lease`: strip W from this lease grant when a
			// conflicting other open or a disconnected durable handle exists.
			// The new open's own lease key (parsed from the RqLs blob) and SMB
			// FileID are excluded so a same-handle reopen/upgrade is never
			// self-capped (mirrors Samba `is_same_lease`). bestGrantableState in
			// the lock manager already caps W against live *lease* records; this
			// predicate adds the cases it cannot see — non-lease live opens and
			// disconnected durable handles (smb2.durable-v2-open.nonstat-and-lease
			// and keep-disconnected-rh-with-rwh-open).
			var newLeaseKey [16]byte
			if parsed, decErr := DecodeLeaseCreateContext(leaseCtx.Data); decErr == nil && parsed != nil {
				newLeaseKey = parsed.LeaseKey
			}
			disallowWriteLease := h.disallowWriteLeaseForFile(
				authCtx.Context, fileHandle, newLeaseKey, smbFileID, connClientGUID(ctx),
			)
			// A stat-open-only, non-destructive CREATE must not break an
			// existing holder when it requests its own lease — the same
			// carve-out breakAndMaybeParkCreate applies to the CREATE-layer
			// break. Routing the lease grant through the break-suppressing
			// variant closes the timing window that produced the intermittent
			// break in smb2.lease.statopen4 (#751).
			statOpenLease := isStatOnlyOpen(req.DesiredAccess) &&
				!isDestructiveDisposition(req.CreateDisposition)
			leaseResponse, err = ProcessLeaseCreateContext(
				authCtx.Context,
				h.LeaseManager,
				leaseCtx.Data,
				lockFileHandle,
				ctx.SessionID,
				connClientGUID(ctx),
				fmt.Sprintf("smb:%d", ctx.SessionID),
				tree.ShareName,
				file.Type == metadata.FileTypeDirectory,
				disallowWriteLease,
				statOpenLease,
			)
			if err != nil {
				if errors.Is(err, lock.ErrLeaseKeyInUse) {
					// Step 7 already executed the open. The cleanup here depends
					// on what that open did:
					//   - FileCreated: roll back the orphan inode so a CREATE_NEW
					//     that failed lease_match doesn't leave a metadata entry
					//     no client can reach. Samba's open path closes the fd
					//     for the same reason.
					//   - FileOverwritten / FileSuperseded: the prior file's
					//     content was already truncated. Returning
					//     STATUS_INVALID_PARAMETER would tell the client the
					//     CREATE failed while the data is gone — unrecoverable.
					//     Complete the CREATE with no lease instead; the data
					//     loss is committed, but the client at least gets a
					//     working handle.
					//   - FileOpened: nothing destructive happened, fail safely.
					switch createAction {
					case types.FileCreated:
						if _, _, delErr := metaSvc.RemoveFile(authCtx, parentHandle, baseName); delErr != nil {
							logger.Warn("CREATE: failed to roll back orphaned file after lease rejection",
								"name", baseName, "error", delErr)
						}
						// If this CREATE also auto-created the base file (ADS
						// path), roll that back too. Without this the base file is
						// left orphaned: the stream entry is gone but the base
						// inode is unreachable and a subsequent FILE_CREATE on the
						// base name hits ErrAlreadyExists.
						if d.adsBaseCreatedByUs && d.adsBaseFileName != "" {
							if _, _, delBaseErr := metaSvc.RemoveFile(authCtx, parentHandle, d.adsBaseFileName); delBaseErr != nil {
								logger.Warn("CREATE: failed to roll back orphaned ADS base file after lease rejection",
									"name", d.adsBaseFileName, "error", delBaseErr)
							}
						}
						return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}
					case types.FileOverwritten, types.FileSuperseded:
						logger.Warn("CREATE: lease request rejected after destructive open; completing CREATE without lease",
							"name", baseName, "createAction", createAction, "error", err)
						leaseResponse = nil
						grantedOplock = OplockLevelNone
					default:
						return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}
					}
				} else {
					logger.Debug("CREATE: lease context processing failed", "error", err)
				}
			}
			if leaseResponse != nil {
				grantedOplock = OplockLevelLease
				if leaseResponse.LeaseState == lock.LeaseStateNone {
					logger.Debug("CREATE: lease denied, returning OplockLevel=0xFF with LeaseState=None")
				}
			}
		} else {
			grantedOplock = OplockLevelNone
			logger.Debug("CREATE: OplockLevel=Lease without RqLs context, granting None")
		}
	}

	if grantedOplock == OplockLevelNone && req.OplockLevel != OplockLevelNone &&
		req.OplockLevel != OplockLevelLease && h.LeaseManager != nil &&
		file.Type != metadata.FileTypeDirectory &&
		// Strip a traditional oplock request to NONE when the new opener's
		// EFFECTIVE access mask is oplock-stat-only (Samba
		// `is_oplock_stat_open` — FILE_READ_ATTRIBUTES / FILE_WRITE_ATTRIBUTES
		// / SYNCHRONIZE only, NO READ_CONTROL). Mirrors `open_file_ntcreate`
		// source3/smbd/open.c line 4000:
		//
		//	if (is_oplock_stat_open(open_access_mask) && lease == NULL) {
		//	    oplock_request &= SAMBA_PRIVATE_OPLOCK_MASK;
		//	}
		//
		// Samba's comment on this gate is load-bearing:
		//
		//	stat opens on existing files don't get oplocks. They can get
		//	leases. Note that we check for stat open on the
		//	*open_access_mask*, i.e. the access mask we actually used to do
		//	the open, not the one the client asked for (which is in
		//	fsp->access_mask). This is due to the fact that FILE_OVERWRITE
		//	and FILE_OVERWRITE_IF add in O_TRUNC, which adds FILE_WRITE_DATA
		//	to open_access_mask.
		//
		// So a destructive disposition (OVERWRITE / OVERWRITE_IF /
		// SUPERSEDE) with attrs-only DesiredAccess still gets the requested
		// oplock — the implicit truncate-WRITE_DATA disqualifies it from
		// the stat-open carve-out. smbtorture smb2.oplock.exclusive5 covers
		// this: OVERWRITE_IF + attrs-only request must grant LEVEL_II
		// (NOT NONE). batch8 / exclusive4 (non-destructive + attrs-only)
		// must strip to NONE.
		(!fileExists || !isOplockStatOpen(effectiveAccessForOpen(req.DesiredAccess, req.CreateDisposition))) {

		var requestedState uint32
		switch req.OplockLevel {
		case OplockLevelBatch:
			requestedState = lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
		case OplockLevelExclusive:
			requestedState = lock.LeaseStateRead | lock.LeaseStateWrite
		case OplockLevelII:
			requestedState = lock.LeaseStateRead
		}
		// Coerce Batch/Exclusive → LEVEL_II when another "effective"
		// holder is present on the same file, mirroring Samba's
		// `disallow_write_lease` predicate (source3/smbd/open.c:2397).
		// The lock layer's bestGrantableState already handles records
		// with active R/W/H bits; this branch covers the case where the
		// record is absent or at LeaseStateNone but the underlying open
		// is still alive — those would otherwise slip past
		// bestGrantableState and yield a too-permissive grant.
		//
		// An "effective" holder is either:
		//   1) A non-stat-only OpenFile on the same metadata handle —
		//      batch10 (tree1 raw-open → tree2 BATCH coerced to LEVEL_II).
		//   2) A live lease/oplock record (not a BrokenViaTimeout tombstone)
		//      regardless of whether the holder's OpenFile is stat-only —
		//      batch9a / batch13 / batch14 / batch16 where tree1's attrs-only
		//      open holds a trad-oplock record that has been acked through a
		//      break.  Samba's `disallow_write_lease` gates on `op_type !=
		//      NO_OPLOCK` independent of the access mask, so an attrs-only
		//      oplock holder still constrains the next opener.
		// Timeout tombstones (BrokenViaTimeout=true) are excluded from BOTH
		// paths so smb2.oplock.batch22b can grant a fresh BATCH after the
		// abandoned holder times out (tree1's OpenFile is still alive but
		// its record is a tombstone).
		//
		// Same-client carve-out (smbtorture smb2.oplock.batch22a, MS-SMB2
		// §3.3.4.6): even when only timeout tombstones remain, if another
		// non-stat OpenFile on the same ClientGUID is still alive, the new
		// grant for THIS client must collapse to LEVEL_II. The abandoned
		// holder may still hold dirty cached state on the client side, so
		// exclusive batch caching cannot be regranted to the same client.
		// Cross-client re-opens (batch22b) bypass this since the new client
		// has no caching relationship with the abandoned holder.
		lockFileHandle := lock.FileHandle(fileHandle)
		onlyTimeoutTombstone := h.LeaseManager.OnlyTimeoutTombstoneRecords(lockFileHandle, tree.ShareName)
		hasActiveRecord := h.LeaseManager.HasActiveLeaseRecord(lockFileHandle, tree.ShareName, [16]byte{})
		// Single O(F) pass over h.files yields both the non-stat-open flag and
		// the same-client carve-out. The same-client check is only consulted
		// once NEGOTIATE has established a connection identity — without
		// CryptoState the requestor's identity is unknown and a zero-vs-zero
		// match would catch unrelated pre-NEGOTIATE opens (regressed
		// smb2.compound.interim2 when the early gate was removed).
		checkSameClient := ctx != nil && ctx.ConnCryptoState != nil
		var sameClientGUID [16]byte
		if checkSameClient {
			sameClientGUID = connClientGUID(ctx)
		}
		hasNonStatOpen, hasSameClientOpen := h.scanNonStatOpensForFile(
			fileHandle, smbFileID, sameClientGUID, checkSameClient,
		)
		if (!onlyTimeoutTombstone && (hasActiveRecord || hasNonStatOpen)) ||
			(onlyTimeoutTombstone && hasSameClientOpen) {
			requestedState &^= (lock.LeaseStateWrite | lock.LeaseStateHandle)

			// A coexisting handle forced the EXCLUSIVE/BATCH request down off
			// its write-caching grant. What remains (LeaseStateRead) would map
			// to LEVEL_II, but MS-SMB2 §3.3.5.9 only permits a read-caching
			// (LEVEL_II) grant when a coexisting handle is itself a read-cache
			// participant — i.e. it holds an oplock/lease the server can break.
			// A plain NO-oplock opener (e.g. tree2 in
			// smb2.kernel-oplocks.kernel_oplocks7, opened with oplock_level
			// NONE) is not such a participant: it caches nothing the server
			// tracks, so granting the requester LEVEL_II would hand out a
			// read-cache the spec does not back. When the strip is driven
			// SOLELY by a non-oplock opener (no active read-cache record), an
			// EXCLUSIVE request must collapse to NONE rather than LEVEL_II.
			//
			// A BATCH request still lands at LEVEL_II in this case: the
			// surviving HANDLE bit qualifies it for the read-caching grant
			// (Samba `disallow_write_lease` only strips WRITE, leaving R|H →
			// LEVEL_II). This preserves smb2.oplock.batch10 (tree1 NO-oplock
			// open → tree2 BATCH → LEVEL_II) while fixing kernel_oplocks7
			// (tree2 NO-oplock open → tree1 EXCLUSIVE reopen → NONE, the
			// userspace-deterministic one of its two spec-valid outcomes).
			// When a read-cache record IS present (smb2.oplock.exclusive9:
			// tree1 holds EXCLUSIVE → tree2 EXCLUSIVE → LEVEL_II) the grant
			// stays LEVEL_II via hasActiveRecord.
			grantDrivenOnlyByNonOplockOpen := !onlyTimeoutTombstone &&
				hasNonStatOpen && !hasActiveRecord
			requesterCarriesHandleBit := req.OplockLevel == OplockLevelBatch
			if grantDrivenOnlyByNonOplockOpen && !requesterCarriesHandleBit {
				requestedState = 0
			}
		}
		if requestedState != 0 {
			syntheticKey := generateSyntheticLeaseKey(smbFileID)
			ownerID := fmt.Sprintf("smb:oplock:%x", smbFileID)
			clientID := fmt.Sprintf("smb:%d", ctx.SessionID)
			// RequestLeaseAsOplock tags the new record IsTraditionalOplock=true
			// so MS-SMB2 §3.3.5.9 cross-tier rules apply on subsequent grants
			// (Samba `state.got_handle_lease` / `state.got_oplock`). See
			// `pkg/metadata/lock/leases.go::bestGrantableState`.
			grantedState, _, err := h.LeaseManager.RequestLeaseAsOplock(
				authCtx.Context,
				lockFileHandle,
				syntheticKey,
				[16]byte{},
				ctx.SessionID,
				connClientGUID(ctx),
				ownerID,
				clientID,
				tree.ShareName,
				requestedState,
				false,
			)
			if err != nil {
				logger.Debug("CREATE: traditional oplock lease request failed", "error", err)
			} else {
				grantedOplock = leaseStateToOplockLevel(grantedState)
				if grantedOplock != OplockLevelNone {
					syntheticLeaseKey = syntheticKey
					h.LeaseManager.RegisterOplockFileID(syntheticKey, smbFileID)
				}
				logger.Debug("CREATE: traditional oplock mapped to lease",
					"requestedOplock", oplockLevelName(req.OplockLevel),
					"grantedOplock", oplockLevelName(grantedOplock),
					"leaseState", lock.LeaseStateToString(grantedState))
			}
		}
	}

	// Step 8b-bis: Directory-CREATE oplock gate (MS-SMB2 §3.3.5.9; Samba
	// smbd_smb2_create_oplock_check). See clampDirectoryOplockLevel for the
	// rationale; covers smbtorture smb2.dirlease.oplocks.
	if file.Type == metadata.FileTypeDirectory {
		clamped, cleared := clampDirectoryOplockLevel(grantedOplock)
		if clamped != grantedOplock {
			logger.Debug("CREATE: clamping non-lease oplock to NONE on directory CREATE",
				"requestedOplock", oplockLevelName(req.OplockLevel),
				"grantedOplock", oplockLevelName(grantedOplock))
			grantedOplock = clamped
			if cleared {
				syntheticLeaseKey = [16]byte{}
			}
		}
	}

	// Step 8c: Process App Instance ID and durable handle grant.
	//
	// The AppInstanceId force-close normally runs in the pre-break CREATE path
	// (Create, before breakAndMaybeParkCreate) so a conflicting open carrying
	// the same AppInstanceId is displaced BEFORE any oplock/lease break is
	// computed — MS-SMB2 §3.3.5.9.13 requires the failover to be silent (no
	// break on the displaced open; smbtorture smb2.durable-v2-open.app-instance
	// asserts break_info.count == 0). When that ran, reuse its result. Only
	// fall back to processing here for callers that bypass the pre-break path.
	var durableResponseCtx *CreateContext
	var appInstanceId [16]byte
	if d.appInstanceProcessed {
		appInstanceId = d.appInstanceId
	} else if h.DurableStore != nil {
		appInstanceId = ProcessAppInstanceId(
			authCtx.Context, h.DurableStore, h, req.CreateContexts,
		)
	}

	openFile := &OpenFile{
		FileID:               smbFileID,
		TreeID:               ctx.TreeID,
		SessionID:            ctx.SessionID,
		ShareName:            tree.ShareName,
		OpenTime:             time.Now(),
		DesiredAccess:        req.DesiredAccess,
		GrantedAccess:        grantedAccess,
		IsDirectory:          file.Type == metadata.FileTypeDirectory,
		MetadataHandle:       fileHandle,
		PayloadID:            file.PayloadID,
		OplockLevel:          grantedOplock,
		ShareAccess:          req.ShareAccess,
		CreateOptions:        req.CreateOptions,
		InitialDeleteOnClose: req.CreateOptions&types.FileDeleteOnClose != 0,
		ClientGUID:           connClientGUID(ctx),
		// Honour the client-requested AllocationSize [MS-SMB2] 2.2.13.2.2 for
		// regular files only. Directories never report the requested reservation
		// (smb2.create.dir-alloc-size), so leave it zero for them. Tracked
		// per-handle so the CREATE response and a later QUERY_INFO on this handle
		// agree (smb2.create.open).
		RequestedAllocSize: allocReservationFor(file.Type == metadata.FileTypeDirectory, req.RequestedAllocSize),
		// Record the requested DH2Q CreateGuid for replay-cache keying,
		// independent of whether V2 durability is granted below. A no-oplock
		// open never sets CreateGuid (durability requires Batch/Handle lease),
		// but its CREATE must still be replay-cacheable (MS-SMB2 §3.3.5.9).
		ReplayCreateGuid: dh2qCreateGuid(req),
	}
	openFile.SetName(OpenName{Path: filename, FileName: baseName, ParentHandle: parentHandle})

	if leaseResponse != nil && leaseResponse.LeaseState != lock.LeaseStateNone {
		openFile.LeaseKey = leaseResponse.LeaseKey
	} else if syntheticLeaseKey != ([16]byte{}) {
		openFile.LeaseKey = syntheticLeaseKey
	}

	// Snapshot the opener's identity so handle-bound ops (notably SET_INFO
	// SecurityDescriptor) stay anchored to the user who actually opened
	// the handle after SESSION_SETUP re-auth swaps Session.User out from
	// under us. MS-SMB2 §3.3.5.5.3 + smbtorture smb2.session.reauth4/5.
	h.CaptureOpenerIdentity(ctx, openFile)

	// Record the RqLs parent-lease-key linkage so downstream operations on
	// this handle (SET_INFO, WRITE, CLOSE-on-delete) can apply the Samba
	// `dirlease_should_break` parent-key suppression rule
	// against the parent directory's lease. Captured even when the file
	// lease itself was denied (response leaseState=None) — the linkage
	// applies regardless of the per-file grant outcome. Gated on HasParent
	// so a zero-key request without LEASE_FLAG_PARENT_LEASE_KEY_SET is
	// treated as "no linkage".
	if leaseResponse != nil && leaseResponse.HasParent {
		openFile.ParentLeaseKey = leaseResponse.ParentLeaseKey
		openFile.HasParentLeaseKey = true
	}

	// Record the DOC setter's parent key for initial delete-on-close
	// (CREATE with FILE_DELETE_ON_CLOSE). Covers dirlease unlink tests.
	if openFile.InitialDeleteOnClose {
		openFile.DeleteOnCloseParentKey = openFile.ParentLeaseKey
		openFile.HasDeleteOnCloseParentKey = openFile.HasParentLeaseKey
	}

	if h.DurableStore != nil {
		hasHandleLease := leaseResponse != nil && leaseResponse.LeaseState&lock.LeaseStateHandle != 0
		if respCtx := ProcessDurableHandleContext(
			req.CreateContexts, openFile, DurableGrantOptions{
				ConfiguredTimeoutMs:    h.DurableTimeoutMs,
				LeaseIncludesHandle:    hasHandleLease,
				ContinuousAvailability: tree.ContinuousAvailability,
			},
		); respCtx != nil {
			durableResponseCtx = respCtx
		}
		if openFile.IsDurable && appInstanceId != ([16]byte{}) {
			openFile.AppInstanceId = appInstanceId
		}
	}

	h.StoreOpenFile(openFile)

	logger.Debug("CREATE successful",
		"fileID", fmt.Sprintf("%x", smbFileID),
		"filename", filename,
		"action", createAction,
		"isDirectory", openFile.IsDirectory,
		"fileType", int(file.Type),
		"fileSize", file.Size,
		"oplock", oplockLevelName(grantedOplock))

	// Step 9: Notify change watchers.
	if h.NotifyRegistry != nil {
		parentPath := GetParentPath(filename)
		switch createAction {
		case types.FileCreated:
			nameFilter := NameChangeFilterFor(baseName, openFile.IsDirectory)
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionAdded, nameFilter)
		case types.FileOverwritten, types.FileSuperseded:
			// One operation, three records: the client must see them in one
			// CHANGE_NOTIFY response, so they are emitted as one batch.
			nameFilter := NameChangeFilterFor(baseName, openFile.IsDirectory)
			h.NotifyRegistry.NotifyChanges(tree.ShareName, parentPath, []NotifyEvent{
				{FileName: baseName, Action: FileActionRemoved, Filter: nameFilter},
				{FileName: baseName, Action: FileActionAdded, Filter: nameFilter},
				{FileName: baseName, Action: FileActionModified, Filter: FileNotifyChangeAttributes | FileNotifyChangeLastWrite | FileNotifyChangeSize},
			})
		}
	}

	// Step 10: Build success response.
	//
	// Per NTFS: alternate data streams share the base file's timestamps
	// and attributes. When the open is for an ADS, resolve the base file
	// and use its metadata for the CREATE response times and attributes.
	// The stream's own size is used for EndOfFile / AllocationSize.
	respAttr := &file.FileAttr
	if colonIdx := strings.Index(baseName, ":"); colonIdx > 0 && len(parentHandle) > 0 {
		baseFileName := baseName[:colonIdx]
		if baseFile, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, baseFileName); baseFile != nil {
			respAttr = &baseFile.FileAttr
		}
	}
	creation, access, write, change := FileAttrToSMBTimes(respAttr)
	size := getSMBSize(&file.FileAttr)
	// Report the larger of the file's cluster-aligned size and the per-handle
	// requested reservation [MS-SMB2] 2.2.13.2.2 (zero for directories), so a
	// freshly-created empty file opened with in.alloc_size reports a non-zero
	// out.alloc_size (smb2.durable-open.alloc-size).
	allocationSize := effectiveAllocationSize(size, openFile.RequestedAllocSize)

	resp := &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     grantedOplock,
		CreateAction:    createAction,
		CreationTime:    creation,
		LastAccessTime:  access,
		LastWriteTime:   write,
		ChangeTime:      change,
		AllocationSize:  allocationSize,
		EndOfFile:       size,
		FileAttributes:  FileAttrToSMBAttributes(respAttr),
		FileID:          smbFileID,
	}

	if leaseResponse != nil {
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: LeaseContextTagResponse,
			Data: leaseResponse.Encode(),
		})
		logger.Debug("CREATE: lease granted in response",
			"leaseKey", fmt.Sprintf("%x", leaseResponse.LeaseKey),
			"grantedState", lock.LeaseStateToString(leaseResponse.LeaseState),
			"epoch", leaseResponse.Epoch)
	}

	if durableResponseCtx != nil {
		resp.CreateContexts = append(resp.CreateContexts, *durableResponseCtx)
		logger.Debug("CREATE: durable handle granted in response",
			"isDurable", openFile.IsDurable,
			"createGuid", fmt.Sprintf("%x", openFile.CreateGuid),
			"timeoutMs", openFile.DurableTimeoutMs)
	}

	if FindCreateContext(req.CreateContexts, "MxAc") != nil {
		maxAccess := metaSvc.ComputeMaximalAccess(file, authCtx)
		mxW := smbenc.NewWriter(8)
		mxW.WriteUint32(0)
		mxW.WriteUint32(maxAccess)
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: "MxAc",
			Data: mxW.Bytes(),
		})
		logger.Debug("CREATE: MxAc response added",
			"maximalAccess", fmt.Sprintf("0x%08x", maxAccess))
	}

	if aapl := FindCreateContext(req.CreateContexts, aaplCreateContextTag); aapl != nil {
		if aaplResp := buildAAPLServerQueryResponse(aapl.Data); aaplResp != nil {
			resp.CreateContexts = append(resp.CreateContexts, CreateContext{
				Name: aaplCreateContextTag,
				Data: aaplResp,
			})
			logger.Debug("CREATE: AAPL response added (UNIX-based volume caps)")
		}
	}

	if FindCreateContext(req.CreateContexts, "QFid") != nil {
		qfidFileID := h.baseFileUUID(authCtx, parentHandle, baseName, file.ID)
		qfidResp := make([]byte, 32)
		copy(qfidResp[0:16], qfidFileID[:16])
		copy(qfidResp[16:32], h.ServerGUID[:])
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: "QFid",
			Data: qfidResp,
		})
		logger.Debug("CREATE: QFid response added",
			"diskFileId", fmt.Sprintf("%x", qfidFileID[:16]))
	}

	// SMB3 replay protection (MS-SMB2 §3.3.5.9): record the freshly-built
	// success response keyed by the REQUESTED DH2Q CreateGuid so a
	// FLAGS_REPLAY_OPERATION retry within the replay window returns the
	// cached result (same FileId) instead of re-running CREATE. Keyed on
	// ReplayCreateGuid, not the durability-granting CreateGuid: a no-oplock
	// open never gets V2 durability (Batch/Handle lease required) yet its
	// CREATE must still be replay-cacheable, and the replay must echo the
	// same handle (smb2.replay.dhv2-pending1n-vs-{oplock,lease}-sane io24).
	if h.CreateReplayCache != nil && openFile != nil && openFile.ReplayCreateGuid != ([16]byte{}) {
		h.CreateReplayCache.Store(openFile.SessionID, openFile.ReplayCreateGuid, resp, openFile)
	}

	return resp
}

// parkCreateOnLeaseBreak reserves an async slot, registers a pending CREATE,
// and spawns a resume goroutine that waits for the break to drain and then
// completes the CREATE via AsyncCreateCompleteCallback. Returns the generated
// AsyncId on success, or 0 when async parking is not possible (e.g. no async
// slots left; registry rejected the entry). Callers fall back to sync wait.
//
// breakWaitTimeout bounds the server-side wait for the break to drain (or
// auto-downgrade on expiry). Callers pass TraditionalOplockBreakWaitTimeout
// (~35 s, MS-SMB2 §3.3.4.6) when the holder is a traditional oplock and
// AsyncCreateBreakWaitTimeout (~5 s) otherwise.
//
// shareConflictWait selects the deferred-open resume semantics for the
// share-violation case (reason == BreakReasonSharingViolation): instead of
// force-completing the holder's lease on timeout and rechecking once, the
// resume goroutine waits for the live share-mode conflict to clear — the holder
// CLOSEs (conflict gone → CREATE proceeds) or only ACKs the break (open kept →
// SHARING_VIOLATION on the final recheck), and the holder's deferred ACK still
// succeeds because the lease is never tombstoned here (smbtorture replay
// dhv2-pending1n-vs-violation-lease-{close,ack}-sane, MS-SMB2 §3.3.5.9 /
// Samba defer_open→retry_open).
