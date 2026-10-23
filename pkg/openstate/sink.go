package openstate

import "github.com/marmos91/dittofs/pkg/metadata/entity"

// Sink is how open state reaches clients and the service: every other call
// goes into the table, and these come out of it. The service implements it.
// Each method is called after the step that raised it has released every
// table lock, by the goroutine that ran it, and only queues and returns.
type Sink interface {
	// ClientEnded: this node ended a client; the service ends it at each
	// shard's primary.
	ClientEnded(c entity.ClientID, shards []ShardID, why RevokeReason)
	// Closed: open state closed opens of f itself, as an expiry does, and
	// left f an orphan or a pending unlink; the service runs r as it runs a
	// Close's. One call per file carries everything the release left there.
	Closed(f entity.FileID, r CloseResult)
}

// Order takes a file's admission lock and returns its unlock. The table does
// not take it yet.
type Order func(f entity.FileID) (unlock func())

// RevokeReason is why a client's state ended without the client asking.
type RevokeReason uint8

const (
	RevokedLeaseExpired RevokeReason = iota // its lease expired, or a reboot replaced it
)
