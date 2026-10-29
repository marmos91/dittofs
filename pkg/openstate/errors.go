package openstate

import "errors"

// The filesystem service maps each of these once to its own neutral errors.
var (
	ErrBadState = errors.New("openstate: reference names no record")
	ErrNotYours = errors.New("openstate: state held by another client or principal")
	ErrOldState = errors.New("openstate: reference sequence older than the state's")
)
