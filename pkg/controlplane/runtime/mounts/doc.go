// Package mounts provides unified mount tracking across all protocol adapters.
//
// The Tracker manages active mount records for NFS, SMB, and other protocol
// adapters, providing thread-safe recording, removal, and querying of
// mount state.
package mounts
