package handlers

import "time"

// ============================================================================
// Server Instance Tracking
// ============================================================================

// serverBootTime stores the time when the NFS server started.
// This is used as the write verifier to help clients detect server restarts.
// When a server restarts, any unstable writes are lost, so clients must
// re-send them. The verifier changing indicates a restart occurred.
var serverBootTime = uint64(time.Now().Unix())
