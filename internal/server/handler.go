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
	Null(ctx *nfs.NullContext, repository metadata.Repository, req *nfs.NullRequest) (*nfs.NullResponse, error)

	// GetAttr returns the attributes for a file system object.
	// RFC 1813 Section 3.3.1
	GetAttr(ctx *nfs.GetAttrContext, repository metadata.Repository, req *nfs.GetAttrRequest) (*nfs.GetAttrResponse, error)

	// SetAttr sets the attributes for a file system object.
	// RFC 1813 Section 3.3.2
	SetAttr(ctx *nfs.SetAttrContext, repository metadata.Repository, req *nfs.SetAttrRequest) (*nfs.SetAttrResponse, error)

	// Lookup searches a directory for a specific name and returns its file handle.
	// RFC 1813 Section 3.3.3
	Lookup(ctx *nfs.LookupContext, repository metadata.Repository, req *nfs.LookupRequest) (*nfs.LookupResponse, error)

	// Access checks access permissions for a file system object.
	// RFC 1813 Section 3.3.4
	Access(ctx *nfs.AccessContext, repository metadata.Repository, req *nfs.AccessRequest) (*nfs.AccessResponse, error)

	// ReadLink reads the data associated with a symbolic link.
	// RFC 1813 Section 3.3.5
	ReadLink(ctx *nfs.ReadLinkContext, repository metadata.Repository, req *nfs.ReadLinkRequest) (*nfs.ReadLinkResponse, error)

	// Read reads data from a file.
	// RFC 1813 Section 3.3.6
	Read(ctx *nfs.ReadContext, content content.Repository, repository metadata.Repository, req *nfs.ReadRequest) (*nfs.ReadResponse, error)

	// Write writes data to a file.
	// RFC 1813 Section 3.3.7
	Write(ctx *nfs.WriteContext, content content.Repository, repository metadata.Repository, req *nfs.WriteRequest) (*nfs.WriteResponse, error)

	// Create creates a regular file.
	// RFC 1813 Section 3.3.8
	Create(ctx *nfs.CreateContext, content content.Repository, repository metadata.Repository, req *nfs.CreateRequest) (*nfs.CreateResponse, error)

	// Mkdir creates a directory.
	// RFC 1813 Section 3.3.9
	Mkdir(ctx *nfs.MkdirContext, repository metadata.Repository, req *nfs.MkdirRequest) (*nfs.MkdirResponse, error)

	// Symlink creates a symbolic link.
	// RFC 1813 Section 3.3.10
	Symlink(ctx *nfs.SymlinkContext, repository metadata.Repository, req *nfs.SymlinkRequest) (*nfs.SymlinkResponse, error)

	// MkNod creates a special device file.
	// RFC 1813 Section 3.3.11
	Mknod(ctx *nfs.MknodContext, repository metadata.Repository, req *nfs.MknodRequest) (*nfs.MknodResponse, error)

	// Remove removes a file.
	// RFC 1813 Section 3.3.12
	Remove(ctx *nfs.RemoveContext, repository metadata.Repository, req *nfs.RemoveRequest) (*nfs.RemoveResponse, error)

	// RmDir removes a directory.
	// RFC 1813 Section 3.3.13
	Rmdir(ctx *nfs.RmdirContext, repository metadata.Repository, req *nfs.RmdirRequest) (*nfs.RmdirResponse, error)

	// Rename renames a file or directory.
	// RFC 1813 Section 3.3.14
	Rename(ctx *nfs.RenameContext, repository metadata.Repository, req *nfs.RenameRequest) (*nfs.RenameResponse, error)

	// Link creates a hard link to a file.
	// RFC 1813 Section 3.3.15
	Link(ctx *nfs.LinkContext, repository metadata.Repository, req *nfs.LinkRequest) (*nfs.LinkResponse, error)

	// ReadDir reads entries from a directory.
	// RFC 1813 Section 3.3.16
	ReadDir(ctx *nfs.ReadDirContext, repository metadata.Repository, req *nfs.ReadDirRequest) (*nfs.ReadDirResponse, error)

	// ReadDirPlus reads entries from a directory with their attributes.
	// RFC 1813 Section 3.3.17
	ReadDirPlus(ctx *nfs.ReadDirPlusContext, repository metadata.Repository, req *nfs.ReadDirPlusRequest) (*nfs.ReadDirPlusResponse, error)

	// // FsStat returns dynamic information about a file system.
	// // RFC 1813 Section 3.3.18
	FsStat(ctx *nfs.FsStatContext, repository metadata.Repository, req *nfs.FsStatRequest) (*nfs.FsStatResponse, error)

	// FsInfo returns static information about a file system.
	// RFC 1813 Section 3.3.19
	FsInfo(ctx *nfs.FsInfoContext, repository metadata.Repository, req *nfs.FsInfoRequest) (*nfs.FsInfoResponse, error)

	// PathConf returns POSIX information about a file system object.
	// RFC 1813 Section 3.3.20
	PathConf(ctx *nfs.PathConfContext, repository metadata.Repository, req *nfs.PathConfRequest) (*nfs.PathConfResponse, error)

	// Commit commits cached data on the server to stable storage.
	// RFC 1813 Section 3.3.21
	Commit(ctx *nfs.CommitContext, repository metadata.Repository, req *nfs.CommitRequest) (*nfs.CommitResponse, error)
}

type MountHandler interface {
	// MountNull does nothing. This is used to test connectivity.
	// RFC 1813 Appendix I
	MountNull(ctx *mount.NullContext, repository metadata.Repository, req *mount.NullRequest) (*mount.NullResponse, error)

	// Mount returns a file handle for the requested export path.
	// This is the primary procedure used to mount an NFS file system.
	// RFC 1813 Appendix I
	Mount(ctx *mount.MountContext, repository metadata.Repository, req *mount.MountRequest) (*mount.MountResponse, error)

	// Dump returns a list of all mounted file systems.
	// RFC 1813 Appendix I
	Dump(ctx *mount.DumpContext, repository metadata.Repository, req *mount.DumpRequest) (*mount.DumpResponse, error)

	// Umnt removes a mount entry from the mount list.
	// RFC 1813 Appendix I
	Umnt(ctx *mount.UmountContext, repository metadata.Repository, req *mount.UmountRequest) (*mount.UmountResponse, error)

	// UmntAll removes all mount entries for the calling client.
	// RFC 1813 Appendix I
	UmntAll(ctx *mount.UmountAllContext, repository metadata.Repository, req *mount.UmountAllRequest) (*mount.UmountAllResponse, error)

	// Export returns a list of all exported file systems.
	// RFC 1813 Appendix I
	Export(ctx *mount.ExportContext, repository metadata.Repository, req *mount.ExportRequest) (*mount.ExportResponse, error)
}
