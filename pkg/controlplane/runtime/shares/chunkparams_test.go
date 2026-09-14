package shares

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// chunkParamsFromConfig degrades rather than failing: a share whose chunk_size
// is missing, malformed or below the floor keeps the default profile, because
// a bad tuning value must not stop a share from starting.
func TestChunkParamsFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		wantOK bool
		want   chunker.Params
	}{
		{"absent", map[string]any{}, false, chunker.Params{}},
		{"non-numeric", map[string]any{"chunk_size": "128k"}, false, chunker.Params{}},
		{"zero", map[string]any{"chunk_size": float64(0)}, false, chunker.Params{}},
		{"negative", map[string]any{"chunk_size": float64(-1)}, false, chunker.Params{}},
		{"fractional", map[string]any{"chunk_size": 1024.5}, false, chunker.Params{}},
		{"below floor", map[string]any{"chunk_size": float64(100)}, false, chunker.Params{}},
		{
			"valid derives avg and max",
			map[string]any{"chunk_size": float64(128 << 10)},
			true,
			chunker.Params{Min: 128 << 10, Avg: 512 << 10, Max: 1 << 20},
		},
		{
			"chunk_max caps the ceiling and clamps avg under it",
			map[string]any{"chunk_size": float64(128 << 10), "chunk_max": float64(256 << 10)},
			true,
			chunker.Params{Min: 128 << 10, Avg: 256 << 10, Max: 256 << 10},
		},
		{
			"invalid chunk_max is ignored, not fatal",
			map[string]any{"chunk_size": float64(128 << 10), "chunk_max": "big"},
			true,
			chunker.Params{Min: 128 << 10, Avg: 512 << 10, Max: 1 << 20},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := chunkParamsFromConfig(tc.config)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (params %+v)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("params = %+v, want %+v", got, tc.want)
			}
		})
	}
}
