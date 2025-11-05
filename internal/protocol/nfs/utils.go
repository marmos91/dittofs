package nfs

import "github.com/marmos91/dittofs/internal/metadata"

// Helper to convert metadata.FileAttr to NFS FileAttr
func MetadataToNFSAttr(mdAttr *metadata.FileAttr, fileid uint64) *FileAttr {
	return &FileAttr{
		Type:   uint32(mdAttr.Type),
		Mode:   mdAttr.Mode,
		Nlink:  1,
		UID:    mdAttr.UID,
		GID:    mdAttr.GID,
		Size:   mdAttr.Size,
		Used:   mdAttr.Size,
		Rdev:   [2]uint32{0, 0},
		Fsid:   0,
		Fileid: fileid,
		Atime: TimeVal{
			Seconds:  uint32(mdAttr.Atime.Unix()),
			Nseconds: uint32(mdAttr.Atime.Nanosecond()),
		},
		Mtime: TimeVal{
			Seconds:  uint32(mdAttr.Mtime.Unix()),
			Nseconds: uint32(mdAttr.Mtime.Nanosecond()),
		},
		Ctime: TimeVal{
			Seconds:  uint32(mdAttr.Ctime.Unix()),
			Nseconds: uint32(mdAttr.Ctime.Nanosecond()),
		},
	}
}
