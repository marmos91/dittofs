package models

// AllModels returns all GORM models for auto-migration.
func AllModels() []any {
	return []any{
		&User{},
		&Group{},
		&MetadataStoreConfig{},
		&BlockStoreConfig{},
		&Share{},
		&Quota{},
		&UserGrace{},
		&Snapshot{},
		&SnapshotPolicy{},
		&RestoreMarker{},
		&ShareAccessRule{},
		&ShareAdapterConfig{},
		&UserSharePermission{},
		&GroupSharePermission{},
		&AdapterConfig{},
		&Setting{},
		&IdentityMapping{},
		&SIDMapping{},
		&NFSAdapterSettings{},
		&SMBAdapterSettings{},
		&Netgroup{},
		&NetgroupMember{},
	}
}
