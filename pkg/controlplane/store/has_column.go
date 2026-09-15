package store

import (
	"slices"

	"gorm.io/gorm"
)

// hasColumn reports whether a table already carries a column, matching the
// name in full.
//
// The SQLite migrator answers HasColumn with a LIKE over the stored CREATE
// TABLE text, so a name that is a suffix of a wider one — "block_store_id"
// against "local_block_store_id" — reports present on a table that does not
// have it. A rename keyed on that answer refuses an upgrade it should have
// performed, and refuses it only on SQLite: the Postgres migrator matches
// information_schema exactly. Reading the column list back and comparing whole
// names behaves the same on both.
//
// A table that does not exist yet reports no columns, which is the answer a
// fresh install needs: nothing to migrate, and AutoMigrate creates it.
func hasColumn(db *gorm.DB, model any, name string) bool {
	cols, err := db.Migrator().ColumnTypes(model)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(cols, func(c gorm.ColumnType) bool { return c.Name() == name })
}
