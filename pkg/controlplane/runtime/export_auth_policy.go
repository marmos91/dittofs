package runtime

// ExportAcceptsNoAuthFlavor reports whether a share's NFS export policy leaves
// no auth flavor a client could ever use.
//
// The flavor set an export advertises is built in the NFS adapter
// (auth.AdvertisedAuthFlavors), which cannot be called from here — it takes a
// runtime share, so importing it would close a cycle. This is therefore a
// second statement of the same rule, and the two are pinned to each other by a
// test in that package rather than kept in step by hand.
//
// The rule: AUTH_UNIX is offered only to a share that allows AUTH_SYS and does
// not mandate Kerberos, and Kerberos pseudoflavors are offered only when the
// server has Kerberos at all. A server that has it always offers at least krb5p,
// which carries the highest protection service and so satisfies any floor a
// share can set — which is why the Kerberos case needs no floor comparison here.
func ExportAcceptsNoAuthFlavor(requireKerberos, allowAuthSys, kerberosEnabled bool) bool {
	if kerberosEnabled {
		return false
	}
	return requireKerberos || !allowAuthSys
}

// ExportNoAuthFlavorCause names the settings that leave an export accepting no
// auth flavor, and the command that repairs them. It is only meaningful when
// ExportAcceptsNoAuthFlavor reports true.
//
// Both flags can be at fault at once, and clearing only one leaves the export
// just as unreachable: re-allowing AUTH_SYS while require_kerberos still stands
// on a server with no Kerberos refuses AUTH_SYS again on the next boot. The
// two-flag case therefore gets a command that sets both, so following the
// remedy actually produces a share that serves.
func ExportNoAuthFlavorCause(shareName string, requireKerberos, allowAuthSys bool) (cause, remedy string) {
	set := "`dfsctl share nfs-config set " + shareName + " "
	switch {
	case requireKerberos && !allowAuthSys:
		return "require_kerberos is set and allow_auth_sys is off",
			set + "--require-kerberos false --allow-auth-sys true`"
	case requireKerberos:
		return "require_kerberos is set", set + "--require-kerberos false`"
	default:
		return "allow_auth_sys is off", set + "--allow-auth-sys true`"
	}
}
