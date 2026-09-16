package handlers

import "time"

// Server Instance Tracking

// serverBootTime stores the time when the NFS server started.
// This is used as the write verifier to help clients detect server restarts.
// When a server restarts, any unstable writes are lost, so clients must
// re-send them. The verifier changing indicates a restart occurred.
var serverBootTime = newWriteVerifier()

// newWriteVerifier mints the WRITE/COMMIT write verifier for this server
// instance (RFC 1813 3.3.7 / 3.3.21).
//
// Its entire job is to differ from the verifier of any previous instance: a
// client compares the value it got from an UNSTABLE WRITE against the one in
// the COMMIT reply and re-sends the data only when they differ. Two instances
// that mint the same value tell the client no restart happened, so it drops
// writes the restart lost.
//
// Nanosecond resolution is what makes that hold. A crash-restart cycle — a
// container restart, a supervisor with an immediate restart policy — completes
// well inside one wall-clock second, so a seconds-resolution value repeats
// across exactly the failure this mechanism exists to report.
func newWriteVerifier() uint64 {
	return uint64(time.Now().UnixNano())
}
