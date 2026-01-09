package migrations

import "embed"

// FS contains the embedded migration files
//
//go:embed *.sql
var FS embed.FS
