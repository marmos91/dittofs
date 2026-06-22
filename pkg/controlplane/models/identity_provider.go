package models

import (
	"errors"
	"time"
)

// Identity provider type discriminators. These are the {type} path segment of
// the /api/v1/identity-providers/{type}/... routes and the primary key of the
// IdentityProviderConfig table.
const (
	IdentityProviderTypeLDAP     = "ldap"
	IdentityProviderTypeKerberos = "kerberos"
)

// IdentityProviderConfig persists the configuration of a single identity
// provider (LDAP/AD or Kerberos) in the control-plane database so it can be
// managed over the REST API without editing config files or restarting the
// server.
//
// Config holds the JSON-serialized domain configuration (ldap.Config or
// config.KerberosConfig) INCLUDING secret material (bind password / keytab
// path). It is serialized with the secret intact — the redacting marshaler on
// the domain types is deliberately bypassed for storage — so the persisted
// config is usable after a restart. The column is never returned over the API:
// the REST layer marshals a redacted DTO instead, so `json:"-"` here guards
// against accidental serialization. Secrets are stored in plaintext, matching
// the existing posture for block-store/S3 credentials in this same database.
type IdentityProviderConfig struct {
	// Type is the provider discriminator: "ldap" or "kerberos". Primary key —
	// there is exactly one configuration row per provider type.
	Type string `gorm:"column:type;primaryKey;type:varchar(32)" json:"type"`

	// Enabled mirrors the domain config's Enabled flag, surfaced as a column so
	// the list endpoint can report enabled state without deserializing Config.
	Enabled bool `gorm:"not null" json:"enabled"`

	// Config is the JSON-serialized domain configuration including secrets.
	// Never serialized to the API (see type doc).
	Config string `gorm:"type:text" json:"-"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for IdentityProviderConfig.
func (IdentityProviderConfig) TableName() string {
	return "identity_provider_configs"
}

var (
	// ErrIdentityProviderConfigNotFound is returned when no configuration row
	// exists for a provider type.
	ErrIdentityProviderConfigNotFound = errors.New("identity provider config not found")
)
