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
