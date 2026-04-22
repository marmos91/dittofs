// Package handlers provides HTTP handlers for the DittoFS API.
package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	bkperrors "github.com/marmos91/dittofs/pkg/backup/errors"
)

// Problem represents an RFC 7807 "problem details" response.
// https://tools.ietf.org/html/rfc7807
type Problem struct {
	// Type is a URI reference that identifies the problem type.
	// If not set, defaults to "about:blank".
	Type string `json:"type,omitempty"`

	// Title is a short, human-readable summary of the problem type.
	Title string `json:"title"`

	// Status is the HTTP status code for this occurrence of the problem.
	Status int `json:"status"`

	// Detail is a human-readable explanation specific to this occurrence.
	Detail string `json:"detail,omitempty"`

	// Instance is a URI reference that identifies the specific occurrence.
	Instance string `json:"instance,omitempty"`

	// Code is a stable, machine-readable error taxonomy value (#414).
	// Clients dispatch i18n / UI hints on this field without parsing Detail.
	Code string `json:"code,omitempty"`

	// Hint is a short operator-facing string paired with Code. Clients
	// typically localize via Code, but Hint is a useful fallback for CLI
	// output and log readers.
	Hint string `json:"hint,omitempty"`
}

// ContentTypeProblemJSON is the Content-Type for RFC 7807 problem responses.
const ContentTypeProblemJSON = "application/problem+json"

// WriteProblem writes an RFC 7807 problem response.
func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
	problem := &Problem{
		Type:   "about:blank",
		Title:  title,
		Status: status,
		Detail: detail,
	}

	w.Header().Set("Content-Type", ContentTypeProblemJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

// Common problem helper functions for standard HTTP errors.

// BadRequest writes a 400 Bad Request problem response.
func BadRequest(w http.ResponseWriter, detail string) {
	WriteProblem(w, http.StatusBadRequest, "Bad Request", detail)
}

// Unauthorized writes a 401 Unauthorized problem response.
func Unauthorized(w http.ResponseWriter, detail string) {
	WriteProblem(w, http.StatusUnauthorized, "Unauthorized", detail)
}

// Forbidden writes a 403 Forbidden problem response.
func Forbidden(w http.ResponseWriter, detail string) {
	WriteProblem(w, http.StatusForbidden, "Forbidden", detail)
}

// NotFound writes a 404 Not Found problem response.
func NotFound(w http.ResponseWriter, detail string) {
	WriteProblem(w, http.StatusNotFound, "Not Found", detail)
}

// Conflict writes a 409 Conflict problem response.
func Conflict(w http.ResponseWriter, detail string) {
	WriteProblem(w, http.StatusConflict, "Conflict", detail)
}

// UnprocessableEntity writes a 422 Unprocessable Entity problem response.
func UnprocessableEntity(w http.ResponseWriter, detail string) {
	WriteProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", detail)
}

// InternalServerError writes a 500 Internal Server Error problem response.
func InternalServerError(w http.ResponseWriter, detail string) {
	WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", detail)
}

// ServiceUnavailable writes a 503 Service Unavailable problem response.
// Use when the route exists but a backing subsystem is not initialized —
// distinguishable from 404 (route/version mismatch) by clients.
func ServiceUnavailable(w http.ResponseWriter, detail string) {
	WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", detail)
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// WriteJSONOK writes a 200 OK JSON response.
func WriteJSONOK(w http.ResponseWriter, data any) {
	WriteJSON(w, http.StatusOK, data)
}

// WriteJSONCreated writes a 201 Created JSON response.
func WriteJSONCreated(w http.ResponseWriter, data any) {
	WriteJSON(w, http.StatusCreated, data)
}

// WriteNoContent writes a 204 No Content response.
func WriteNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// BackupAlreadyRunningProblem extends Problem with the conflicting running
// BackupJob ID so clients can surface the in-flight run (D-13). The
// embedded Problem base fields serialize at the top level per RFC 7807.
type BackupAlreadyRunningProblem struct {
	Problem
	RunningJobID string `json:"running_job_id"`
}

// RestorePreconditionFailedProblem extends Problem with the list of shares
// still enabled on the target store, so the operator knows which shares to
// disable before retrying (D-29).
type RestorePreconditionFailedProblem struct {
	Problem
	EnabledShares []string `json:"enabled_shares"`
}

// WriteBackupAlreadyRunningProblem emits a 409 Conflict problem+json body
// with the running BackupJob ID (D-13). Paired with the
// storebackups.ErrBackupAlreadyRunning sentinel in the handler layer.
func WriteBackupAlreadyRunningProblem(w http.ResponseWriter, runningJobID string) {
	code := bkperrors.CodeBackupAlreadyRunning
	p := &BackupAlreadyRunningProblem{
		Problem: Problem{
			Type:   "about:blank",
			Title:  "Conflict",
			Status: http.StatusConflict,
			Detail: "backup already running",
			Code:   string(code),
			Hint:   bkperrors.HintFor(code),
		},
		RunningJobID: runningJobID,
	}
	writeProblemJSON(w, http.StatusConflict, p)
}

// WriteRestorePreconditionFailedProblem emits a 409 Conflict problem+json
// body with the enabled_shares list (D-29). Detail text reports the count
// so the CLI can render a short summary without reflowing the list.
func WriteRestorePreconditionFailedProblem(w http.ResponseWriter, enabledShares []string) {
	code := bkperrors.CodeRestorePreconditionFailed
	p := &RestorePreconditionFailedProblem{
		Problem: Problem{
			Type:   "about:blank",
			Title:  "Restore precondition failed",
			Status: http.StatusConflict,
			Detail: fmt.Sprintf("%d share(s) still enabled", len(enabledShares)),
			Code:   string(code),
			Hint:   bkperrors.HintFor(code),
		},
		EnabledShares: enabledShares,
	}
	writeProblemJSON(w, http.StatusConflict, p)
}

// WriteBackupProblem emits an RFC 7807 problem body that includes a
// machine-readable code plus hint (#414). Prefer this over WriteProblem
// on any backup / restore error path so clients get a stable taxonomy.
func WriteBackupProblem(w http.ResponseWriter, status int, title, detail string, code bkperrors.Code, hint string) {
	if hint == "" {
		hint = bkperrors.HintFor(code)
	}
	p := &Problem{
		Type:   "about:blank",
		Title:  title,
		Status: status,
		Detail: detail,
		Code:   string(code),
		Hint:   hint,
	}
	writeProblemJSON(w, status, p)
}

// statusForBackupCode maps a classified backup error code to the HTTP
// status that best conveys it to clients (#414).
func statusForBackupCode(code bkperrors.Code) (int, string) {
	switch code {
	case bkperrors.CodeDestinationPermissionDenied:
		return http.StatusForbidden, "Forbidden"
	case bkperrors.CodeDestinationNotFound:
		return http.StatusNotFound, "Not Found"
	case bkperrors.CodeDestinationNoSpace:
		return http.StatusInsufficientStorage, "Insufficient Storage"
	case bkperrors.CodeDestinationUnreachable:
		return http.StatusBadGateway, "Bad Gateway"
	case bkperrors.CodeDestinationCredentialsInvalid:
		return http.StatusUnauthorized, "Unauthorized"
	case bkperrors.CodeDestinationPathConflict,
		bkperrors.CodeDestinationConfigInvalid:
		return http.StatusUnprocessableEntity, "Unprocessable Entity"
	case bkperrors.CodeSourceUnavailable:
		return http.StatusServiceUnavailable, "Service Unavailable"
	case bkperrors.CodeBackupAlreadyRunning, bkperrors.CodeRestorePreconditionFailed:
		return http.StatusConflict, "Conflict"
	}
	return http.StatusInternalServerError, "Internal Server Error"
}

// WriteClassifiedBackupError classifies err and emits the matching
// problem+json body. The default fallback is 500 + code=internal.
func WriteClassifiedBackupError(w http.ResponseWriter, err error) {
	be := bkperrors.Classify(err)
	if be == nil {
		WriteBackupProblem(w, http.StatusInternalServerError, "Internal Server Error",
			"unexpected error", bkperrors.CodeInternal, "")
		return
	}
	status, title := statusForBackupCode(be.Code)
	detail := ""
	if be.Err != nil {
		detail = be.Err.Error()
	}
	WriteBackupProblem(w, status, title, detail, be.Code, be.Hint)
}

// writeProblemJSON serializes a typed problem variant using the RFC 7807
// content type. Separate from WriteProblem (which composes a generic
// *Problem) so typed variants can emit their extra fields as peers of the
// embedded base.
func writeProblemJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", ContentTypeProblemJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
