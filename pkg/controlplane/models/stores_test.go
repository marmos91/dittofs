package models

import (
	"testing"
)

func TestBlockStoreConfigTableName(t *testing.T) {
	b := BlockStoreConfig{}
	if b.TableName() != "block_store_configs" {
		t.Errorf("expected table name 'block_store_configs', got %q", b.TableName())
	}
}

func TestBlockStoreConfigGetSetConfig(t *testing.T) {
	b := &BlockStoreConfig{}

	cfg := map[string]any{"path": "/data/blocks"}
	if err := b.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}
	if b.Config == "" {
		t.Error("expected Config to be non-empty after SetConfig")
	}

	got, err := b.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if got["path"] != "/data/blocks" {
		t.Errorf("expected path '/data/blocks', got %v", got["path"])
	}
}

func TestBlockStoreConfigEmptyGetConfig(t *testing.T) {
	b := &BlockStoreConfig{}
	got, err := b.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if got == nil {
		t.Error("expected non-nil map from empty GetConfig")
	}
}
