package basestore_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/quota"
)

func TestBaseStore(t *testing.T) {
	// Create a new BaseStore instance
	store := basestore.NewBaseStore()

	// Test SetUsedBytes and GetUsedBytes
	store.SetUsedBytes(100)
	if store.GetUsedBytes() != 100 {
		t.Errorf("Expected used bytes to be 100, got %d", store.GetUsedBytes())
	}

	// Test AddUsedBytes
	store.AddUsedBytes(50)
	if store.GetUsedBytes() != 150 {
		t.Errorf("Expected used bytes to be 150, got %d", store.GetUsedBytes())
	}

	// Test ResetUsage
	store.ResetUsage()
	if store.GetUsedBytes() != 0 {
		t.Errorf("Expected used bytes to be 0 after reset, got %d", store.GetUsedBytes())
	}

	// Test Negative values
	store.SetUsedBytes(-50)
	if store.GetUsedBytes() != -50 {
		t.Errorf("Expected used bytes to be -50, got %d", store.GetUsedBytes())
	}

	store.AddUsedBytes(-200)
	if store.GetUsedBytes() != -250 {
		t.Errorf("Expected used bytes to be -250, got %d", store.GetUsedBytes())
	}

	// Test Adding Zero value
	store.AddUsedBytes(0)
	if store.GetUsedBytes() != -250 {
		t.Errorf("Expected used bytes to remain -250 after adding 0, got %d", store.GetUsedBytes())
	}
}

func TestQuotaUsage(t *testing.T) {
	// Create a new BaseStore instance
	store := basestore.NewBaseStore()

	// Test GetQuotaUsage for a non-existent key
	usage, err := store.GetQuotaUsage(0, 1)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if usage.Bytes != 0 || usage.Files != 0 {
		t.Errorf("Expected zero usage for non-existent key, got %+v", usage)
	}

	// Test ApplyQuotaDelta and GetQuotaUsage
	delta := map[quota.Key]metadata.UsageStat{
		{Scope: metadata.QuotaScopeUser, ID: 1}: {Bytes: 100, Files: 2},
	}
	store.ApplyQuotaDelta(delta)

	usage, err = store.GetQuotaUsage(0, 1)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if usage.Bytes != 100 || usage.Files != 2 {
		t.Errorf("Expected usage to be updated, got %+v", usage)
	}

	// Test ResetUsage clears the quota
	store.ResetUsage()
	usage, err = store.GetQuotaUsage(0, 1)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if usage.Bytes != 0 || usage.Files != 0 {
		t.Errorf("Expected zero usage after reset, got %+v", usage)
	}
}
