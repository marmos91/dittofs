package models

import (
	"errors"
	"time"
)

// IdentityMapping maps an NFSv4 principal to a control plane username.
// This is used for resolving Kerberos principals (e.g., "alice@EXAMPLE.COM")
// to local DittoFS user accounts.
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
