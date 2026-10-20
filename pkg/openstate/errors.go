package openstate

import "errors"

// The filesystem service maps each of these once to its own neutral errors.
var (
	ErrBadState   = errors.New("openstate: stateid names no record")
	ErrStaleState = errors.New("openstate: state lost with its shard's open state")
	ErrNotYours   = errors.New("openstate: state held by another client")
	ErrOldState   = errors.New("openstate: reference sequence older than the state's")
)
