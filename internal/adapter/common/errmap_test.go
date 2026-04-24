package common

import (
	goerrors "errors"
	"fmt"
	"testing"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/blockstore"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// allErrorCodes enumerates every merrs.ErrorCode re-exported from
// pkg/metadata/errors.go. This is the canonical list TestErrorMapCoverage
// iterates over: adding a new ErrorCode without updating this list AND
// adding a row in errorMap will fail the count-length assertion.
func allErrorCodes() []merrs.ErrorCode {
	return []merrs.ErrorCode{
		merrs.ErrNotFound,
		merrs.ErrAccessDenied,
		merrs.ErrAuthRequired,
		merrs.ErrPermissionDenied,
		merrs.ErrAlreadyExists,
		merrs.ErrNotEmpty,
		merrs.ErrIsDirectory,
		merrs.ErrNotDirectory,
		merrs.ErrInvalidArgument,
		merrs.ErrIOError,
		merrs.ErrNoSpace,
		merrs.ErrQuotaExceeded,
		merrs.ErrReadOnly,
		merrs.ErrNotSupported,
		merrs.ErrInvalidHandle,
		merrs.ErrStaleHandle,
		merrs.ErrLocked,
		merrs.ErrLockNotFound,
		merrs.ErrPrivilegeRequired,
		merrs.ErrNameTooLong,
		merrs.ErrDeadlock,
		merrs.ErrGracePeriod,
		merrs.ErrLockLimitExceeded,
		merrs.ErrLockConflict,
		merrs.ErrConnectionLimitReached,
	}
}

// TestErrorMapCoverage asserts every merrs.ErrorCode constant has a row in
// errorMap. Also asserts the enumeration itself has the expected count so a
// drift in pkg/metadata/errors.go is caught by a failing test here.
func TestErrorMapCoverage(t *testing.T) {
	const expectedCount = 25
	if n := len(allErrorCodes()); n != expectedCount {
		t.Fatalf("allErrorCodes has %d entries; update allErrorCodes() AND errorMap when adding ErrorCodes (expected %d)", n, expectedCount)
	}
	for _, code := range allErrorCodes() {
		if _, ok := errorMap[code]; !ok {
			t.Errorf("errorMap missing row for %v", code)
		}
	}
}

// TestMapToNFS3 exercises nil, non-StoreError, wrapped, and every row.
func TestMapToNFS3(t *testing.T) {
	if got := MapToNFS3(nil); got != nfs3types.NFS3OK {
		t.Errorf("MapToNFS3(nil) = %d, want NFS3OK", got)
	}
	if got := MapToNFS3(goerrors.New("random")); got != defaultCodes.NFS3 {
		t.Errorf("MapToNFS3(non-StoreError) = %d, want defaultCodes.NFS3 = %d", got, defaultCodes.NFS3)
	}
	// Wrapped-error unwrap (goerrors.As path).
	wrapped := fmt.Errorf("wrap: %w", &merrs.StoreError{Code: merrs.ErrNotFound, Message: "x"})
	if got := MapToNFS3(wrapped); got != nfs3types.NFS3ErrNoEnt {
		t.Errorf("MapToNFS3(wrapped ErrNotFound) = %d, want NFS3ErrNoEnt", got)
	}
	// Every row.
	for code, want := range errorMap {
		err := &merrs.StoreError{Code: code, Message: code.String()}
		if got := MapToNFS3(err); got != want.NFS3 {
			t.Errorf("MapToNFS3(%v) = %d, want %d", code, got, want.NFS3)
		}
	}
}

// TestMapToNFS4 exercises nil, non-StoreError, wrapped, and every row.
func TestMapToNFS4(t *testing.T) {
	if got := MapToNFS4(nil); got != nfs4types.NFS4_OK {
		t.Errorf("MapToNFS4(nil) = %d, want NFS4_OK", got)
	}
	if got := MapToNFS4(goerrors.New("random")); got != defaultCodes.NFS4 {
		t.Errorf("MapToNFS4(non-StoreError) = %d, want defaultCodes.NFS4 = %d", got, defaultCodes.NFS4)
	}
	wrapped := fmt.Errorf("wrap: %w", &merrs.StoreError{Code: merrs.ErrLocked})
	if got := MapToNFS4(wrapped); got != nfs4types.NFS4ERR_LOCKED {
		t.Errorf("MapToNFS4(wrapped ErrLocked) = %d, want NFS4ERR_LOCKED", got)
	}
	for code, want := range errorMap {
		err := &merrs.StoreError{Code: code, Message: code.String()}
		if got := MapToNFS4(err); got != want.NFS4 {
			t.Errorf("MapToNFS4(%v) = %d, want %d", code, got, want.NFS4)
		}
	}
}

// TestMapToSMB exercises nil, non-StoreError, wrapped (Test D — latent bug
// fix), and every row.
func TestMapToSMB(t *testing.T) {
	if got := MapToSMB(nil); got != smbtypes.StatusSuccess {
		t.Errorf("MapToSMB(nil) = %v, want StatusSuccess", got)
	}
	if got := MapToSMB(goerrors.New("random")); got != defaultCodes.SMB {
		t.Errorf("MapToSMB(non-StoreError) = %v, want defaultCodes.SMB", got)
	}
	// Test D: wrapped StoreError unwraps correctly — this is the fix for
	// converters.go:364's pre-consolidation type assertion bug.
	wrapped := fmt.Errorf("wrap: %w", &merrs.StoreError{Code: merrs.ErrNotFound, Message: "x"})
	if got := MapToSMB(wrapped); got != smbtypes.StatusObjectNameNotFound {
		t.Errorf("MapToSMB(wrapped ErrNotFound) = %v, want StatusObjectNameNotFound", got)
	}
	for code, want := range errorMap {
		err := &merrs.StoreError{Code: code, Message: code.String()}
		if got := MapToSMB(err); got != want.SMB {
			t.Errorf("MapToSMB(%v) = %v, want %v", code, got, want.SMB)
		}
	}
}

// TestMapContentToNFS3 exercises nil + ErrRemoteUnavailable + unknown.
func TestMapContentToNFS3(t *testing.T) {
	if got := MapContentToNFS3(nil); got != nfs3types.NFS3OK {
		t.Errorf("MapContentToNFS3(nil) = %d, want NFS3OK", got)
	}
	if got := MapContentToNFS3(blockstore.ErrRemoteUnavailable); got != nfs3types.NFS3ErrIO {
		t.Errorf("MapContentToNFS3(ErrRemoteUnavailable) = %d, want NFS3ErrIO", got)
	}
	if got := MapContentToNFS3(goerrors.New("unknown")); got != nfs3types.NFS3ErrIO {
		t.Errorf("MapContentToNFS3(unknown) = %d, want NFS3ErrIO", got)
	}
}

// TestMapContentToNFS4 exercises nil + ErrRemoteUnavailable + unknown.
func TestMapContentToNFS4(t *testing.T) {
	if got := MapContentToNFS4(nil); got != nfs4types.NFS4_OK {
		t.Errorf("MapContentToNFS4(nil) = %d, want NFS4_OK", got)
	}
	if got := MapContentToNFS4(blockstore.ErrRemoteUnavailable); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(ErrRemoteUnavailable) = %d, want NFS4ERR_IO", got)
	}
	if got := MapContentToNFS4(goerrors.New("unknown")); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(unknown) = %d, want NFS4ERR_IO", got)
	}
}

// TestMapContentToSMB exercises nil + unknown fallback (Test F per plan).
func TestMapContentToSMB(t *testing.T) {
	if got := MapContentToSMB(nil); got != smbtypes.StatusSuccess {
		t.Errorf("MapContentToSMB(nil) = %v, want StatusSuccess", got)
	}
	// Plan Test F: "cache full" and other unknown content errors fall back to
	// StatusUnexpectedIOError per D-08 §2.
	if got := MapContentToSMB(goerrors.New("cache full")); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB(cache full) = %v, want StatusUnexpectedIOError", got)
	}
	if got := MapContentToSMB(blockstore.ErrRemoteUnavailable); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB(ErrRemoteUnavailable) = %v, want StatusUnexpectedIOError", got)
	}
}

// TestMapLockToSMB covers Test G (lock-context codes) and Test H
// (lock-vs-general divergence for merrs.ErrLocked).
func TestMapLockToSMB(t *testing.T) {
	// Test G: lock-context codes.
	tests := []struct {
		name string
		code merrs.ErrorCode
		want smbtypes.Status
	}{
		{"ErrLocked → StatusLockNotGranted", merrs.ErrLocked, smbtypes.StatusLockNotGranted},
		{"ErrLockNotFound → StatusRangeNotLocked", merrs.ErrLockNotFound, smbtypes.StatusRangeNotLocked},
		{"ErrNotFound → StatusFileClosed", merrs.ErrNotFound, smbtypes.StatusFileClosed},
		{"ErrPermissionDenied → StatusAccessDenied", merrs.ErrPermissionDenied, smbtypes.StatusAccessDenied},
		{"ErrIsDirectory → StatusFileIsADirectory", merrs.ErrIsDirectory, smbtypes.StatusFileIsADirectory},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &merrs.StoreError{Code: tt.code}
			if got := MapLockToSMB(err); got != tt.want {
				t.Errorf("MapLockToSMB(%v) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}

	// Nil handling.
	if got := MapLockToSMB(nil); got != smbtypes.StatusSuccess {
		t.Errorf("MapLockToSMB(nil) = %v, want StatusSuccess", got)
	}

	// Test H: lock-vs-general divergence for ErrLocked.
	// In general context (errorMap) ErrLocked → StatusFileLockConflict
	// In lock context (lockErrorMap) ErrLocked → StatusLockNotGranted
	lockedErr := &merrs.StoreError{Code: merrs.ErrLocked}
	general := MapToSMB(lockedErr)
	lockCtx := MapLockToSMB(lockedErr)
	if general != smbtypes.StatusFileLockConflict {
		t.Errorf("MapToSMB(ErrLocked) = %v, want StatusFileLockConflict (general context)", general)
	}
	if lockCtx != smbtypes.StatusLockNotGranted {
		t.Errorf("MapLockToSMB(ErrLocked) = %v, want StatusLockNotGranted (lock context)", lockCtx)
	}
	if general == lockCtx {
		t.Errorf("Expected general-vs-lock divergence for ErrLocked; both returned %v", general)
	}
}

// TestMapLockToNFS3 spot-checks a handful of lock-context rows.
func TestMapLockToNFS3(t *testing.T) {
	if got := MapLockToNFS3(nil); got != nfs3types.NFS3OK {
		t.Errorf("MapLockToNFS3(nil) = %d, want NFS3OK", got)
	}
	locked := &merrs.StoreError{Code: merrs.ErrLocked}
	if got := MapLockToNFS3(locked); got != nfs3types.NFS3ErrJukebox {
		t.Errorf("MapLockToNFS3(ErrLocked) = %d, want NFS3ErrJukebox", got)
	}
	lnf := &merrs.StoreError{Code: merrs.ErrLockNotFound}
	if got := MapLockToNFS3(lnf); got != nfs3types.NFS3ErrInval {
		t.Errorf("MapLockToNFS3(ErrLockNotFound) = %d, want NFS3ErrInval", got)
	}
}

// TestMapLockToNFS4 spot-checks a handful of lock-context rows.
func TestMapLockToNFS4(t *testing.T) {
	if got := MapLockToNFS4(nil); got != nfs4types.NFS4_OK {
		t.Errorf("MapLockToNFS4(nil) = %d, want NFS4_OK", got)
	}
	locked := &merrs.StoreError{Code: merrs.ErrLocked}
	if got := MapLockToNFS4(locked); got != nfs4types.NFS4ERR_DENIED {
		t.Errorf("MapLockToNFS4(ErrLocked) = %d, want NFS4ERR_DENIED", got)
	}
	deadlock := &merrs.StoreError{Code: merrs.ErrDeadlock}
	if got := MapLockToNFS4(deadlock); got != nfs4types.NFS4ERR_DEADLOCK {
		t.Errorf("MapLockToNFS4(ErrDeadlock) = %d, want NFS4ERR_DEADLOCK", got)
	}
	grace := &merrs.StoreError{Code: merrs.ErrGracePeriod}
	if got := MapLockToNFS4(grace); got != nfs4types.NFS4ERR_GRACE {
		t.Errorf("MapLockToNFS4(ErrGracePeriod) = %d, want NFS4ERR_GRACE", got)
	}
}

// ============================================================================
// Unit-tier exotic codes (ADAPT-05 / D-13 unit tier).
// ============================================================================

// exoticCodes is the D-13 unit-tier list: codes that cannot be reliably
// e2e-triggered through kernel NFS/SMB file-I/O syscalls (they require
// backend fault injection, quota-constrained fixtures, domain controllers,
// or protocol-specific lock RPCs). The unit tier covers them by synthesizing
// a *merrs.StoreError directly and asserting the mapper returns the expected
// per-protocol code from common/'s errmap.
//
// Adding a new code that becomes e2e-triggerable: remove from this list and
// add a row to TestCrossProtocol_ErrorConformance in
// test/e2e/cross_protocol_test.go with a real trigger helper.
//
// Adding a new code that is purely exotic: add to this list; the row in
// errorMap (enforced by TestErrorMapCoverage) provides the expected values.
func exoticCodes() []merrs.ErrorCode {
	return []merrs.ErrorCode{
		merrs.ErrConnectionLimitReached,
		merrs.ErrLockLimitExceeded,
		merrs.ErrDeadlock,
		merrs.ErrGracePeriod,
		merrs.ErrPrivilegeRequired,
		merrs.ErrQuotaExceeded,
		merrs.ErrLockConflict,
		merrs.ErrLockNotFound,
		merrs.ErrIOError, // Triggerable only via backend fault injection.
	}
}

// TestExoticErrorCodes asserts that every exotic (non-e2e-triggerable)
// ErrorCode per D-13 has an errorMap row AND that each protocol mapper
// returns the row's expected value when given a synthesized StoreError.
//
// This is the "unit tier" leg of the ADAPT-05 two-tier conformance test —
// the e2e tier in test/e2e/cross_protocol_test.go covers the ~18
// kernel-triggerable codes; this test covers the ~9 remaining codes that
// need error injection to reproduce.
//
// Both tiers drive assertions from common/'s tables: this test iterates
// exoticCodes() and looks up errorMap (and, where applicable, lockErrorMap)
// — so adding a new exotic code requires only updating exoticCodes() and
// adding a row in errorMap (the latter is already enforced by
// TestErrorMapCoverage).
func TestExoticErrorCodes(t *testing.T) {
	for _, code := range exoticCodes() {
		code := code
		t.Run(code.String(), func(t *testing.T) {
			row, ok := errorMap[code]
			if !ok {
				t.Fatalf("exoticCodes() lists %v but errorMap has no row — update errorMap", code)
			}

			storeErr := &merrs.StoreError{Code: code, Message: code.String()}

			if got := MapToNFS3(storeErr); got != row.NFS3 {
				t.Errorf("MapToNFS3(%v) = %d, want row.NFS3 = %d", code, got, row.NFS3)
			}
			if got := MapToNFS4(storeErr); got != row.NFS4 {
				t.Errorf("MapToNFS4(%v) = %d, want row.NFS4 = %d", code, got, row.NFS4)
			}
			if got := MapToSMB(storeErr); got != row.SMB {
				t.Errorf("MapToSMB(%v) = %v, want row.SMB = %v", code, got, row.SMB)
			}

			// For codes that ALSO live in lockErrorMap, assert the
			// lock-context mappers surface the lockErrorMap values (not
			// errorMap's general-context values). This catches the
			// lock-vs-general divergence that D-13 calls out for
			// ErrDeadlock, ErrGracePeriod, ErrLockLimitExceeded,
			// ErrLockConflict, ErrLockNotFound.
			if lockRow, lockOK := lockErrorMap[code]; lockOK {
				if got := MapLockToNFS3(storeErr); got != lockRow.NFS3 {
					t.Errorf("MapLockToNFS3(%v) = %d, want lockRow.NFS3 = %d", code, got, lockRow.NFS3)
				}
				if got := MapLockToNFS4(storeErr); got != lockRow.NFS4 {
					t.Errorf("MapLockToNFS4(%v) = %d, want lockRow.NFS4 = %d", code, got, lockRow.NFS4)
				}
				if got := MapLockToSMB(storeErr); got != lockRow.SMB {
					t.Errorf("MapLockToSMB(%v) = %v, want lockRow.SMB = %v", code, got, lockRow.SMB)
				}
			}
		})
	}
}

// TestCrossProtocolUnitConformance is the unit-tier belt-and-braces for
// ADAPT-05: every code in allErrorCodes() must be covered by EITHER the
// e2e-triggerable tier (denoted implicitly — not in exoticCodes()) OR the
// exotic unit tier (in exoticCodes()). A code that falls off both lists is
// drift and fails here.
//
// Defined in addition to TestErrorMapCoverage (which asserts errorMap has a
// row for every code) — this test asserts the TEST COVERAGE list itself is
// complete. The e2e-tier list lives at
// test/e2e/cross_protocol_test.go:TestCrossProtocol_ErrorConformance; this
// unit test reconstructs it by subtraction: all codes minus exoticCodes().
func TestCrossProtocolUnitConformance(t *testing.T) {
	// e2eTriggerableCodes mirrors the D-13 e2e-triggerable list and must
	// match the table driving TestCrossProtocol_ErrorConformance. When the
	// e2e table changes, this list must change too — coverage will drift
	// silently otherwise.
	e2eTriggerableCodes := map[merrs.ErrorCode]bool{
		merrs.ErrNotFound:          true,
		merrs.ErrAccessDenied:      true,
		merrs.ErrAlreadyExists:     true,
		merrs.ErrNotEmpty:          true,
		merrs.ErrIsDirectory:       true,
		merrs.ErrNotDirectory:      true,
		merrs.ErrInvalidArgument:   true,
		merrs.ErrNoSpace:           true,
		merrs.ErrReadOnly:          true,
		merrs.ErrStaleHandle:       true,
		merrs.ErrNameTooLong:       true,
		merrs.ErrIOError:           true,
		merrs.ErrInvalidHandle:     true,
		merrs.ErrNotSupported:      true,
		merrs.ErrAuthRequired:      true,
		merrs.ErrPermissionDenied:  true,
		merrs.ErrLocked:            true,
		merrs.ErrLockNotFound:      true,
	}

	exoticSet := map[merrs.ErrorCode]bool{}
	for _, c := range exoticCodes() {
		exoticSet[c] = true
	}

	for _, code := range allErrorCodes() {
		inE2E := e2eTriggerableCodes[code]
		inUnit := exoticSet[code]
		if !inE2E && !inUnit {
			t.Errorf("code %v is in allErrorCodes() but neither e2e-triggerable nor exotic — add to TestCrossProtocol_ErrorConformance or exoticCodes()", code)
		}
	}
}
