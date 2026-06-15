package schedule

import (
	"testing"
	"time"
)

func TestParseInterval(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"empty", "", 0, true},
		{"plain hours", "24h", 24 * time.Hour, false},
		{"plain minutes", "30m", 30 * time.Minute, false},
		{"plain seconds", "90s", 90 * time.Second, false},
		{"composite", "1h30m", 90 * time.Minute, false},
		{"shorthand hourly", "@hourly", time.Hour, false},
		{"shorthand daily", "@daily", 24 * time.Hour, false},
		{"shorthand weekly", "@weekly", 7 * 24 * time.Hour, false},
		{"shorthand trimmed and cased", "  @Daily ", 24 * time.Hour, false},
		{"zero rejected", "0s", 0, true},
		{"negative rejected", "-1h", 0, true},
		{"garbage", "abc", 0, true},
		{"unknown shorthand", "@yearly", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseInterval(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseInterval(%q) = %v, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseInterval(%q) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("ParseInterval(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestFormatInterval(t *testing.T) {
	// FormatInterval is the inverse used for display; it returns the Go
	// duration string (shorthands are an input convenience only).
	if got := FormatInterval(24 * time.Hour); got != "24h0m0s" {
		t.Errorf("FormatInterval(24h) = %q, want 24h0m0s", got)
	}
}
