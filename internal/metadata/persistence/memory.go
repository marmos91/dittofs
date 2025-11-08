package persistence

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/metadata"
)

// Mount tracking
type mountKey struct {
	exportPath string
	clientAddr string
}

// MemoryRepository implements Repository using in-memory storage
type MemoryRepository struct {
	mu           sync.RWMutex
	exports      map[string]*exportData
	files        map[string]*metadata.FileAttr
	parents      map[string]metadata.FileHandle
	children     map[string]map[string]metadata.FileHandle
	handleIndex  uint64
	mounts       map[mountKey]*metadata.MountEntry
	serverConfig metadata.ServerConfig
}

type exportData struct {
	Export     metadata.Export
	RootHandle metadata.FileHandle
}

// NewMemoryRepository creates a new in-memory repository
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		exports:     make(map[string]*exportData),
		files:       make(map[string]*metadata.FileAttr),
		parents:     make(map[string]metadata.FileHandle),
		children:    make(map[string]map[string]metadata.FileHandle),
		mounts:      make(map[mountKey]*metadata.MountEntry),
		handleIndex: 0,
	}
}

// RemoveMount removes a mount record when a client unmounts
func (r *MemoryRepository) RemoveMount(exportPath string, clientAddr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := mountKey{exportPath: exportPath, clientAddr: clientAddr}
	delete(r.mounts, key)
	return nil
}

// GetMounts returns all active mounts, optionally filtered by export path
func (r *MemoryRepository) GetMounts(exportPath string) ([]metadata.MountEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]metadata.MountEntry, 0)
	for key, mount := range r.mounts {
		if exportPath == "" || key.exportPath == exportPath {
			result = append(result, *mount)
		}
	}

	return result, nil
}

// IsClientMounted checks if a specific client has an active mount
func (r *MemoryRepository) IsClientMounted(exportPath string, clientAddr string) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := mountKey{exportPath: exportPath, clientAddr: clientAddr}
	_, exists := r.mounts[key]
	return exists, nil
}

// CheckExportAccess verifies if a client can access an export and applies squashing rules.
//
// This method:
//  1. Verifies the export exists
//  2. Checks authentication requirements
//  3. Validates allowed/denied client lists
//  4. Applies UID/GID squashing rules (AllSquash, RootSquash)
//  5. Returns access decision with effective credentials
//
// The returned AccessDecision contains the final determination of whether
// access should be granted and which authentication flavors are allowed.
//
// Squashing is applied here (at mount time) so that all subsequent operations
// use the squashed credentials consistently.
func (r *MemoryRepository) CheckExportAccess(
	exportPath string,
	clientAddr string,
	authFlavor uint32,
	uid *uint32,
	gid *uint32,
	gids []uint32,
) (*metadata.AccessDecision, *metadata.AuthContext, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Check if export exists
	ed, exists := r.exports[exportPath]
	if !exists {
		return nil, nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("export not found: %s", exportPath),
			Export:  exportPath,
		}
	}

	opts := ed.Export.Options

	// Check authentication requirements
	if opts.RequireAuth && authFlavor == 0 {
		return nil, nil, &metadata.ExportError{
			Code:    metadata.ExportErrAuthRequired,
			Message: "authentication required for this export",
			Export:  exportPath,
		}
	}

	// Check if auth flavor is allowed
	if len(opts.AllowedAuthFlavors) > 0 {
		allowed := slices.Contains(opts.AllowedAuthFlavors, authFlavor)
		if !allowed {
			return nil, nil, &metadata.ExportError{
				Code:    metadata.ExportErrAuthRequired,
				Message: fmt.Sprintf("authentication flavor %d not allowed", authFlavor),
				Export:  exportPath,
			}
		}
	}

	// Check denied clients first
	if len(opts.DeniedClients) > 0 {
		for _, denied := range opts.DeniedClients {
			if matchesIPPattern(clientAddr, denied) {
				return nil, nil, &metadata.ExportError{
					Code:    metadata.ExportErrAccessDenied,
					Message: fmt.Sprintf("client %s is explicitly denied", clientAddr),
					Export:  exportPath,
				}
			}
		}
	}

	// Check allowed clients (if specified)
	if len(opts.AllowedClients) > 0 {
		allowed := false
		for _, pattern := range opts.AllowedClients {
			if matchesIPPattern(clientAddr, pattern) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, nil, &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: fmt.Sprintf("client %s not in allowed list", clientAddr),
				Export:  exportPath,
			}
		}
	}

	// Determine allowed auth flavors
	allowedAuth := opts.AllowedAuthFlavors
	if len(allowedAuth) == 0 {
		// If not specified, allow AUTH_NULL and AUTH_UNIX
		allowedAuth = []uint32{0, 1}
	}

	// Apply squashing rules to get effective credentials
	effectiveUID, effectiveGID, effectiveGIDs := metadata.ApplySquashing(
		&opts,
		authFlavor,
		uid,
		gid,
		gids,
	)

	// Build authentication context with effective (squashed) credentials
	authCtx := &metadata.AuthContext{
		AuthFlavor: authFlavor,
		UID:        effectiveUID,
		GID:        effectiveGID,
		GIDs:       effectiveGIDs,
		ClientAddr: clientAddr,
	}

	// Access granted
	decision := &metadata.AccessDecision{
		Allowed:     true,
		Reason:      "access granted",
		AllowedAuth: allowedAuth,
		ReadOnly:    opts.ReadOnly,
	}

	return decision, authCtx, nil
}

// matchesIPPattern checks if an IP matches a pattern (IP address or CIDR)
func matchesIPPattern(clientIP string, pattern string) bool {
	// Try parsing as CIDR first
	_, ipNet, err := net.ParseCIDR(pattern)
	if err == nil {
		ip := net.ParseIP(clientIP)
		if ip != nil {
			return ipNet.Contains(ip)
		}
		return false
	}

	// Otherwise, exact IP match
	return clientIP == pattern
}

// RecordMount records an active mount by a client with auth details
func (r *MemoryRepository) RecordMount(exportPath string, clientAddr string, authFlavor uint32, machineName string, uid *uint32, gid *uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := mountKey{exportPath: exportPath, clientAddr: clientAddr}

	entry := &metadata.MountEntry{
		ExportPath:  exportPath,
		ClientAddr:  clientAddr,
		MountedAt:   time.Now(),
		AuthFlavor:  authFlavor,
		MachineName: machineName,
		UnixUID:     uid,
		UnixGID:     gid,
	}

	// Update if already exists
	r.mounts[key] = entry
	return nil
}

// Helper to convert FileHandle to string key
func handleToKey(handle metadata.FileHandle) string {
	return string(handle)
}

// generateFileHandle creates a unique file handle
func (r *MemoryRepository) generateFileHandle(seed string) metadata.FileHandle {
	r.handleIndex++
	data := fmt.Sprintf("%s-%d", seed, r.handleIndex)
	hash := sha256.Sum256([]byte(data))
	return hash[:]
}

// Export operations

func (r *MemoryRepository) AddExport(path string, options metadata.ExportOptions, rootAttr *metadata.FileAttr) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if export already exists
	if _, exists := r.exports[path]; exists {
		return fmt.Errorf("export already exists: %s", path)
	}

	// Generate root handle
	rootHandle := r.generateFileHandle(path)
	key := handleToKey(rootHandle)

	// Store root attributes
	r.files[key] = rootAttr

	// Initialize empty children map for the root directory
	r.children[key] = make(map[string]metadata.FileHandle)

	// Store export data
	r.exports[path] = &exportData{
		Export: metadata.Export{
			Path:    path,
			Options: options,
		},
		RootHandle: rootHandle,
	}

	return nil
}

func (r *MemoryRepository) GetExports() ([]metadata.Export, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]metadata.Export, 0, len(r.exports))
	for _, ed := range r.exports {
		result = append(result, ed.Export)
	}
	return result, nil
}

func (r *MemoryRepository) FindExport(path string) (*metadata.Export, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ed, exists := r.exports[path]
	if !exists {
		return nil, fmt.Errorf("export not found: %s", path)
	}
	return &ed.Export, nil
}

func (r *MemoryRepository) GetRootHandle(exportPath string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ed, exists := r.exports[exportPath]
	if !exists {
		return nil, fmt.Errorf("export not found: %s", exportPath)
	}
	return ed.RootHandle, nil
}

func (r *MemoryRepository) DeleteExport(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.exports[path]; !exists {
		return fmt.Errorf("export not found: %s", path)
	}

	delete(r.exports, path)
	return nil
}

// File operations

func (r *MemoryRepository) CreateFile(handle metadata.FileHandle, attr *metadata.FileAttr) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := handleToKey(handle)
	if _, exists := r.files[key]; exists {
		return fmt.Errorf("file already exists")
	}

	r.files[key] = attr

	// If it's a directory, initialize children map
	if attr.Type == metadata.FileTypeDirectory {
		r.children[key] = make(map[string]metadata.FileHandle)
	}

	return nil
}

func (r *MemoryRepository) GetFile(handle metadata.FileHandle) (*metadata.FileAttr, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := handleToKey(handle)
	attr, exists := r.files[key]
	if !exists {
		return nil, fmt.Errorf("file not found")
	}

	return attr, nil
}

func (r *MemoryRepository) UpdateFile(handle metadata.FileHandle, attr *metadata.FileAttr) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return fmt.Errorf("file not found")
	}

	r.files[key] = attr
	return nil
}

func (r *MemoryRepository) DeleteFile(handle metadata.FileHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return fmt.Errorf("file not found")
	}

	delete(r.files, key)
	return nil
}

// Directory hierarchy operations

func (r *MemoryRepository) SetParent(child metadata.FileHandle, parent metadata.FileHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.parents[handleToKey(child)] = parent
	return nil
}

func (r *MemoryRepository) GetParent(child metadata.FileHandle) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parent, exists := r.parents[handleToKey(child)]
	if !exists {
		return nil, fmt.Errorf("parent not found")
	}

	return parent, nil
}

func (r *MemoryRepository) AddChild(parent metadata.FileHandle, name string, child metadata.FileHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[parentKey][name]; exists {
		return fmt.Errorf("child already exists: %s", name)
	}

	r.children[parentKey][name] = child
	return nil
}

func (r *MemoryRepository) GetChild(parent metadata.FileHandle, name string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		return nil, fmt.Errorf("parent has no children")
	}

	child, exists := r.children[parentKey][name]
	if !exists {
		return nil, fmt.Errorf("child not found: %s", name)
	}

	return child, nil
}

func (r *MemoryRepository) GetChildren(parent metadata.FileHandle) (map[string]metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parentKey := handleToKey(parent)
	children := r.children[parentKey]
	if children == nil {
		return make(map[string]metadata.FileHandle), nil
	}

	// Return a copy to avoid concurrent access issues
	result := make(map[string]metadata.FileHandle, len(children))
	maps.Copy(result, children)

	return result, nil
}

func (r *MemoryRepository) DeleteChild(parent metadata.FileHandle, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		return fmt.Errorf("parent has no children")
	}

	if _, exists := r.children[parentKey][name]; !exists {
		return fmt.Errorf("child not found: %s", name)
	}

	delete(r.children[parentKey], name)
	return nil
}

// Helper method to add files and directories easily
func (r *MemoryRepository) AddFileToDirectory(parentHandle metadata.FileHandle, name string, attr *metadata.FileAttr) (metadata.FileHandle, error) {
	fileHandle := r.generateFileHandle(name)

	if err := r.CreateFile(fileHandle, attr); err != nil {
		return nil, err
	}

	if err := r.AddChild(parentHandle, name, fileHandle); err != nil {
		return nil, err
	}

	if err := r.SetParent(fileHandle, parentHandle); err != nil {
		return nil, err
	}

	return fileHandle, nil
}

// GetMountsByClient returns all active mounts for a specific client
func (r *MemoryRepository) GetMountsByClient(clientAddr string) ([]metadata.MountEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]metadata.MountEntry, 0)
	for key, mount := range r.mounts {
		if key.clientAddr == clientAddr {
			result = append(result, *mount)
		}
	}

	return result, nil
}

// RemoveAllMounts removes all mount records for a specific client
func (r *MemoryRepository) RemoveAllMounts(clientAddr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Find all mount keys for this client
	keysToDelete := make([]mountKey, 0)
	for key := range r.mounts {
		if key.clientAddr == clientAddr {
			keysToDelete = append(keysToDelete, key)
		}
	}

	// Delete all found mounts
	for _, key := range keysToDelete {
		delete(r.mounts, key)
	}

	return nil
}

// SetServerConfig sets the server-wide configuration
func (r *MemoryRepository) SetServerConfig(config metadata.ServerConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.serverConfig = config
	return nil
}

// GetServerConfig returns the current server configuration
func (r *MemoryRepository) GetServerConfig() (metadata.ServerConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.serverConfig, nil
}

// GetFSInfo returns the static filesystem information and capabilities
func (r *MemoryRepository) GetFSInfo(handle metadata.FileHandle) (*metadata.FSInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Return filesystem information
	// These are reasonable defaults that can be customized via ServerConfig in the future
	return &metadata.FSInfo{
		RtMax:       65536,     // 64KB max read
		RtPref:      32768,     // 32KB preferred read
		RtMult:      4096,      // 4KB read multiple
		WtMax:       65536,     // 64KB max write
		WtPref:      32768,     // 32KB preferred write
		WtMult:      4096,      // 4KB write multiple
		DtPref:      8192,      // 8KB preferred readdir
		MaxFileSize: 1<<63 - 1, // Max file size (practically unlimited)
		TimeDelta: metadata.TimeDelta{
			Seconds:  0,
			Nseconds: 1, // 1 nanosecond time granularity
		},
		// Properties: hard links, symlinks, homogeneous PATHCONF, can set time
		// These correspond to FSFLink | FSFSymlink | FSFHomogeneous | FSFCanSetTime
		Properties: 0x0001 | 0x0002 | 0x0008 | 0x0010,
	}, nil
}

// CheckDumpAccess verifies if a client can call the DUMP procedure
func (r *MemoryRepository) CheckDumpAccess(clientAddr string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// If no restrictions configured, allow all (RFC 1813 default)
	if len(r.serverConfig.DumpAllowedClients) == 0 &&
		len(r.serverConfig.DumpDeniedClients) == 0 {
		return nil
	}

	// Check denied clients first (deny takes precedence)
	if len(r.serverConfig.DumpDeniedClients) > 0 {
		for _, denied := range r.serverConfig.DumpDeniedClients {
			if matchesIPPattern(clientAddr, denied) {
				return &metadata.ExportError{
					Code:    metadata.ExportErrAccessDenied,
					Message: fmt.Sprintf("client %s is denied DUMP access", clientAddr),
					Export:  "DUMP",
				}
			}
		}
	}

	// Check allowed clients (if specified)
	if len(r.serverConfig.DumpAllowedClients) > 0 {
		allowed := false
		for _, pattern := range r.serverConfig.DumpAllowedClients {
			if matchesIPPattern(clientAddr, pattern) {
				allowed = true
				break
			}
		}
		if !allowed {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: fmt.Sprintf("client %s not in DUMP allowed list", clientAddr),
				Export:  "DUMP",
			}
		}
	}

	// Access granted
	return nil
}

// GetFSStats returns the dynamic filesystem statistics
func (r *MemoryRepository) GetFSStats(handle metadata.FileHandle) (*metadata.FSStat, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Return filesystem statistics
	// These are reasonable defaults for a virtual/in-memory filesystem
	// Real implementations should query actual backend storage
	return &metadata.FSStat{
		TotalBytes: 1024 * 1024 * 1024 * 1024, // 1TB
		FreeBytes:  512 * 1024 * 1024 * 1024,  // 512GB
		AvailBytes: 512 * 1024 * 1024 * 1024,  // 512GB (same as free for non-root)
		TotalFiles: 1000000,                   // 1M inodes
		FreeFiles:  900000,                    // 900K free inodes
		AvailFiles: 900000,                    // 900K available (same as free for non-root)
		Invarsec:   0,                         // Filesystem can change at any time
	}, nil
}

// CreateLink creates a hard link to an existing file.
//
// This implementation:
//  1. Verifies the file and directory exist
//  2. Checks write access to the directory (if auth context provided)
//  3. Verifies the name doesn't already exist
//  4. Adds the new directory entry pointing to the same file handle
//  5. Updates directory modification time
//
// Note: This implementation doesn't increment nlink as we're using
// a handle-based system where multiple directory entries can reference
// the same handle without tracking a separate link count.
func (r *MemoryRepository) CreateLink(dirHandle metadata.FileHandle, name string, fileHandle metadata.FileHandle, ctx *metadata.AuthContext) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// ========================================================================
	// Step 1: Verify file exists
	// ========================================================================

	fileKey := handleToKey(fileHandle)
	fileAttr, exists := r.files[fileKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "source file not found",
		}
	}

	// ========================================================================
	// Step 2: Verify directory exists
	// ========================================================================

	dirKey := handleToKey(dirHandle)
	dirAttr, exists := r.files[dirKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "target directory not found",
		}
	}

	// Verify target is a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "target is not a directory",
		}
	}

	// ========================================================================
	// Step 3: Check write access to directory (if auth context provided)
	// ========================================================================

	if ctx != nil && ctx.AuthFlavor != 0 && ctx.UID != nil {
		// Check if user has write permission on the directory
		uid := *ctx.UID
		gid := *ctx.GID

		var hasWrite bool

		// Owner permissions
		if uid == dirAttr.UID {
			hasWrite = (dirAttr.Mode & 0200) != 0 // Owner write bit
		} else if gid == dirAttr.GID || containsGID(ctx.GIDs, dirAttr.GID) {
			// Group permissions
			hasWrite = (dirAttr.Mode & 0020) != 0 // Group write bit
		} else {
			// Other permissions
			hasWrite = (dirAttr.Mode & 0002) != 0 // Other write bit
		}

		if !hasWrite {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "write permission denied on target directory",
			}
		}
	}

	// ========================================================================
	// Step 4: Verify name doesn't already exist
	// ========================================================================

	if r.children[dirKey] == nil {
		r.children[dirKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[dirKey][name]; exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("name already exists: %s", name),
		}
	}

	// ========================================================================
	// Step 5: Create the link
	// ========================================================================

	// Add the directory entry pointing to the existing file handle
	r.children[dirKey][name] = fileHandle

	// ========================================================================
	// Step 6: Update directory modification time
	// ========================================================================

	dirAttr.Mtime = time.Now()
	dirAttr.Ctime = time.Now()
	r.files[dirKey] = dirAttr

	// ========================================================================
	// Step 7: Update file change time (metadata changed)
	// ========================================================================

	fileAttr.Ctime = time.Now()
	r.files[fileKey] = fileAttr

	// Note: In a real implementation with link counting, you would also
	// increment fileAttr.Nlink here. However, in this handle-based system,
	// the link count is computed dynamically when needed.

	return nil
}

// CreateDirectory creates a new directory with the specified attributes.
//
// This implementation:
//  1. Verifies the parent directory exists and is a directory
//  2. Checks write access to the parent directory (if auth context provided)
//  3. Verifies the name doesn't already exist
//  4. Completes directory attributes with defaults (size, timestamps)
//  5. Creates the directory metadata
//  6. Links it to the parent directory
//  7. Updates parent directory timestamps
//
// Parameters:
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new directory
//   - attr: Partial attributes (type, mode, uid, gid) from protocol layer
//   - ctx: Authentication context for access control
//
// Returns:
//   - FileHandle: Handle of the newly created directory
//   - error: Returns error if access denied, name exists, or I/O error
func (r *MemoryRepository) CreateDirectory(parentHandle metadata.FileHandle, name string, attr *metadata.FileAttr, ctx *metadata.AuthContext) (metadata.FileHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// ========================================================================
	// Step 1: Verify parent directory exists
	// ========================================================================

	parentKey := handleToKey(parentHandle)
	parentAttr, exists := r.files[parentKey]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "parent directory not found",
		}
	}

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "parent is not a directory",
		}
	}

	// ========================================================================
	// Step 2: Check write access to parent directory (if auth context provided)
	// ========================================================================

	if ctx != nil && ctx.AuthFlavor != 0 && ctx.UID != nil {
		// Check if user has write permission on the parent directory
		uid := *ctx.UID
		gid := *ctx.GID

		var hasWrite bool

		// Owner permissions
		if uid == parentAttr.UID {
			hasWrite = (parentAttr.Mode & 0200) != 0 // Owner write bit
		} else if gid == parentAttr.GID || containsGID(ctx.GIDs, parentAttr.GID) {
			// Group permissions
			hasWrite = (parentAttr.Mode & 0020) != 0 // Group write bit
		} else {
			// Other permissions
			hasWrite = (parentAttr.Mode & 0002) != 0 // Other write bit
		}

		if !hasWrite {
			return nil, &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "write permission denied on parent directory",
			}
		}
	}

	// ========================================================================
	// Step 3: Verify name doesn't already exist
	// ========================================================================

	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[parentKey][name]; exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("directory already exists: %s", name),
		}
	}

	// ========================================================================
	// Step 4: Complete directory attributes with defaults
	// ========================================================================

	now := time.Now()

	// Start with the attributes provided by the protocol layer
	completeAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, // Always directory
		Mode: attr.Mode,                  // From protocol layer (or default 0755)
		UID:  attr.UID,                   // From protocol layer (or authenticated user)
		GID:  attr.GID,                   // From protocol layer (or authenticated group)

		// Server-assigned attributes
		Size:      4096, // Standard directory size
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Directories don't have content blobs
	}

	// ========================================================================
	// Step 5: Generate unique directory handle
	// ========================================================================

	dirHandle := r.generateFileHandle(name)

	// ========================================================================
	// Step 6: Create directory metadata
	// ========================================================================

	dirKey := handleToKey(dirHandle)
	r.files[dirKey] = completeAttr

	// Initialize empty children map for the new directory
	r.children[dirKey] = make(map[string]metadata.FileHandle)

	// ========================================================================
	// Step 7: Link directory to parent
	// ========================================================================

	r.children[parentKey][name] = dirHandle

	// Set parent relationship
	r.parents[dirKey] = parentHandle

	// ========================================================================
	// Step 8: Update parent directory timestamps
	// ========================================================================
	// The parent directory's mtime and ctime should be updated when a child
	// is added, as this modifies the directory's contents.

	parentAttr.Mtime = now
	parentAttr.Ctime = now
	r.files[parentKey] = parentAttr

	return dirHandle, nil
}

// CheckAccess performs Unix-style permission checking for file access.
// This implements the access control logic for the ACCESS NFS procedure.
//
// The check follows standard Unix permission semantics:
//  1. If AUTH_NULL, grant minimal permissions (read-only for world-readable files)
//  2. If owner (UID matches), check owner permission bits
//  3. If group member (GID or supplementary GID matches), check group bits
//  4. Otherwise, check other permission bits
//
// Returns a bitmap of granted permissions (subset of requested).
func (r *MemoryRepository) CheckAccess(handle metadata.FileHandle, requested uint32, ctx *metadata.AccessCheckContext) (uint32, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Get file attributes
	key := handleToKey(handle)
	attr, exists := r.files[key]
	if !exists {
		return 0, fmt.Errorf("file not found")
	}

	granted := uint32(0)

	// For AUTH_NULL, grant very limited permissions
	if ctx.AuthFlavor == 0 || ctx.UID == nil {
		// Only grant read/lookup if world-readable
		if attr.Mode&0004 != 0 { // Other read
			if requested&0x0001 != 0 { // AccessRead
				granted |= 0x0001
			}
		}
		if attr.Type == metadata.FileTypeDirectory && attr.Mode&0001 != 0 { // Other execute (search)
			if requested&0x0002 != 0 { // AccessLookup
				granted |= 0x0002
			}
		}
		return granted, nil
	}

	uid := *ctx.UID
	gid := *ctx.GID

	// Determine which permission bits apply
	var permBits uint32

	if uid == attr.UID {
		// Owner permissions (bits 6-8)
		permBits = (attr.Mode >> 6) & 0x7
	} else if gid == attr.GID || containsGID(ctx.GIDs, attr.GID) {
		// Group permissions (bits 3-5)
		permBits = (attr.Mode >> 3) & 0x7
	} else {
		// Other permissions (bits 0-2)
		permBits = attr.Mode & 0x7
	}

	// Map Unix permission bits to NFS access bits
	// permBits: rwx = 0x4 (read), 0x2 (write), 0x1 (execute)

	hasRead := (permBits & 0x4) != 0
	hasWrite := (permBits & 0x2) != 0
	hasExecute := (permBits & 0x1) != 0

	// For directories
	if attr.Type == metadata.FileTypeDirectory {
		if hasRead && (requested&0x0001 != 0) { // AccessRead
			granted |= 0x0001 // Can list directory
		}
		if hasExecute && (requested&0x0002 != 0) { // AccessLookup
			granted |= 0x0002 // Can search/lookup in directory
		}
		if hasWrite {
			if requested&0x0004 != 0 { // AccessModify
				granted |= 0x0004 // Can modify directory entries
			}
			if requested&0x0008 != 0 { // AccessExtend
				granted |= 0x0008 // Can add entries
			}
			if requested&0x0010 != 0 { // AccessDelete
				granted |= 0x0010 // Can delete entries
			}
		}
		if hasExecute && (requested&0x0020 != 0) { // AccessExecute
			granted |= 0x0020 // Can traverse directory
		}
	} else {
		// For regular files and other types
		if hasRead && (requested&0x0001 != 0) { // AccessRead
			granted |= 0x0001 // Can read file
		}
		if hasWrite {
			if requested&0x0004 != 0 { // AccessModify
				granted |= 0x0004 // Can modify file
			}
			if requested&0x0008 != 0 { // AccessExtend
				granted |= 0x0008 // Can extend file
			}
		}
		if hasExecute && (requested&0x0020 != 0) { // AccessExecute
			granted |= 0x0020 // Can execute file
		}

		// Delete permission requires write access to parent directory
		// We can't check that here without parent context, so we grant it
		// if the user has write permission on the file itself
		// The actual delete operation will do proper parent directory checks
		if hasWrite && (requested&0x0010 != 0) { // AccessDelete
			granted |= 0x0010
		}
	}

	return granted, nil
}

// GetPathConf returns POSIX-compatible filesystem information and properties.
func (r *MemoryRepository) GetPathConf(handle metadata.FileHandle) (*metadata.PathConf, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Verify handle exists
	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return nil, fmt.Errorf("file not found")
	}

	// Return filesystem PATHCONF properties
	// These are reasonable defaults that match POSIX-compliant filesystems
	return &metadata.PathConf{
		Linkmax:         32767, // Max hard links (typical for ext4)
		NameMax:         255,   // Max filename length in bytes
		NoTrunc:         true,  // Reject names that are too long
		ChownRestricted: true,  // Only root can chown (POSIX standard)
		CaseInsensitive: false, // Case-sensitive filenames (Unix standard)
		CasePreserving:  true,  // Preserve filename case
	}, nil
}

// CreateSpecialFile creates a special file (device, socket, or FIFO).
//
// This implementation:
//  1. Verifies the parent directory exists and is a directory
//  2. Checks write permission on the parent directory (if auth context provided)
//  3. Checks privilege requirements for device creation (root-only)
//  4. Verifies the name doesn't already exist
//  5. Completes file attributes with defaults (size=0, timestamps)
//  6. Creates the special file metadata
//  7. Stores device numbers in implementation-specific manner
//  8. Links it to the parent directory
//  9. Updates parent directory timestamps
//
// Parameters:
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new special file
//   - attr: Partial attributes (type, mode, uid, gid) from protocol layer
//   - majorDevice: Major device number (for block/char devices, 0 otherwise)
//   - minorDevice: Minor device number (for block/char devices, 0 otherwise)
//   - ctx: Authentication context for access control
//
// Returns:
//   - FileHandle: Handle of the newly created special file
//   - error: Returns error if access denied, name exists, or I/O error
func (r *MemoryRepository) CreateSpecialFile(
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
	majorDevice uint32,
	minorDevice uint32,
	ctx *metadata.AuthContext,
) (metadata.FileHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// ========================================================================
	// Step 1: Verify parent directory exists
	// ========================================================================

	parentKey := handleToKey(parentHandle)
	parentAttr, exists := r.files[parentKey]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "parent directory not found",
		}
	}

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "parent is not a directory",
		}
	}

	// ========================================================================
	// Step 2: Check write access to parent directory (if auth context provided)
	// ========================================================================

	if ctx != nil && ctx.AuthFlavor != 0 && ctx.UID != nil {
		// Check if user has write permission on the parent directory
		uid := *ctx.UID
		gid := *ctx.GID

		var hasWrite bool

		// Owner permissions
		if uid == parentAttr.UID {
			hasWrite = (parentAttr.Mode & 0200) != 0 // Owner write bit
		} else if gid == parentAttr.GID || containsGID(ctx.GIDs, parentAttr.GID) {
			// Group permissions
			hasWrite = (parentAttr.Mode & 0020) != 0 // Group write bit
		} else {
			// Other permissions
			hasWrite = (parentAttr.Mode & 0002) != 0 // Other write bit
		}

		if !hasWrite {
			return nil, &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "write permission denied on parent directory",
			}
		}
	}

	// ========================================================================
	// Step 3: Check privilege requirements for device creation
	// ========================================================================
	// Device files (character and block devices) typically require root
	// privileges to create. This is a security measure to prevent
	// unauthorized hardware access.

	if attr.Type == metadata.FileTypeChar || attr.Type == metadata.FileTypeBlock {
		// Check if user has sufficient privileges (root or CAP_MKNOD)
		if ctx != nil && ctx.UID != nil && *ctx.UID != 0 {
			// Non-root user attempting to create a device file
			return nil, &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "device file creation requires root privileges",
			}
		}
	}

	// ========================================================================
	// Step 4: Verify name doesn't already exist
	// ========================================================================

	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[parentKey][name]; exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("file already exists: %s", name),
		}
	}

	// ========================================================================
	// Step 5: Complete file attributes with defaults
	// ========================================================================

	now := time.Now()

	// Start with the attributes provided by the protocol layer
	completeAttr := &metadata.FileAttr{
		Type: attr.Type, // FileTypeChar, FileTypeBlock, FileTypeSocket, or FileTypeFifo
		Mode: attr.Mode, // From protocol layer (or default)
		UID:  attr.UID,  // From protocol layer (or authenticated user)
		GID:  attr.GID,  // From protocol layer (or authenticated group)

		// Server-assigned attributes
		Size:      0, // Special files have no content
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Special files don't have content blobs
	}

	// Apply default mode if not specified
	if completeAttr.Mode == 0 {
		completeAttr.Mode = 0644 // Default: rw-r--r--
	}

	// ========================================================================
	// Step 6: Store device numbers (implementation-specific)
	// ========================================================================
	// In this in-memory implementation, we'll store device numbers in the
	// SymlinkTarget field as a formatted string. In a real implementation,
	// you would store this in a proper device-specific field.
	//
	// Note: This is a demonstration of how to handle device numbers.
	// A production implementation should use a proper storage mechanism.

	if attr.Type == metadata.FileTypeChar || attr.Type == metadata.FileTypeBlock {
		// Store device numbers as a string in the format "major:minor"
		// This is just for demonstration - a real implementation would
		// store these properly in the filesystem metadata
		completeAttr.SymlinkTarget = fmt.Sprintf("device:%d:%d", majorDevice, minorDevice)
	}

	// ========================================================================
	// Step 7: Generate unique file handle
	// ========================================================================

	fileHandle := r.generateFileHandle(name)

	// ========================================================================
	// Step 8: Create special file metadata
	// ========================================================================

	fileKey := handleToKey(fileHandle)
	r.files[fileKey] = completeAttr

	// ========================================================================
	// Step 9: Link special file to parent
	// ========================================================================

	r.children[parentKey][name] = fileHandle

	// Set parent relationship
	r.parents[fileKey] = parentHandle

	// ========================================================================
	// Step 10: Update parent directory timestamps
	// ========================================================================
	// The parent directory's mtime and ctime should be updated when a child
	// is added, as this modifies the directory's contents.

	parentAttr.Mtime = now
	parentAttr.Ctime = now
	r.files[parentKey] = parentAttr

	return fileHandle, nil
}

// ReadDir reads directory entries with pagination support.
//
// This implementation:
//  1. Verifies the directory handle exists and is a directory
//  2. Checks read/execute permission on the directory (if auth context provided)
//  3. Builds "." entry (cookie 1)
//  4. Builds ".." entry (cookie 2)
//  5. Iterates through regular children (cookies 3+)
//  6. Handles cookie-based pagination
//  7. Respects count limits (approximate)
//
// Cookie semantics:
//   - 0: Start of directory
//   - 1: After "." entry
//   - 2: After ".." entry
//   - 3+: After regular entries (one cookie per entry)
//
// The count parameter is used as a hint to limit response size. The actual
// number of entries returned may be more or less than would fit in count bytes.
//
// Parameters:
//   - dirHandle: Directory to read
//   - cookie: Starting position (0 = beginning)
//   - count: Maximum response size in bytes (approximate)
//   - ctx: Authentication context for access control
//
// Returns:
//   - []DirEntry: List of entries starting from cookie
//   - bool: EOF flag (true if all entries returned)
//   - error: Access denied or I/O errors
func (r *MemoryRepository) ReadDir(dirHandle metadata.FileHandle, cookie uint64, count uint32, ctx *metadata.AuthContext) ([]metadata.DirEntry, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// ========================================================================
	// Step 1: Verify directory exists and is a directory
	// ========================================================================

	dirKey := handleToKey(dirHandle)
	dirAttr, exists := r.files[dirKey]
	if !exists {
		return nil, false, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "directory not found",
		}
	}

	// Verify it's actually a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		return nil, false, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "not a directory",
		}
	}

	// ========================================================================
	// Step 2: Check read/execute permission on directory (if auth provided)
	// ========================================================================
	// Execute (search) permission is required to read directory contents
	// Read permission is required to list the directory

	if ctx != nil && ctx.AuthFlavor != 0 && ctx.UID != nil {
		uid := *ctx.UID
		gid := *ctx.GID

		var hasRead, hasExecute bool

		// Owner permissions
		if uid == dirAttr.UID {
			hasRead = (dirAttr.Mode & 0400) != 0    // Owner read bit
			hasExecute = (dirAttr.Mode & 0100) != 0 // Owner execute bit
		} else if gid == dirAttr.GID || containsGID(ctx.GIDs, dirAttr.GID) {
			// Group permissions
			hasRead = (dirAttr.Mode & 0040) != 0    // Group read bit
			hasExecute = (dirAttr.Mode & 0010) != 0 // Group execute bit
		} else {
			// Other permissions
			hasRead = (dirAttr.Mode & 0004) != 0    // Other read bit
			hasExecute = (dirAttr.Mode & 0001) != 0 // Other execute bit
		}

		// Need both read and execute to list directory
		if !hasRead || !hasExecute {
			return nil, false, &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "read/execute permission denied on directory",
			}
		}
	}

	// ========================================================================
	// Step 3: Build entries list with pagination
	// ========================================================================

	entries := make([]metadata.DirEntry, 0)
	currentCookie := uint64(1)

	// Track estimated size incrementally as we add entries
	// XDR encoding overhead per entry:
	//   4 bytes (value_follows) + 8 bytes (fileid) +
	//   4 bytes (name length) + name bytes + padding (0-3 bytes) + 8 bytes (cookie)
	//   = 24 bytes + name length + padding
	estimatedSize := uint32(0)

	// Reserve space for response overhead (status, attrs, verifier, eof, end marker)
	const responseOverhead = 200
	estimatedSize += responseOverhead

	// Extract directory file ID for "." entry
	dirFileid := extractFileIDFromHandle(dirHandle)

	// ========================================================================
	// Add "." entry (cookie 1)
	// ========================================================================

	if cookie == 0 {
		entry := metadata.DirEntry{
			Fileid: dirFileid,
			Name:   ".",
			Cookie: currentCookie,
		}

		// Calculate size for this entry
		nameLen := len(entry.Name)
		padding := (4 - (nameLen % 4)) % 4
		entrySize := 24 + uint32(nameLen) + uint32(padding)

		entries = append(entries, entry)
		estimatedSize += entrySize
	}
	currentCookie++

	// ========================================================================
	// Add ".." entry (cookie 2)
	// ========================================================================

	if cookie <= 1 {
		// Get parent file ID
		parentFileid := dirFileid // Default to self if no parent
		if parentHandle, err := r.GetParent(dirHandle); err == nil {
			parentFileid = extractFileIDFromHandle(parentHandle)
		}

		entry := metadata.DirEntry{
			Fileid: parentFileid,
			Name:   "..",
			Cookie: currentCookie,
		}

		// Calculate size for this entry
		nameLen := len(entry.Name)
		padding := (4 - (nameLen % 4)) % 4
		entrySize := 24 + uint32(nameLen) + uint32(padding)

		entries = append(entries, entry)
		estimatedSize += entrySize
	}
	currentCookie++

	// ========================================================================
	// Add regular entries (cookies 3+)
	// ========================================================================

	// Get all children
	children := r.children[dirKey]
	if children != nil {
		// We need a stable ordering for pagination to work correctly
		// Sort names alphabetically for consistent iteration
		names := make([]string, 0, len(children))
		for name := range children {
			names = append(names, name)
		}

		// Sort for stable ordering (O(n log n) with optimized quicksort)
		slices.Sort(names)

		// Iterate through children, skipping entries before cookie
		for _, name := range names {
			handle := children[name]

			// Skip entries before the requested cookie
			if currentCookie <= cookie {
				currentCookie++
				continue
			}

			// Calculate size for this entry BEFORE adding it
			nameLen := len(name)
			padding := (4 - (nameLen % 4)) % 4
			entrySize := 24 + uint32(nameLen) + uint32(padding)

			// Check if adding this entry would exceed the count limit
			if estimatedSize+entrySize > count {
				// We've reached the count limit, but haven't seen all entries
				// Return what we have so far (EOF = false)
				return entries, false, nil
			}

			// Extract file ID from handle
			fileid := extractFileIDFromHandle(handle)

			entry := metadata.DirEntry{
				Fileid: fileid,
				Name:   name,
				Cookie: currentCookie,
			}

			// Add entry and increment size (O(1) operation)
			entries = append(entries, entry)
			estimatedSize += entrySize

			currentCookie++
		}
	}

	// ========================================================================
	// Step 4: Return results with EOF flag
	// ========================================================================

	// We've returned all entries - EOF = true
	return entries, true, nil
}

// extractFileIDFromHandle extracts a file ID from a handle.
// Uses the first 8 bytes as the file ID.
func extractFileIDFromHandle(handle metadata.FileHandle) uint64 {
	if len(handle) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(handle[:8])
}

// sortStrings performs an in-place sort of a string slice.
// This provides stable ordering for directory entries.
func sortStrings(slice []string) {
	// Simple insertion sort - good enough for most directories
	for i := 1; i < len(slice); i++ {
		key := slice[i]
		j := i - 1
		for j >= 0 && slice[j] > key {
			slice[j+1] = slice[j]
			j--
		}
		slice[j+1] = key
	}
}

// Helper function to check if a GID is in a list
func containsGID(gids []uint32, target uint32) bool {
	return slices.Contains(gids, target)
}
