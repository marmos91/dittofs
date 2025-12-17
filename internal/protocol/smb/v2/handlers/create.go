package handlers

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Create handles SMB2 CREATE command [MS-SMB2] 2.2.13, 2.2.14
func (h *Handler) Create(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeCreateRequest(body)
	if err != nil {
		logger.Debug("CREATE: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("CREATE request",
		"filename", req.FileName,
		"disposition", req.CreateDisposition,
		"options", req.CreateOptions,
		"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess))

	// ========================================================================
	// Step 2: Get tree connection and validate session
	// ========================================================================

	tree, ok := h.GetTree(ctx.TreeID)
	if !ok {
		logger.Debug("CREATE: invalid tree ID", "treeID", ctx.TreeID)
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	sess, ok := h.GetSession(ctx.SessionID)
	if !ok {
		logger.Debug("CREATE: invalid session ID", "sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusUserSessionDeleted), nil
	}

	// Update context with session info
	ctx.ShareName = tree.ShareName
	ctx.User = sess.User
	ctx.IsGuest = sess.IsGuest
	ctx.Permission = tree.Permission

	// ========================================================================
	// Step 3: Get metadata store from registry
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(tree.ShareName)
	if err != nil {
		logger.Warn("CREATE: failed to get metadata store", "share", tree.ShareName, "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// ========================================================================
	// Step 4: Build AuthContext from SMB session
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("CREATE: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// ========================================================================
	// Step 5: Resolve path and get parent directory
	// ========================================================================

	// Normalize the filename (convert backslashes to forward slashes)
	filename := strings.ReplaceAll(req.FileName, "\\", "/")
	filename = strings.TrimPrefix(filename, "/")

	// Get root handle for the share
	rootHandle, err := h.Registry.GetRootHandle(tree.ShareName)
	if err != nil {
		logger.Warn("CREATE: failed to get root handle", "share", tree.ShareName, "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// Handle root directory case
	if filename == "" {
		return h.handleOpenRoot(ctx, req, metadataStore, authCtx, rootHandle, tree)
	}

	// Split path into directory and name components
	dirPath := path.Dir(filename)
	baseName := path.Base(filename)

	// Walk to parent directory
	parentHandle := rootHandle
	if dirPath != "." && dirPath != "" {
		parentHandle, err = h.walkPath(authCtx, metadataStore, rootHandle, dirPath)
		if err != nil {
			logger.Debug("CREATE: parent path not found", "path", dirPath, "error", err)
			return NewErrorResult(types.StatusObjectPathNotFound), nil
		}
	}

	// ========================================================================
	// Step 6: Check if file exists
	// ========================================================================

	existingFile, lookupErr := metadataStore.Lookup(authCtx, parentHandle, baseName)
	fileExists := (lookupErr == nil)

	// Check create options constraints
	isDirectoryRequest := req.CreateOptions&types.FileDirectoryFile != 0
	isNonDirectoryRequest := req.CreateOptions&types.FileNonDirectoryFile != 0

	// Validate directory vs file constraints for existing files
	if fileExists {
		if isDirectoryRequest && existingFile.Type != metadata.FileTypeDirectory {
			return NewErrorResult(types.StatusNotADirectory), nil
		}
		if isNonDirectoryRequest && existingFile.Type == metadata.FileTypeDirectory {
			return NewErrorResult(types.StatusFileIsADirectory), nil
		}
	}

	// ========================================================================
	// Step 7: Handle create disposition
	// ========================================================================

	createAction, dispErr := ResolveCreateDisposition(req.CreateDisposition, fileExists)
	if dispErr != nil {
		return NewErrorResult(MetadataErrorToSMBStatus(dispErr)), nil
	}

	// ========================================================================
	// Step 8: Perform create/open operation
	// ========================================================================

	var file *metadata.File
	var fileHandle metadata.FileHandle

	switch createAction {
	case types.FileOpened:
		// Open existing file
		file = existingFile
		fileHandle, err = metadata.EncodeFileHandle(file)
		if err != nil {
			logger.Warn("CREATE: failed to encode handle", "error", err)
			return NewErrorResult(types.StatusInternalError), nil
		}

	case types.FileCreated:
		// Create new file or directory
		file, fileHandle, err = h.createNewFile(authCtx, metadataStore, parentHandle, baseName, req, isDirectoryRequest)
		if err != nil {
			logger.Warn("CREATE: failed to create file", "name", baseName, "error", err)
			return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
		}

	case types.FileOverwritten, types.FileSuperseded:
		// Open and truncate/replace existing file
		file, fileHandle, err = h.overwriteFile(authCtx, metadataStore, existingFile, req)
		if err != nil {
			logger.Warn("CREATE: failed to overwrite file", "name", baseName, "error", err)
			return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
		}
	}

	// ========================================================================
	// Step 9: Store open file with metadata handle
	// ========================================================================

	smbFileID := h.GenerateFileID()

	openFile := &OpenFile{
		FileID:         smbFileID,
		TreeID:         ctx.TreeID,
		SessionID:      ctx.SessionID,
		Path:           filename,
		ShareName:      tree.ShareName,
		OpenTime:       time.Now(),
		DesiredAccess:  req.DesiredAccess,
		IsDirectory:    file.Type == metadata.FileTypeDirectory,
		MetadataHandle: fileHandle,
		ContentID:      file.ContentID,
		// Store parent info for delete-on-close support
		ParentHandle: parentHandle,
		FileName:     baseName,
	}
	h.StoreOpenFile(openFile)

	logger.Debug("CREATE successful",
		"fileID", fmt.Sprintf("%x", smbFileID),
		"filename", filename,
		"action", createAction,
		"isDirectory", openFile.IsDirectory)

	// ========================================================================
	// Step 10: Build and encode response
	// ========================================================================

	creation, access, write, change := FileAttrToSMBTimes(&file.FileAttr)
	// Use MFsymlink size for symlinks
	size := getSMBSize(&file.FileAttr)
	allocationSize := ((size + 4095) / 4096) * 4096

	resp := &CreateResponse{
		OplockLevel:    0, // No oplock
		CreateAction:   createAction,
		CreationTime:   creation,
		LastAccessTime: access,
		LastWriteTime:  write,
		ChangeTime:     change,
		AllocationSize: allocationSize,
		EndOfFile:      size,
		FileAttributes: FileAttrToSMBAttributes(&file.FileAttr),
		FileID:         smbFileID,
	}

	respBytes, err := EncodeCreateResponse(resp)
	if err != nil {
		logger.Warn("CREATE: failed to encode response", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	return NewResult(types.StatusSuccess, respBytes), nil
}

// handleOpenRoot handles opening the root directory of a share.
func (h *Handler) handleOpenRoot(
	ctx *SMBHandlerContext,
	req *CreateRequest,
	metadataStore metadata.MetadataStore,
	authCtx *metadata.AuthContext,
	rootHandle metadata.FileHandle,
	tree *TreeConnection,
) (*HandlerResult, error) {
	// Root can only be opened with FILE_OPEN disposition
	if req.CreateDisposition != types.FileOpen && req.CreateDisposition != types.FileOpenIf {
		return NewErrorResult(types.StatusObjectNameCollision), nil
	}

	// Get root file attributes
	rootFile, err := metadataStore.GetFile(authCtx.Context, rootHandle)
	if err != nil {
		logger.Warn("CREATE: failed to get root file", "error", err)
		return NewErrorResult(types.StatusObjectNameNotFound), nil
	}

	// Store open file
	smbFileID := h.GenerateFileID()
	openFile := &OpenFile{
		FileID:         smbFileID,
		TreeID:         ctx.TreeID,
		SessionID:      ctx.SessionID,
		Path:           "",
		ShareName:      tree.ShareName,
		OpenTime:       time.Now(),
		DesiredAccess:  req.DesiredAccess,
		IsDirectory:    true,
		MetadataHandle: rootHandle,
	}
	h.StoreOpenFile(openFile)

	creation, access, write, change := FileAttrToSMBTimes(&rootFile.FileAttr)

	resp := &CreateResponse{
		OplockLevel:    0,
		CreateAction:   types.FileOpened,
		CreationTime:   creation,
		LastAccessTime: access,
		LastWriteTime:  write,
		ChangeTime:     change,
		AllocationSize: 0,
		EndOfFile:      0,
		FileAttributes: types.FileAttributeDirectory,
		FileID:         smbFileID,
	}

	respBytes, err := EncodeCreateResponse(resp)
	if err != nil {
		logger.Warn("CREATE: failed to encode root response", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	return NewResult(types.StatusSuccess, respBytes), nil
}

// walkPath walks a path from a starting handle, returning the final handle.
func (h *Handler) walkPath(
	authCtx *metadata.AuthContext,
	metadataStore metadata.MetadataStore,
	startHandle metadata.FileHandle,
	pathStr string,
) (metadata.FileHandle, error) {
	currentHandle := startHandle

	// Split path into components
	parts := strings.Split(pathStr, "/")
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			// TODO: Handle parent directory navigation
			continue
		}

		file, err := metadataStore.Lookup(authCtx, currentHandle, part)
		if err != nil {
			return nil, err
		}

		if file.Type != metadata.FileTypeDirectory {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: fmt.Sprintf("%s is not a directory", part),
			}
		}

		currentHandle, err = metadata.EncodeFileHandle(file)
		if err != nil {
			return nil, err
		}
	}

	return currentHandle, nil
}

// createNewFile creates a new file or directory in the metadata store.
func (h *Handler) createNewFile(
	authCtx *metadata.AuthContext,
	metadataStore metadata.MetadataStore,
	parentHandle metadata.FileHandle,
	name string,
	req *CreateRequest,
	isDirectory bool,
) (*metadata.File, metadata.FileHandle, error) {
	// Build file attributes
	fileAttr := &metadata.FileAttr{
		Mode: SMBModeFromAttrs(req.FileAttributes, isDirectory),
	}

	// Set owner from auth context
	if authCtx.Identity.UID != nil {
		fileAttr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		fileAttr.GID = *authCtx.Identity.GID
	}

	var file *metadata.File
	var err error

	if isDirectory {
		fileAttr.Type = metadata.FileTypeDirectory
	} else {
		fileAttr.Type = metadata.FileTypeRegular
		fileAttr.Size = 0
	}

	// Create() handles both files and directories based on fileAttr.Type
	file, err = metadataStore.Create(authCtx, parentHandle, name, fileAttr)

	if err != nil {
		return nil, nil, err
	}

	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		return nil, nil, err
	}

	return file, fileHandle, nil
}

// overwriteFile truncates an existing file for OVERWRITE/SUPERSEDE operations.
func (h *Handler) overwriteFile(
	authCtx *metadata.AuthContext,
	metadataStore metadata.MetadataStore,
	existingFile *metadata.File,
	req *CreateRequest,
) (*metadata.File, metadata.FileHandle, error) {
	fileHandle, err := metadata.EncodeFileHandle(existingFile)
	if err != nil {
		return nil, nil, err
	}

	// Truncate to zero size
	zeroSize := uint64(0)
	setAttrs := &metadata.SetAttrs{
		Size: &zeroSize,
	}

	err = metadataStore.SetFileAttributes(authCtx, fileHandle, setAttrs)
	if err != nil {
		return nil, nil, err
	}

	// Get updated file
	updatedFile, err := metadataStore.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		return nil, nil, err
	}

	return updatedFile, fileHandle, nil
}
