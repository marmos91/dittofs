// Package mount implements the NFS Mount protocol (RFC 1813 Appendix I).
//
// The Mount protocol is a separate RPC program that runs alongside the NFS protocol.
// It provides the following procedures:
//
//   - NULL: Connectivity test (does nothing)
//   - MOUNT (MNT): Obtain a file handle for an export path
//   - DUMP: List all active mounts
//   - UMOUNT (UMNT): Remove a specific mount entry
//   - UMOUNTALL (UMNTALL): Remove all mount entries for a client
//   - EXPORT: List all available exports
//
// Design principles:
//
//   - Protocol layer handles only encoding/decoding and RPC concerns
//   - All business logic is delegated to the metadata.Repository interface
//   - Access control, authentication, and mount tracking are repository responsibilities
//   - Context structs provide request metadata (client address, auth info)
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// Separation of concerns:
//
//   - This package: XDR encoding/decoding, RPC message handling
//   - metadata.Repository: Export configuration, access control, mount tracking
//   - server package: Network handling, RPC routing
//
// Example usage:
//
//	handler := &mount.DefaultMountHandler{}
//	ctx := &mount.MountContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: rpc.AuthUnix,
//	}
//	resp, err := handler.Mount(repository, request, ctx)
package mount
