// Package vfs is the filesystem service's contract: the one interface the
// protocol adapters call, the values they pass and receive, the errors they
// map, and the codecs of every name a client holds. An adapter imports this
// package and nothing else of the core, so every type it needs from the
// metadata store or open state is aliased here.
package vfs

import (
	"net/netip"
	"time"

	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
)

// The metadata store's types an adapter passes or receives.
type (
	Handle     = entity.Handle // opaque: stored and returned, never built, parsed or compared
	File       = entity.File
	FileID     = entity.FileID
	Attrs      = entity.Attrs
	ACL        = entity.ACL
	Xattr      = entity.Xattr
	Principal  = entity.Principal
	Access     = entity.AccessMask // an access mask in the store's terms
	Usage      = entity.Usage
	Quota      = entity.Quota
	ShareID    = entity.ShareID
	ShardID    = entity.ShardID
	ClientID   = entity.ClientID
	OpenID     = entity.OpenID
	SessionRef = entity.SessionRef
	Durability = entity.Durability
	Protocol   = entity.Protocol // the client record's kind: NFSv3, NFSv4, NLM or SMB
	TimeMask   = entity.TimeMask
	SMBFlags   = entity.SMBShareFlags
)

// Open state's types an adapter passes or receives.
type (
	StateRef     = openstate.StateRef
	StateType    = openstate.StateType
	OpenRef      = openstate.OpenRef
	RefKind      = openstate.RefKind
	Seqs         = openstate.Seqs
	GrantID      = openstate.GrantID
	GrantKind    = openstate.GrantKind
	LeaseKey     = openstate.LeaseKey
	WatchID      = openstate.WatchID
	ClientStatus = openstate.ClientStatus
	ClientRecord = openstate.Client
	Proposal     = openstate.Proposal
	GraceRef     = openstate.GraceRef
	Revocation   = openstate.Revocation
	RevokeReason = openstate.RevokeReason
	LockOwner    = openstate.LockOwner
	ByteRange    = openstate.ByteRange
	LockRange    = openstate.LockRange
	Lock         = openstate.Lock
	LockSequence = openstate.LockSequence
	LockWaiter   = openstate.LockWaiter
	WaitKind     = openstate.WaitKind
	CachingGrant = openstate.CachingGrant
	ChangeMask   = openstate.ChangeMask
	Change       = openstate.Change
	AppInstance  = openstate.AppInstance
)

// The values of open state's enumerations, so an adapter names them from here.
const (
	StateOpen  = openstate.StateOpen
	StateLock  = openstate.StateLock
	StateGrant = openstate.StateGrant
	StateWatch = openstate.StateWatch

	RefOpened    = openstate.RefOpened
	RefLocked    = openstate.RefLocked
	RefDelegated = openstate.RefDelegated
	RefAnonymous = openstate.RefAnonymous

	GrantNone   = openstate.GrantNone
	GrantRead   = openstate.GrantRead
	GrantWrite  = openstate.GrantWrite
	GrantHandle = openstate.GrantHandle

	WaitNone     = openstate.WaitNone
	WaitPoll     = openstate.WaitPoll
	WaitCallback = openstate.WaitCallback

	RevokedLeaseExpired = openstate.RevokedLeaseExpired
	RevokedByAdmin      = openstate.RevokedByAdmin
	RevokedGrant        = openstate.RevokedGrant
	RevokedShardState   = openstate.RevokedShardState
	RevokedShare        = openstate.RevokedShare

	ChangeRemove    = openstate.ChangeRemove
	ChangeRename    = openstate.ChangeRename
	ChangeAddEntry  = openstate.ChangeAddEntry
	ChangeAttrs     = openstate.ChangeAttrs
	ChangeSetID     = openstate.ChangeSetID
	ChangeSize      = openstate.ChangeSize
	ChangeTimes     = openstate.ChangeTimes
	ChangeSecurity  = openstate.ChangeSecurity
	ChangeStreams   = openstate.ChangeStreams
	ChangeEnumerate = openstate.ChangeEnumerate
)

// Identity is what the adapter proved for one request, not yet mapped through
// any share's policy, in no protocol's terms: each adapter maps its own
// flavour and mechanism to a ProofKind and a Protection. It never carries a
// session key.
//
// ponytail: Identity, its parts, and the sign-in and spelling types below are
// declared here because the identity package that should declare them does
// not exist yet; move them there and alias them here once it does.
type Identity struct {
	Proof      ProofKind
	Protection Protection
	Claims     Claims     // what the client asserted; read only at ProofClaimed, never as proof
	Evidence   Evidence   // what sign-in established; zero below ProofSecret
	Client     netip.Addr // the client's address, which client rules read
}

// Call is one call's caller: the Identity it proved and, beside it, never in
// it, the client, session and channel it came on. Every Service method that
// acts for a caller takes one; a call naming no open leaves Client unused.
// Each call's client is Client, never one the Identity names. Identity is
// what a resolver caches, keyed on its proof; Identity.Client, its address,
// is per machine and carried in the key separately wherever a client rule
// decided. The call's Client is per call, so it is never cached, and two
// clients proving the same user never pass for each other.
//
// An NFSv4.0 call has no session. Its Client is the client on the state its
// stateid resolves to or, for an operation that names an open or lock owner,
// that owner's client. NFSv4.0 is offered no delegation, so checking that
// client against the state's opener is the whole check.
type Call struct {
	Identity Identity
	Protocol Protocol
	Client   ClientID   // the client the adapter proved the call came from; zero: none, as NFSv3 has
	Session  SessionRef // the SMB or NFSv4.1 session; zero: none, as NFSv4.0 and NFSv3 have
	Channel  struct {
		Seq    uint16 // the SMB channel sequence the request carried
		Replay bool   // the request is marked a replay
	}
}

// ProofKind is how the sender was proved, in increasing order.
type ProofKind uint8

const (
	ProofNone    ProofKind = iota // an anonymous or guest caller
	ProofClaimed                  // POSIX ids asserted, nothing verified
	ProofSecret                   // a shared-secret challenge, such as NTLM
	ProofTicket                   // a third party's mutual proof, such as Kerberos
)

// Protection is how the request's messages are protected, in increasing order.
type Protection uint8

const (
	ProtectionNone      Protection = iota
	ProtectionIntegrity            // every message signed
	ProtectionPrivacy              // every message sealed
)

// Claims are POSIX ids a client asserted. They name no principal until the
// filesystem maps them, and never make a caller authenticated.
type Claims struct {
	UID, GID uint32
	GIDs     []uint32
}

// Evidence is what one sign-in established, and no key.
type Evidence struct {
	Name      string    // the proven name; empty: none
	Principal Principal // what sign-in bound Name to; zero: none
	SIDs      []sid.SID // the user's first
	DenyOnly  []sid.SID
	Expires   time.Time // the ticket's end; zero for NTLM
}

// SignInStep is one verification round: a Kerberos AP-REQ for Service, or a
// decoded NTLM AUTHENTICATE answering Challenge.
type SignInStep struct {
	Kerberos  []byte
	Service   SignInService
	NTLM      *NTLMAuthenticate
	Challenge [8]byte
	Client    netip.Addr
	Context   *Evidence // the context's current evidence, on a re-authentication; nil otherwise
}

// SignInService is the keytab service name a ticket is for.
type SignInService uint8

const (
	ServiceNFS SignInService = iota + 1
	ServiceCIFS
	ServiceHost
)

// NTLMAuthenticate is a decoded NTLM AUTHENTICATE message, with the bytes of
// the three messages kept as sent so the MIC can be checked.
type NTLMAuthenticate struct {
	User, Domain, Workstation       string
	NTResponse, EncryptedSessionKey []byte
	MIC                             []byte
	Flags                           uint32
	AVFlags                         uint32
	Negotiate, Challenge, Raw       []byte
}

// SignInResult is the Identity a sign-in proved and, beside it, never in it,
// the keys the adapter's mechanism needs.
type SignInResult struct {
	Identity Identity
	Keys     Keys
}

// Keys is what signing, sealing and the AP-REP need.
type Keys struct{ Session, Subkey, APRep []byte }

// Scheme names how a protocol spells a principal.
type Scheme uint8

const (
	SchemeUID Scheme = iota + 1
	SchemeGID
	SchemeSID
	SchemeKrb
	SchemeDir
	SchemeUserName
	SchemeGroupName
	SchemeOIDC
	SchemeOwner // an NFSv4 owner string
	SchemeQualified
)

// Spelling is one protocol's name for a principal: Num, SID or Text, by Scheme.
type Spelling struct {
	Scheme Scheme
	Num    uint32
	SID    sid.SID
	Text   string
}

// Target is what a mutation acts on: the file for an attribute change, the
// directory for a name change. Open, when set, is the open the request came
// through, whose grant authorises it: always an open of the file the change
// acts on, never of the directory in H.
type Target struct {
	H    Handle
	Open *OpenRef
}

// Cursor is where a listing resumes: the 63-bit digest of the last entry
// returned, zero to start.
type Cursor uint64

// EntryFields says which fields of each listed entry a listing fills.
type EntryFields uint8

const (
	EntryHandle EntryFields = 1 << iota
	EntryFile
)

// DirEntry is one listed entry; Handle and File are filled as the listing's
// EntryFields asks.
type DirEntry struct {
	Name   []byte
	Handle Handle
	Cursor Cursor
	File   *File
}

// Page is one page of a listing.
type Page struct {
	Entries []DirEntry
	Next    Cursor
	EOF     bool
}

// AttrChange is an attribute change. Guard, when set, refuses it unless the
// file's ctime still equals it. Suspend and Resume are set only through an
// open.
type AttrChange struct {
	Attrs
	Guard           *time.Time
	DeleteOnClose   *bool
	Suspend, Resume TimeMask
}

// WCC is one object's attributes either side of a mutation, from one hold of
// its accept lock. Before is nil where the service cannot capture it
// atomically with the change.
type WCC struct {
	Before *File
	After  File
}

// CreateResult is what Create and Link answer.
type CreateResult struct {
	Handle Handle
	File   File
	Dir    WCC
}

// UnlinkResult is what Unlink and Rmdir answer.
type UnlinkResult struct{ Dir WCC }

// RenameResult is what Rename answers; Moved is set when the moved file's own
// attributes changed.
type RenameResult struct {
	From, To WCC
	Moved    *WCC
}

// ShareRef names a share as the client sent it: Path over NFS, Name over SMB.
// The zero value is the pseudo-root.
type ShareRef struct {
	Path string
	Name string
}

// ShareInfo is what a share tells a client about itself.
type ShareInfo struct {
	Name          string
	Flavors       []AuthFlavor
	Comment       string
	Hidden        bool
	MaximalAccess Access   // the caller's at the share's root
	SMB           SMBFlags // encrypt, continuously available
}

// FilesystemInfo is what a share reports about itself.
//
// ponytail: declared here because the store has no Capacity.Info yet; alias
// the store's once it declares one.
type FilesystemInfo struct {
	Share        ShareID
	Capabilities Capabilities
	Usage        Usage // the share's, or its project's
	Quota        Quota // the share-wide limit, or its project's
}

// Capabilities is what a share's files can carry, fixed while it is mounted.
type Capabilities struct {
	CaseSensitive, CasePreserving bool
	MaxNameBytes                  int
	ACLs, Xattrs, Streams, Links  bool
	TimeGranularity               time.Duration
}

// SnapshotInfo is one snapshot visible at a path.
//
// ponytail: it carries nothing while the store has no snapshot view and
// Snapshots answers an empty list; give it the snapshot's fields with that
// view.
type SnapshotInfo struct{}

// ClientInfo is what Propose and Connect register. Proposal is the proposal
// Connect confirms; nil for a client that confirms in one step.
type ClientInfo struct {
	Protocol Protocol
	Name     []byte
	Boot     [8]byte
	Protect  bool   // state protection: only the client's owner may act on its state
	Adapter  []byte // the adapter's own bytes, kept with the record and never read by the service
	Proposal *Proposal
}

// AuthFlavor is a flavour a share admits.
type AuthFlavor uint8

const (
	FlavorNone AuthFlavor = iota + 1
	FlavorSys
	FlavorKrb5
	FlavorKrb5i
	FlavorKrb5p
	FlavorNTLM
)

// Stability is how durable a write must be before it is answered.
type Stability uint8

const (
	Unstable Stability = iota
	DataSync
	FileSync
)

// XattrMode is what SetXattr requires of the name.
type XattrMode uint8

const (
	XattrEither  XattrMode = iota
	XattrCreate            // the name must not exist
	XattrReplace           // the name must exist
)

// SeekWhat is what Seek looks for.
type SeekWhat uint8

const (
	SeekData SeekWhat = iota
	SeekHole
)

// CreateKind is the type of object Create makes.
type CreateKind uint8

const (
	CreateRegular CreateKind = iota + 1
	CreateDir
	CreateSymlink
	CreateFifo
	CreateSocket
	CreateChar
	CreateBlock
)

// CreateArgs is what a create makes. Verifier is an exclusive create's;
// Target a symlink's; Dev a device's.
type CreateArgs struct {
	Kind     CreateKind
	Attrs    Attrs
	Verifier *[8]byte
	Target   []byte
	Dev      uint64
}

// WriteResult is what Write answers.
type WriteResult struct {
	N        int
	Verifier Verifier
	WCC      WCC
}

// CopyArgs is a copy's or a clone's source and destination: for Copy the
// extents in order, for Clone exactly one.
type CopyArgs struct {
	Src, Dst OpenRef
	Extents  []Extent
}

// CopyResult is what Copy answers; N may be short.
type CopyResult struct {
	N   int64
	WCC WCC
}

// LockReq is one lock request: every range in one step, all or none.
type LockReq struct {
	Ranges  []LockRange
	Wait    WaitKind
	Reclaim bool
	Owner   LockOwner
	Replay  *LockSequence
	Seqs    Seqs
}

// WatchSpec is what a watch observes.
type WatchSpec struct {
	Dir       Handle
	Recursive bool
	Filter    ChangeMask
}
