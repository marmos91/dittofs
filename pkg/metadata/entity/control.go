package entity

import "net/netip"

// User and Group are principals with a name. A file refers to them only by
// Principal, never by name, so renaming one touches no file, and deleting one
// leaves its principal in ACLs as an unknown principal.
type User struct {
	Principal Principal
	Name      string
	IDs       ProtocolIDs // each also a protocol-ID index row
	Primary   Principal   // the group a new file gets; zero: the creator's own principal
	Disabled  bool
	Secret    SecretRef // by reference, never the credential itself
}

type Group struct {
	Principal Principal
	Name      string
	IDs       ProtocolIDs
}

// Membership is one member of one group, its own record in both directions,
// so a large group is never a list on the group.
type Membership struct {
	Group  Principal
	Member Principal // a user or a nested group
}

// ProtocolID is one protocol's spelling of a principal. Value is the
// spelling the scheme's index row holds: decimal for UID and GID, the
// binary form for a SID, and "name@REALM" as the KDC spells it.
type ProtocolID struct {
	Scheme ProtocolScheme
	Value  string
}

type ProtocolIDs []ProtocolID

// ProtocolScheme's codes are the protocol-ID index's scheme component.
type ProtocolScheme uint8

const (
	SchemeUID ProtocolScheme = iota + 1
	SchemeGID
	SchemeSID
	SchemeKerberos
)

var protocolSchemes = []string{SchemeUID: "uid", SchemeGID: "gid", SchemeSID: "sid", SchemeKerberos: "krb"}

func (s ProtocolScheme) String() string                { return enumString(protocolSchemes, s) }
func (s ProtocolScheme) MarshalText() ([]byte, error)  { return enumMarshal(protocolSchemes, s) }
func (s *ProtocolScheme) UnmarshalText(b []byte) error { return enumUnmarshal(protocolSchemes, s, b) }

// Share is one exported filesystem. Name is what an SMB client tree-connects
// to; Path is its place in the NFS namespace. Both are indexed and unique,
// both may change, and neither appears in a handle, which carries ID.
type Share struct {
	ID   ShareID
	Name string
	Path string // absolute and normalised: "/photos", "/home/alice"
	Root FileID
	// Fold, Names and DigestKey are bound when the share is created and never
	// change: changing either rule would re-fold or refuse names already
	// stored, and a new key would reorder every listing and void its cookies.
	Fold      FoldRule
	Names     NameRule
	DigestKey [DigestKeyLen]byte
	// Tag is the share's journal tag, minted from the installation's tag
	// counter when the share arrives and never given to another share; a
	// share put never changes it. Zero names no share.
	Tag uint32
	// State is its own record, not part of the one a share put writes: a cut
	// and a deletion change it as well as the management API.
	State ShareState
	// Namespaces lists the block namespaces the share's content is counted
	// in, each with its generation: one outside a re-home, two during one.
	// Write is the generation new puts carve under. Both are their own
	// record, written at create and then only by a re-home or an import,
	// never by a share put.
	Namespaces []NamespaceGen
	Write      uint32
}

type ShareState uint8

const (
	ShareEnabled ShareState = iota + 1
	ShareQuiesced
	ShareRemoving
	ShareRetired
)

var shareStates = []string{
	ShareEnabled: "enabled", ShareQuiesced: "quiesced", ShareRemoving: "removing", ShareRetired: "retired",
}

func (s ShareState) String() string                { return enumString(shareStates, s) }
func (s ShareState) MarshalText() ([]byte, error)  { return enumMarshal(shareStates, s) }
func (s *ShareState) UnmarshalText(b []byte) error { return enumUnmarshal(shareStates, s, b) }

// SnapshotDisposition is what a share's removal does with its snapshots:
// refuse to start while any exist, the default; delete them; or detach them,
// keeping what they see after the share is gone.
type SnapshotDisposition uint8

const (
	SnapshotsRefuse SnapshotDisposition = iota
	SnapshotsDelete
	SnapshotsDetach
)

var snapshotDispositions = []string{SnapshotsRefuse: "refuse", SnapshotsDelete: "delete", SnapshotsDetach: "detach"}

func (d SnapshotDisposition) String() string { return enumString(snapshotDispositions, d) }
func (d SnapshotDisposition) MarshalText() ([]byte, error) {
	return enumMarshal(snapshotDispositions, d)
}
func (d *SnapshotDisposition) UnmarshalText(b []byte) error {
	return enumUnmarshal(snapshotDispositions, d, b)
}

// ShareGrant is one principal's access to one share, evaluated on every call
// before any file ACL.
type ShareGrant struct {
	Share     ShareID
	Principal Principal
	Access    ShareAccess
	Version   uint64 // raised by every change; the authorisation cache key carries it
}

// ShareAccess is ordered: each level includes the ones below it.
type ShareAccess uint8

const (
	AccessNone ShareAccess = iota
	AccessRead
	AccessReadWrite
	AccessAdmin
)

var shareAccesses = []string{AccessNone: "none", AccessRead: "read", AccessReadWrite: "read-write", AccessAdmin: "admin"}

func (a ShareAccess) String() string                { return enumString(shareAccesses, a) }
func (a ShareAccess) MarshalText() ([]byte, error)  { return enumMarshal(shareAccesses, a) }
func (a *ShareAccess) UnmarshalText(b []byte) error { return enumUnmarshal(shareAccesses, a, b) }

// ExportPolicy is the share's admission and identity-mapping rule, one per
// share, applied by the filesystem service on every call.
type ExportPolicy struct {
	Share       ShareID
	Flavors     []AuthFlavor  // admitted, in preference order: SECINFO's answer
	MinKerberos KerberosLevel // zero when Kerberos is not required
	Clients     []ClientRule  // first match decides; no match refuses. Empty admits every client
	Squash      Squash
	Anonymous   Principal // what a squashed caller becomes
	SMB         SMBShareFlags
	Version     uint64 // raised by every change; the authorisation cache key carries it
}

type AuthFlavor uint8

const (
	FlavorNone AuthFlavor = iota + 1
	FlavorSys
	FlavorKerberos
)

var authFlavors = []string{FlavorNone: "none", FlavorSys: "sys", FlavorKerberos: "kerberos"}

func (f AuthFlavor) String() string                { return enumString(authFlavors, f) }
func (f AuthFlavor) MarshalText() ([]byte, error)  { return enumMarshal(authFlavors, f) }
func (f *AuthFlavor) UnmarshalText(b []byte) error { return enumUnmarshal(authFlavors, f, b) }

// KerberosLevel is ordered: each level includes the protection below it.
type KerberosLevel uint8

const (
	KerberosNone KerberosLevel = iota
	Krb5
	Krb5i
	Krb5p
)

var kerberosLevels = []string{KerberosNone: "none", Krb5: "krb5", Krb5i: "krb5i", Krb5p: "krb5p"}

func (l KerberosLevel) String() string                { return enumString(kerberosLevels, l) }
func (l KerberosLevel) MarshalText() ([]byte, error)  { return enumMarshal(kerberosLevels, l) }
func (l *KerberosLevel) UnmarshalText(b []byte) error { return enumUnmarshal(kerberosLevels, l, b) }

type Squash uint8

const (
	SquashNone Squash = iota
	SquashRoot
	SquashAll
)

var squashes = []string{SquashNone: "none", SquashRoot: "root", SquashAll: "all"}

func (s Squash) String() string                { return enumString(squashes, s) }
func (s Squash) MarshalText() ([]byte, error)  { return enumMarshal(squashes, s) }
func (s *Squash) UnmarshalText(b []byte) error { return enumUnmarshal(squashes, s, b) }

type SMBShareFlags uint8

const (
	SMBEncrypt SMBShareFlags = 1 << iota
	SMBHidden                // left out of share enumeration
	SMBContinuouslyAvailable
)

// ClientRule admits or refuses clients by address range or netgroup, and may
// narrow the share grant for them.
type ClientRule struct {
	Match  ClientMatch
	Access ShareAccess // none, read or read-write: never wider than the grant
}

// ClientMatch is a CIDR or a netgroup name, exactly one of them.
type ClientMatch struct {
	CIDR     netip.Prefix
	Netgroup string
}

// Valid reports whether the rule names exactly one match and an access a
// client rule may give.
func (r ClientRule) Valid() bool {
	return r.Match.CIDR.IsValid() != (r.Match.Netgroup != "") && r.Access <= AccessReadWrite
}

// Netgroup is a named set of hosts referenced by name from client rules. A
// rule naming a netgroup that does not exist fails closed: an allow rule
// matches no host, and a rule of access none matches every host, so deleting
// a netgroup an early deny names cannot let its hosts reach a later, broader
// allow.
type Netgroup struct {
	Name    string
	Members []string // host names, addresses or CIDRs
	Version uint64
}

// Installation is the installation's identity, one record, minted by the
// explicit initialisation step and never changed.
type Installation struct {
	ID [16]byte
	// Instance is minted afresh whenever a start finds the platform's
	// machine-generation identifier differs from Generation, so a cloned
	// image is a new instance.
	Instance   [16]byte
	Generation []byte // empty where the platform offers none
}

// ShareList is raised in the transaction of every share create, delete,
// rename, path change and state change: the pseudo-filesystem's change
// attribute.
type ShareList struct {
	Version uint64
}

// Setting is one configuration record at one scope.
type Setting struct {
	Scope      SettingScope
	Key        string
	Value      []byte // secrets appear only as references
	Generation uint64 // raised by every change
	Schema     uint32 // a reader that does not know it refuses the record
}

// SettingScope is one of the five scopes and, below installation, the ID of
// the node, namespace, remote store or share it is the scope of.
type SettingScope struct {
	Kind ScopeKind
	ID   [16]byte // zero at installation scope
}

type ScopeKind uint8

const (
	ScopeInstallation ScopeKind = iota + 1
	ScopeNode
	ScopeNamespace
	ScopeRemote
	ScopeShare
)

var scopeKinds = []string{
	ScopeInstallation: "installation", ScopeNode: "node", ScopeNamespace: "namespace",
	ScopeRemote: "remote", ScopeShare: "share",
}

func (k ScopeKind) String() string                { return enumString(scopeKinds, k) }
func (k ScopeKind) MarshalText() ([]byte, error)  { return enumMarshal(scopeKinds, k) }
func (k *ScopeKind) UnmarshalText(b []byte) error { return enumUnmarshal(scopeKinds, k, b) }

// SecretRef names a sealed credential; the zero ref names none.
type SecretRef [16]byte
