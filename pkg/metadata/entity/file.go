package entity

import (
	"errors"
	"io/fs"
	"time"
)

// File is every object a name can resolve to: regular file, directory,
// symlink, device, FIFO, socket, and a named stream. It carries no name, no
// path and no list of the entries naming it: entries name it.
type File struct {
	ID    FileID
	Share ShareID
	Type  FileType

	// Mode agrees with the ACL when one is stored.
	Owner, Group Principal
	Mode         uint32 // permission bits only, setuid, setgid and sticky included; the type is Type
	Flags        FileFlags

	// Birth is set once at create; Change advances on every change; Modify
	// only on a content change.
	Birth, Access, Modify, Change time.Time

	// Version advances on every change, metadata or content: the NFSv4
	// change attribute and what SMB change detection compares.
	Version uint64

	// Size, Charged and Applied are the write path's: only an existence
	// commit sets them.
	Size    int64
	Charged int64 // logical bytes charged to Owner, Group and Project, holes excluded
	Applied uint64

	// Nlink is the number of entries naming the file. A share's root and a
	// named stream have none.
	Nlink uint32

	Parent FileID // Directory only: the directory holding its one entry; a root's is itself

	Shard   uint64 // zero: the whole share is one shard
	Project uint64 // the tree quota it is charged to, inherited at create; zero: none
	Number  uint64 // the numeric file id within the share: set at create, never reused, never zero

	// CreateVerifier is the verifier of the exclusive create that made the
	// file; nil otherwise, and cleared by the first SetAttrs.
	CreateVerifier []byte

	Target   []byte   // Symlink only: the target as given, never resolved here
	Device   DeviceID // BlockDevice and CharDevice only
	StreamOf FileID   // Stream only: the file it belongs to
}

type DeviceID struct{ Major, Minor uint32 }

type FileType uint8

const (
	Regular FileType = iota + 1
	Directory
	Symlink
	BlockDevice
	CharDevice
	FIFO
	Socket
	Stream // a named stream; never has an entry
)

var fileTypes = []string{
	Regular: "regular", Directory: "directory", Symlink: "symlink", BlockDevice: "block-device",
	CharDevice: "char-device", FIFO: "fifo", Socket: "socket", Stream: "stream",
}

func (t FileType) String() string                { return enumString(fileTypes, t) }
func (t FileType) MarshalText() ([]byte, error)  { return enumMarshal(fileTypes, t) }
func (t *FileType) UnmarshalText(b []byte) error { return enumUnmarshal(fileTypes, t, b) }

// mode is the type's io/fs mode bits. A stream reads as a regular file.
func (t FileType) mode() fs.FileMode {
	switch t {
	case Directory:
		return fs.ModeDir
	case Symlink:
		return fs.ModeSymlink
	case BlockDevice:
		return fs.ModeDevice
	case CharDevice:
		return fs.ModeDevice | fs.ModeCharDevice
	case FIFO:
		return fs.ModeNamedPipe
	case Socket:
		return fs.ModeSocket
	}
	return 0
}

// FileFlags are the generic flags both protocols set: SMB's DOS attributes
// and the immutable and append-only flags.
type FileFlags uint32

const (
	FlagHidden FileFlags = 1 << iota
	FlagSystem
	FlagArchive
	FlagReadOnly
	FlagImmutable
	FlagAppendOnly
)

// Permission bits beyond rwx.
const (
	ModeSetuid uint32 = 0o4000
	ModeSetgid uint32 = 0o2000
	ModeSticky uint32 = 0o1000
	ModePerm   uint32 = 0o7777
)

func (f File) IsDir() bool              { return f.Type == Directory }
func (f File) IsRegular() bool          { return f.Type == Regular }
func (f File) IsSymlink() bool          { return f.Type == Symlink }
func (f File) IsStream() bool           { return f.Type == Stream }
func (f File) IsSpecial() bool          { return f.Type >= BlockDevice && f.Type <= Socket }
func (f File) IsRoot() bool             { return f.Type == Directory && f.Parent == f.ID }
func (f File) HasContent() bool         { return f.Type == Regular || f.Type == Stream }
func (f File) HasFlag(x FileFlags) bool { return f.Flags&x == x }
func (f File) IsHidden() bool           { return f.HasFlag(FlagHidden) }
func (f File) IsReadOnly() bool         { return f.HasFlag(FlagReadOnly) }
func (f File) IsImmutable() bool        { return f.HasFlag(FlagImmutable) }
func (f File) IsAppendOnly() bool       { return f.HasFlag(FlagAppendOnly) }

// FSMode is the type bits plus Mode, for io/fs and logs.
func (f File) FSMode() fs.FileMode {
	m := f.Type.mode() | fs.FileMode(f.Mode&0o777)
	if f.Mode&ModeSetuid != 0 {
		m |= fs.ModeSetuid
	}
	if f.Mode&ModeSetgid != 0 {
		m |= fs.ModeSetgid
	}
	if f.Mode&ModeSticky != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// ErrInvalidFile is a File whose fields disagree with its type.
var ErrInvalidFile = errors.New("entity: invalid file")

// Validate checks that the File has an identity and a known type, that Mode
// holds permission bits only, and that each type-specific field is set for
// its type and empty otherwise.
func (f File) Validate() error {
	switch {
	case f.ID.IsZero(), f.Share.IsZero(), f.Type.String() == "", f.Number == 0:
		return ErrInvalidFile
	case f.Mode&^ModePerm != 0:
		return ErrInvalidFile
	case (f.Type == Symlink) != (len(f.Target) > 0):
		return ErrInvalidFile
	case f.Device != (DeviceID{}) && f.Type != BlockDevice && f.Type != CharDevice:
		return ErrInvalidFile
	case (f.Type == Stream) != !f.StreamOf.IsZero():
		return ErrInvalidFile
	case (f.Type == Directory) != !f.Parent.IsZero():
		return ErrInvalidFile
	}
	return nil
}

// AttrMask names the fields an Attrs changes.
type AttrMask uint16

const (
	AttrOwner AttrMask = 1 << iota
	AttrGroup
	AttrMode
	AttrFlags
	AttrBirth
	AttrAccess
	AttrModify
	AttrSize
)

// Attrs is a SetAttrs: the fields it names in Mask change, the rest are
// untouched. The builders set the mask, so a caller cannot change a field
// and forget to say so.
type Attrs struct {
	Mask                  AttrMask
	Owner, Group          Principal
	Mode                  uint32
	Flags                 FileFlags
	Birth, Access, Modify time.Time
	Size                  int64
}

func (a Attrs) WithOwner(p Principal) Attrs  { a.Owner, a.Mask = p, a.Mask|AttrOwner; return a }
func (a Attrs) WithGroup(p Principal) Attrs  { a.Group, a.Mask = p, a.Mask|AttrGroup; return a }
func (a Attrs) WithMode(m uint32) Attrs      { a.Mode, a.Mask = m, a.Mask|AttrMode; return a }
func (a Attrs) WithFlags(f FileFlags) Attrs  { a.Flags, a.Mask = f, a.Mask|AttrFlags; return a }
func (a Attrs) WithBirth(t time.Time) Attrs  { a.Birth, a.Mask = t, a.Mask|AttrBirth; return a }
func (a Attrs) WithAccess(t time.Time) Attrs { a.Access, a.Mask = t, a.Mask|AttrAccess; return a }
func (a Attrs) WithModify(t time.Time) Attrs { a.Modify, a.Mask = t, a.Mask|AttrModify; return a }
func (a Attrs) WithSize(n int64) Attrs       { a.Size, a.Mask = n, a.Mask|AttrSize; return a }

// Changes reports whether a changes every field in m.
func (a Attrs) Changes(m AttrMask) bool { return a.Mask&m == m }

// Apply is the File a SetAttrs of a would produce, before the store stamps
// Change and Version. A change of a regular file's owner or group clears
// setuid, and setgid where group execute is set, whoever makes it: a program
// given away keeps no privilege. Every SetAttrs clears the exclusive-create
// verifier.
func (f File) Apply(a Attrs) File {
	if a.Changes(AttrMode) {
		f.Mode = a.Mode & ModePerm
	}
	if (a.Changes(AttrOwner) && a.Owner != f.Owner) || (a.Changes(AttrGroup) && a.Group != f.Group) {
		if f.Type == Regular {
			f.Mode = clearSetID(f.Mode)
		}
	}
	if a.Changes(AttrOwner) {
		f.Owner = a.Owner
	}
	if a.Changes(AttrGroup) {
		f.Group = a.Group
	}
	if a.Changes(AttrFlags) {
		f.Flags = a.Flags
	}
	if a.Changes(AttrBirth) {
		f.Birth = a.Birth
	}
	if a.Changes(AttrAccess) {
		f.Access = a.Access
	}
	if a.Changes(AttrModify) {
		f.Modify = a.Modify
	}
	if a.Changes(AttrSize) {
		f.Size = a.Size
	}
	f.CreateVerifier = nil
	return f
}

// clearSetID clears setuid, and setgid when group execute is set: without
// group execute the setgid bit marks mandatory locking, and stays.
func clearSetID(mode uint32) uint32 {
	mode &^= ModeSetuid
	if mode&0o010 != 0 {
		mode &^= ModeSetgid
	}
	return mode
}

// ClearSetID is the mode an unprivileged caller's write leaves on a regular
// file; other types keep their bits.
func (f File) ClearSetID() uint32 {
	if f.Type != Regular {
		return f.Mode
	}
	return clearSetID(f.Mode)
}

// Entry is one name in one directory. A file with two hard links has two
// entries and one File.
type Entry struct {
	Parent FileID
	Name   []byte // the bytes as given; the key may be folded
	Child  FileID
	Type   FileType // the child's type, written with the entry
}

// IsDir reports whether the entry names a directory, from the type copied
// into it. Entry does not implement io/fs.DirEntry: its Name and Type fields
// would collide with that interface's methods.
func (e Entry) IsDir() bool { return e.Type == Directory }

// Xattr is one small named value on a file, its own record: a share setting
// bounds its size and how many a file may have, and larger data is a named
// stream.
type Xattr struct {
	File  FileID
	Name  []byte
	Value []byte
}
