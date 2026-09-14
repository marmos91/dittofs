package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// migrateShareDurability moves a share's durability choice off the local block
// store config and onto the share itself.
//
// The tier used to live in that config's JSON as a "durability" enum, or as the
// two older bools it composes. The config is going away, and defaulting every
// share back to journal would quietly weaken a promise the operator made
// deliberately — nothing about the running system would reveal it.
//
// Only shares whose acknowledgement is still unset are touched, so an operator
// who changes the promise after upgrading does not have the old config put back
// on the next restart. Shares that predate the column and configured nothing
// fall through to the blanket backfill.
func migrateShareDurability(db *gorm.DB) error {
	// The join column is the only route back to the config; once it is gone
	// the migration has already run.
	if !db.Migrator().HasColumn(&models.Share{}, "local_block_store_id") {
		return nil
	}

	// A share's reference normally holds the block store's UUID, but the REST
	// update path historically persisted the name instead, so match either.
	var rows []struct {
		ID     string
		Config string
	}
	if err := db.Raw(`
		SELECT s.id AS id, COALESCE(b.config, '') AS config
		FROM shares s
		JOIN block_store_configs b
		  ON b.id = s.local_block_store_id OR b.name = s.local_block_store_id
		WHERE s.commit_ack IS NULL OR s.commit_ack = ''
	`).Scan(&rows).Error; err != nil {
		return fmt.Errorf("failed to read share durability config: %w", err)
	}

	for _, row := range rows {
		ack, relaxed := durabilityFromStoreConfig(row.Config)
		if err := db.Exec(
			"UPDATE shares SET commit_ack = ?, relaxed_metadata_commit = ? WHERE id = ?",
			string(ack), relaxed, row.ID,
		).Error; err != nil {
			return fmt.Errorf("failed to carry durability onto share %s: %w", row.ID, err)
		}
	}
	return nil
}

// durabilityFromStoreConfig reads a local block store config blob the way the
// share service read it: the "durability" enum wins when present, otherwise the
// two bools it composes are honoured independently. Anything unrecognised
// resolves to the default tier, matching what the running system did with it.
func durabilityFromStoreConfig(blob string) (models.CommitAck, bool) {
	if strings.TrimSpace(blob) == "" {
		return models.CommitAckJournal, false
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(blob), &cfg); err != nil {
		return models.CommitAckJournal, false
	}

	if v, ok := cfg["durability"]; ok {
		tier, isStr := v.(string)
		if !isStr {
			return models.CommitAckJournal, false
		}
		switch strings.ToLower(strings.TrimSpace(tier)) {
		case "writeback":
			return models.CommitAckJournal, true
		case "remote":
			return models.CommitAckBlockStore, false
		default: // "", "local", and anything unrecognised
			return models.CommitAckJournal, false
		}
	}

	ack := models.CommitAckJournal
	if b, ok := cfg["require_durable_commit"].(bool); ok && b {
		ack = models.CommitAckBlockStore
	}
	relaxed, _ := cfg["writeback"].(bool)
	return ack, relaxed
}
