package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidate_RefusesMalformedSMBEncryption(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "encryption is not an object", raw: `{"encryption":"required"}`, want: "object", wantErr: true},
		{name: "encryption_mode is a bool", raw: `{"encryption":{"encryption_mode":true}}`, want: "string", wantErr: true},
		{name: "encryption_mode is a number", raw: `{"encryption":{"encryption_mode":1}}`, want: "string", wantErr: true},
		{name: "unknown encryption key", raw: `{"encryption":{"mode":"required"}}`, want: `"mode"`, wantErr: true},
		{name: "known keys", raw: `{"encryption":{"encryption_mode":"required","allowed_ciphers":[]}}`},
		{name: "encryption absent", raw: `{}`},
		{name: "encryption null is unset", raw: `{"encryption":null}`},
		{name: "allowed_ciphers with a known cipher", raw: `{"encryption":{"allowed_ciphers":[2]}}`, want: "fixed", wantErr: true},
		{name: "allowed_ciphers with an unknown cipher", raw: `{"encryption":{"allowed_ciphers":[99]}}`, want: "fixed", wantErr: true},
		{name: "allowed_ciphers with a string", raw: `{"encryption":{"allowed_ciphers":["x"]}}`, want: "fixed", wantErr: true},
		{name: "allowed_ciphers is not a list", raw: `{"encryption":{"allowed_ciphers":"x"}}`, want: "fixed", wantErr: true},
		{name: "allowed_ciphers empty is unset", raw: `{"encryption":{"allowed_ciphers":[]}}`},
		{name: "allowed_ciphers null is unset", raw: `{"encryption":{"allowed_ciphers":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &parsed); err != nil {
				t.Fatal(err)
			}
			cfg := &AdapterConfig{Type: "smb", Port: 12445}
			if err := cfg.SetConfig(parsed); err != nil {
				t.Fatal(err)
			}
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate accepted %s", tc.raw)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Validate(%s) = %q, want it to mention %s", tc.raw, err.Error(), tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%s) = %v", tc.raw, err)
			}
		})
	}
}
