package persistence

import (
	"crypto/sha256"
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

// Helper function to check if a GID is in a list
func containsGID(gids []uint32, target uint32) bool {
	return slices.Contains(gids, target)
}
