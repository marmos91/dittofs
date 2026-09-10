package common

import (
	goerrors "errors"
	"fmt"
	"testing"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// walkAllErrorCodes enumerates every merrs.ErrorCode by walking the
// contiguous iota range in pkg/metadata/errors (ErrNotFound through
// ErrConflict), the same pattern the errors package's own test uses.
// Adding a new ErrorCode automatically extends the walk; adding a row in
// errorMap is then enforced by TestErrorMapCoverage.
func walkAllErrorCodes() []merrs.ErrorCode {
	codes := make([]merrs.ErrorCode, 0, 26)
	for c := merrs.ErrNotFound; c <= merrs.ErrConflict; c++ {
		codes = append(codes, c)
	}
	return codes
}

// TestErrorMapCoverage asserts every merrs.ErrorCode constant has a row in
// errorMap. A literal count guard sits alongside the walk so the enum's
// shape drifts loudly: if pkg/metadata/errors.go adds or removes a code,
// both the walk range and this number must be confirmed together.
func TestErrorMapCoverage(t *testing.T) {
	const expectedCount = 26
	codes := walkAllErrorCodes()
	if n := len(codes); n != expectedCount {
		t.Fatalf("enum walk covers %d codes; update the walk range in walkAllErrorCodes() AND errorMap when the ErrorCode enum changes (expected %d)", n, expectedCount)
	}
	for _, code := range codes {
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

// TestHandleErrorFactoriesMapToHandleStatuses asserts the end-to-end contract
// for area-6 PR-B4: the StoreError values produced by the handle decode/routing
// path (NewInvalidHandleError / NewStaleHandleError) map to the correct
// per-protocol handle statuses, NOT the generic server-fault fallback. A
// regression that reverted these paths to fmt.Errorf would fall through to
// defaultCodes and this test would catch it.
func TestHandleErrorFactoriesMapToHandleStatuses(t *testing.T) {
	invalid := merrs.NewInvalidHandleError()
	if got := MapToNFS3(invalid); got != nfs3types.NFS3ErrBadHandle {
		t.Errorf("MapToNFS3(NewInvalidHandleError) = %d, want NFS3ErrBadHandle", got)
	}
	if got := MapToNFS4(invalid); got != nfs4types.NFS4ERR_BADHANDLE {
		t.Errorf("MapToNFS4(NewInvalidHandleError) = %d, want NFS4ERR_BADHANDLE", got)
	}
	if got := MapToSMB(invalid); got != smbtypes.StatusInvalidHandle {
		t.Errorf("MapToSMB(NewInvalidHandleError) = %v, want StatusInvalidHandle", got)
	}

	stale := merrs.NewStaleHandleError("/removed")
	if got := MapToNFS3(stale); got != nfs3types.NFS3ErrStale {
		t.Errorf("MapToNFS3(NewStaleHandleError) = %d, want NFS3ErrStale", got)
	}
	if got := MapToNFS4(stale); got != nfs4types.NFS4ERR_STALE {
		t.Errorf("MapToNFS4(NewStaleHandleError) = %d, want NFS4ERR_STALE", got)
	}
	if got := MapToSMB(stale); got != smbtypes.StatusFileClosed {
		t.Errorf("MapToSMB(NewStaleHandleError) = %v, want StatusFileClosed", got)
	}
}

// TestQuotaExceededMapsToDquot locks the #1151 contract: a per-identity quota
// breach (ErrQuotaExceeded) maps to DQUOT on NFS and STATUS_QUOTA_EXCEEDED on
// SMB — distinct from a genuine full volume (ErrNoSpace -> NOSPC / DISK_FULL).
func TestQuotaExceededMapsToDquot(t *testing.T) {
	quota := &merrs.StoreError{Code: merrs.ErrQuotaExceeded, Message: "quota exceeded"}
	if got := MapToNFS3(quota); got != nfs3types.NFS3ErrDquot {
		t.Errorf("MapToNFS3(ErrQuotaExceeded) = %d, want NFS3ErrDquot", got)
	}
	if got := MapToNFS4(quota); got != nfs4types.NFS4ERR_DQUOT {
		t.Errorf("MapToNFS4(ErrQuotaExceeded) = %d, want NFS4ERR_DQUOT", got)
	}
	if got := MapToSMB(quota); got != smbtypes.StatusQuotaExceeded {
		t.Errorf("MapToSMB(ErrQuotaExceeded) = %v, want StatusQuotaExceeded", got)
	}

	// ErrNoSpace stays NOSPC / DISK_FULL — quota must NOT collapse into it.
	nospace := &merrs.StoreError{Code: merrs.ErrNoSpace, Message: "no space"}
	if got := MapToSMB(nospace); got != smbtypes.StatusDiskFull {
		t.Errorf("MapToSMB(ErrNoSpace) = %v, want StatusDiskFull", got)
	}
}

// TestMapContentToNFS3 exercises nil + ErrRemoteUnavailable + unknown.
func TestMapContentToNFS3(t *testing.T) {
	if got := MapContentToNFS3(nil); got != nfs3types.NFS3OK {
		t.Errorf("MapContentToNFS3(nil) = %d, want NFS3OK", got)
	}
	if got := MapContentToNFS3(block.ErrRemoteUnavailable); got != nfs3types.NFS3ErrIO {
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
	if got := MapContentToNFS4(block.ErrRemoteUnavailable); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(ErrRemoteUnavailable) = %d, want NFS4ERR_IO", got)
	}
	if got := MapContentToNFS4(goerrors.New("unknown")); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(unknown) = %d, want NFS4ERR_IO", got)
	}
}

// TestMapContentToSMB exercises nil + unknown fallback.
func TestMapContentToSMB(t *testing.T) {
	if got := MapContentToSMB(nil); got != smbtypes.StatusSuccess {
		t.Errorf("MapContentToSMB(nil) = %v, want StatusSuccess", got)
	}
	// "cache full" and other unknown content errors fall back to
	// StatusUnexpectedIOError.
	if got := MapContentToSMB(goerrors.New("cache full")); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB(cache full) = %v, want StatusUnexpectedIOError", got)
	}
	if got := MapContentToSMB(block.ErrRemoteUnavailable); got != smbtypes.StatusUnexpectedIOError {
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

// ============================================================================
// Unit-tier exotic codes.
// ============================================================================

// exoticCodes is the unit-tier list: codes that cannot be reliably
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
		merrs.ErrConflict,
	}
}

// TestExoticErrorCodes asserts that every exotic (non-e2e-triggerable)
// ErrorCode has an errorMap row AND that each protocol mapper returns the
// row's expected value when given a synthesized StoreError.
//
// This is the "unit tier" leg of the two-tier conformance test — the e2e
// tier in test/e2e/cross_protocol_test.go covers the ~18 kernel-triggerable
// codes; this test covers the ~9 remaining codes that need error injection
// to reproduce.
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
			// lock-context mapper surfaces the lockErrorMap value (not
			// errorMap's general-context value). This catches the
			// lock-vs-general divergence called out for ErrDeadlock,
			// ErrGracePeriod, ErrLockLimitExceeded, ErrLockConflict,
			// ErrLockNotFound.
			if lockRow, lockOK := lockErrorMap[code]; lockOK {
				if got := MapLockToSMB(storeErr); got != lockRow.SMB {
					t.Errorf("MapLockToSMB(%v) = %v, want lockRow.SMB = %v", code, got, lockRow.SMB)
				}
			}
		})
	}
}

// TestCrossProtocolUnitConformance is the unit-tier belt-and-braces:
// every code in allErrorCodes() must be covered by EITHER the
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
	// e2eTriggerableCodes mirrors the e2e-triggerable list and must
	// match the table driving TestCrossProtocol_ErrorConformance. When the
	// e2e table changes, this list must change too — coverage will drift
	// silently otherwise.
	e2eTriggerableCodes := map[merrs.ErrorCode]bool{
		merrs.ErrNotFound:         true,
		merrs.ErrAccessDenied:     true,
		merrs.ErrAlreadyExists:    true,
		merrs.ErrNotEmpty:         true,
		merrs.ErrIsDirectory:      true,
		merrs.ErrNotDirectory:     true,
		merrs.ErrInvalidArgument:  true,
		merrs.ErrNoSpace:          true,
		merrs.ErrReadOnly:         true,
		merrs.ErrStaleHandle:      true,
		merrs.ErrNameTooLong:      true,
		merrs.ErrIOError:          true,
		merrs.ErrInvalidHandle:    true,
		merrs.ErrNotSupported:     true,
		merrs.ErrAuthRequired:     true,
		merrs.ErrPermissionDenied: true,
		merrs.ErrLocked:           true,
		merrs.ErrLockNotFound:     true,
	}

	exoticSet := map[merrs.ErrorCode]bool{}
	for _, c := range exoticCodes() {
		exoticSet[c] = true
	}

	for _, code := range walkAllErrorCodes() {
		inE2E := e2eTriggerableCodes[code]
		inUnit := exoticSet[code]
		if !inE2E && !inUnit {
			t.Errorf("code %v is in allErrorCodes() but neither e2e-triggerable nor exotic — add to TestCrossProtocol_ErrorConformance or exoticCodes()", code)
		}
		if inE2E && inUnit {
			t.Errorf("code %v is listed in BOTH e2eTriggerableCodes and exoticCodes() — pick one tier so coverage drift is detectable", code)
		}
	}
}

// TestErrStoreClosedMapsToStale verifies engine.ErrStoreClosed (block store
// Closed under an in-flight op — area-7 H-A) maps to a stale-handle status on
// every adapter path: NFS *_STALE and SMB STATUS_FILE_CLOSED. Both the
// general (errmap.go) and content (content_errmap.go) tables are covered,
// plus wrapped forms via fmt.Errorf, since the data path returns wrapped
// errors from the engine.
func TestErrStoreClosedMapsToStale(t *testing.T) {
	wrapped := fmt.Errorf("write content: %w", engine.ErrStoreClosed)

	for name, err := range map[string]error{
		"direct":  engine.ErrStoreClosed,
		"wrapped": wrapped,
	} {
		t.Run(name, func(t *testing.T) {
			// General table (MapTo*).
			if got := MapToNFS3(err); got != nfs3types.NFS3ErrStale {
				t.Errorf("MapToNFS3 = %d, want NFS3ErrStale", got)
			}
			if got := MapToNFS4(err); got != nfs4types.NFS4ERR_STALE {
				t.Errorf("MapToNFS4 = %d, want NFS4ERR_STALE", got)
			}
			if got := MapToSMB(err); got != smbtypes.StatusFileClosed {
				t.Errorf("MapToSMB = %v, want StatusFileClosed", got)
			}
			// Content table (MapContentTo*) — the data-path WRITE/READ seam.
			if got := MapContentToNFS3(err); got != nfs3types.NFS3ErrStale {
				t.Errorf("MapContentToNFS3 = %d, want NFS3ErrStale", got)
			}
			if got := MapContentToNFS4(err); got != nfs4types.NFS4ERR_STALE {
				t.Errorf("MapContentToNFS4 = %d, want NFS4ERR_STALE", got)
			}
			if got := MapContentToSMB(err); got != smbtypes.StatusFileClosed {
				t.Errorf("MapContentToSMB = %v, want StatusFileClosed", got)
			}
		})
	}
}
