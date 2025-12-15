package bytesize

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ByteSize represents a size in bytes that can be unmarshaled from human-readable
// strings like "1Gi", "500Mi", "100MB", or plain numbers.
//
// Supported formats:
//   - Plain numbers: 1024, 1073741824
//   - Binary units (×1024): Ki/KiB, Mi/MiB, Gi/GiB, Ti/TiB
//   - Decimal units (×1000): K/KB, M/MB, G/GB, T/TB
//   - Bytes: B
//
// Examples: "1Gi" (1 gibibyte), "500Mi" (500 mebibytes), "100MB" (100 megabytes)
type ByteSize uint64

// Common byte size constants
const (
	B  ByteSize = 1
	KB ByteSize = 1000
	MB ByteSize = 1000 * KB
	GB ByteSize = 1000 * MB
	TB ByteSize = 1000 * GB

	KiB ByteSize = 1024
	MiB ByteSize = 1024 * KiB
	GiB ByteSize = 1024 * MiB
	TiB ByteSize = 1024 * GiB
)

// byteSizePattern matches a number followed by an optional unit suffix
var byteSizePattern = regexp.MustCompile(`(?i)^\s*(\d+(?:\.\d+)?)\s*([a-z]*)\s*$`)

// unitMultipliers maps unit suffixes to their byte multipliers
var unitMultipliers = map[string]ByteSize{
	"":    B,
	"b":   B,
	"k":   KB,
	"kb":  KB,
	"m":   MB,
	"mb":  MB,
	"g":   GB,
	"gb":  GB,
	"t":   TB,
	"tb":  TB,
	"ki":  KiB,
	"kib": KiB,
	"mi":  MiB,
	"mib": MiB,
	"gi":  GiB,
	"gib": GiB,
	"ti":  TiB,
	"tib": TiB,
}

// ParseByteSize parses a human-readable byte size string into a ByteSize value.
// It accepts formats like "1Gi", "500Mi", "100MB", "1024", etc.
func ParseByteSize(s string) (ByteSize, error) {
	// Handle empty string
	if strings.TrimSpace(s) == "" {
		return 0, fmt.Errorf("empty byte size string")
	}

	matches := byteSizePattern.FindStringSubmatch(s)
	if matches == nil {
		return 0, fmt.Errorf("invalid byte size format: %q", s)
	}

	// Parse the numeric part
	numStr := matches[1]
	unit := strings.ToLower(matches[2])

	// Check if it's a floating point number
	if strings.Contains(numStr, ".") {
		num, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid number in byte size: %q", numStr)
		}

		multiplier, ok := unitMultipliers[unit]
		if !ok {
			return 0, fmt.Errorf("unknown byte size unit: %q", matches[2])
		}

		return ByteSize(num * float64(multiplier)), nil
	}

	// Parse as integer
	num, err := strconv.ParseUint(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number in byte size: %q", numStr)
	}

	multiplier, ok := unitMultipliers[unit]
	if !ok {
		return 0, fmt.Errorf("unknown byte size unit: %q", matches[2])
	}

	return ByteSize(num) * multiplier, nil
}

// UnmarshalText implements encoding.TextUnmarshaler for ByteSize.
// This allows ByteSize to be used directly in structs with mapstructure.
func (b *ByteSize) UnmarshalText(text []byte) error {
	size, err := ParseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = size
	return nil
}

// String returns a human-readable representation of the byte size.
func (b ByteSize) String() string {
	switch {
	case b >= TiB:
		return fmt.Sprintf("%.2fTiB", float64(b)/float64(TiB))
	case b >= GiB:
		return fmt.Sprintf("%.2fGiB", float64(b)/float64(GiB))
	case b >= MiB:
		return fmt.Sprintf("%.2fMiB", float64(b)/float64(MiB))
	case b >= KiB:
		return fmt.Sprintf("%.2fKiB", float64(b)/float64(KiB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// Uint64 returns the ByteSize as a uint64.
func (b ByteSize) Uint64() uint64 {
	return uint64(b)
}

// Int64 returns the ByteSize as an int64.
// Note: This may overflow for very large values.
func (b ByteSize) Int64() int64 {
	return int64(b)
}
