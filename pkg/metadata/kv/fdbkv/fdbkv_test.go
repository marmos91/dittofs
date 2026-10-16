//go:build fdb

package fdbkv_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
	"github.com/marmos91/dittofs/pkg/metadata/kv/kvtest"
)

func TestConformance(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.KV { return fdbkvtest.Open(t, fdbkv.Options{}) })
}
