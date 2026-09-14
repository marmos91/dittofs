package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// ErrBlockStoreNameCollision reports two block store rows that shared a name
// under the old (name, kind) uniqueness and cannot both survive it being
// dropped.
var ErrBlockStoreNameCollision = errors.New("two block stores share a name")

// checkBlockStoreNameCollisions refuses an upgrade whose block store names stop
// being unique once the kind column is dropped.
//
// Renaming one row would change a name the operator's scripts use, failing them
// at some later point instead of now; deleting one would silently orphan any
// share that referenced it. Refusing costs a restart and loses nothing.
//
// A database with no kind column has already been migrated and is skipped.
func checkBlockStoreNameCollisions(db *gorm.DB) error {
	if !db.Migrator().HasTable("block_store_configs") {
		return nil
	}
	// No need to probe for the kind column: once it is gone the surviving
	// unique index on name makes duplicates impossible, so the query below
	// returns nothing and the check is a no-op. Probing would mean a
	// dialect-specific catalog query for no gain.

	var names []string
	if err := db.Raw(
		"SELECT name FROM block_store_configs GROUP BY name HAVING COUNT(*) > 1",
	).Scan(&names).Error; err != nil {
		return fmt.Errorf("failed to check block store names: %w", err)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %q is used by more than one block store\n", n)
	}
	b.WriteString("\nBlock stores no longer have a kind, so their names must be unique.\n")
	b.WriteString("Rename one of each pair with `dfsctl store block edit`, then restart.")
	return fmt.Errorf("%w%s", ErrBlockStoreNameCollision, b.String())
}
