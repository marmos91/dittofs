package server

import (
	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/mount"
	"github.com/marmos91/dittofs/internal/protocol/nfs"
)

type NFSHandler interface {
	// Null does nothing. This is used to test connectivity.
	// RFC 1813 Section 3.3.0
	Null(repository metadata.Repository) ([]byte, error)

	// GetAttr returns the attributes for a file system object.
	// RFC 1813 Section 3.3.1
	GetAttr(repository metadata.Repository, req *nfs.GetAttrRequest, ctx *nfs.GetAttrContext) (*nfs.GetAttrResponse, error)

	// SetAttr sets the attributes for a file system object.
	// RFC 1813 Section 3.3.2
	SetAttr(repository metadata.Repository, req *nfs.SetAttrRequest, ctx *nfs.SetAttrContext) (*nfs.SetAttrResponse, error)

	// Lookup searches a directory for a specific name and returns its file handle.
	// RFC 1813 Section 3.3.3
	Lookup(repository metadata.Repository, req *nfs.LookupRequest, ctx *nfs.LookupContext) (*nfs.LookupResponse, error)

	// Access checks access permissions for a file system object.
	// RFC 1813 Section 3.3.4
	Access(repository metadata.Repository, req *nfs.AccessRequest, ctx *nfs.AccessContext) (*nfs.AccessResponse, error)

	// ReadLink reads the data associated with a symbolic link.
	// RFC 1813 Section 3.3.5
	ReadLink(repository metadata.Repository, req *nfs.ReadLinkRequest, ctx *nfs.ReadLinkContext) (*nfs.ReadLinkResponse, error)

	// Read reads data from a file.
	// RFC 1813 Section 3.3.6
	Read(content content.Repository, repository metadata.Repository, req *nfs.ReadRequest, ctx *nfs.ReadContext) (*nfs.ReadResponse, error)

	// Write writes data to a file.
	// RFC 1813 Section 3.3.7
	Write(content content.Repository, repository metadata.Repository, req *nfs.WriteRequest) (*nfs.WriteResponse, error)

	// Create creates a regular file.
	// RFC 1813 Section 3.3.8
	Create(content content.Repository, repository metadata.Repository, req *nfs.CreateRequest, ctx *nfs.CreateContext) (*nfs.CreateResponse, error)

	// Mkdir creates a directory.
	// RFC 1813 Section 3.3.9
	Mkdir(repository metadata.Repository, req *nfs.MkdirRequest, ctx *nfs.MkdirContext) (*nfs.MkdirResponse, error)

	// Symlink creates a symbolic link.
	// RFC 1813 Section 3.3.10
	Symlink(repository metadata.Repository, req *nfs.SymlinkRequest) (*nfs.SymlinkResponse, error)

	// MkNod creates a special device file.
	// RFC 1813 Section 3.3.11
	Mknod(repository metadata.Repository, req *nfs.MknodRequest, ctx *nfs.MknodContext) (*nfs.MknodResponse, error)

	// Remove removes a file.
	// RFC 1813 Section 3.3.12
	Remove(repository metadata.Repository, req *nfs.RemoveRequest, ctx *nfs.RemoveContext) (*nfs.RemoveResponse, error)

	// RmDir removes a directory.
	// RFC 1813 Section 3.3.13
	Rmdir(repository metadata.Repository, req *nfs.RmdirRequest, ctx *nfs.RmdirContext) (*nfs.RmdirResponse, error)

	// Rename renames a file or directory.
	// RFC 1813 Section 3.3.14
	Rename(repository metadata.Repository, req *nfs.RenameRequest, ctx *nfs.RenameContext) (*nfs.RenameResponse, error)

	// Link creates a hard link to a file.
	// RFC 1813 Section 3.3.15
	Link(repository metadata.Repository, req *nfs.LinkRequest, ctx *nfs.LinkContext) (*nfs.LinkResponse, error)

	// ReadDir reads entries from a directory.
	// RFC 1813 Section 3.3.16
	ReadDir(repository metadata.Repository, req *nfs.ReadDirRequest, ctx *nfs.ReadDirContext) (*nfs.ReadDirResponse, error)

	// ReadDirPlus reads entries from a directory with their attributes.
	// RFC 1813 Section 3.3.17
	ReadDirPlus(repository metadata.Repository, req *nfs.ReadDirPlusRequest, ctx *nfs.ReadDirPlusContext) (*nfs.ReadDirPlusResponse, error)

	// // FsStat returns dynamic information about a file system.
	// // RFC 1813 Section 3.3.18
	FsStat(repository metadata.Repository, req *nfs.FsStatRequest, ctx *nfs.FsStatContext) (*nfs.FsStatResponse, error)

	// FsInfo returns static information about a file system.
	// RFC 1813 Section 3.3.19
	FsInfo(repository metadata.Repository, req *nfs.FsInfoRequest, ctx *nfs.FsInfoContext) (*nfs.FsInfoResponse, error)

	// PathConf returns POSIX information about a file system object.
	// RFC 1813 Section 3.3.20
	PathConf(repository metadata.Repository, req *nfs.PathConfRequest, ctx *nfs.PathConfContext) (*nfs.PathConfResponse, error)

	// Commit commits cached data on the server to stable storage.
	// RFC 1813 Section 3.3.21
	Commit(repository metadata.Repository, req *nfs.CommitRequest) (*nfs.CommitResponse, error)
}

type MountHandler interface {
	// MountNull does nothing. This is used to test connectivity.
	// RFC 1813 Appendix I
	MountNull(repository metadata.Repository) ([]byte, error)

	// Mount returns a file handle for the requested export path.
	// This is the primary procedure used to mount an NFS file system.
	// RFC 1813 Appendix I
	Mount(repository metadata.Repository, req *mount.MountRequest, ctx *mount.MountContext) (*mount.MountResponse, error)

	// Dump returns a list of all mounted file systems.
	// RFC 1813 Appendix I
	Dump(repository metadata.Repository, req *mount.DumpRequest, ctx *mount.DumpContext) (*mount.DumpResponse, error)

	// Umnt removes a mount entry from the mount list.
	// RFC 1813 Appendix I
	Umnt(repository metadata.Repository, req *mount.UmountRequest, ctx *mount.UmountContext) (*mount.UmountResponse, error)

	// UmntAll removes all mount entries for the calling client.
	// RFC 1813 Appendix I
	UmntAll(repository metadata.Repository, req *mount.UmountAllRequest, ctx *mount.UmountAllContext) (*mount.UmountAllResponse, error)

	// Export returns a list of all exported file systems.
	// RFC 1813 Appendix I
	Export(repository metadata.Repository, req *mount.ExportRequest) (*mount.ExportResponse, error)
}
