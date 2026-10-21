//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/test/e2e/helpers"
)

// TestSMBSharePolicy applies the share's access policy over the SMB wire. Every
// session authenticates with NTLM from the loopback address. The server has
// Kerberos configured only so a share may require it: no KDC is involved,
// because the refusal comes before any Kerberos would be needed.
func TestSMBSharePolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB share policy tests in short mode")
	}

	dir := t.TempDir()
	krb5conf := filepath.Join(dir, "krb5.conf")
	keytab := filepath.Join(dir, "empty.keytab")
	require.NoError(t, os.WriteFile(krb5conf, []byte("[libdefaults]\n    default_realm = EXAMPLE.TEST\n"), 0o644))
	require.NoError(t, os.WriteFile(keytab, []byte{0x05, 0x02, 0, 0, 0, 0}, 0o600))
	t.Setenv("DITTOFS_KERBEROS_ENABLED", "true")
	t.Setenv("DITTOFS_KERBEROS_PRINCIPAL", "nfs/localhost@EXAMPLE.TEST")
	t.Setenv("DITTOFS_KERBEROS_KRB5CONF", krb5conf)
	t.Setenv("DITTOFS_KERBEROS_KEYTAB", keytab)

	env := helpers.SetupSMB3TestEnv(t)
	shareName := strings.TrimPrefix(env.ShareName, "/")

	// setPolicy changes the share's policy and puts it back when the subtest ends.
	setPolicy := func(t *testing.T, set, reset []string) {
		t.Helper()
		run := func(args []string) error {
			_, err := env.CLIRunner.Run(append([]string{"share", "nfs-config", "set", env.ShareName}, args...)...)
			return err
		}
		require.NoError(t, run(set))
		t.Cleanup(func() { require.NoError(t, run(reset)) })
	}

	t.Run("NTLMRefusedOnKerberosOnlyShare", func(t *testing.T) {
		session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
		helpers.MountSMB3Share(t, session, env.ShareName)

		setPolicy(t, []string{"--require-kerberos", "true"}, []string{"--require-kerberos", "false"})

		_, err := session.Mount(shareName)
		require.ErrorIs(t, err, os.ErrPermission, "an NTLM session must not connect to a Kerberos-only share")
	})

	t.Run("ClientOutsideNetgroupRefused", func(t *testing.T) {
		session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
		helpers.MountSMB3Share(t, session, env.ShareName)

		netgroup := helpers.UniqueTestName("office")
		_, err := env.CLIRunner.Run("netgroup", "create", "--name", netgroup)
		require.NoError(t, err)
		_, err = env.CLIRunner.Run("netgroup", "add-member", netgroup, "--type", "cidr", "--value", "10.99.99.0/24")
		require.NoError(t, err)
		setPolicy(t, []string{"--netgroup", netgroup}, []string{"--netgroup", ""})

		_, err = session.Mount(shareName)
		require.ErrorIs(t, err, os.ErrPermission, "a client outside the share's netgroup must not connect")
	})

	t.Run("EstablishedTreeRefusedWhenPolicyTightens", func(t *testing.T) {
		session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
		share := helpers.MountSMB3Share(t, session, env.ShareName)
		require.NoError(t, share.WriteFile("before.txt", []byte("admitted"), 0o644))

		setPolicy(t, []string{"--require-kerberos", "true"}, []string{"--require-kerberos", "false"})

		// The policy is applied again on every request on the tree, so the
		// change takes effect on the next one.
		_, err := share.Stat("before.txt")
		require.ErrorIs(t, err, os.ErrPermission, "a request on the established tree must be refused once the policy refuses its session")
	})
}
