package models

// AllModels returns all GORM models for auto-migration.
func AllModels() []any {
	return []any{
		&User{},
		&Group{},
		&MetadataStoreConfig{},
		&PayloadStoreConfig{},
		&Share{},
		&ShareAccessRule{},
		&UserSharePermission{},
		&GroupSharePermission{},
		&AdapterConfig{},
		&Setting{},
		&IdentityMapping{},
		&NFSAdapterSettings{},
		&SMBAdapterSettings{},
		&Netgroup{},
		&NetgroupMember{},
	}
}
