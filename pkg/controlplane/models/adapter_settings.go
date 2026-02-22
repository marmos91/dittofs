package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// parseBlockedOps deserializes a JSON-encoded blocked operations string into a string slice.
// Returns nil for empty, "null", or invalid JSON.
func parseBlockedOps(raw string) []string {
	if raw == "" || raw == "null" {
		return nil
	}
	var ops []string
	if err := json.Unmarshal([]byte(raw), &ops); err != nil {
		return nil
	}
	return ops
}

// marshalBlockedOps serializes a string slice into a JSON string for storage.
// Returns an empty string for nil or empty slices.
func marshalBlockedOps(ops []string) string {
	if len(ops) == 0 {
		return ""
	}
	data, err := json.Marshal(ops)
	if err != nil {
		return ""
	}
	return string(data)
}

// NFSAdapterSettings stores NFSv4-specific adapter settings.
// Each NFS adapter has exactly one settings record (1:1 relationship).
type NFSAdapterSettings struct {
	ID        string `gorm:"primaryKey;size:36" json:"id"`
	AdapterID string `gorm:"uniqueIndex;not null;size:36" json:"adapter_id"`

	// Version negotiation
	MinVersion string `gorm:"default:3;size:10" json:"min_version"`
	MaxVersion string `gorm:"default:4.0;size:10" json:"max_version"`

	// Timeouts (seconds)
	LeaseTime               int `gorm:"default:90" json:"lease_time"`
	GracePeriod             int `gorm:"default:90" json:"grace_period"`
	DelegationRecallTimeout int `gorm:"default:90" json:"delegation_recall_timeout"`
	CallbackTimeout         int `gorm:"default:5" json:"callback_timeout"`
	LeaseBreakTimeout       int `gorm:"default:35" json:"lease_break_timeout"`

	// Connection limits
	MaxConnections int `gorm:"default:0" json:"max_connections"` // 0 = unlimited
	MaxClients     int `gorm:"default:10000" json:"max_clients"`
	MaxCompoundOps int `gorm:"default:50" json:"max_compound_ops"`

	// Transport tuning
	MaxReadSize           int `gorm:"default:1048576" json:"max_read_size"`           // 1MB
	MaxWriteSize          int `gorm:"default:1048576" json:"max_write_size"`          // 1MB
	PreferredTransferSize int `gorm:"default:1048576" json:"preferred_transfer_size"` // 1MB

	// Delegation policy
	DelegationsEnabled    bool `gorm:"default:true" json:"delegations_enabled"`
	MaxDelegations        int  `gorm:"default:10000" json:"max_delegations"`        // max total outstanding delegations (file + directory combined)
	DirDelegBatchWindowMs int  `gorm:"default:50" json:"dir_deleg_batch_window_ms"` // notification batching window in milliseconds

	// NFSv4 minor version range (0=v4.0, 1=v4.1)
	V4MinMinorVersion int `gorm:"default:0" json:"v4_min_minor_version"` // minimum NFSv4 minor version
	V4MaxMinorVersion int `gorm:"default:1" json:"v4_max_minor_version"` // maximum NFSv4 minor version

	// TODO: Wire NFSv4.1 session limits into StateManager (these fields are not yet active; future: settings watcher).
	V4MaxSessionSlots          int `gorm:"default:64" json:"v4_max_session_slots"`           // fore channel max slots per session
	V4MaxSessionsPerClient     int `gorm:"default:16" json:"v4_max_sessions_per_client"`     // max sessions per client
	V4MaxConnectionsPerSession int `gorm:"default:16" json:"v4_max_connections_per_session"` // max connections per session (0=unlimited)

	// Operation blocklist (JSON array stored as text)
	BlockedOperations string `gorm:"type:text" json:"-"`

	// Portmapper settings
	PortmapperEnabled bool `gorm:"default:false" json:"portmapper_enabled"`
	PortmapperPort    int  `gorm:"default:10111" json:"portmapper_port"`

	// Version counter for change detection (monotonic, starts at 1, incremented on every update)
	Version int `gorm:"default:1" json:"version"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for NFSAdapterSettings.
func (NFSAdapterSettings) TableName() string {
	return "nfs_adapter_settings"
}

// GetBlockedOperations returns the blocked operations as a string slice.
func (s *NFSAdapterSettings) GetBlockedOperations() []string {
	return parseBlockedOps(s.BlockedOperations)
}

// SetBlockedOperations serializes the blocked operations from a string slice.
func (s *NFSAdapterSettings) SetBlockedOperations(ops []string) {
	s.BlockedOperations = marshalBlockedOps(ops)
}

// GetV4MinMinorVersion returns the minimum NFSv4 minor version as uint32.
// Defaults to 0 (v4.0) for negative values, which is the lowest supported version.
func (s *NFSAdapterSettings) GetV4MinMinorVersion() uint32 {
	if s.V4MinMinorVersion < 0 {
		return 0
	}
	return uint32(s.V4MinMinorVersion)
}

// GetV4MaxMinorVersion returns the maximum NFSv4 minor version as uint32.
// Defaults to 1 (v4.1) for negative values, which is the highest supported version.
func (s *NFSAdapterSettings) GetV4MaxMinorVersion() uint32 {
	if s.V4MaxMinorVersion < 0 {
		return 1
	}
	return uint32(s.V4MaxMinorVersion)
}

// SMBAdapterSettings stores SMB-specific adapter settings.
// Each SMB adapter has exactly one settings record (1:1 relationship).
type SMBAdapterSettings struct {
	ID        string `gorm:"primaryKey;size:36" json:"id"`
	AdapterID string `gorm:"uniqueIndex;not null;size:36" json:"adapter_id"`

	// Dialect negotiation
	MinDialect string `gorm:"default:SMB2.0;size:20" json:"min_dialect"`
	MaxDialect string `gorm:"default:SMB3.1.1;size:20" json:"max_dialect"`

	// Timeouts (seconds)
	SessionTimeout     int `gorm:"default:900" json:"session_timeout"`     // 15 minutes
	OplockBreakTimeout int `gorm:"default:35" json:"oplock_break_timeout"` // seconds

	// Connection limits
	MaxConnections int `gorm:"default:0" json:"max_connections"` // 0 = unlimited
	MaxSessions    int `gorm:"default:10000" json:"max_sessions"`

	// Encryption (stub)
	EnableEncryption bool `gorm:"default:false" json:"enable_encryption"`

	// Operation blocklist (JSON array stored as text)
	BlockedOperations string `gorm:"type:text" json:"-"`

	// Version counter for change detection (monotonic, starts at 1, incremented on every update)
	Version int `gorm:"default:1" json:"version"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for SMBAdapterSettings.
func (SMBAdapterSettings) TableName() string {
	return "smb_adapter_settings"
}

// GetBlockedOperations returns the blocked operations as a string slice.
func (s *SMBAdapterSettings) GetBlockedOperations() []string {
	return parseBlockedOps(s.BlockedOperations)
}

// SetBlockedOperations serializes the blocked operations from a string slice.
func (s *SMBAdapterSettings) SetBlockedOperations(ops []string) {
	s.BlockedOperations = marshalBlockedOps(ops)
}

// NewDefaultNFSSettings creates an NFSAdapterSettings with all default values.
func NewDefaultNFSSettings(adapterID string) *NFSAdapterSettings {
	return &NFSAdapterSettings{
		ID:                         uuid.New().String(),
		AdapterID:                  adapterID,
		MinVersion:                 "3",
		MaxVersion:                 "4.0",
		LeaseTime:                  90,
		GracePeriod:                90,
		DelegationRecallTimeout:    90,
		CallbackTimeout:            5,
		LeaseBreakTimeout:          35,
		MaxConnections:             0,
		MaxClients:                 10000,
		MaxCompoundOps:             50,
		MaxReadSize:                1048576,
		MaxWriteSize:               1048576,
		PreferredTransferSize:      1048576,
		DelegationsEnabled:         true,
		MaxDelegations:             10000,
		DirDelegBatchWindowMs:      50,
		V4MinMinorVersion:          0,
		V4MaxMinorVersion:          1,
		V4MaxSessionSlots:          64,
		V4MaxSessionsPerClient:     16,
		V4MaxConnectionsPerSession: 16,
		PortmapperEnabled:          false,
		PortmapperPort:             10111,
		Version:                    1,
	}
}

// NewDefaultSMBSettings creates an SMBAdapterSettings with all default values.
func NewDefaultSMBSettings(adapterID string) *SMBAdapterSettings {
	return &SMBAdapterSettings{
		ID:                 uuid.New().String(),
		AdapterID:          adapterID,
		MinDialect:         "SMB2.0",
		MaxDialect:         "SMB3.1.1",
		SessionTimeout:     900,
		OplockBreakTimeout: 35,
		MaxConnections:     0,
		MaxSessions:        10000,
		EnableEncryption:   false,
		Version:            1,
	}
}

// NFSSettingsValidRange defines valid ranges for NFS adapter settings.
type NFSSettingsValidRange struct {
	LeaseTimeMin               int
	LeaseTimeMax               int
	GracePeriodMin             int
	GracePeriodMax             int
	DelegationRecallTimeoutMin int
	DelegationRecallTimeoutMax int
	CallbackTimeoutMin         int
	CallbackTimeoutMax         int
	LeaseBreakTimeoutMin       int
	LeaseBreakTimeoutMax       int
	MaxConnectionsMin          int
	MaxConnectionsMax          int
	MaxClientsMin              int
	MaxClientsMax              int
	MaxCompoundOpsMin          int
	MaxCompoundOpsMax          int
	MaxReadSizeMin             int
	MaxReadSizeMax             int
	MaxWriteSizeMin            int
	MaxWriteSizeMax            int
	PreferredTransferSizeMin   int
	PreferredTransferSizeMax   int
	PortmapperPortMin          int
	PortmapperPortMax          int
}

// DefaultNFSSettingsValidRange returns the default valid ranges for NFS settings.
func DefaultNFSSettingsValidRange() NFSSettingsValidRange {
	return NFSSettingsValidRange{
		LeaseTimeMin:               10,
		LeaseTimeMax:               3600,
		GracePeriodMin:             10,
		GracePeriodMax:             3600,
		DelegationRecallTimeoutMin: 10,
		DelegationRecallTimeoutMax: 600,
		CallbackTimeoutMin:         1,
		CallbackTimeoutMax:         60,
		LeaseBreakTimeoutMin:       5,
		LeaseBreakTimeoutMax:       120,
		MaxConnectionsMin:          0,
		MaxConnectionsMax:          100000,
		MaxClientsMin:              1,
		MaxClientsMax:              1000000,
		MaxCompoundOpsMin:          4,
		MaxCompoundOpsMax:          1000,
		MaxReadSizeMin:             4096,
		MaxReadSizeMax:             16777216, // 16MB
		MaxWriteSizeMin:            4096,
		MaxWriteSizeMax:            16777216, // 16MB
		PreferredTransferSizeMin:   4096,
		PreferredTransferSizeMax:   16777216, // 16MB
		PortmapperPortMin:          1,
		PortmapperPortMax:          65535,
	}
}

// SMBSettingsValidRange defines valid ranges for SMB adapter settings.
type SMBSettingsValidRange struct {
	SessionTimeoutMin     int
	SessionTimeoutMax     int
	OplockBreakTimeoutMin int
	OplockBreakTimeoutMax int
	MaxConnectionsMin     int
	MaxConnectionsMax     int
	MaxSessionsMin        int
	MaxSessionsMax        int
}

// DefaultSMBSettingsValidRange returns the default valid ranges for SMB settings.
func DefaultSMBSettingsValidRange() SMBSettingsValidRange {
	return SMBSettingsValidRange{
		SessionTimeoutMin:     60,
		SessionTimeoutMax:     86400, // 24 hours
		OplockBreakTimeoutMin: 5,
		OplockBreakTimeoutMax: 120,
		MaxConnectionsMin:     0,
		MaxConnectionsMax:     100000,
		MaxSessionsMin:        1,
		MaxSessionsMax:        1000000,
	}
}

// ValidNFSVersions lists supported NFS version strings.
var ValidNFSVersions = []string{"3", "4.0"}

// ValidSMBDialects lists supported SMB dialect strings.
var ValidSMBDialects = []string{"SMB2.0", "SMB2.1", "SMB3.0", "SMB3.1.1"}

// ValidKerberosLevels lists valid Kerberos authentication levels.
var ValidKerberosLevels = []string{"krb5", "krb5i", "krb5p"}
