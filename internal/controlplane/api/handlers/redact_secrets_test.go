package handlers

import (
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestRedactSecretJSON(t *testing.T) {
	tests := []struct {
		name     string
		blob     string
		mustHide []string // substrings that must NOT appear in output
		mustKeep []string // substrings that must still appear (non-secret)
		wantSame bool     // output should equal input unchanged
	}{
		{
			name:     "s3 secret access key",
			blob:     `{"bucket":"data","access_key_id":"AKIA123","secret_access_key":"s3cr3tVALUE"}`,
			mustHide: []string{"s3cr3tVALUE"},
			// access_key_id is an identifier (not a secret), so it is kept.
			mustKeep: []string{"data", "AKIA123"},
		},
		{
			name:     "postgres password",
			blob:     `{"host":"db","user":"dfs","password":"pgPASSWORD"}`,
			mustHide: []string{"pgPASSWORD"},
			mustKeep: []string{"db", "dfs"},
		},
		{
			name:     "nested object",
			blob:     `{"outer":{"api_secret":"deep","name":"keep"}}`,
			mustHide: []string{"deep"},
			mustKeep: []string{"keep"},
		},
		{
			name:     "array of objects",
			blob:     `{"endpoints":[{"password":"x1"},{"password":"x2"}]}`,
			mustHide: []string{"x1", "x2"},
		},
		{
			name:     "empty blob unchanged",
			blob:     "",
			wantSame: true,
		},
		{
			name:     "invalid json unchanged",
			blob:     "not-json",
			wantSame: true,
		},
		{
			name:     "no secrets unchanged in value",
			blob:     `{"path":"/var/data","size":100}`,
			mustKeep: []string{"/var/data", "100"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecretJSON(tc.blob)
			if tc.wantSame {
				if got != tc.blob {
					t.Fatalf("expected unchanged, got %q", got)
				}
				return
			}
			for _, h := range tc.mustHide {
				if strings.Contains(got, h) {
					t.Errorf("secret %q leaked in %q", h, got)
				}
			}
			for _, k := range tc.mustKeep {
				if !strings.Contains(got, k) {
					t.Errorf("non-secret %q dropped from %q", k, got)
				}
			}
		})
	}
}

// TestIsSecretKey documents the key-matching convention.
func TestIsSecretKey(t *testing.T) {
	secret := []string{"password", "secret_access_key", "Password", "API_SECRET", "access_key", "private_key"}
	plain := []string{"host", "port", "bucket", "path", "user", "region", "key_id"}

	for _, k := range secret {
		if !isSecretKey(k) {
			t.Errorf("expected %q to be treated as secret", k)
		}
	}
	for _, k := range plain {
		if isSecretKey(k) {
			t.Errorf("expected %q to be treated as non-secret", k)
		}
	}
}

// TestBlockStoreToResponse_RedactsConfig verifies the READ-path response
// scrubs secrets from the stored config blob (round-2 §2.1).
func TestBlockStoreToResponse_RedactsConfig(t *testing.T) {
	m := &models.BlockStoreConfig{
		ID:        "id1",
		Name:      "remote-s3",
		Kind:      models.BlockStoreKindRemote,
		Type:      "s3",
		Config:    `{"bucket":"b","secret_access_key":"LEAKED-S3-SECRET"}`,
		CreatedAt: time.Now(),
	}
	resp := blockStoreToResponse(m)
	if strings.Contains(resp.Config, "LEAKED-S3-SECRET") {
		t.Fatalf("block store response leaked secret: %s", resp.Config)
	}
	if !strings.Contains(resp.Config, "b") {
		t.Fatalf("non-secret bucket dropped: %s", resp.Config)
	}
}

// TestMergeRedactedSecrets verifies the Update-path reconciliation: a client
// that fetched a redacted config and echoed the sentinel back must not destroy
// the stored credential. Non-sentinel values (including a genuinely changed
// secret) pass through untouched.
func TestMergeRedactedSecrets(t *testing.T) {
	tests := []struct {
		name     string
		old      string
		new      string
		mustHave []string // substrings the merged blob must contain
		mustHide []string // substrings it must NOT contain
		wantSame bool     // merged should equal new unchanged
	}{
		{
			name:     "sentinel preserves stored secret",
			old:      `{"bucket":"b","secret_access_key":"REAL-SECRET"}`,
			new:      `{"bucket":"b2","secret_access_key":"********"}`,
			mustHave: []string{"REAL-SECRET", "b2"},
		},
		{
			name:     "explicit new secret overrides",
			old:      `{"password":"old"}`,
			new:      `{"password":"NEW-REAL"}`,
			mustHave: []string{"NEW-REAL"},
			mustHide: []string{"********"},
		},
		{
			name:     "no sentinel returns new unchanged",
			old:      `{"password":"old"}`,
			new:      `{"host":"db","password":"keep"}`,
			wantSame: true,
		},
		{
			name:     "nested sentinel preserved",
			old:      `{"creds":{"password":"REAL"}}`,
			new:      `{"creds":{"password":"********"}}`,
			mustHave: []string{"REAL"},
		},
		{
			name:     "array-nested object sentinel preserved",
			old:      `{"endpoints":[{"password":"REAL1"},{"password":"REAL2"}]}`,
			new:      `{"endpoints":[{"password":"********"},{"password":"********"}]}`,
			mustHave: []string{"REAL1", "REAL2"},
			mustHide: []string{"********"},
		},
		{
			name:     "array-of-strings sentinel preserved",
			old:      `{"keys":["REALKEY","plain"]}`,
			new:      `{"keys":["********","plain"]}`,
			mustHave: []string{"REALKEY", "plain"},
			mustHide: []string{"********"},
		},
		{
			name:     "empty old returns new",
			old:      "",
			new:      `{"password":"********"}`,
			wantSame: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeRedactedSecrets(tc.old, tc.new)
			if tc.wantSame {
				if got != tc.new {
					t.Fatalf("expected unchanged %q, got %q", tc.new, got)
				}
				return
			}
			for _, h := range tc.mustHave {
				if !strings.Contains(got, h) {
					t.Errorf("merged blob missing %q: %s", h, got)
				}
			}
			for _, h := range tc.mustHide {
				if strings.Contains(got, h) {
					t.Errorf("merged blob unexpectedly contains %q: %s", h, got)
				}
			}
		})
	}
}

// TestMetadataStoreToResponse_RedactsConfig verifies the metadata store
// READ-path response scrubs the postgres password (round-2 §2.1).
func TestMetadataStoreToResponse_RedactsConfig(t *testing.T) {
	m := &models.MetadataStoreConfig{
		ID:        "id1",
		Name:      "pg",
		Type:      "postgres",
		Config:    `{"host":"db","password":"LEAKED-PG-PASSWORD"}`,
		CreatedAt: time.Now(),
	}
	resp := metadataStoreToResponse(m)
	if strings.Contains(resp.Config, "LEAKED-PG-PASSWORD") {
		t.Fatalf("metadata store response leaked secret: %s", resp.Config)
	}
	if !strings.Contains(resp.Config, "db") {
		t.Fatalf("non-secret host dropped: %s", resp.Config)
	}
}
