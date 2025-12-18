package mfsymlink

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
)

func TestEncode(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr error
	}{
		{
			name:    "simple target",
			target:  "target.txt",
			wantErr: nil,
		},
		{
			name:    "absolute path",
			target:  "/usr/local/bin/myapp",
			wantErr: nil,
		},
		{
			name:    "relative path with dots",
			target:  "../../../some/file.txt",
			wantErr: nil,
		},
		{
			name:    "path with spaces",
			target:  "/path/with spaces/file name.txt",
			wantErr: nil,
		},
		{
			name:    "empty target",
			target:  "",
			wantErr: nil, // Will be a generic error, not ErrTargetTooLong
		},
		{
			name:    "max length target",
			target:  strings.Repeat("a", MaxTargetLength),
			wantErr: nil,
		},
		{
			name:    "too long target",
			target:  strings.Repeat("a", MaxTargetLength+1),
			wantErr: ErrTargetTooLong,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := Encode(tt.target)

			if tt.target == "" {
				if err == nil {
					t.Error("expected error for empty target")
				}
				return
			}

			if tt.wantErr != nil {
				if err != tt.wantErr {
					t.Errorf("Encode() error = %v, wantErr %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Encode() unexpected error: %v", err)
			}

			// Verify size
			if len(data) != Size {
				t.Errorf("Encode() size = %d, want %d", len(data), Size)
			}

			// Verify magic
			if !IsMFsymlink(data) {
				t.Error("Encode() result doesn't have magic marker")
			}

			// Verify MD5
			if !ValidateMD5(data) {
				t.Error("Encode() result has invalid MD5")
			}

			// Verify roundtrip
			decoded, err := Decode(data)
			if err != nil {
				t.Fatalf("Decode() after Encode() failed: %v", err)
			}
			if decoded != tt.target {
				t.Errorf("roundtrip failed: got %q, want %q", decoded, tt.target)
			}
		})
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		want    string
		wantErr bool
	}{
		{
			name:    "valid simple",
			data:    mustEncode("target.txt"),
			want:    "target.txt",
			wantErr: false,
		},
		{
			name:    "valid path",
			data:    mustEncode("/usr/local/bin/app"),
			want:    "/usr/local/bin/app",
			wantErr: false,
		},
		{
			name:    "wrong size too small",
			data:    []byte("XSym\n"),
			want:    "",
			wantErr: true,
		},
		{
			name:    "wrong size too large",
			data:    append(mustEncode("test"), byte(0)),
			want:    "",
			wantErr: true,
		},
		{
			name:    "missing magic",
			data:    make([]byte, Size),
			want:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decode(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("Decode() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Decode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecodeCorruptMD5(t *testing.T) {
	// Create valid MFsymlink
	data := mustEncode("test.txt")

	// Corrupt the MD5 (offset 10-42)
	data[10] = 'x'
	data[11] = 'x'

	_, err := Decode(data)
	if err == nil {
		t.Error("Decode() should fail with corrupted MD5")
	}
}

func TestIsMFsymlink(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "valid mfsymlink",
			data: mustEncode("target"),
			want: true,
		},
		{
			name: "just magic",
			data: []byte("XSym\n"),
			want: true,
		},
		{
			name: "partial magic",
			data: []byte("XSym"),
			want: false,
		},
		{
			name: "wrong magic",
			data: []byte("YSym\n"),
			want: false,
		},
		{
			name: "empty",
			data: []byte{},
			want: false,
		},
		{
			name: "nil",
			data: nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMFsymlink(tt.data); got != tt.want {
				t.Errorf("IsMFsymlink() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateMD5(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "valid",
			data: mustEncode("target.txt"),
			want: true,
		},
		{
			name: "too small",
			data: []byte("XSym\n"),
			want: false,
		},
		{
			name: "wrong size",
			data: make([]byte, 100),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateMD5(tt.data); got != tt.want {
				t.Errorf("ValidateMD5() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQuickCheck(t *testing.T) {
	validData := mustEncode("test")

	tests := []struct {
		name       string
		size       int64
		firstBytes []byte
		want       bool
	}{
		{
			name:       "valid",
			size:       Size,
			firstBytes: validData[:10],
			want:       true,
		},
		{
			name:       "wrong size",
			size:       100,
			firstBytes: validData[:10],
			want:       false,
		},
		{
			name:       "wrong magic",
			size:       Size,
			firstBytes: []byte("XXXX\n"),
			want:       false,
		},
		{
			name:       "both wrong",
			size:       100,
			firstBytes: []byte("XXXX\n"),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := QuickCheck(tt.size, tt.firstBytes); got != tt.want {
				t.Errorf("QuickCheck() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMFsymlinkFormat(t *testing.T) {
	target := "myfile.txt"
	data := mustEncode(target)

	// Verify exact format
	// Magic: "XSym\n" (5 bytes)
	if string(data[0:5]) != "XSym\n" {
		t.Errorf("magic = %q, want %q", string(data[0:5]), "XSym\n")
	}

	// Length: "0010\n" (5 bytes for "myfile.txt" which is 10 chars)
	if string(data[5:10]) != "0010\n" {
		t.Errorf("length = %q, want %q", string(data[5:10]), "0010\n")
	}

	// MD5: 32 hex chars + newline
	hash := md5.Sum([]byte(target))
	expectedMD5 := hex.EncodeToString(hash[:])
	if string(data[10:42]) != expectedMD5 {
		t.Errorf("MD5 = %q, want %q", string(data[10:42]), expectedMD5)
	}
	if data[42] != '\n' {
		t.Error("missing newline after MD5")
	}

	// Target: "myfile.txt\n"
	if string(data[43:53]) != target {
		t.Errorf("target = %q, want %q", string(data[43:53]), target)
	}
	if data[53] != '\n' {
		t.Error("missing newline after target")
	}

	// Padding: spaces until end
	for i := 54; i < Size; i++ {
		if data[i] != ' ' {
			t.Errorf("padding at %d = %q, want space", i, data[i])
			break
		}
	}
}

func TestSpecialCharacters(t *testing.T) {
	targets := []string{
		"file with spaces.txt",
		"file\twith\ttabs",
		"file-with-dashes",
		"file_with_underscores",
		"file.multiple.dots.txt",
		"UPPERCASE",
		"MixedCase",
		"unicode-日本語",
		"path/with/slashes",
		"..\\windows\\path",
	}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			data, err := Encode(target)
			if err != nil {
				t.Fatalf("Encode(%q) failed: %v", target, err)
			}

			decoded, err := Decode(data)
			if err != nil {
				t.Fatalf("Decode() failed: %v", err)
			}

			if decoded != target {
				t.Errorf("roundtrip failed: got %q, want %q", decoded, target)
			}
		})
	}
}

// mustEncode is a test helper that panics on encode error.
func mustEncode(target string) []byte {
	data, err := Encode(target)
	if err != nil {
		panic(err)
	}
	return data
}
