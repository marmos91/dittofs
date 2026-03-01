package signing

import (
	"testing"
)

func TestDefaultSigningConfig(t *testing.T) {
	config := DefaultSigningConfig()

	if !config.Enabled {
		t.Error("Default config should have Enabled = true")
	}
	if config.Required {
		t.Error("Default config should have Required = false")
	}
}
