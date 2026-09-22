package compression

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Algo identifies a compression algorithm. The wire byte values are
// stable: changing them would invalidate existing framed blocks.
type Algo uint8

const (
	// AlgoZstd selects zstd via github.com/klauspost/compress/zstd at
	// the library's default level.
	AlgoZstd Algo = 1

	// AlgoLZ4 selects lz4 via github.com/pierrec/lz4/v4 at the library's
	// default level.
	AlgoLZ4 Algo = 2
)

// String returns the canonical lowercase config string for an Algo.
func (a Algo) String() string {
	switch a {
	case AlgoZstd:
		return "zstd"
	case AlgoLZ4:
		return "lz4"
	default:
		return fmt.Sprintf("Algo(%d)", uint8(a))
	}
}

// parseAlgoString converts the lowercase config form into an Algo.
func parseAlgoString(s string) (Algo, error) {
	switch s {
	case "zstd":
		return AlgoZstd, nil
	case "lz4":
		return AlgoLZ4, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnsupportedCompressionAlgo, s)
	}
}

// CompressionPolicy holds the per-remote compression configuration.
// Captured at remote-store construction and immutable thereafter.
type CompressionPolicy struct {
	Algo Algo
}

// policyJSON is the JSON shape stored in BlockStoreConfig.Config under
// the "compression" key.
type policyJSON struct {
	Algo string `json:"algo"`
}

// ParsePolicy decodes the JSON value sitting under the "compression"
// key. An empty object (`{}`) yields the zstd default. Non-object
// inputs (null, arrays, scalars) are rejected so misconfigurations
// fail fast instead of silently enabling compression with defaults.
func ParsePolicy(raw json.RawMessage) (CompressionPolicy, error) {
	if len(raw) == 0 {
		return CompressionPolicy{Algo: AlgoZstd}, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return CompressionPolicy{}, fmt.Errorf("compression: parse policy: expected JSON object, got %s", trimmed)
	}
	var pj policyJSON
	if err := json.Unmarshal(trimmed, &pj); err != nil {
		return CompressionPolicy{}, fmt.Errorf("compression: parse policy: %w", err)
	}
	if pj.Algo == "" {
		return CompressionPolicy{Algo: AlgoZstd}, nil
	}
	a, err := parseAlgoString(pj.Algo)
	if err != nil {
		return CompressionPolicy{}, err
	}
	return CompressionPolicy{Algo: a}, nil
}
