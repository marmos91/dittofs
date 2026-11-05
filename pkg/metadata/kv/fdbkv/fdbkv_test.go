//go:build linux && cgo

package fdbkv_test

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
)

// Two nodes over one prefix, one with its clock ahead.
func TestNowAcrossNodes(t *testing.T) {
	prefix, node := fdbkvtest.Prefix(), byte(0)
	kvtest.NowAcrossStores(t, func(t *testing.T, skew time.Duration) kv.KV {
		node++
		opts := fdbkv.Options{Prefix: prefix, Clock: func() time.Time { return time.Now().Add(skew) }}
		opts.NodeID[15] = node
		return fdbkvtest.Open(t, opts)
	})
}

func TestConformance(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.KV { return fdbkvtest.Open(t, fdbkv.Options{}) })
}
