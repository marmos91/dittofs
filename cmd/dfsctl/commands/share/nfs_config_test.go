package share

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// rowValue returns the value column for field, or "" when the row is absent.
func rowValue(rows [][]string, field string) string {
	for _, r := range rows {
		if len(r) == 2 && r[0] == field {
			return r[1]
		}
	}
	return ""
}

func TestNFSConfigDetail_Rows_Defaults(t *testing.T) {
	rows := NFSConfigDetail{cfg: &apiclient.ShareNFSConfig{
		Squash:       "none",
		AllowAuthSys: true,
	}}.Rows()

	// An unset netgroup means every client is allowed; say so rather than
	// rendering an empty cell the reader has to interpret.
	if got := rowValue(rows, "Netgroup"); got == "" || got == "(none)" {
		t.Errorf("Netgroup = %q, want an explicit all-clients-allowed value", got)
	}
	// DisableReaddirplus is stored negated, so a zero value must read as enabled.
	if got := rowValue(rows, "READDIRPLUS"); got != "true" {
		t.Errorf("READDIRPLUS = %q, want true when DisableReaddirplus is false", got)
	}
	// Unset anonymous IDs are server defaults, not export settings: omitted.
	if got := rowValue(rows, "Anonymous UID"); got != "" {
		t.Errorf("Anonymous UID rendered as %q despite being unset", got)
	}
}

func TestNFSConfigDetail_Rows_Overrides(t *testing.T) {
	uid, gid := uint32(65534), uint32(65533)
	rows := NFSConfigDetail{cfg: &apiclient.ShareNFSConfig{
		Squash:             "all_to_guest",
		AnonymousUID:       &uid,
		AnonymousGID:       &gid,
		RequireKerberos:    true,
		MinKerberosLevel:   "krb5p",
		Netgroup:           "office",
		DisableReaddirplus: true,
	}}.Rows()

	for field, want := range map[string]string{
		"Netgroup":           "office",
		"Squash":             "all_to_guest",
		"Anonymous UID":      "65534",
		"Anonymous GID":      "65533",
		"Allow AUTH_SYS":     "false",
		"Require Kerberos":   "true",
		"Min Kerberos level": "krb5p",
		"READDIRPLUS":        "false",
	} {
		if got := rowValue(rows, field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}
