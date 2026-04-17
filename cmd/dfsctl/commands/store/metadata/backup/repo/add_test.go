package repo

import (
	"strings"
	"testing"
)

// resetAddFlags resets package-level flag state between tests.
// runAdd/buildAddRequest read these globals directly (cobra pattern).
func resetAddFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		addName = ""
		addKind = ""
		addConfigJSON = ""
		addSchedule = ""
		addKeepCount = 0
		addKeepAgeDays = 0
		addEncryption = ""
		addEncryptionKeyRef = ""
		addPath = ""
		addBucket = ""
		addRegion = ""
		addEndpoint = ""
		addPrefix = ""
		addAccessKeyID = ""
		addSecretAccessKey = ""
	})
}

func TestRepoAdd_LocalKind_FlagsOnly(t *testing.T) {
	resetAddFlags(t)

	addName = "nightly-local"
	addKind = "local"
	addPath = "/var/backups/dittofs"

	req, err := buildAddRequest()
	if err != nil {
		t.Fatalf("buildAddRequest: %v", err)
	}
	if req.Name != "nightly-local" {
		t.Errorf("Name = %q, want nightly-local", req.Name)
	}
	if req.Kind != "local" {
		t.Errorf("Kind = %q, want local", req.Kind)
	}
	if got, _ := req.Config["path"].(string); got != "/var/backups/dittofs" {
		t.Errorf("Config[path] = %v, want /var/backups/dittofs", req.Config["path"])
	}
	if req.Schedule != nil {
		t.Errorf("Schedule should be nil, got %v", req.Schedule)
	}
	if req.KeepCount != nil {
		t.Errorf("KeepCount should be nil, got %v", req.KeepCount)
	}
	if req.KeepAgeDays != nil {
		t.Errorf("KeepAgeDays should be nil, got %v", req.KeepAgeDays)
	}
	if req.EncryptionEnabled != nil {
		t.Errorf("EncryptionEnabled should be nil, got %v", req.EncryptionEnabled)
	}
}

func TestRepoAdd_S3Kind_BuildsConfig(t *testing.T) {
	resetAddFlags(t)

	addName = "daily-s3"
	addKind = "s3"
	addBucket = "my-backups"
	addRegion = "us-east-1"
	addAccessKeyID = "AKIAFAKE"
	addSecretAccessKey = "SECRETFAKE"
	addSchedule = "CRON_TZ=UTC 0 2 * * *"
	addKeepCount = 7

	req, err := buildAddRequest()
	if err != nil {
		t.Fatalf("buildAddRequest: %v", err)
	}
	if req.Kind != "s3" {
		t.Errorf("Kind = %q, want s3", req.Kind)
	}
	for k, want := range map[string]string{
		"bucket":            "my-backups",
		"region":            "us-east-1",
		"access_key_id":     "AKIAFAKE",
		"secret_access_key": "SECRETFAKE",
	} {
		got, _ := req.Config[k].(string)
		if got != want {
			t.Errorf("Config[%q] = %q, want %q", k, got, want)
		}
	}
	if req.Schedule == nil || *req.Schedule != "CRON_TZ=UTC 0 2 * * *" {
		t.Errorf("Schedule mismatch: %v", req.Schedule)
	}
	if req.KeepCount == nil || *req.KeepCount != 7 {
		t.Errorf("KeepCount mismatch: %v", req.KeepCount)
	}
}

func TestRepoAdd_ConfigFile_Expands(t *testing.T) {
	resetAddFlags(t)

	addName = "from-file"
	addKind = "s3"
	addConfigJSON = "@testdata/repo-s3.json"

	req, err := buildAddRequest()
	if err != nil {
		t.Fatalf("buildAddRequest: %v", err)
	}
	for k, want := range map[string]string{
		"bucket":            "from-file-bucket",
		"region":            "eu-west-1",
		"prefix":            "meta-backups/",
		"access_key_id":     "AKIAFROMFILE",
		"secret_access_key": "secretfromfile",
	} {
		got, _ := req.Config[k].(string)
		if got != want {
			t.Errorf("Config[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestRepoAdd_ConfigInlineJSON_Expands(t *testing.T) {
	resetAddFlags(t)

	addName = "inline-json"
	addKind = "s3"
	addConfigJSON = `{"bucket":"inline","region":"eu-west-2"}`

	req, err := buildAddRequest()
	if err != nil {
		t.Fatalf("buildAddRequest: %v", err)
	}
	if got, _ := req.Config["bucket"].(string); got != "inline" {
		t.Errorf("bucket = %q, want inline", got)
	}
	if got, _ := req.Config["region"].(string); got != "eu-west-2" {
		t.Errorf("region = %q, want eu-west-2", got)
	}
}

func TestRepoAdd_EncryptionOn_RequiresKeyRef(t *testing.T) {
	resetAddFlags(t)

	addName = "enc-no-keyref"
	addKind = "local"
	addPath = "/tmp/x"
	addEncryption = "on"
	// Intentionally no --encryption-key-ref

	_, err := buildAddRequest()
	if err == nil {
		t.Fatal("expected error when --encryption on without --encryption-key-ref")
	}
	if !strings.Contains(err.Error(), "requires --encryption-key-ref") {
		t.Errorf("error = %q, want message containing 'requires --encryption-key-ref'", err.Error())
	}
}

func TestRepoAdd_EncryptionOn_WithKeyRef_Succeeds(t *testing.T) {
	resetAddFlags(t)

	addName = "enc-ok"
	addKind = "local"
	addPath = "/tmp/x"
	addEncryption = "on"
	addEncryptionKeyRef = "env:BACKUP_KEY"

	req, err := buildAddRequest()
	if err != nil {
		t.Fatalf("buildAddRequest: %v", err)
	}
	if req.EncryptionEnabled == nil || !*req.EncryptionEnabled {
		t.Errorf("EncryptionEnabled should be true, got %v", req.EncryptionEnabled)
	}
	if req.EncryptionKeyRef == nil || *req.EncryptionKeyRef != "env:BACKUP_KEY" {
		t.Errorf("EncryptionKeyRef = %v, want env:BACKUP_KEY", req.EncryptionKeyRef)
	}
}

func TestRepoAdd_EncryptionOff_AllowedWithoutKeyRef(t *testing.T) {
	resetAddFlags(t)

	addName = "enc-off"
	addKind = "local"
	addPath = "/tmp/x"
	addEncryption = "off"

	req, err := buildAddRequest()
	if err != nil {
		t.Fatalf("buildAddRequest: %v", err)
	}
	if req.EncryptionEnabled == nil || *req.EncryptionEnabled {
		t.Errorf("EncryptionEnabled should be false, got %v", req.EncryptionEnabled)
	}
}

func TestRepoAdd_UnknownKind_Rejected(t *testing.T) {
	resetAddFlags(t)

	addName = "weird"
	addKind = "azure"

	_, err := buildAddRequest()
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
	if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("error = %q, want message containing 'unknown kind'", err.Error())
	}
}
