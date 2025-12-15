package bytesize

import (
	"testing"
)

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    ByteSize
		wantErr bool
	}{
		// Plain numbers
		{"plain zero", "0", 0, false},
		{"plain bytes", "1024", 1024, false},
		{"plain large", "1073741824", 1073741824, false},

		// Bytes suffix
		{"bytes B", "1024B", 1024, false},
		{"bytes b lowercase", "1024b", 1024, false},

		// Binary units (×1024)
		{"kibibytes Ki", "1Ki", 1024, false},
		{"kibibytes KiB", "1KiB", 1024, false},
		{"mebibytes Mi", "100Mi", 100 * 1024 * 1024, false},
		{"mebibytes MiB", "100MiB", 100 * 1024 * 1024, false},
		{"gibibytes Gi", "1Gi", 1024 * 1024 * 1024, false},
		{"gibibytes GiB", "1GiB", 1024 * 1024 * 1024, false},
		{"tebibytes Ti", "1Ti", 1024 * 1024 * 1024 * 1024, false},
		{"tebibytes TiB", "1TiB", 1024 * 1024 * 1024 * 1024, false},

		// Decimal units (×1000)
		{"kilobytes K", "1K", 1000, false},
		{"kilobytes KB", "1KB", 1000, false},
		{"megabytes M", "100M", 100 * 1000 * 1000, false},
		{"megabytes MB", "100MB", 100 * 1000 * 1000, false},
		{"gigabytes G", "1G", 1000 * 1000 * 1000, false},
		{"gigabytes GB", "1GB", 1000 * 1000 * 1000, false},
		{"terabytes T", "1T", 1000 * 1000 * 1000 * 1000, false},
		{"terabytes TB", "1TB", 1000 * 1000 * 1000 * 1000, false},

		// Case insensitivity
		{"lowercase gi", "1gi", 1024 * 1024 * 1024, false},
		{"uppercase GI", "1GI", 1024 * 1024 * 1024, false},
		{"mixed case Gi", "1Gi", 1024 * 1024 * 1024, false},

		// Whitespace handling
		{"leading space", "  1Gi", 1024 * 1024 * 1024, false},
		{"trailing space", "1Gi  ", 1024 * 1024 * 1024, false},
		{"space between", "1 Gi", 1024 * 1024 * 1024, false},

		// Floating point
		{"float mebibytes", "1.5Mi", ByteSize(1.5 * 1024 * 1024), false},
		{"float gibibytes", "0.5Gi", ByteSize(0.5 * 1024 * 1024 * 1024), false},

		// Real-world examples
		{"100MB cache", "100Mi", 100 * 1024 * 1024, false},
		{"1GB cache", "1Gi", 1024 * 1024 * 1024, false},
		{"512KB chunk", "512Ki", 512 * 1024, false},

		// Error cases
		{"empty string", "", 0, true},
		{"whitespace only", "   ", 0, true},
		{"invalid unit", "1Xi", 0, true},
		{"negative number", "-1Gi", 0, true},
		{"no number", "Gi", 0, true},
		{"garbage", "abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseByteSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseByteSize(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseByteSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestByteSize_UnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    ByteSize
		wantErr bool
	}{
		{"simple", "1Gi", 1024 * 1024 * 1024, false},
		{"numeric", "1024", 1024, false},
		{"invalid", "invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b ByteSize
			err := b.UnmarshalText([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("ByteSize.UnmarshalText(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && b != tt.want {
				t.Errorf("ByteSize.UnmarshalText(%q) = %d, want %d", tt.input, b, tt.want)
			}
		})
	}
}

func TestByteSize_String(t *testing.T) {
	tests := []struct {
		name  string
		input ByteSize
		want  string
	}{
		{"bytes", 512, "512B"},
		{"kibibytes", 2 * KiB, "2.00KiB"},
		{"mebibytes", 100 * MiB, "100.00MiB"},
		{"gibibytes", 1 * GiB, "1.00GiB"},
		{"tebibytes", 2 * TiB, "2.00TiB"},
		{"fractional gibibytes", ByteSize(1.5 * float64(GiB)), "1.50GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.input.String(); got != tt.want {
				t.Errorf("ByteSize(%d).String() = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestByteSize_Conversions(t *testing.T) {
	size := ByteSize(1024 * 1024 * 1024) // 1 GiB

	if got := size.Uint64(); got != 1024*1024*1024 {
		t.Errorf("ByteSize.Uint64() = %d, want %d", got, 1024*1024*1024)
	}

	if got := size.Int64(); got != 1024*1024*1024 {
		t.Errorf("ByteSize.Int64() = %d, want %d", got, 1024*1024*1024)
	}
}

func TestByteSize_Constants(t *testing.T) {
	// Verify binary unit constants
	if KiB != 1024 {
		t.Errorf("KiB = %d, want 1024", KiB)
	}
	if MiB != 1024*1024 {
		t.Errorf("MiB = %d, want %d", MiB, 1024*1024)
	}
	if GiB != 1024*1024*1024 {
		t.Errorf("GiB = %d, want %d", GiB, 1024*1024*1024)
	}
	if TiB != 1024*1024*1024*1024 {
		t.Errorf("TiB = %d, want %d", TiB, 1024*1024*1024*1024)
	}

	// Verify decimal unit constants
	if KB != 1000 {
		t.Errorf("KB = %d, want 1000", KB)
	}
	if MB != 1000*1000 {
		t.Errorf("MB = %d, want %d", MB, 1000*1000)
	}
	if GB != 1000*1000*1000 {
		t.Errorf("GB = %d, want %d", GB, 1000*1000*1000)
	}
	if TB != 1000*1000*1000*1000 {
		t.Errorf("TB = %d, want %d", TB, 1000*1000*1000*1000)
	}
}
