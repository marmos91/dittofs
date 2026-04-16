package scheduler

import (
	"errors"
	"fmt"

	cron "github.com/robfig/cron/v3"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ValidateSchedule parses expr using robfig/cron/v3 ParseStandard (5-field
// cron, CRON_TZ= prefix supported). Returns nil on success; on failure
// returns an error that wraps models.ErrScheduleInvalid so callers can
// use errors.Is(err, models.ErrScheduleInvalid) (D-06).
//
// Use at:
//   - Phase 6 repo-create / repo-update handlers: reject invalid schedules
//     with 400 before persisting the DB row (strict at write time).
//   - Serve-time in storebackups.Service.Serve: skip repos whose stored
//     schedule no longer parses (WARN-and-continue, not fatal boot).
//
// The empty string is REJECTED — scheduling a repo requires a non-empty
// schedule. Callers that want to leave a repo unscheduled skip
// ValidateSchedule entirely by passing schedule == nil / schedule == "".
func ValidateSchedule(expr string) error {
	if expr == "" {
		return fmt.Errorf("%w: empty schedule", models.ErrScheduleInvalid)
	}
	if _, err := cron.ParseStandard(expr); err != nil {
		return fmt.Errorf("%w: %v", models.ErrScheduleInvalid, err)
	}
	return nil
}

// wrapScheduleError normalizes parse errors to ensure the returned
// error wraps models.ErrScheduleInvalid. If err already wraps the
// sentinel, it is returned as-is; otherwise a new wrapped error is
// constructed that carries both expr and the original cause.
func wrapScheduleError(expr string, err error) error {
	if errors.Is(err, models.ErrScheduleInvalid) {
		return err
	}
	return fmt.Errorf("%w: %q: %v", models.ErrScheduleInvalid, expr, err)
}
