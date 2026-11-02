package vfs

import "context"

// Service is the filesystem service: every operation any protocol needs, one
// interface per area. An adapter is handed the whole Service; a component
// that needs one area takes that area's interface.
//
// Every method that acts for a caller takes it as a Call: the Identity the
// adapter proved, beside the client, session and channel it came on. A call
// naming no open leaves its Client unused. Every mutating method returns the
// attributes its reply needs, so no handler follows a mutation with a
// GetAttr. Every open-state call is refused StateError NotYours for an
// ID, or state reached through one, that the call's client or principal does
// not hold; a delegation is held by its client alone, and lends no access.
type Service interface {
	Clients
	Names
	Attributes
	Data
	Locking
	Shares
	Principals
}

// Clients is client records and leases. A call naming no share is mapped
// under the installation's defaults, never a share's. The record a call acts
// on is the ClientID it names; c.Client is the client the call came on, zero
// until Connect has answered one.
type Clients interface {
	// Propose is a two-step client's first call; it changes no state and is
	// held by the node that answered it.
	Propose(ctx context.Context, c Call, info ClientInfo) (Proposal, error)
	// Connect registers info, confirming info.Proposal where set, with cb as
	// the client's one Callbacks, the latest Connect's replacing any earlier.
	// Again on another node it moves them there; for an NFSv4 client, refused
	// StateError ClientInUse for any principal but the client's own. An SMB
	// client is connected once per node, at its first authenticated session
	// there, whatever its principal, and never by a null or guest one, and
	// again at each new node epoch while the node holds a session of it:
	// that lists the node in the record's sessions and answers the entry's
	// Gen, which the last session's Disconnect passes back; any other
	// client's is zero. Each time it answers a Gen, a re-list's included,
	// the adapter decides again under its per-client step, and if the node
	// then holds no session of the client, its last having ended or the
	// setup's connection having dropped while the call ran, it sends
	// Disconnect with Last and that Gen itself.
	Connect(ctx context.Context, c Call, info ClientInfo, cb Callbacks) (ClientID, uint64, error)
	// AddSession records this node's session, listing the node in the
	// record's sessions with its entry's Gen raised, and writes next as the
	// record's adapter bytes if they still equal prev; otherwise it writes
	// nothing and answers StateError RecordChanged with the current record.
	// On its success the adapter reads its node's entry's Gen from the
	// record answered and decides again, as after Connect: if the node then
	// holds no session of the client, it sends Disconnect with Last and that
	// Gen.
	AddSession(ctx context.Context, c Call, cl ClientID, prev, next []byte) (ClientRecord, error)
	// Record is the record as written, its adapter bytes included.
	Record(ctx context.Context, c Call, cl ClientID) (ClientRecord, error)
	Renew(ctx context.Context, c Call, cl ClientID) (ClientStatus, error)
	// Disconnect ends one session of client cl as end says: SMB LOGOFF or
	// a failed re-authentication's teardown with Logoff, closing every open
	// of the session; the end of its last channel without, keeping its
	// durable opens for their Timeout; NFSv4.1 DESTROY_SESSION, whose
	// session names no open, with Last only. Its closes run as a Close's do:
	// locks released, waiters served, delete-on-close and orphaned content
	// released. Last marks this node's last session of cl: once the closes
	// have run, it removes the node from the record's sessions in the same
	// call if the entry's Gen is still end.Gen, so a Destroy is no longer
	// refused for it. A listing since keeps the node listed, and Disconnect
	// answers that entry's newer Gen; it answers zero once the entry is
	// gone, and StateError StaleClient once the record is released.
	//
	// The adapter decides first and last under a per-client step of its own,
	// held only around that decision and never across this call or Connect.
	// On a non-zero Gen it stores that Gen and decides again under its step,
	// repeating the Disconnect with it if the node still holds no session of
	// the client, and stopping otherwise. An error leaves the node listed:
	// the adapter repeats the call until it succeeds, and a repeat finds the
	// closed opens gone and closes nothing twice; StaleClient, a repeat's
	// answer once the record is released, it counts as success.
	Disconnect(ctx context.Context, c Call, cl ClientID, s SessionRef, end SessionEnd) (uint64, error)
	// Destroy ends a client holding no state, StateError ClientBusy
	// otherwise; a revoked reference is not state it holds.
	Destroy(ctx context.Context, c Call, cl ClientID) error
	// ReclaimComplete ends only the grace instances shown, as Renew reported
	// them.
	ReclaimComplete(ctx context.Context, c Call, cl ClientID, shown []GraceRef) error
	// FreeState frees a revoked reference, or a lock state holding no locks.
	FreeState(ctx context.Context, c Call, cl ClientID, s StateRef) error
	// TestState answers one error per reference, in order, changing nothing.
	TestState(ctx context.Context, c Call, cl ClientID, s []StateRef) []error
	// ReleaseLockOwner is StateError LocksHeld while o holds a lock.
	ReleaseLockOwner(ctx context.Context, c Call, cl ClientID, o LockOwner) error
	// HostRestarted expires the host's old client: an NSM notice, or an NLM
	// FREE_ALL.
	HostRestarted(ctx context.Context, c Call, host []byte, state uint32) error
}

// Names is the namespace. Every method also takes a pseudo-filesystem handle.
type Names interface {
	Lookup(ctx context.Context, c Call, dir Handle, name []byte) (Handle, File, error)
	ReadDir(ctx context.Context, c Call, dir Handle, after Cursor, max int, want EntryFields) (Page, error)
	// Create makes a directory, symlink, special file, or an NFSv3 regular
	// file.
	Create(ctx context.Context, c Call, dir Handle, name []byte, a CreateArgs) (CreateResult, error)
	Link(ctx context.Context, c Call, dir Handle, name []byte, target Handle) (CreateResult, error)
	// Unlink removes any entry, a directory only when empty.
	Unlink(ctx context.Context, c Call, dir Target, name []byte) (UnlinkResult, error)
	// Rmdir removes a directory only: ErrNotDir otherwise.
	Rmdir(ctx context.Context, c Call, dir Target, name []byte) (UnlinkResult, error)
	Rename(ctx context.Context, c Call, from Target, fromName []byte, to Handle, toName []byte, noReplace bool) (RenameResult, error)
}

// Attributes is attributes, ACLs, xattrs and streams.
type Attributes interface {
	GetAttr(ctx context.Context, c Call, h Handle) (File, error)
	SetAttr(ctx context.Context, c Call, t Target, a AttrChange) (WCC, error)
	GetACL(ctx context.Context, c Call, h Handle) (ACL, error)
	SetACL(ctx context.Context, c Call, t Target, acl ACL) (WCC, error)
	Xattrs(ctx context.Context, c Call, h Handle) ([]Xattr, error)
	SetXattr(ctx context.Context, c Call, t Target, x Xattr, m XattrMode) (WCC, error)
	RemoveXattr(ctx context.Context, c Call, t Target, name []byte) (WCC, error)
	Streams(ctx context.Context, c Call, h Handle) ([]File, error)
	// Access is the part of want the caller is granted; parent is zero when
	// unknown.
	Access(ctx context.Context, c Call, h, parent Handle, want Access) (Access, error)
}

// Data is opens and I/O.
type Data interface {
	Open(ctx context.Context, c Call, req OpenRequest) (OpenResult, error)
	// Downgrade and ConfirmOpen answer the open's new stateid seqid.
	Downgrade(ctx context.Context, c Call, ref OpenRef, q Seqs, access, deny Access) (uint32, error)
	ConfirmOpen(ctx context.Context, c Call, ref OpenRef, q Seqs) (uint32, error)
	// Close answers the parent's WCC when a delete on close ran.
	Close(ctx context.Context, c Call, ref OpenRef, q Seqs) (*WCC, error)
	Read(ctx context.Context, c Call, ref OpenRef, off int64, dst []byte) (n int, eof bool, err error)
	Write(ctx context.Context, c Call, ref OpenRef, off int64, src []byte, s Stability) (WriteResult, error)
	Commit(ctx context.Context, c Call, ref OpenRef, off, length int64) (Verifier, error)
	Deallocate(ctx context.Context, c Call, ref OpenRef, off, length int64) (WCC, error)
	Seek(ctx context.Context, c Call, ref OpenRef, off int64, what SeekWhat) (int64, error)
	// Copy copies in bounded chunks, synchronously, and may be short.
	Copy(ctx context.Context, c Call, a CopyArgs) (CopyResult, error)
	// Clone is atomic.
	Clone(ctx context.Context, c Call, a CopyArgs) (WCC, error)
}

// Locking is byte-range locks, caching grants and watches. AckRecall, Watch
// and Unwatch act for c.Client.
type Locking interface {
	// Lock answers the lock state's reference, its new Seq included; zero
	// where the protocol names none.
	Lock(ctx context.Context, c Call, ref OpenRef, r LockReq) (StateRef, error)
	CancelLock(ctx context.Context, c Call, ref OpenRef, r ByteRange) (granted bool, err error)
	TestLock(ctx context.Context, c Call, ref OpenRef, r ByteRange, exclusive bool) (*Lock, error)
	// Unlock releases rs in order, stopping at the first range not held with
	// ErrRangeNotLocked, and answers the lock state's new seqid.
	Unlock(ctx context.Context, c Call, ref OpenRef, rs []ByteRange, q Seqs) (uint32, error)
	// AckRecall answers a recall of g; to GrantNone returns it.
	AckRecall(ctx context.Context, c Call, g GrantID, to GrantKind) error
	Watch(ctx context.Context, c Call, w WatchSpec) (WatchID, error)
	Unwatch(ctx context.Context, c Call, w WatchID) error
}

// Shares is shares and what they report.
type Shares interface {
	// Root is a share's root handle; the zero ShareRef names the
	// pseudo-root.
	Root(ctx context.Context, c Call, s ShareRef) (Handle, ShareInfo, error)
	SecInfo(ctx context.Context, c Call, dir Handle, name []byte) ([]AuthFlavor, error)
	ListShares(ctx context.Context, c Call) ([]ShareInfo, error)
	// StatFS is the share's, or the tighter of it and h's project.
	StatFS(ctx context.Context, c Call, h Handle) (FilesystemInfo, error)
	// Quota is p's own, else the share's default for its kind.
	Quota(ctx context.Context, c Call, h Handle, p Principal) (Usage, Quota, error)
	Snapshots(ctx context.Context, c Call, h Handle) ([]SnapshotInfo, error)
}

// Principals is sign-in and principal spellings. They act on no share, are
// not admitted, and run on the node holding the connection.
type Principals interface {
	SignIn(ctx context.Context, step SignInStep) (SignInResult, error)
	Spell(ctx context.Context, p Principal, as Scheme) (Spelling, error)
	// Find is the principal holding s; it mints a directory principal only
	// for a proven id.
	Find(ctx context.Context, c Call, s Spelling) (Principal, error)
}

// Op names one Service method, as an event or a metric names it.
type Op uint8

const (
	OpPropose Op = iota + 1
	OpConnect
	OpAddSession
	OpRecord
	OpRenew
	OpDisconnect
	OpDestroy
	OpReclaimComplete
	OpFreeState
	OpTestState
	OpReleaseLockOwner
	OpHostRestarted

	OpLookup
	OpReadDir
	OpCreate
	OpLink
	OpUnlink
	OpRmdir
	OpRename

	OpGetAttr
	OpSetAttr
	OpGetACL
	OpSetACL
	OpXattrs
	OpSetXattr
	OpRemoveXattr
	OpStreams
	OpAccess

	OpOpen
	OpDowngrade
	OpConfirmOpen
	OpClose
	OpRead
	OpWrite
	OpCommit
	OpDeallocate
	OpSeek
	OpCopy
	OpClone

	OpLock
	OpCancelLock
	OpTestLock
	OpUnlock
	OpAckRecall
	OpWatch
	OpUnwatch

	OpRoot
	OpSecInfo
	OpListShares
	OpStatFS
	OpQuota
	OpSnapshots

	OpSignIn
	OpSpell
	OpFind

	opEnd
)

var opNames = [...]string{
	OpPropose: "Propose", OpConnect: "Connect", OpAddSession: "AddSession", OpRecord: "Record",
	OpRenew: "Renew", OpDisconnect: "Disconnect",
	OpDestroy: "Destroy", OpReclaimComplete: "ReclaimComplete",
	OpFreeState: "FreeState", OpTestState: "TestState", OpReleaseLockOwner: "ReleaseLockOwner",
	OpHostRestarted: "HostRestarted",
	OpLookup:        "Lookup", OpReadDir: "ReadDir", OpCreate: "Create", OpLink: "Link", OpUnlink: "Unlink",
	OpRmdir: "Rmdir", OpRename: "Rename",
	OpGetAttr: "GetAttr", OpSetAttr: "SetAttr", OpGetACL: "GetACL", OpSetACL: "SetACL", OpXattrs: "Xattrs",
	OpSetXattr: "SetXattr", OpRemoveXattr: "RemoveXattr", OpStreams: "Streams", OpAccess: "Access",
	OpOpen: "Open", OpDowngrade: "Downgrade", OpConfirmOpen: "ConfirmOpen", OpClose: "Close", OpRead: "Read",
	OpWrite: "Write", OpCommit: "Commit", OpDeallocate: "Deallocate", OpSeek: "Seek", OpCopy: "Copy",
	OpClone: "Clone",
	OpLock:  "Lock", OpCancelLock: "CancelLock", OpTestLock: "TestLock", OpUnlock: "Unlock",
	OpAckRecall: "AckRecall", OpWatch: "Watch", OpUnwatch: "Unwatch",
	OpRoot: "Root", OpSecInfo: "SecInfo", OpListShares: "ListShares", OpStatFS: "StatFS", OpQuota: "Quota",
	OpSnapshots: "Snapshots",
	OpSignIn:    "SignIn", OpSpell: "Spell", OpFind: "Find",
}

// String is the Service method op names.
func (op Op) String() string {
	if op == 0 || op >= opEnd {
		return "Op(?)"
	}
	return opNames[op]
}

// Mutates reports whether op may change state: a file, open state, a client
// record or a lease. A handler makes at most one call for which it is true,
// and any number of the rest.
//
// decision: the calls that change nothing are exactly the twenty in the
// switch below, the service contract's list of calls a handler may repeat.
// Record and Spell are on it: an NFSv4 GETATTR asking for owner and
// owner_group spells twice, and a READDIR asking for them once per entry.
// A call added here must change nothing, as the one-call
// check stops seeing it.
func Mutates(op Op) bool {
	switch op {
	case OpPropose, OpTestState, OpLookup, OpReadDir, OpGetAttr, OpGetACL, OpXattrs, OpStreams,
		OpAccess, OpRead, OpSeek, OpTestLock, OpRoot, OpSecInfo, OpListShares, OpStatFS, OpQuota,
		OpSnapshots, OpRecord, OpSpell:
		return false
	}
	return true
}
