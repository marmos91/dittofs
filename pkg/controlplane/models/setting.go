package models

import "time"

// Setting stores system-wide key-value settings.
type Setting struct {
	Key       string    `gorm:"primaryKey;size:255" json:"key"`
	Value     string    `gorm:"type:text" json:"value"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for Setting.
func (Setting) TableName() string {
	return "settings"
}
