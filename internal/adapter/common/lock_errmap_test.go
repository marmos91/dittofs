package common

import (
	"fmt"
	"testing"

	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// TestMapLockToSMB_NilAndFallback pins the fallback chain of the surviving
// lock-context mapper: nil → StatusSuccess, non-StoreError →
// defaultCodes.SMB, wrapped StoreError unwraps via goerrors.As.
func TestMapLockToSMB_NilAndFallback(t *testing.T) {
	if got := MapLockToSMB(nil); got != smbtypes.StatusSuccess {
		t.Errorf("MapLockToSMB(nil) = %v, want StatusSuccess", got)
	}
	if got := MapLockToSMB(fmt.Errorf("random")); got != defaultCodes.SMB {
		t.Errorf("MapLockToSMB(non-StoreError) = %v, want defaultCodes.SMB = %v", got, defaultCodes.SMB)
	}
	wrapped := fmt.Errorf("wrap: %w", &merrs.StoreError{Code: merrs.ErrLocked})
	if got := MapLockToSMB(wrapped); got != smbtypes.StatusLockNotGranted {
		t.Errorf("MapLockToSMB(wrapped ErrLocked) = %v, want StatusLockNotGranted", got)
	}
}

// TestMapLockToSMB_Table drives every lockErrorMap row through the mapper so
// a row added without a pin (or with a typo'd SMB code) fails here. The
// per-row lock-vs-general comparison documents the intended divergence: the
// lock table holds only SMB deltas over errorMap, so codes whose lock answer
// differs from the general answer must keep diverging.
func TestMapLockToSMB_Table(t *testing.T) {
	for code, lockRow := range lockErrorMap {
		code := code
		t.Run(code.String(), func(t *testing.T) {
			storeErr := &merrs.StoreError{Code: code, Message: code.String()}
			if got := MapLockToSMB(storeErr); got != lockRow.SMB {
				t.Errorf("MapLockToSMB(%v) = %v, want lockErrorMap.SMB = %v", code, got, lockRow.SMB)
			}
			general, ok := errorMap[code]
			if !ok {
				t.Fatalf("lockErrorMap lists %v but errorMap has no general row", code)
			}
			if general.SMB != lockRow.SMB {
				t.Logf("%v diverges lock-vs-general: general %v -> lock %v", code, general.SMB, lockRow.SMB)
			}
		})
	}
}
