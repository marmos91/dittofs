package auth

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

func TestAdvertisedAuthFlavors(t *testing.T) {
	const authUnix = int32(1)
	krb5 := int32(gss.PseudoFlavorKrb5)
	krb5i := int32(gss.PseudoFlavorKrb5i)
	krb5p := int32(gss.PseudoFlavorKrb5p)

	cases := []struct {
		name      string
		share     *runtime.Share
		kerberos  bool
		want      []int32
		rationale string
	}{
		{
			name:      "no kerberos configured, share allows authsys",
			share:     &runtime.Share{AllowAuthSys: true},
			kerberos:  false,
			want:      []int32{authUnix},
			rationale: "nothing to advertise beyond AUTH_UNIX without a GSS processor",
		},
		{
			name:      "share forbids authsys",
			share:     &runtime.Share{AllowAuthSys: false},
			kerberos:  false,
			want:      nil,
			rationale: "the mount path denies AUTH_SYS here, so offering it advertises a mount that cannot succeed",
		},
		{
			name:      "share requires kerberos",
			share:     &runtime.Share{AllowAuthSys: true, RequireKerberos: true},
			kerberos:  true,
			want:      []int32{krb5, krb5i, krb5p},
			rationale: "AUTH_SYS is refused by RequireKerberos even though AllowAuthSys is set",
		},
		{
			name:      "krb5p floor hides the weaker pseudoflavors",
			share:     &runtime.Share{AllowAuthSys: true, MinKerberosLevel: models.KerberosLevelKrb5p},
			kerberos:  true,
			want:      []int32{authUnix, krb5p},
			rationale: "krb5 and krb5i are below the floor and would be denied on arrival",
		},
		{
			name:      "krb5i floor keeps krb5i and krb5p",
			share:     &runtime.Share{AllowAuthSys: true, MinKerberosLevel: models.KerberosLevelKrb5i},
			kerberos:  true,
			want:      []int32{authUnix, krb5i, krb5p},
			rationale: "the floor is a minimum, not an exact match",
		},
		{
			name:      "no floor offers all three",
			share:     &runtime.Share{AllowAuthSys: true},
			kerberos:  true,
			want:      []int32{authUnix, krb5, krb5i, krb5p},
			rationale: "an unset floor enforces nothing on arrival either",
		},
		{
			name:      "authsys forbidden and no kerberos accepts nothing",
			share:     &runtime.Share{AllowAuthSys: false},
			kerberos:  false,
			want:      nil,
			rationale: "an empty list is the honest answer, not a reason to fall back to AUTH_UNIX",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AdvertisedAuthFlavors(tc.share, tc.kerberos)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v — %s", got, tc.want, tc.rationale)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v — %s", got, tc.want, tc.rationale)
				}
			}
		})
	}
}
