package kv_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
)

func TestCountingFlagsGuardOnlyUpdates(t *testing.T) {
	s, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &kv.Counting{KV: s}
	ctx := context.Background()
	if err := c.Update(ctx, func(tx kv.Txn) error { return tx.Guard([]byte("k")) }); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, func(tx kv.Txn) error {
		if err := tx.GuardRange([]byte("p/")); err != nil {
			return err
		}
		return tx.Set([]byte("k"), []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.Take(); got.GuardOnly != 1 || got.Guards != 1 || got.RangeGuards != 1 || got.Sets != 1 {
		t.Fatalf("counts %+v, want one guard-only Update of two", got)
	}
}
