package entity

import (
	"encoding/binary"
	"errors"
	"time"
)

// ErrSeqMisordered is an NFSv4.1 CREATE_SESSION whose sequence ID is neither
// the last one applied, a replay, nor the next.
var ErrSeqMisordered = errors.New("entity: create-session sequence out of order")

// The durable part of open state: the records a lost process leaves behind
// for the next one. Opens, locks and caching grants are otherwise volatile,
// held at the file's primary; only the cases below reach the store.

// Protocol is the kind of client that holds open state. Adapters and the
// metadata layer both name it from here, so neither imports the other.
type Protocol uint8

const (
	ProtocolNFSv4 Protocol = iota + 1
	ProtocolSMB
	ProtocolNLM
)

var protocols = []string{ProtocolNFSv4: "nfsv4", ProtocolSMB: "smb", ProtocolNLM: "nlm"}

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

// Client is the durable record of one protocol client instance: what a
// reclaim and its identity need after a restart. Its lease expiry is
// volatile and never stored, so a renewal writes nothing.
type Client struct {
	ID       ClientID
	Protocol Protocol
	// Owner is the client's own name for itself: NFSv4's co_ownerid or
	// SETCLIENTID id, SMB's ClientGuid, NLM's host name. Owner and Protocol
	// are unique, and indexed.
	Owner []byte
	// Verifier is what tells two boots of one owner apart: NFSv4's boot
	// verifier, or an NLM host's NSM state number (NSMVerifier). A new one
	// under the same owner is a reboot.
	Verifier [8]byte
	// Principal established the client ID over NFSv4. Zero for SMB, whose
	// record names a machine and holds many users' sessions, and for NLM.
	Principal Principal
	MachCred  *MachCred // NFSv4.1 state protection; nil: none
	CSSeq     uint32    // NFSv4.1: the last CREATE_SESSION applied
	CSReply   []byte    // that CREATE_SESSION's reply, opaque here
	Revoked   bool      // an administrator revoked its state
}

// NSMVerifier is an NLM host's NSM state number as its client's Verifier:
// big-endian in the last four bytes, so a host that restarts with a new
// number is a reboot of the same owner.
func NSMVerifier(state uint32) (v [8]byte) {
	binary.BigEndian.PutUint32(v[4:], state)
	return v
}

// MachCred is NFSv4.1's SP4_MACH_CRED: the machine principal and the
// operations it protects, a bitmap opaque here.
type MachCred struct {
	Principal Principal
	Ops       []byte
}

// ClientShard is a client's record in one shard it has held state in. A
// reclaim in a shard the client has no record in is refused.
type ClientShard struct {
	Shard uint64
	// Reclaimed is the shard's grace instance at the client's last
	// RECLAIM_COMPLETE there.
	Reclaimed   uint32
	LossPending bool   // NFSv4.0: a loss of the shard's state is still to be signalled
	Revoked     bool   // an administrator's revoke released its state here: its reclaims are refused
	Notify      []byte // NLM: where its restart notification goes; opaque here
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

// Open is a durable open record. Only three opens are stored: one that
// keeps a file with no entry alive, an SMB persistent open, and a clone's
// hold on its source. Every other open lives only at the file's primary.
type Open struct {
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

// Lock is a durable byte-range lock: only one under a persistent open is
// stored, beneath it, so closing the open drops it.
type Lock struct {
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

// DeletePending is a file a persistent open marked for deletion at its last
// close: the entry the unlink removes, and as whom and with what it runs.
type DeletePending struct {
	Parent     FileID
	Name       []byte
	By         Principal
	Access     AccessMask // the marking open's grant
	Privileged bool
}
