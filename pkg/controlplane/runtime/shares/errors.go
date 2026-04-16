// Package shares provides typed errors for share lifecycle operations.
package shares

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ErrShareAlreadyDisabled is returned when DisableShare is called on a
// share whose DB row already has enabled=false. Callers may treat this
// as a benign no-op or surface it (Phase-5 restore treats as OK).
var ErrShareAlreadyDisabled = errors.New("share is already disabled")

// ErrShareStillInUse is returned when DisableShare completes the DB
// write and runtime flip but the adapter callbacks time out before
// tearing down all active connections. DisableShare returns success
// anyway (D-03 — the side-engine swap is safe regardless); callers
// may log loudly.
var ErrShareStillInUse = errors.New("share still has active mounts after disable timeout")

// ErrShareNotFound re-exports models.ErrShareNotFound to preserve
// errors.Is matching across package boundaries.
var ErrShareNotFound = models.ErrShareNotFound
