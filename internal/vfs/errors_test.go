package vfs

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// The store contradicting itself is ErrInconsistent, not a store failure.
func TestAnInconsistentStoreIsErrInconsistent(t *testing.T) {
	for _, err := range []error{entity.ErrInconsistent, fmt.Errorf("%w: refcount below zero", kvstore.ErrInconsistent)} {
		if got := storeErr(err); !errors.Is(got, vfs.ErrInconsistent) {
			t.Errorf("storeErr(%v): %v", err, got)
		}
	}
}

// The store's lock, grace, conflict and cross-share codes are the service's
// own errors, not a failed store.
func TestTheStoresRetryAndLockCodesAreTheirOwn(t *testing.T) {
	var delay *vfs.Delay
	var cross *vfs.CrossError
	for code, is := range map[merrs.ErrorCode]func(error) bool{
		merrs.ErrLocked:       func(e error) bool { return errors.Is(e, vfs.ErrLocked) },
		merrs.ErrLockConflict: func(e error) bool { return errors.Is(e, vfs.ErrLocked) },
		merrs.ErrGracePeriod:  func(e error) bool { return errors.Is(e, vfs.ErrGrace) },
		merrs.ErrConflict:     func(e error) bool { return errors.As(e, &delay) && delay.Cause == vfs.DelayConflict },
		merrs.ErrCrossShare:   func(e error) bool { return errors.As(e, &cross) && cross.Kind == vfs.CrossShare },
	} {
		if got := storeErr(&merrs.StoreError{Code: code, Message: "m"}); !is(got) || errors.Is(got, vfs.ErrUnavailable) {
			t.Errorf("storeErr of code %v: %v", code, got)
		}
	}
}
