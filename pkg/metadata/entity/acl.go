package entity

// ACL is the one access-control list a file has. Audit and alarm entries
// are kept apart from the ones that grant and deny: they never decide
// access, they select audit events.
type ACL struct {
	Entries []ACE
	Flags   ACLFlags
	Audit   []ACE
}

type ACE struct {
	Type  ACEType
	Flags ACEFlags
	Mask  AccessMask
	Who   Principal // a user, a group, or Owner, OwnerGroup or Everyone
}

type ACEType uint8

const (
	ACEAllow ACEType = iota + 1
	ACEDeny
	ACEAudit
	ACEAlarm
)

var aceTypes = []string{ACEAllow: "allow", ACEDeny: "deny", ACEAudit: "audit", ACEAlarm: "alarm"}

func (t ACEType) String() string                { return enumString(aceTypes, t) }
func (t ACEType) MarshalText() ([]byte, error)  { return enumMarshal(aceTypes, t) }
func (t *ACEType) UnmarshalText(b []byte) error { return enumUnmarshal(aceTypes, t, b) }

// ACEFlags are the NFSv4 ACE flags, which Windows shares.
type ACEFlags uint32

const (
	ACEFileInherit      ACEFlags = 0x01
	ACEDirInherit       ACEFlags = 0x02
	ACENoPropagate      ACEFlags = 0x04
	ACEInheritOnly      ACEFlags = 0x08
	ACESuccessfulAccess ACEFlags = 0x10
	ACEFailedAccess     ACEFlags = 0x20
	ACEInherited        ACEFlags = 0x80
)

// Effective reports whether the entry governs the file it is on, rather than
// only the children that inherit it.
func (e ACE) Effective() bool { return e.Flags&ACEInheritOnly == 0 }

// Inheritable reports whether children inherit the entry.
func (e ACE) Inheritable() bool { return e.Flags&(ACEFileInherit|ACEDirInherit) != 0 }

type ACLFlags uint8

const (
	ACLProtected ACLFlags = 1 << iota
	ACLAutoInherited
)

// AccessMask holds the rights both protocols grant: NFSv4's ACE mask bits,
// which are Windows' file access rights. It is what Authorize is asked for.
type AccessMask uint32

const (
	ReadData        AccessMask = 0x00000001 // on a directory: list it
	WriteData       AccessMask = 0x00000002 // on a directory: add a file
	AppendData      AccessMask = 0x00000004 // on a directory: add a subdirectory
	ReadNamedAttrs  AccessMask = 0x00000008
	WriteNamedAttrs AccessMask = 0x00000010
	Execute         AccessMask = 0x00000020 // on a directory: traverse it
	DeleteChild     AccessMask = 0x00000040
	ReadAttributes  AccessMask = 0x00000080
	WriteAttributes AccessMask = 0x00000100
	Delete          AccessMask = 0x00010000
	ReadACL         AccessMask = 0x00020000
	WriteACL        AccessMask = 0x00040000
	WriteOwner      AccessMask = 0x00080000
	Synchronize     AccessMask = 0x00100000

	ListDirectory   = ReadData
	AddFile         = WriteData
	AddSubdirectory = AppendData
)

// The rights each mode bit implies. Write on a directory is adding and
// removing entries, so it carries delete-a-child as well.
const (
	modeRead  = ReadData | ReadNamedAttrs
	modeWrite = WriteData | AppendData | WriteNamedAttrs | DeleteChild
	modeExec  = Execute
	modeRWX   = modeRead | modeWrite | modeExec

	// Granted to every class whatever the mode: reading a file's attributes
	// and ACL is never refused by the mode.
	alwaysAllowed = ReadAttributes | ReadACL | Synchronize
	// The owner may change attributes and the ACL whatever its own bits.
	ownerAllowed = WriteAttributes | WriteACL
)

// rights is the mask three permission bits imply.
func rights(bits uint32) AccessMask {
	var m AccessMask
	if bits&4 != 0 {
		m |= modeRead
	}
	if bits&2 != 0 {
		m |= modeWrite
	}
	if bits&1 != 0 {
		m |= modeExec
	}
	return m
}

// bits is the three permission bits a mask implies: each bit only when the
// mask holds every right that bit grants.
func bits(m AccessMask) uint32 {
	var b uint32
	if m&modeRead == modeRead {
		b |= 4
	}
	if m&(WriteData|AppendData) == WriteData|AppendData {
		b |= 2
	}
	if m&modeExec == modeExec {
		b |= 1
	}
	return b
}

// ACLFromMode is the ACL a file with no stored ACL has. The owner and the
// owning group each get an allow entry for their bits and a deny entry for
// the rwx rights those bits withhold, ahead of everyone's allow, so the list
// grants exactly what the mode does: an owner in a group granted more than
// the owner still gets only the owner bits.
func ACLFromMode(mode uint32) ACL {
	var es []ACE
	for _, c := range []struct {
		who   Principal
		bits  uint32
		extra AccessMask
	}{
		{Owner, mode >> 6, ownerAllowed},
		{OwnerGroup, mode >> 3, 0},
	} {
		r := rights(c.bits)
		es = append(es, ACE{Type: ACEAllow, Mask: r | alwaysAllowed | c.extra, Who: c.who})
		if rest := modeRWX &^ r; rest != 0 {
			es = append(es, ACE{Type: ACEDeny, Mask: rest, Who: c.who})
		}
	}
	return ACL{Entries: append(es, ACE{Type: ACEAllow, Mask: rights(mode) | alwaysAllowed, Who: Everyone})}
}

// Mode is the rwx bits this ACL implies, from its effective entries for the
// owner, the owning group and everyone. Each right is decided by the first
// entry that names it for the class: everyone's entries apply to the owner
// and group classes too. Setuid, setgid and sticky are not in an ACL: the
// caller keeps them from the File.
func (a ACL) Mode() uint32 {
	var allow, decided [3]AccessMask // owner, group, other
	for _, e := range a.Entries {
		if !e.Effective() {
			continue
		}
		var classes []int
		switch e.Who {
		case Owner:
			classes = []int{0}
		case OwnerGroup:
			classes = []int{1}
		case Everyone:
			classes = []int{0, 1, 2}
		default:
			continue
		}
		for _, c := range classes {
			fresh := e.Mask &^ decided[c]
			if e.Type == ACEAllow {
				allow[c] |= fresh
			}
			decided[c] |= fresh
		}
	}
	return bits(allow[0])<<6 | bits(allow[1])<<3 | bits(allow[2])
}

// WithMode is chmod's merge into a stored ACL, as RFC 8881 §6.4.1.1 has it,
// so that the result's Mode is the new mode:
//   - the effective entries for the owner, the owning group and everyone are
//     replaced by ones matching the new mode, appended last;
//   - every effective allow entry for a named principal is limited to the
//     rights the new group bits imply, so chmod 600 leaves no named user or
//     group with access;
//   - an entry both effective and inheritable is split into an unchanged
//     inherit-only copy and an effective copy, treated as above;
//   - inherit-only, deny, audit and alarm entries are left as they are.
func (a ACL) WithMode(mode uint32) ACL {
	limit := rights(mode >> 3)
	out := ACL{Flags: a.Flags, Audit: append([]ACE(nil), a.Audit...)}
	for _, e := range a.Entries {
		if !e.Effective() {
			out.Entries = append(out.Entries, e)
			continue
		}
		if e.Inheritable() {
			kept := e
			kept.Flags |= ACEInheritOnly
			out.Entries = append(out.Entries, kept)
			e.Flags &^= ACEFileInherit | ACEDirInherit | ACENoPropagate
		}
		switch {
		case e.Who == Owner || e.Who == OwnerGroup || e.Who == Everyone:
			continue // replaced below
		case e.Type == ACEAllow:
			e.Mask &^= modeRWX &^ limit
		}
		out.Entries = append(out.Entries, e)
	}
	out.Entries = append(out.Entries, ACLFromMode(mode).Entries...)
	return out
}

// Inherit is the ACL a new file, or a new directory when dir, created under a
// directory holding a, gets, as RFC 8881 §6.4.3 and MS-DTYP 2.5.3.4 derive
// it; ok is false when nothing is inherited, and the new file's mode then
// governs with no stored ACL.
//   - A new file gets an effective copy of each file-inherit entry.
//   - A new directory gets an effective copy of each directory-inherit
//     entry, which, unless it is no-propagate, it also keeps for its own
//     children; a file-inherit entry passes through it inherit-only.
//   - In an effective copy CREATOR OWNER and CREATOR GROUP become owner and
//     group; the copy that propagates keeps them, inherit-only.
//   - mode then applies as an and, limiting the effective allow entries'
//     read, write and execute rights as chmod's merge would — the owner's
//     class to the owner bits, everyone to the other bits, the rest to the
//     group bits — never adding an entry or a right.
//
// Audit and alarm entries are inherited by the same rules, unlimited.
func (a ACL) Inherit(dir bool, owner, group Principal, mode uint32) (ACL, bool) {
	out := ACL{
		Entries: inherit(a.Entries, dir, owner, group),
		Audit:   inherit(a.Audit, dir, owner, group),
	}
	if len(out.Entries) == 0 && len(out.Audit) == 0 {
		return ACL{}, false
	}
	for i, e := range out.Entries {
		if !e.Effective() || e.Type != ACEAllow {
			continue
		}
		limit := rights(mode >> 3)
		switch e.Who {
		case Owner:
			limit = rights(mode >> 6)
		case Everyone:
			limit = rights(mode)
		}
		out.Entries[i].Mask &^= modeRWX &^ limit
	}
	return out, true
}

func inherit(es []ACE, dir bool, owner, group Principal) []ACE {
	const flow = ACEFileInherit | ACEDirInherit | ACENoPropagate | ACEInheritOnly
	var out []ACE
	for _, e := range es {
		file, sub := e.Flags&ACEFileInherit != 0, e.Flags&ACEDirInherit != 0
		propagates := dir && e.Flags&ACENoPropagate == 0
		effective := (!dir && file) || (dir && sub)
		if effective {
			c := e
			c.Flags = c.Flags&^flow | ACEInherited
			switch c.Who {
			case CreatorOwner:
				c.Who = owner
			case CreatorGroup:
				c.Who = group
			}
			out = append(out, c)
		}
		if propagates && (file || sub) {
			c := e
			c.Flags = c.Flags | ACEInheritOnly | ACEInherited
			out = append(out, c)
		}
	}
	return out
}

// Stored is the ACL as the store keeps it: an inheritable entry naming
// CREATOR OWNER or CREATOR GROUP is inherit-only, since those match no caller
// and act only through inheritance, which turns them into effective entries
// for the new file's owner or group.
func (a ACL) Stored() ACL {
	out := ACL{Flags: a.Flags, Entries: append([]ACE(nil), a.Entries...), Audit: append([]ACE(nil), a.Audit...)}
	for i, e := range out.Entries {
		if e.Who.IsCreator() && e.Inheritable() {
			out.Entries[i].Flags |= ACEInheritOnly
		}
	}
	return out
}

// LimitsOwner reports an effective entry naming owner rights: the owner then
// gets what such entries grant, without the rights ownership implies.
func (a ACL) LimitsOwner() bool {
	for _, e := range a.Entries {
		if e.Effective() && e.Who == OwnerRights {
			return true
		}
	}
	return false
}

// Valid reports whether every entry is of the list it sits in: allow and
// deny in Entries, audit and alarm in Audit, each naming a principal. A deny
// naming a special principal that is not well known is invalid: no caller
// matches it, so every walk would skip it and it would fail open. An allow
// naming one is valid and grants nothing.
func (a ACL) Valid() bool {
	for _, e := range a.Entries {
		if (e.Type != ACEAllow && e.Type != ACEDeny) || e.Who.IsZero() {
			return false
		}
		if e.Type == ACEDeny && e.Who.Kind == PrincipalSpecial && !e.Who.WellKnown() {
			return false
		}
	}
	for _, e := range a.Audit {
		if (e.Type != ACEAudit && e.Type != ACEAlarm) || e.Who.IsZero() {
			return false
		}
	}
	return true
}
