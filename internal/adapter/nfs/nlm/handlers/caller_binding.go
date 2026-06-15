package handlers

import (
	"net"
	"sync"
)

// MaxCallerNameLen bounds an NLM caller_name (a client hostname). RFC 1813
// MAXNAMELEN is 1024; anything larger is rejected as malformed/abusive.
const MaxCallerNameLen = 1024

// callerBinding maps an NLM caller_name to the transport source host (IP) that
// first acquired a lock under that name. NLM v4 is advisory and unauthenticated
// by RFC 1813 design: every field of an UNLOCK/CANCEL request (caller_name,
// svid, owner handle) is client-supplied, so without binding any peer that can
// reach the NLM port could reconstruct a victim's ownerID and release or cancel
// that victim's byte-range lock (lock theft / silent data corruption between
// NFSv3 writers).
//
// This binding closes that gap to the extent the transport allows: a caller_name
// is pinned to the source host that introduced it, and a request from a
// different source host claiming that caller_name is rejected. It does not (and
// cannot, without per-RPC authentication) defend against a same-host spoofer.
type callerBinding struct {
	mu      sync.RWMutex
	hostFor map[string]string // caller_name -> source host (IP)
}

func newCallerBinding() *callerBinding {
	return &callerBinding{hostFor: make(map[string]string)}
}

// hostOf extracts the host (IP) portion from a "host:port" transport address.
// If the address has no port it is returned unchanged.
func hostOf(clientAddr string) string {
	host, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		return clientAddr
	}
	return host
}

// bind records that callerName was used from sourceHost. The first source host
// to use a caller_name owns it; subsequent locks from the same host refresh it.
// An empty callerName or sourceHost is ignored.
func (b *callerBinding) bind(callerName, clientAddr string) {
	if b == nil || callerName == "" {
		return
	}
	host := hostOf(clientAddr)
	if host == "" {
		return
	}
	b.mu.Lock()
	b.hostFor[callerName] = host
	b.mu.Unlock()
}

// authorized reports whether a request from clientAddr may act on callerName.
//
// It is allowed when the caller_name is unbound (first use / lock not seen by
// this server instance) or when the request's source host matches the bound
// host. It is denied only when the caller_name is bound to a DIFFERENT host,
// which is the lock-theft case.
func (b *callerBinding) authorized(callerName, clientAddr string) bool {
	if b == nil || callerName == "" {
		return true
	}
	b.mu.RLock()
	bound, ok := b.hostFor[callerName]
	b.mu.RUnlock()
	if !ok {
		return true
	}
	return bound == hostOf(clientAddr)
}
