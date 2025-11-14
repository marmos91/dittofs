package handlers

import (
	"sync"
)

// Response pools for frequently allocated response structures.
// These pools reduce GC pressure by reusing response objects across requests.
//
// Usage pattern:
//  1. Get response from pool: resp := GetGetAttrResponse()
//  2. Populate response fields
//  3. Encode response
//  4. Return to pool: PutGetAttrResponse(resp)
//
// IMPORTANT: Always reset response fields before returning to pool to avoid
// data leakage between requests.

var (
	getAttrResponsePool = sync.Pool{
		New: func() interface{} {
			return &GetAttrResponse{}
		},
	}

	writeResponsePool = sync.Pool{
		New: func() interface{} {
			return &WriteResponse{}
		},
	}

	readResponsePool = sync.Pool{
		New: func() interface{} {
			return &ReadResponse{}
		},
	}

	lookupResponsePool = sync.Pool{
		New: func() interface{} {
			return &LookupResponse{}
		},
	}

	setAttrResponsePool = sync.Pool{
		New: func() interface{} {
			return &SetAttrResponse{}
		},
	}

	createResponsePool = sync.Pool{
		New: func() interface{} {
			return &CreateResponse{}
		},
	}

	mkdirResponsePool = sync.Pool{
		New: func() interface{} {
			return &MkdirResponse{}
		},
	}

	removeResponsePool = sync.Pool{
		New: func() interface{} {
			return &RemoveResponse{}
		},
	}

	rmdirResponsePool = sync.Pool{
		New: func() interface{} {
			return &RmdirResponse{}
		},
	}

	renameResponsePool = sync.Pool{
		New: func() interface{} {
			return &RenameResponse{}
		},
	}

	linkResponsePool = sync.Pool{
		New: func() interface{} {
			return &LinkResponse{}
		},
	}

	symlinkResponsePool = sync.Pool{
		New: func() interface{} {
			return &SymlinkResponse{}
		},
	}

	readLinkResponsePool = sync.Pool{
		New: func() interface{} {
			return &ReadLinkResponse{}
		},
	}

	readdirResponsePool = sync.Pool{
		New: func() interface{} {
			return &ReadDirResponse{}
		},
	}

	readdirPlusResponsePool = sync.Pool{
		New: func() interface{} {
			return &ReadDirPlusResponse{}
		},
	}

	fsStatResponsePool = sync.Pool{
		New: func() interface{} {
			return &FsStatResponse{}
		},
	}

	fsInfoResponsePool = sync.Pool{
		New: func() interface{} {
			return &FsInfoResponse{}
		},
	}

	pathConfResponsePool = sync.Pool{
		New: func() interface{} {
			return &PathConfResponse{}
		},
	}

	accessResponsePool = sync.Pool{
		New: func() interface{} {
			return &AccessResponse{}
		},
	}

	commitResponsePool = sync.Pool{
		New: func() interface{} {
			return &CommitResponse{}
		},
	}

	mknodResponsePool = sync.Pool{
		New: func() interface{} {
			return &MknodResponse{}
		},
	}
)

// GetAttrResponse pool

func GetGetAttrResponse() *GetAttrResponse {
	return getAttrResponsePool.Get().(*GetAttrResponse)
}

func PutGetAttrResponse(resp *GetAttrResponse) {
	// Reset to avoid data leakage
	*resp = GetAttrResponse{}
	getAttrResponsePool.Put(resp)
}

// WriteResponse pool

func GetWriteResponse() *WriteResponse {
	return writeResponsePool.Get().(*WriteResponse)
}

func PutWriteResponse(resp *WriteResponse) {
	*resp = WriteResponse{}
	writeResponsePool.Put(resp)
}

// ReadResponse pool

func GetReadResponse() *ReadResponse {
	return readResponsePool.Get().(*ReadResponse)
}

func PutReadResponse(resp *ReadResponse) {
	*resp = ReadResponse{}
	readResponsePool.Put(resp)
}

// LookupResponse pool

func GetLookupResponse() *LookupResponse {
	return lookupResponsePool.Get().(*LookupResponse)
}

func PutLookupResponse(resp *LookupResponse) {
	*resp = LookupResponse{}
	lookupResponsePool.Put(resp)
}

// SetAttrResponse pool

func GetSetAttrResponse() *SetAttrResponse {
	return setAttrResponsePool.Get().(*SetAttrResponse)
}

func PutSetAttrResponse(resp *SetAttrResponse) {
	*resp = SetAttrResponse{}
	setAttrResponsePool.Put(resp)
}

// CreateResponse pool

func GetCreateResponse() *CreateResponse {
	return createResponsePool.Get().(*CreateResponse)
}

func PutCreateResponse(resp *CreateResponse) {
	*resp = CreateResponse{}
	createResponsePool.Put(resp)
}

// MkdirResponse pool

func GetMkdirResponse() *MkdirResponse {
	return mkdirResponsePool.Get().(*MkdirResponse)
}

func PutMkdirResponse(resp *MkdirResponse) {
	*resp = MkdirResponse{}
	mkdirResponsePool.Put(resp)
}

// RemoveResponse pool

func GetRemoveResponse() *RemoveResponse {
	return removeResponsePool.Get().(*RemoveResponse)
}

func PutRemoveResponse(resp *RemoveResponse) {
	*resp = RemoveResponse{}
	removeResponsePool.Put(resp)
}

// RmdirResponse pool

func GetRmdirResponse() *RmdirResponse {
	return rmdirResponsePool.Get().(*RmdirResponse)
}

func PutRmdirResponse(resp *RmdirResponse) {
	*resp = RmdirResponse{}
	rmdirResponsePool.Put(resp)
}

// RenameResponse pool

func GetRenameResponse() *RenameResponse {
	return renameResponsePool.Get().(*RenameResponse)
}

func PutRenameResponse(resp *RenameResponse) {
	*resp = RenameResponse{}
	renameResponsePool.Put(resp)
}

// LinkResponse pool

func GetLinkResponse() *LinkResponse {
	return linkResponsePool.Get().(*LinkResponse)
}

func PutLinkResponse(resp *LinkResponse) {
	*resp = LinkResponse{}
	linkResponsePool.Put(resp)
}

// SymlinkResponse pool

func GetSymlinkResponse() *SymlinkResponse {
	return symlinkResponsePool.Get().(*SymlinkResponse)
}

func PutSymlinkResponse(resp *SymlinkResponse) {
	*resp = SymlinkResponse{}
	symlinkResponsePool.Put(resp)
}

// ReadLinkResponse pool

func GetReadLinkResponse() *ReadLinkResponse {
	return readLinkResponsePool.Get().(*ReadLinkResponse)
}

func PutReadLinkResponse(resp *ReadLinkResponse) {
	*resp = ReadLinkResponse{}
	readLinkResponsePool.Put(resp)
}

// ReadDirResponse pool

func GetReadDirResponse() *ReadDirResponse {
	return readdirResponsePool.Get().(*ReadDirResponse)
}

func PutReadDirResponse(resp *ReadDirResponse) {
	*resp = ReadDirResponse{}
	readdirResponsePool.Put(resp)
}

// ReadDirPlusResponse pool

func GetReadDirPlusResponse() *ReadDirPlusResponse {
	return readdirPlusResponsePool.Get().(*ReadDirPlusResponse)
}

func PutReadDirPlusResponse(resp *ReadDirPlusResponse) {
	*resp = ReadDirPlusResponse{}
	readdirPlusResponsePool.Put(resp)
}

// FsStatResponse pool

func GetFsStatResponse() *FsStatResponse {
	return fsStatResponsePool.Get().(*FsStatResponse)
}

func PutFsStatResponse(resp *FsStatResponse) {
	*resp = FsStatResponse{}
	fsStatResponsePool.Put(resp)
}

// FsInfoResponse pool

func GetFsInfoResponse() *FsInfoResponse {
	return fsInfoResponsePool.Get().(*FsInfoResponse)
}

func PutFsInfoResponse(resp *FsInfoResponse) {
	*resp = FsInfoResponse{}
	fsInfoResponsePool.Put(resp)
}

// PathConfResponse pool

func GetPathConfResponse() *PathConfResponse {
	return pathConfResponsePool.Get().(*PathConfResponse)
}

func PutPathConfResponse(resp *PathConfResponse) {
	*resp = PathConfResponse{}
	pathConfResponsePool.Put(resp)
}

// AccessResponse pool

func GetAccessResponse() *AccessResponse {
	return accessResponsePool.Get().(*AccessResponse)
}

func PutAccessResponse(resp *AccessResponse) {
	*resp = AccessResponse{}
	accessResponsePool.Put(resp)
}

// CommitResponse pool

func GetCommitResponse() *CommitResponse {
	return commitResponsePool.Get().(*CommitResponse)
}

func PutCommitResponse(resp *CommitResponse) {
	*resp = CommitResponse{}
	commitResponsePool.Put(resp)
}

// MknodResponse pool

func GetMknodResponse() *MknodResponse {
	return mknodResponsePool.Get().(*MknodResponse)
}

func PutMknodResponse(resp *MknodResponse) {
	*resp = MknodResponse{}
	mknodResponsePool.Put(resp)
}
