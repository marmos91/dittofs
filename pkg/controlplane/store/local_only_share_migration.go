package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// ErrLocalOnlyShareUnbound reports shares that reached the new model with no
// block store to bind to.
var ErrLocalOnlyShareUnbound = errors.New("share has no block store after upgrade")

// checkLocalOnlyShares refuses to drop local_block_store_id while a share's
// only surviving binding lives in it.
//
// A local-only share carried its data in the local store and left the remote
// reference empty. The rename that turns remote_block_store_id into
// block_store_id therefore hands such a share an empty one, and dropping the
// local column would leave it referencing nothing — it would fail to start with
// an error naming neither the cause nor the upgrade.
//
// Carrying the local ID across is not a repair: the local tier was typically an
// "fs" store, a type that exists only locally, so the share would come up bound
// to a block store that cannot be built. The operator has to choose a real one,
// which is a decision no migration can make. Refuse, name the shares, and leave
// the column in place so the choice is still recorded when they come back.
func checkLocalOnlyShares(db *gorm.DB) error {
	var names []string
	if err := db.Raw(`
		SELECT name FROM shares
		WHERE (block_store_id IS NULL OR block_store_id = '')
		  AND local_block_store_id IS NOT NULL
		  AND local_block_store_id != ''
	`).Scan(&names).Error; err != nil {
		return fmt.Errorf("failed to check shares for a block store: %w", err)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %q has no block store\n", n)
	}
	b.WriteString("\nThese shares kept their data in a local block store and never had a\n")
	b.WriteString("remote one. Every share now needs a block store, and the local store\n")
	b.WriteString("they used cannot become one — pick an s3 or memory block store for\n")
	b.WriteString("each, with the server stopped:\n")
	b.WriteString("  UPDATE shares SET block_store_id = '<block store id>' WHERE name = '<share>';")
	return fmt.Errorf("%w%s", ErrLocalOnlyShareUnbound, b.String())
}
