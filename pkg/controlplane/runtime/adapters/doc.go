// Package adapters provides protocol adapter lifecycle management.
//
// The Service manages protocol adapter (NFS, SMB) creation, startup,
// shutdown, and configuration. It coordinates with the persistent store
// to ensure adapter configurations are saved alongside in-memory state.
package adapters
