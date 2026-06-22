//go:build ad_dc

//go:debug x509negativeserial=1

// Samba AD-DC ships TLS certs with negative serial numbers, which Go rejects at
// parse time since 1.23. cmd/dfs/main.go sets this same godebug for the server
// binary (#1289); the ad-dc test binary needs it too so in-process LDAPS clients
// (e.g. the LDAP resolver tests) can complete the handshake.
package ad_dc_test
