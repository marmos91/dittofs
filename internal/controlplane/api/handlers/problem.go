// Package handlers provides HTTP handlers for the DittoFS API.
package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	p := &BackupAlreadyRunningProblem{
		Problem: Problem{
			Type:   "about:blank",
			Title:  "Conflict",
			Status: http.StatusConflict,
			Detail: "backup already running",
		},
		RunningJobID: runningJobID,
	}
	writeProblemJSON(w, http.StatusConflict, p)
}

// WriteRestorePreconditionFailedProblem emits a 409 Conflict problem+json
// body with the enabled_shares list (D-29). Detail text reports the count
// so the CLI can render a short summary without reflowing the list.
func WriteRestorePreconditionFailedProblem(w http.ResponseWriter, enabledShares []string) {
	p := &RestorePreconditionFailedProblem{
		Problem: Problem{
			Type:   "about:blank",
			Title:  "Restore precondition failed",
			Status: http.StatusConflict,
			Detail: fmt.Sprintf("%d share(s) still enabled", len(enabledShares)),
		},
		EnabledShares: enabledShares,
	}
	writeProblemJSON(w, http.StatusConflict, p)
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
