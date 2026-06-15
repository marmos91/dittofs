package api

import "testing"

func TestRequiresInitialPasswordChange(t *testing.T) {
	tests := []struct {
		name string
		set  *bool
		want bool
	}{
		{"unset defaults to required", nil, true},
		{"explicit true", boolPtr(true), true},
		{"explicit false opts out", boolPtr(false), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &APIConfig{RequireInitialPasswordChange: tt.set}
			if got := c.RequiresInitialPasswordChange(); got != tt.want {
				t.Errorf("RequiresInitialPasswordChange() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyDefaults_RequireInitialPasswordChange(t *testing.T) {
	// An unset value must default to true (forced change on) to preserve the
	// secure-by-default behavior.
	c := &APIConfig{}
	c.ApplyDefaults()
	if c.RequireInitialPasswordChange == nil {
		t.Fatal("ApplyDefaults left RequireInitialPasswordChange nil")
	}
	if !*c.RequireInitialPasswordChange {
		t.Error("ApplyDefaults defaulted RequireInitialPasswordChange to false, want true")
	}

	// An explicit false must be preserved (operator opt-out).
	off := false
	c2 := &APIConfig{RequireInitialPasswordChange: &off}
	c2.ApplyDefaults()
	if c2.RequireInitialPasswordChange == nil || *c2.RequireInitialPasswordChange {
		t.Error("ApplyDefaults overrode an explicit opt-out (false)")
	}
}

func boolPtr(b bool) *bool { return &b }
