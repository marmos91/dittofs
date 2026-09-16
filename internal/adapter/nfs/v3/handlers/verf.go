package handlers

import "time"

// Server Instance Tracking

// serverBootTime is this instance's write verifier, minted once at startup and
// reported by every WRITE and COMMIT reply.
var serverBootTime = newWriteVerifier()

// newWriteVerifier mints the WRITE/COMMIT write verifier for this server
// instance (RFC 1813 3.3.7 / 3.3.21).
//
// Its entire job is to differ from the verifier of any previous instance: a
// client re-sends an UNSTABLE WRITE only when the verifier in the COMMIT reply
// differs from the one the WRITE returned, so two instances minting the same
// value tell the client no restart happened and it drops the writes the restart
// lost.
//
// Nanosecond resolution is what makes that hold. A crash-restart cycle
// completes well inside one wall-clock second, so a seconds-resolution value
// repeats across exactly the failure this mechanism exists to report.
func newWriteVerifier() uint64 {
	return uint64(time.Now().UnixNano())
}
