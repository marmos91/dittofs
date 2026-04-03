package s3

import (
	"testing"
)

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"no scheme", "s3.cubbit.eu", "https://s3.cubbit.eu"},
		{"no scheme with port", "s3.fr-par.scw.cloud:443", "https://s3.fr-par.scw.cloud:443"},
		{"no scheme with path", "s3.example.com/custom", "https://s3.example.com/custom"},
		{"scheme in path", "s3.example.com/path://foo", "https://s3.example.com/path://foo"},
		{"https scheme", "https://s3.cubbit.eu", "https://s3.cubbit.eu"},
		{"http scheme", "http://localhost:4566", "http://localhost:4566"},
		{"non-http scheme", "s3://my-bucket", "s3://my-bucket"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeEndpoint(tt.endpoint)
			if got != tt.want {
				t.Errorf("normalizeEndpoint(%q) = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}
