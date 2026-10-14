package kvstoretest_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
)

// The suite through its own exported surface, against the embedded backend
// in memory: every case it holds runs and passes on a conforming store.
func TestRun(t *testing.T) {
	kvstoretest.Run(t, func(tb testing.TB) kv.KV {
		s, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() {
			if err := s.Close(); err != nil {
				tb.Error(err)
			}
		})
		return s
	})
}
