// Package openstate is the open-state table: who holds each file open, who
// locked which bytes, what each client may cache, and what happens to all of
// it when a client, a server or a node goes away.
//
// The table is held in memory, one entry per file at the file's primary, each
// entry serialised by its own step. Only the pieces that must outlive a
// restart reach the metadata store, through its open-state views; everything
// else is rebuilt by reclaim after a loss. The filesystem service is the only
// caller: adapters never import this package.
package openstate
