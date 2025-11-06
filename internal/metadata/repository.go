package metadata

// Interface for NFS metadata persistence
type Repository interface {
	// Export operations
	AddExport(path string, options ExportOptions, rootAttr *FileAttr) error
	GetExports() ([]Export, error)
	FindExport(path string) (*Export, error)
	DeleteExport(path string) error

	// GetRootHandle returns the root file handle for an export path
	GetRootHandle(exportPath string) (FileHandle, error)

	// File operations
	CreateFile(handle FileHandle, attr *FileAttr) error
	GetFile(handle FileHandle) (*FileAttr, error)
	UpdateFile(handle FileHandle, attr *FileAttr) error
	DeleteFile(handle FileHandle) error

	// Directory hierarchy operations
	SetParent(child FileHandle, parent FileHandle) error
	GetParent(child FileHandle) (FileHandle, error)

	AddChild(parent FileHandle, name string, child FileHandle) error
	GetChild(parent FileHandle, name string) (FileHandle, error)
	GetChildren(parent FileHandle) (map[string]FileHandle, error)
	DeleteChild(parent FileHandle, name string) error

	// Helper method to add files/directories to a parent
	AddFileToDirectory(parentHandle FileHandle, name string, attr *FileAttr) (FileHandle, error)

	// Mount tracking operations
	// RecordMount records an active mount by a client
	RecordMount(exportPath string, clientAddr string, authFlavor uint32, machineName string, uid *uint32, gid *uint32) error

	// RemoveMount removes a mount record when a client unmounts
	RemoveMount(exportPath string, clientAddr string) error

	// GetMounts returns all active mounts, optionally filtered by export path
	// If exportPath is empty, returns all mounts
	GetMounts(exportPath string) ([]MountEntry, error)

	// IsClientMounted checks if a specific client has an active mount
	IsClientMounted(exportPath string, clientAddr string) (bool, error)

	// Access control operations
	// CheckExportAccess verifies if a client can access an export
	// Returns an AccessDecision with details about the authorization
	CheckExportAccess(exportPath string, clientAddr string, authFlavor uint32) (*AccessDecision, error)

	// GetMountsByClient returns all active mounts for a specific client
	// Used by UMNTALL to determine what will be removed
	GetMountsByClient(clientAddr string) ([]MountEntry, error)

	// RemoveAllMounts removes all mount records for a specific client
	// Used by UMNTALL to clean up all mounts in one operation
	RemoveAllMounts(clientAddr string) error

	// ServerConfig operations
	// SetServerConfig sets the server-wide configuration
	SetServerConfig(config ServerConfig) error

	// GetServerConfig returns the current server configuration
	GetServerConfig() (ServerConfig, error)

	// CheckDumpAccess verifies if a client can call the DUMP procedure
	// Returns error if access is denied, nil if allowed
	CheckDumpAccess(clientAddr string) error

	// GetFSInfo returns the static filesystem information and capabilities
	// This is used by the FSINFO NFS procedure to inform clients about
	// server limits and preferences (max transfer sizes, properties, etc.)
	GetFSInfo(handle FileHandle) (*FSInfo, error)
}
