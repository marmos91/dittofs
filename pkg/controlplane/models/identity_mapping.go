package models

import (
	"errors"
	"time"
)

// IdentityMapping links an external identity to a DittoFS user account.
// The composite key (ProviderName, Principal) supports multiple identity
// providers (Kerberos, OIDC, AD, LDAP) and linked identities where one
// DittoFS user has credentials from different external systems.
type IdentityMapping struct {
	ID           string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	ProviderName string    `gorm:"uniqueIndex:idx_provider_principal;type:varchar(50);not null;default:kerberos" json:"provider_name"`
	Principal    string    `gorm:"uniqueIndex:idx_provider_principal;type:varchar(255);not null" json:"principal"`
	Username     string    `gorm:"type:varchar(255);not null" json:"username"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Error types for identity mapping operations.
var (
	ErrMappingNotFound  = errors.New("identity mapping not found")
	ErrDuplicateMapping = errors.New("identity mapping already exists")
)
