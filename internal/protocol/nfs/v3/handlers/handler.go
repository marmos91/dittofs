package handlers

import (
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

type NFSHandler interface {
	// Null does nothing. This is used to test connectivity.
	// RFC 1813 Section 3.3.0
	Null(ctx *NullContext, repository metadata.Repository, req *NullRequest) (*NullResponse, error)

	// GetAttr returns the attributes for a file system object.
	// RFC 1813 Section 3.3.1
	GetAttr(ctx *GetAttrContext, repository metadata.Repository, req *GetAttrRequest) (*GetAttrResponse, error)

	// SetAttr sets the attributes for a file system object.
	// RFC 1813 Section 3.3.2
	SetAttr(ctx *SetAttrContext, repository metadata.Repository, req *SetAttrRequest) (*SetAttrResponse, error)

	// Lookup searches a directory for a specific name and returns its file handle.
	// RFC 1813 Section 3.3.3
	Lookup(ctx *LookupContext, repository metadata.Repository, req *LookupRequest) (*LookupResponse, error)

	// Access checks access permissions for a file system object.
	// RFC 1813 Section 3.3.4
	Access(ctx *AccessContext, repository metadata.Repository, req *AccessRequest) (*AccessResponse, error)

	// ReadLink reads the data associated with a symbolic link.
	// RFC 1813 Section 3.3.5
	ReadLink(ctx *ReadLinkContext, repository metadata.Repository, req *ReadLinkRequest) (*ReadLinkResponse, error)

	// Read reads data from a file.
	// RFC 1813 Section 3.3.6
	Read(ctx *ReadContext, content content.Repository, repository metadata.Repository, req *ReadRequest) (*ReadResponse, error)

	// Write writes data to a file.
	// RFC 1813 Section 3.3.7
	Write(ctx *WriteContext, content content.Repository, repository metadata.Repository, req *WriteRequest) (*WriteResponse, error)

	// Create creates a regular file.
	// RFC 1813 Section 3.3.8
	Create(ctx *CreateContext, content content.Repository, repository metadata.Repository, req *CreateRequest) (*CreateResponse, error)

	// Mkdir creates a directory.
	// RFC 1813 Section 3.3.9
	Mkdir(ctx *MkdirContext, repository metadata.Repository, req *MkdirRequest) (*MkdirResponse, error)

	// Symlink creates a symbolic link.
	// RFC 1813 Section 3.3.10
	Symlink(ctx *SymlinkContext, repository metadata.Repository, req *SymlinkRequest) (*SymlinkResponse, error)

	// MkNod creates a special device file.
	// RFC 1813 Section 3.3.11
	Mknod(ctx *MknodContext, repository metadata.Repository, req *MknodRequest) (*MknodResponse, error)

	// Remove removes a file.
	// RFC 1813 Section 3.3.12
	Remove(ctx *RemoveContext, repository metadata.Repository, req *RemoveRequest) (*RemoveResponse, error)

	// RmDir removes a directory.
	// RFC 1813 Section 3.3.13
	Rmdir(ctx *RmdirContext, repository metadata.Repository, req *RmdirRequest) (*RmdirResponse, error)

	// Rename renames a file or directory.
	// RFC 1813 Section 3.3.14
	Rename(ctx *RenameContext, repository metadata.Repository, req *RenameRequest) (*RenameResponse, error)

	// Link creates a hard link to a file.
	// RFC 1813 Section 3.3.15
	Link(ctx *LinkContext, repository metadata.Repository, req *LinkRequest) (*LinkResponse, error)

	// ReadDir reads entries from a directory.
	// RFC 1813 Section 3.3.16
	ReadDir(ctx *ReadDirContext, repository metadata.Repository, req *ReadDirRequest) (*ReadDirResponse, error)

	// ReadDirPlus reads entries from a directory with their attributes.
	// RFC 1813 Section 3.3.17
	ReadDirPlus(ctx *ReadDirPlusContext, repository metadata.Repository, req *ReadDirPlusRequest) (*ReadDirPlusResponse, error)

	// // FsStat returns dynamic information about a file system.
	// // RFC 1813 Section 3.3.18
	FsStat(ctx *FsStatContext, repository metadata.Repository, req *FsStatRequest) (*FsStatResponse, error)

	// FsInfo returns static information about a file system.
	// RFC 1813 Section 3.3.19
	FsInfo(ctx *FsInfoContext, repository metadata.Repository, req *FsInfoRequest) (*FsInfoResponse, error)

	// PathConf returns POSIX information about a file system object.
	// RFC 1813 Section 3.3.20
	PathConf(ctx *PathConfContext, repository metadata.Repository, req *PathConfRequest) (*PathConfResponse, error)

	// Commit commits cached data on the server to stable storage.
	// RFC 1813 Section 3.3.21
	Commit(ctx *CommitContext, repository metadata.Repository, req *CommitRequest) (*CommitResponse, error)
}
