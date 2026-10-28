package vfs

// Disposition is what an open does when the name does or does not exist.
type Disposition uint8

const (
	OpenExisting      Disposition = iota + 1 // open it; refused if it does not exist
	CreateNew                                // create it; refused if it exists
	OpenOrCreate                             // open it, or create it
	Overwrite                                // open and truncate it; refused if it does not exist
	OverwriteOrCreate                        // open and truncate it, or create it
	Supersede                                // replace it, or create it
)

// OpenRequest is what an open asks for. It names the file by Dir and Name, the
// directory the path resolved through and the entry in it, or, with no Name,
// by H.
type OpenRequest struct {
	Dir         Handle
	Name        []byte
	H           Handle
	Disposition Disposition
	Create      CreateArgs // the object a create makes

	Access, Deny  Access
	DeleteOnClose bool
	Backup        bool // backup or restore intent: privileged when the caller is, else an ordinary open

	Want   GrantKind // the caching grant asked for; GrantNone: none
	Lease  LeaseKey  // the lease to cover the open; zero: none
	Parent LeaseKey  // a v2 lease request's parent key
	Epoch  *uint16   // a v2 lease request's epoch; nil: a v1 lease or an oplock

	Durability Durability
	CreateID   [16]byte    // the create's own ID, which a reconnect or a replay names; zero: none
	Instance   AppInstance // zero: none
	Replay     bool        // a create marked as a replay
	Reconnect  *Reconnect  // a durable reconnect's match; nil: a new open
	Reclaim    bool        // an open the client held before its shard lost its state
	Snapshot   []byte      // the snapshot opened; nil: the live file

	Owner LockOwner // the owner of opens; zero where the protocol has none
	Seqs  Seqs
}

// OpenResult is what an open answers. Dir is the parent's WCC when it
// created.
type OpenResult struct {
	Ref           OpenRef
	File          File
	MaximalAccess Access
	Grant         *CachingGrant // nil: none
	Confirm       bool          // the owner must ConfirmOpen before any other request
	Created       bool
	Dir           *WCC
}

// Anonymous is the anonymous open of h: I/O with no open.
func Anonymous(h Handle) OpenRef { return OpenRef{Kind: RefAnonymous, Handle: h} }

// Delegated is I/O under delegation g on h.
func Delegated(g GrantID, h Handle) OpenRef {
	return OpenRef{
		Kind:   RefDelegated,
		State:  StateRef{Type: StateGrant, Shard: g.Shard, Instance: g.Instance, Token: g.Token},
		Handle: h,
	}
}
