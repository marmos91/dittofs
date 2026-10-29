package entity

import (
	"encoding/binary"
	"time"
)

// The durable part of open state: the records a lost process leaves behind
// for the next one. Opens, locks and caching grants are otherwise volatile,
// held at the file's primary; only the cases below reach the store.

// Protocol is the kind of client that holds open state. Adapters and the
// metadata layer both name it from here, so neither imports the other.
type Protocol uint8

// Its byte is part of the store format: it keys a client's name index and is
// written in its record, so a value once given is never changed or reused.
const (
	ProtocolNFSv3 Protocol = iota + 1 // holds no client record; reserves 0x01
	ProtocolNFSv4
	ProtocolNLM
	ProtocolSMB
)

var protocols = []string{ProtocolNFSv3: "nfsv3", ProtocolNFSv4: "nfsv4", ProtocolNLM: "nlm", ProtocolSMB: "smb"}

func (p Protocol) String() string                { return enumString(protocols, p) }
func (p Protocol) MarshalText() ([]byte, error)  { return enumMarshal(protocols, p) }
func (p *Protocol) UnmarshalText(b []byte) error { return enumUnmarshal(protocols, p, b) }

// ClientID, OpenID and LockID are issued unique across the installation and
// never reused. None is random: an ID a client presents alone is checked
// against its holder and principal by the caller.
type (
	ClientID uint64
	OpenID   uint64
	LockID   uint64
)

// ShardID names one shard. Only its low 24 bits are usable, so a state
// reference carries it in three bytes: every shard is below ShardLimit.
type ShardID uint32

const ShardLimit ShardID = 1 << 24

// Shard is a shard's durable record, of which only two fields are built: the
// shard incarnation, raised each time a node or journal begins serving the
// shard as primary, which the write verifier and open state read, and the
// grace instance, the incarnation the shard was first served under after it
// last lost its open state. A stateid carries the grace instance's low 5 bits.
//
// ponytail: the record's cluster fields (primary, replicas, placement) are
// not built, so codec v1 holds these two alone. Add them, as a v2 decoding
// v1, when the first cluster code reads one.
type Shard struct {
	ID            ShardID
	Incarnation   uint64
	GraceInstance uint64
}

// ClientRecord is a client's durable record: one per client instance, found
// by ID and by its protocol and name. Each field is one open state decides
// on; protocol detail it never interprets is the adapter's, in Adapter. The
// lease's expiry and a pending confirmation are volatile and never stored, so
// a renewal writes nothing.
type ClientRecord struct {
	ID       ClientID
	Protocol Protocol
	// Name is the client's own stable name for itself: NFSv4's co_ownerid or
	// SETCLIENTID id, SMB's ClientGuid, NLM's host name with its source
	// address. Name and Protocol are unique, and indexed.
	Name []byte
	// Boot is the instance under Name: a new Boot is a reboot, under a new ID.
	// An NLM host's is its NSM state number (NSMBoot).
	Boot [8]byte
	// Owner is who may act for the client; zero when it names a machine or a
	// host, or its principal maps to the anonymous one.
	Owner Principal
	// Protect is state protection: Owner may act on any of the client's state,
	// and alone may destroy it or rebind its back channel. It needs an Owner.
	Protect  bool
	Recovery RecoveryKind
	Lease    ClientLease
	Reach    Reach
	// Shards lists every shard the client holds state in, in shard order.
	Shards []ShardMark
	// Ended marks an expired client kept until every shard it names has
	// released its state.
	Ended bool
	// Adapter is the adapter's own durable record, at most 4 KiB, its first
	// byte a version the adapter defines; never read by the store.
	Adapter []byte
}

// ClientLease names the one node that renews and expires the client; Lease
// is an SMB lease, a different thing.
type ClientLease struct {
	Node NodeID
}

// Reach is how a recall reaches the client.
type Reach struct {
	Callback NodeID   // the node holding its back channel; zero: none
	Down     bool     // the back channel is lost, until the client rebinds
	Sessions []NodeID // the nodes holding a session of it, each once
}

// ShardMark is one shard the client holds state in, and Done: the grace
// instance it last finished reclaiming in there, or the shard's instance when
// it joined.
type ShardMark struct {
	Shard ShardID
	Done  uint64
}

// RecoveryKind is how a loss of the client's state reaches it; the adapter
// picks one at first contact.
type RecoveryKind uint8

const (
	RecoverStatus RecoveryKind = iota + 1
	RecoverRefusal
	RecoverReconnect
	RecoverNotice
)

var recoveryKinds = []string{RecoverStatus: "status", RecoverRefusal: "refusal", RecoverReconnect: "reconnect", RecoverNotice: "notice"}

func (k RecoveryKind) String() string                { return enumString(recoveryKinds, k) }
func (k RecoveryKind) MarshalText() ([]byte, error)  { return enumMarshal(recoveryKinds, k) }
func (k *RecoveryKind) UnmarshalText(b []byte) error { return enumUnmarshal(recoveryKinds, k, b) }

// NSMBoot is an NLM host's NSM state number as its client's Boot: big-endian
// in the last four bytes, so a host that restarts with a new number is a
// reboot of the same name, and the bytes order as the numbers do.
func NSMBoot(state uint32) (b [8]byte) {
	binary.BigEndian.PutUint32(b[4:], state)
	return b
}

// OpenKind tells a client's open from the one a clone holds on its source.
type OpenKind uint8

const (
	OpenClient OpenKind = iota
	OpenCloneSource
)

// Durability is what an open survives: nothing, a disconnect, or a failover.
type Durability uint8

const (
	DurabilityNone Durability = iota
	DurabilityDurable
	DurabilityPersistent
)

// TimeMask names the times an open suspends: a write through it leaves them
// alone.
type TimeMask uint8

const (
	TimeModify TimeMask = 1 << iota
	TimeAccess
)

// OpenRecord is a durable open record. Only three opens are stored: one that
// keeps a file with no entry alive, an SMB persistent open, and a clone's
// hold on its source. Every other open lives only at the file's primary.
type OpenRecord struct {
	ID            OpenID
	Client        ClientID // zero for a clone's source open, which no client holds
	Principal     Principal
	File          FileID
	Access        AccessMask
	Privileged    bool // opened with backup or restore intent by a caller privileged for it
	Deny          AccessMask
	Durability    Durability
	DeleteOnClose bool
	DeleteBy      Principal // the unlink delete-on-close leads to runs as it
	Lease         *Lease    // the SMB lease covering a persistent open; nil: none
	Suspended     TimeMask
	Via           FileID // the directory it was opened through; zero: none named
	StateSeq      uint32 // NFSv4: its stateid's seqid
	Nonce         uint64 // NFSv4: its stateid's random bits
	LossSeen      uint64 // SMB: the file's loss sequence it last reported
	Session       SessionRef

	Kind         OpenKind
	CloneDst     FileID         // a clone source's destination, whose removal holds the clone's spec
	CloneVersion JournalVersion // the clone's version

	// SMB durable and persistent opens; zero for NFS.
	CreateGUID  [16]byte
	AppInstance [16]byte
	AppVersion  [2]uint64
	Timeout     time.Duration // how long it is kept once its client disconnects
	LockSeq     [64]uint8     // per index: a valid bit and a 4-bit sequence
	ChannelSeq  uint16
}

// Lease is an SMB lease as a persistent open carries it.
type Lease struct {
	Key, ParentKey [16]byte
	State          uint8 // the lease's read, handle and write bits, as SMB numbers them
	Epoch          uint16
}

// SessionRef names the session that opened or last reconnected an open: a
// session ID is unique only within one node's process, which a restart
// gives a new node epoch.
type SessionRef struct {
	Node      uint64
	NodeEpoch uint64
	ID        uint64
}

// LockRecord is a durable byte-range lock: only one under a persistent open is
// stored, beneath it, so closing the open drops it.
type LockRecord struct {
	ID        LockID
	Open      OpenID
	Owner     LockOwner
	Offset    int64
	Length    int64 // to the end of file when EndOfFile
	Exclusive bool
}

// LockOwner is who holds a lock: the client and its own opaque name for the
// owner (an NFSv4 lock-owner, an NLM svid, an SMB open).
type LockOwner struct {
	Client ClientID
	Owner  []byte
}

// DeleteRecord is a file a persistent open marked for deletion at its last
// close: the entry the unlink removes, and as whom and with what it runs.
type DeleteRecord struct {
	Parent     FileID
	Name       []byte
	By         Principal
	Access     AccessMask // the marking open's grant
	Privileged bool
}
