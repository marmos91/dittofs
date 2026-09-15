package auth

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// TestAdvertisedFlavorsAgreesWithExportAcceptsNoAuthFlavor pins two statements
// of one rule to each other.
//
// AdvertisedAuthFlavors builds the flavor set an export offers. The share load
// refuses to boot a share whose set would be empty, but it cannot call this
// function — this package imports runtime, so runtime calling back would close
// a cycle. The rule therefore exists twice, and two copies of a rule drift:
// this test fails the moment they disagree, which is the only thing standing
// between them.
func TestAdvertisedFlavorsAgreesWithExportAcceptsNoAuthFlavor(t *testing.T) {
	levels := []string{"", "krb5", "krb5i", "krb5p"}

	for _, requireKerberos := range []bool{false, true} {
		for _, allowAuthSys := range []bool{false, true} {
			for _, kerberosEnabled := range []bool{false, true} {
				for _, minLevel := range levels {
					share := &runtime.Share{
						Name:             "/export",
						RequireKerberos:  requireKerberos,
						AllowAuthSys:     allowAuthSys,
						MinKerberosLevel: minLevel,
					}

					advertised := AdvertisedAuthFlavors(share, kerberosEnabled)
					wantEmpty := runtime.ExportAcceptsNoAuthFlavor(requireKerberos, allowAuthSys, kerberosEnabled)

					if gotEmpty := len(advertised) == 0; gotEmpty != wantEmpty {
						t.Errorf("require_kerberos=%v allow_auth_sys=%v kerberos_enabled=%v min_level=%q: "+
							"AdvertisedAuthFlavors returned %d flavors but ExportAcceptsNoAuthFlavor says empty=%v; "+
							"the boot refusal and the advertised set no longer agree on which shares are unusable",
							requireKerberos, allowAuthSys, kerberosEnabled, minLevel, len(advertised), wantEmpty)
					}
				}
			}
		}
	}
}
