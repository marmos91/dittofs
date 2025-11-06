package persistence

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net"
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

// CheckExportAccess verifies if a client can access an export
func (r *MemoryRepository) CheckExportAccess(exportPath string, clientAddr string, authFlavor uint32) (*metadata.AccessDecision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Check if export exists
	ed, exists := r.exports[exportPath]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("export not found: %s", exportPath),
			Export:  exportPath,
		}
	}

	opts := ed.Export.Options

	// Check authentication requirements
	if opts.RequireAuth && authFlavor == 0 {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrAuthRequired,
			Message: "authentication required for this export",
			Export:  exportPath,
		}
	}

	// Check if auth flavor is allowed
	if len(opts.AllowedAuthFlavors) > 0 {
		allowed := false
		for _, flavor := range opts.AllowedAuthFlavors {
			if flavor == authFlavor {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, &metadata.ExportError{
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
				return nil, &metadata.ExportError{
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
			return nil, &metadata.ExportError{
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

	// Access granted
	return &metadata.AccessDecision{
		Allowed:     true,
		Reason:      "access granted",
		AllowedAuth: allowedAuth,
		ReadOnly:    opts.ReadOnly,
	}, nil
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
