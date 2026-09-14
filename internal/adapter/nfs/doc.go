// Package nfs implements the RPC record layer beneath every ONC RPC program
// the server speaks.
//
// Its subject is the framing rather than the protocols: reading a record off a
// stream, reassembling the fragments a record is split across, rejecting a
// declared fragment size before it is used to size anything, and
// demultiplexing a backchannel reply that arrives interleaved with
// fore-channel calls. The exception is status_string.go, which renders NFSv3
// and MOUNT status codes for logs; it is shared by callers on both sides of
// this boundary and has not been given a better home.
//
// The programs themselves live beside this package, each owning its own
// procedure table and dispatch: v3, mount, nlm, nsm and portmap. Routing a
// decoded call to one of them is the connection layer's job in
// pkg/adapter/nfs, which is the only caller of this package.
//
// # Fragment reassembly
//
// An RPC record is a sequence of fragments, each prefixed with a four-byte
// header whose top bit marks the last one. A peer controls both the fragment
// count and the size each fragment declares, so both are bounded here: the
// declared size is validated before allocation, and the reassembled total is
// checked again, because a record within the limit at every individual
// fragment can still exceed it once joined.
package nfs
