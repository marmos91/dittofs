package blockstore

import (
	"testing"
	"time"
)

func TestParseRetentionPolicy(t *testing.T) {
	tests := []struct {
		input   string
		want    RetentionPolicy
		wantErr bool
	}{
		{"pin", RetentionPin, false},
		{"ttl", RetentionTTL, false},
		{"lru", RetentionLRU, false},
		{"", RetentionLRU, false},        // empty defaults to LRU (CACHE-06)
		{"PIN", RetentionPin, false},     // case-insensitive
		{"TTL", RetentionTTL, false},     // case-insensitive
		{"LRU", RetentionLRU, false},     // case-insensitive
		{"Pin", RetentionPin, false},     // mixed case
		{"  lru  ", RetentionLRU, false}, // whitespace trimmed
		{"invalid", "", true},
		{"evict", "", true},
	}

	for _, tt := range tests {
		t.Run("input="+tt.input, func(t *testing.T) {
			got, err := ParseRetentionPolicy(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRetentionPolicy(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseRetentionPolicy(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRetentionPolicy_String(t *testing.T) {
	tests := []struct {
		policy RetentionPolicy
		want   string
	}{
		{RetentionPin, "pin"},
		{RetentionTTL, "ttl"},
		{RetentionLRU, "lru"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.policy.String(); got != tt.want {
				t.Errorf("RetentionPolicy.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRetentionPolicy_IsValid(t *testing.T) {
	tests := []struct {
		policy RetentionPolicy
		valid  bool
	}{
		{RetentionPin, true},
		{RetentionTTL, true},
		{RetentionLRU, true},
		{RetentionPolicy("invalid"), false},
		{RetentionPolicy(""), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.policy), func(t *testing.T) {
			if got := tt.policy.IsValid(); got != tt.valid {
				t.Errorf("RetentionPolicy(%q).IsValid() = %v, want %v", tt.policy, got, tt.valid)
			}
		})
	}
}

func TestValidateRetentionPolicy(t *testing.T) {
	tests := []struct {
		name    string
		policy  RetentionPolicy
		ttl     time.Duration
		wantErr bool
	}{
		{"TTL mode requires duration", RetentionTTL, 0, true},
		{"TTL mode with valid duration", RetentionTTL, 72 * time.Hour, false},
		{"TTL mode with negative duration", RetentionTTL, -1 * time.Hour, true},
		{"Pin ignores TTL (zero)", RetentionPin, 0, false},
		{"Pin ignores TTL (nonzero)", RetentionPin, 24 * time.Hour, false},
		{"LRU with zero TTL", RetentionLRU, 0, false},
		{"LRU with nonzero TTL", RetentionLRU, 24 * time.Hour, false},
		{"Invalid policy", RetentionPolicy("invalid"), 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRetentionPolicy(tt.policy, tt.ttl)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateRetentionPolicy(%q, %v) error = %v, wantErr %v",
					tt.policy, tt.ttl, err, tt.wantErr)
			}
		})
	}
}
