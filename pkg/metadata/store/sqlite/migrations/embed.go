package migrations

import "embed"

// FS contains the embedded SQLite migration files.
//
//go:embed *.sql
var FS embed.FS
