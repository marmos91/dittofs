// Package identity provides share-level identity mapping.
//
// The Service implements Synology-style squash modes for NFS identity
// mapping: none, root_to_admin, root_to_guest, all_to_admin, all_to_guest.
// AUTH_NULL connections are always mapped to the anonymous identity.
package identity
