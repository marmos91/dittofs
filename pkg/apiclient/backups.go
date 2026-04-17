package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Model types
// -----------------------------------------------------------------------------

// BackupRecord mirrors internal/controlplane/api/handlers.BackupRecordResponse.
type BackupRecord struct {
	ID           string    `json:"id"`
	RepoID       string    `json:"repo_id"`
	CreatedAt    time.Time `json:"created_at"`
	SizeBytes    int64     `json:"size_bytes"`
	Status       string    `json:"status"`
	Pinned       bool      `json:"pinned"`
	ManifestPath string    `json:"manifest_path"`
	SHA256       string    `json:"sha256"`
	StoreID      string    `json:"store_id"`
	Error        string    `json:"error,omitempty"`
}

// BackupJob mirrors internal/controlplane/api/handlers.BackupJobResponse.
type BackupJob struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	RepoID         string     `json:"repo_id"`
	BackupRecordID *string    `json:"backup_record_id,omitempty"`
	Status         string     `json:"status"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	Progress       int        `json:"progress"`
}

// BackupRepo mirrors the handler BackupRepoResponse.
type BackupRepo struct {
	ID                string         `json:"id"`
	TargetID          string         `json:"target_id"`
	TargetKind        string         `json:"target_kind"`
	Name              string         `json:"name"`
	Kind              string         `json:"kind"`
	Schedule          *string        `json:"schedule,omitempty"`
	KeepCount         *int           `json:"keep_count,omitempty"`
	KeepAgeDays       *int           `json:"keep_age_days,omitempty"`
	EncryptionEnabled bool           `json:"encryption_enabled"`
	EncryptionKeyRef  string         `json:"encryption_key_ref,omitempty"`
	Config            map[string]any `json:"config,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

// -----------------------------------------------------------------------------
// Request types
// -----------------------------------------------------------------------------

// TriggerBackupRequest is the body for POST /backups.
type TriggerBackupRequest struct {
	Repo string `json:"repo,omitempty"`
}

// TriggerBackupResponse is returned by POST /backups (HTTP 202 Accepted).
type TriggerBackupResponse struct {
	Record *BackupRecord `json:"record"`
	Job    *BackupJob    `json:"job"`
}

// RestoreRequest is the body for POST /restore and /restore/dry-run.
type RestoreRequest struct {
	FromBackupID string `json:"from_backup_id,omitempty"`
}

// DryRunResult mirrors storebackups.DryRunResult.
type DryRunResult struct {
	Record        *BackupRecord `json:"record"`
	ManifestValid bool          `json:"manifest_valid"`
	EnabledShares []string      `json:"enabled_shares"`
}

// BackupJobFilter bundles the filters accepted by GET /backup-jobs.
type BackupJobFilter struct {
	RepoName string // "" = all repos on the store
	Status   string // "" = all statuses
	Kind     string // "" = all kinds; "backup" or "restore"
	Limit    int    // 0 = server default (50); server caps at 200
}

// BackupRepoRequest is the body for POST /repos and PATCH /repos/{repo}.
type BackupRepoRequest struct {
	Name              string         `json:"name"`
	Kind              string         `json:"kind"`
	Config            map[string]any `json:"config,omitempty"`
	Schedule          *string        `json:"schedule,omitempty"`
	KeepCount         *int           `json:"keep_count,omitempty"`
	KeepAgeDays       *int           `json:"keep_age_days,omitempty"`
	EncryptionEnabled *bool          `json:"encryption_enabled,omitempty"`
	EncryptionKeyRef  *string        `json:"encryption_key_ref,omitempty"`
}

// PatchBackupRecordRequest is the body for PATCH /backups/{id}.
type PatchBackupRecordRequest struct {
	Pinned *bool `json:"pinned,omitempty"`
}

// -----------------------------------------------------------------------------
// Typed errors for D-13 / D-29
// -----------------------------------------------------------------------------

// BackupAlreadyRunningError is returned when POST /backups receives a 409
// with a running_job_id field (D-13). Callers can errors.As to surface the
// in-flight job ID to the end user.
type BackupAlreadyRunningError struct {
	RunningJobID string
}

func (e *BackupAlreadyRunningError) Error() string {
	return fmt.Sprintf("backup already running (job %s)", e.RunningJobID)
}

// RestorePreconditionError is returned when POST /restore receives a 409
// with an enabled_shares array (D-29).
type RestorePreconditionError struct {
	EnabledShares []string
}

func (e *RestorePreconditionError) Error() string {
	return fmt.Sprintf("restore precondition failed: %d share(s) still enabled", len(e.EnabledShares))
}

// -----------------------------------------------------------------------------
// Client methods
// -----------------------------------------------------------------------------

// storePath builds `/api/v1/store/metadata/<storeName>`.
func storePath(storeName string) string {
	return fmt.Sprintf("/api/v1/store/metadata/%s", url.PathEscape(storeName))
}

// TriggerBackup triggers a backup run. Returns 202 Accepted; body carries
// both the persisted Record and the spawned Job. On 409 the server returns a
// typed problem; this client unwraps it into *BackupAlreadyRunningError.
func (c *Client) TriggerBackup(storeName string, req *TriggerBackupRequest) (*TriggerBackupResponse, error) {
	var resp TriggerBackupResponse
	if err := c.doWithTypedProblem(http.MethodPost, storePath(storeName)+"/backups", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListBackupRecords lists records for the given store, optionally filtered
// by repo name.
func (c *Client) ListBackupRecords(storeName, repo string) ([]BackupRecord, error) {
	p := storePath(storeName) + "/backups"
	if repo != "" {
		p += "?repo=" + url.QueryEscape(repo)
	}
	return listResources[BackupRecord](c, p)
}

// GetBackupRecord fetches a single record by ID.
func (c *Client) GetBackupRecord(storeName, recordID string) (*BackupRecord, error) {
	return getResource[BackupRecord](c, storePath(storeName)+"/backups/"+url.PathEscape(recordID))
}

// SetBackupRecordPinned sets/unsets the pinned flag on a record and returns
// the updated row.
func (c *Client) SetBackupRecordPinned(storeName, recordID string, pinned bool) (*BackupRecord, error) {
	req := PatchBackupRecordRequest{Pinned: &pinned}
	var rec BackupRecord
	if err := c.patch(storePath(storeName)+"/backups/"+url.PathEscape(recordID), req, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// StartRestore triggers a restore. Returns 202 Accepted; body is the spawned
// BackupJob. On 409 the server may return a typed precondition problem;
// this client unwraps it into *RestorePreconditionError.
func (c *Client) StartRestore(storeName string, req *RestoreRequest) (*BackupJob, error) {
	var job BackupJob
	if err := c.doWithTypedProblem(http.MethodPost, storePath(storeName)+"/restore", req, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// RestoreDryRun runs the restore pre-flight. Returns 200 OK + DryRunResult.
// No state mutation; no job row created (D-31).
func (c *Client) RestoreDryRun(storeName string, req *RestoreRequest) (*DryRunResult, error) {
	var result DryRunResult
	if err := c.post(storePath(storeName)+"/restore/dry-run", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// -----------------------------------------------------------------------------
// Typed-problem-aware request path
// -----------------------------------------------------------------------------

// doWithTypedProblem is used for endpoints that can return typed D-13 / D-29
// problem+json bodies so the client surfaces dedicated error types. Standard
// success paths round-trip JSON into `out`.
func (c *Client) doWithTypedProblem(method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		if typed := parseTypedProblem(resp, respBody); typed != nil {
			return typed
		}
		return genericAPIError(resp.StatusCode, respBody)
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// parseTypedProblem inspects an error response for D-13 / D-29 typed fields
// (running_job_id, enabled_shares). Returns a typed *BackupAlreadyRunningError
// or *RestorePreconditionError when recognized, else nil (caller falls back
// to the generic APIError path).
func parseTypedProblem(resp *http.Response, body []byte) error {
	if ct := resp.Header.Get("Content-Type"); ct != "" && !isProblemJSON(ct) && !isJSON(ct) {
		return nil
	}
	if resp.StatusCode != http.StatusConflict {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	if jobID, ok := raw["running_job_id"]; ok {
		var s string
		if json.Unmarshal(jobID, &s) == nil {
			return &BackupAlreadyRunningError{RunningJobID: s}
		}
	}
	if shares, ok := raw["enabled_shares"]; ok {
		var list []string
		if json.Unmarshal(shares, &list) == nil {
			return &RestorePreconditionError{EnabledShares: list}
		}
	}
	return nil
}

// isProblemJSON / isJSON accept both the bare media type and a parameterised
// form like "application/problem+json; charset=utf-8".
func isProblemJSON(ct string) bool { return strings.HasPrefix(ct, "application/problem+json") }
func isJSON(ct string) bool        { return strings.HasPrefix(ct, "application/json") }

// genericAPIError mirrors client.go's error path: try to decode the body as
// an APIError, else wrap the raw body as a message.
func genericAPIError(status int, body []byte) error {
	var apiErr APIError
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
		apiErr.StatusCode = status
		return &apiErr
	}
	// Try RFC 7807 Problem (title/detail) before falling back to raw.
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &p) == nil && (p.Detail != "" || p.Title != "") {
		msg := p.Detail
		if msg == "" {
			msg = p.Title
		}
		return &APIError{StatusCode: status, Message: msg}
	}
	return &APIError{StatusCode: status, Message: string(body)}
}
