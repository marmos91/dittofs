package runtime

import (
	"strings"
	"testing"
)

func TestValidateCompressionSubconfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     map[string]any
		wantErr string // substring; "" means accept
	}{
		{"absent", map[string]any{}, ""},
		{"empty_object_defaults_zstd", map[string]any{"compression": map[string]any{}}, ""},
		{"explicit_zstd", map[string]any{"compression": map[string]any{"algo": "zstd"}}, ""},
		{"explicit_lz4", map[string]any{"compression": map[string]any{"algo": "lz4"}}, ""},
		{"unknown_algo", map[string]any{"compression": map[string]any{"algo": "snappy"}}, "unsupported value"},
		{"wrong_type_block", map[string]any{"compression": "zstd"}, "expected object"},
		{"wrong_type_algo", map[string]any{"compression": map[string]any{"algo": 7}}, "expected string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCompressionSubconfig(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
