package handlers

import (
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

type NFSHandler interface {
	// Null does nothing. This is used to test connectivity.
	// RFC 1813 Section 3.3.0
	Null(ctx *NullContext, metadataStore metadata.MetadataStore, req *NullRequest) (*NullResponse, error)

	// GetAttr returns the attributes for a file system object.
	// RFC 1813 Section 3.3.1
	GetAttr(ctx *GetAttrContext, metadataStore metadata.MetadataStore, req *GetAttrRequest) (*GetAttrResponse, error)

	// SetAttr sets the attributes for a file system object.
	// RFC 1813 Section 3.3.2
	SetAttr(ctx *SetAttrContext, metadataStore metadata.MetadataStore, req *SetAttrRequest) (*SetAttrResponse, error)

	// Lookup searches a directory for a specific name and returns its file handle.
	// RFC 1813 Section 3.3.3
	Lookup(ctx *LookupContext, metadataStore metadata.MetadataStore, req *LookupRequest) (*LookupResponse, error)

	// Access checks access permissions for a file system object.
	// RFC 1813 Section 3.3.4
	Access(ctx *AccessContext, metadataStore metadata.MetadataStore, req *AccessRequest) (*AccessResponse, error)

	// ReadLink reads the data associated with a symbolic link.
	// RFC 1813 Section 3.3.5
	ReadLink(ctx *ReadLinkContext, metadataStore metadata.MetadataStore, req *ReadLinkRequest) (*ReadLinkResponse, error)

	// Read reads data from a file.
	// RFC 1813 Section 3.3.6
	Read(ctx *ReadContext, content content.Repository, metadataStore metadata.MetadataStore, req *ReadRequest) (*ReadResponse, error)

	// Write writes data to a file.
	// RFC 1813 Section 3.3.7
	Write(ctx *WriteContext, content content.Repository, metadataStore metadata.MetadataStore, req *WriteRequest) (*WriteResponse, error)

	// Create creates a regular file.
	// RFC 1813 Section 3.3.8
	Create(ctx *CreateContext, content content.Repository, metadataStore metadata.MetadataStore, req *CreateRequest) (*CreateResponse, error)

	// Mkdir creates a directory.
	// RFC 1813 Section 3.3.9
	Mkdir(ctx *MkdirContext, metadataStore metadata.MetadataStore, req *MkdirRequest) (*MkdirResponse, error)

	// Symlink creates a symbolic link.
	// RFC 1813 Section 3.3.10
	Symlink(ctx *SymlinkContext, metadataStore metadata.MetadataStore, req *SymlinkRequest) (*SymlinkResponse, error)

	// MkNod creates a special device file.
	// RFC 1813 Section 3.3.11
	Mknod(ctx *MknodContext, metadataStore metadata.MetadataStore, req *MknodRequest) (*MknodResponse, error)

	// Remove removes a file.
	// RFC 1813 Section 3.3.12
	Remove(ctx *RemoveContext, metadataStore metadata.MetadataStore, req *RemoveRequest) (*RemoveResponse, error)

	// RmDir removes a directory.
	// RFC 1813 Section 3.3.13
	Rmdir(ctx *RmdirContext, metadataStore metadata.MetadataStore, req *RmdirRequest) (*RmdirResponse, error)

	// Rename renames a file or directory.
	// RFC 1813 Section 3.3.14
	Rename(ctx *RenameContext, metadataStore metadata.MetadataStore, req *RenameRequest) (*RenameResponse, error)

	// Link creates a hard link to a file.
	// RFC 1813 Section 3.3.15
	Link(ctx *LinkContext, metadataStore metadata.MetadataStore, req *LinkRequest) (*LinkResponse, error)

	// ReadDir reads entries from a directory.
	// RFC 1813 Section 3.3.16
	ReadDir(ctx *ReadDirContext, metadataStore metadata.MetadataStore, req *ReadDirRequest) (*ReadDirResponse, error)

	// ReadDirPlus reads entries from a directory with their attributes.
	// RFC 1813 Section 3.3.17
	ReadDirPlus(ctx *ReadDirPlusContext, metadataStore metadata.MetadataStore, req *ReadDirPlusRequest) (*ReadDirPlusResponse, error)

	// // FsStat returns dynamic information about a file system.
	// // RFC 1813 Section 3.3.18
	FsStat(ctx *FsStatContext, metadataStore metadata.MetadataStore, req *FsStatRequest) (*FsStatResponse, error)

	// FsInfo returns static information about a file system.
	// RFC 1813 Section 3.3.19
	FsInfo(ctx *FsInfoContext, metadataStore metadata.MetadataStore, req *FsInfoRequest) (*FsInfoResponse, error)

	// PathConf returns POSIX information about a file system object.
	// RFC 1813 Section 3.3.20
	PathConf(ctx *PathConfContext, metadataStore metadata.MetadataStore, req *PathConfRequest) (*PathConfResponse, error)

	// Commit commits cached data on the server to stable storage.
	// RFC 1813 Section 3.3.21
	Commit(ctx *CommitContext, metadataStore metadata.MetadataStore, req *CommitRequest) (*CommitResponse, error)
}
