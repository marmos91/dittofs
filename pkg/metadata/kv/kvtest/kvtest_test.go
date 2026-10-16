package kvtest_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/kvtest"
)

// The suite through its own exported surface, against the embedded backend
// in memory: every case it holds runs and passes on a conforming store.
func TestRun(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.KV {
		s, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		return s
	})
}
