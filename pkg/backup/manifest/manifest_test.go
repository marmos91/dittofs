package manifest

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fullyPopulated returns a manifest with every field set to a non-zero value.
func fullyPopulated(t *testing.T) *Manifest {
	t.Helper()
	return &Manifest{
		ManifestVersion: CurrentVersion,
		BackupID:        "01HKQ2C5XY7N8P9Q0RSTUVWXYZ",
		CreatedAt:       time.Date(2026, 4, 15, 12, 34, 56, 0, time.UTC),
		StoreID:         "store-uuid-1",
		StoreKind:       "badger",
		SHA256:          "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		SizeBytes:       12345,
		Encryption: Encryption{
			Enabled:   true,
			Algorithm: "aes-256-gcm",
			KeyRef:    "env:BACKUP_KEY",
		},
		PayloadIDSet: []string{"a", "b", "c"},
		EngineMetadata: map[string]string{
			"badger_version": "v4",
			"schema":         "1",
		},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	in := fullyPopulated(t)

	data, err := in.Marshal()
	require.NoError(t, err)

	out, err := Parse(data)
	require.NoError(t, err)

	require.Equal(t, in.ManifestVersion, out.ManifestVersion)
	require.Equal(t, in.BackupID, out.BackupID)
	require.True(t, in.CreatedAt.Equal(out.CreatedAt), "CreatedAt round-trip: in=%s out=%s", in.CreatedAt, out.CreatedAt)
	require.Equal(t, in.StoreID, out.StoreID)
	require.Equal(t, in.StoreKind, out.StoreKind)
	require.Equal(t, in.SHA256, out.SHA256)
	require.Equal(t, in.SizeBytes, out.SizeBytes)
	require.Equal(t, in.Encryption, out.Encryption)
	require.Equal(t, in.PayloadIDSet, out.PayloadIDSet)
	require.Equal(t, in.EngineMetadata, out.EngineMetadata)
}

func TestManifestVersionGuard_RejectsZero(t *testing.T) {
	var m Manifest
	err := m.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "manifest_version is required")
}

func TestManifestVersionGuard_RejectsFuture(t *testing.T) {
	m := fullyPopulated(t)
	m.ManifestVersion = 999
	err := m.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported manifest_version")
}

func TestManifestVersionGuard_AcceptsCurrent(t *testing.T) {
	m := fullyPopulated(t)
	require.NoError(t, m.Validate())
}

func TestManifestRequiredFields(t *testing.T) {
	cases := []struct {
		name  string
		mutate func(*Manifest)
		want  string
	}{
		{"BackupID", func(m *Manifest) { m.BackupID = "" }, "backup_id"},
		{"StoreID", func(m *Manifest) { m.StoreID = "" }, "store_id"},
		{"StoreKind", func(m *Manifest) { m.StoreKind = "" }, "store_kind"},
		{"SHA256", func(m *Manifest) { m.SHA256 = "" }, "sha256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fullyPopulated(t)
			tc.mutate(m)
			err := m.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestManifestEncryptionOmitEmpty(t *testing.T) {
	m := fullyPopulated(t)
	m.Encryption = Encryption{Enabled: false}

	data, err := m.Marshal()
	require.NoError(t, err)
	out := string(data)

	require.NotContains(t, out, "algorithm:")
	require.NotContains(t, out, "key_ref:")
}

func TestManifestPayloadIDSetAlwaysPresent(t *testing.T) {
	m := fullyPopulated(t)
	m.PayloadIDSet = []string{}

	data, err := m.Marshal()
	require.NoError(t, err)
	out := string(data)

	require.Contains(t, out, "payload_id_set:",
		"payload_id_set must appear in serialized YAML even when empty (SAFETY-01)")
}

func TestManifestPayloadIDSetDeterministic(t *testing.T) {
	m1 := fullyPopulated(t)
	m2 := fullyPopulated(t)
	m1.PayloadIDSet = []string{"a", "b", "c"}
	m2.PayloadIDSet = []string{"a", "b", "c"}

	d1, err := m1.Marshal()
	require.NoError(t, err)
	d2, err := m2.Marshal()
	require.NoError(t, err)

	require.Equal(t, d1, d2, "marshal output must be byte-identical for identical input (reproducible SHA-256)")
}

func TestManifestWriteThenRead(t *testing.T) {
	in := fullyPopulated(t)

	var buf bytes.Buffer
	n, err := in.WriteTo(&buf)
	require.NoError(t, err)
	require.Greater(t, n, int64(0))

	out, err := ReadFrom(&buf)
	require.NoError(t, err)

	require.Equal(t, in.BackupID, out.BackupID)
	require.Equal(t, in.StoreID, out.StoreID)
	require.Equal(t, in.StoreKind, out.StoreKind)
	require.Equal(t, in.SHA256, out.SHA256)
	require.Equal(t, in.Encryption, out.Encryption)
	require.Equal(t, in.PayloadIDSet, out.PayloadIDSet)
	require.Equal(t, in.EngineMetadata, out.EngineMetadata)
	require.True(t, in.CreatedAt.Equal(out.CreatedAt))
}

func TestParseRejectsBrokenYAML(t *testing.T) {
	_, err := Parse([]byte("not: valid: yaml: here:"))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "decode manifest"),
		"expected error to wrap with 'decode manifest', got: %v", err)
}
