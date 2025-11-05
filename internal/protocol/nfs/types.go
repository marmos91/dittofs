package nfs

// WccAttr contains weak cache consistency attributes
type WccAttr struct {
	Size  uint64
	Mtime TimeVal
	Ctime TimeVal
}

// FileAttr represents NFS file attributes
type FileAttr struct {
	Type   uint32
	Mode   uint32
	Nlink  uint32
	UID    uint32
	GID    uint32
	Size   uint64
	Used   uint64
	Rdev   [2]uint32
	Fsid   uint64
	Fileid uint64
	Atime  TimeVal
	Mtime  TimeVal
	Ctime  TimeVal
}

type TimeVal struct {
	Seconds  uint32
	Nseconds uint32
}
