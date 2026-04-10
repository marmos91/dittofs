package models

import (
	"errors"
	"time"
)

// IdentityMapping maps an authentication principal to a control plane username.
// This is used for resolving Kerberos principals (e.g., "alice@EXAMPLE.COM")
// or NTLM principals (e.g., "CORP\alice") to local DittoFS user accounts.
// Mappings are shared across protocols (NFS and SMB) to ensure consistent
// uid/gid resolution in mixed-protocol deployments.
type IdentityMapping struct {
	ID        string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	Principal string    `gorm:"uniqueIndex;type:varchar(255);not null" json:"principal"`
	Username  string    `gorm:"type:varchar(255);not null" json:"username"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Error types for identity mapping operations.
var (
	ErrMappingNotFound  = errors.New("identity mapping not found")
	ErrDuplicateMapping = errors.New("identity mapping already exists")
)
