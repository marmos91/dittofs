package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestMapStoreError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		// Not found errors -> 404
		{"user not found", models.ErrUserNotFound, http.StatusNotFound, "User not found"},
		{"group not found", models.ErrGroupNotFound, http.StatusNotFound, "Group not found"},
		{"share not found", models.ErrShareNotFound, http.StatusNotFound, "Share not found"},
		{"store not found", models.ErrStoreNotFound, http.StatusNotFound, "Store not found"},
		{"adapter not found", models.ErrAdapterNotFound, http.StatusNotFound, "Adapter not found"},
		{"setting not found", models.ErrSettingNotFound, http.StatusNotFound, "Setting not found"},
		{"netgroup not found", models.ErrNetgroupNotFound, http.StatusNotFound, "Netgroup not found"},

		// Duplicate/conflict errors -> 409
		{"duplicate user", models.ErrDuplicateUser, http.StatusConflict, "User already exists"},
		{"duplicate group", models.ErrDuplicateGroup, http.StatusConflict, "Group already exists"},
		{"duplicate share", models.ErrDuplicateShare, http.StatusConflict, "Share already exists"},
		{"duplicate store", models.ErrDuplicateStore, http.StatusConflict, "Store already exists"},
		{"duplicate adapter", models.ErrDuplicateAdapter, http.StatusConflict, "Adapter already exists"},
		{"duplicate netgroup", models.ErrDuplicateNetgroup, http.StatusConflict, "Netgroup already exists"},
		{"store in use", models.ErrStoreInUse, http.StatusConflict, "Store is referenced by shares"},
		{"netgroup in use", models.ErrNetgroupInUse, http.StatusConflict, "Netgroup is referenced by shares"},

		// Forbidden errors -> 403
		{"user disabled", models.ErrUserDisabled, http.StatusForbidden, "User account is disabled"},
		{"guest disabled", models.ErrGuestDisabled, http.StatusForbidden, "Guest access is disabled"},

		// Unknown errors -> 500
		{"unknown error", errors.New("something unexpected"), http.StatusInternalServerError, "Internal server error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, msg := MapStoreError(tt.err)
			if status != tt.wantStatus {
				t.Errorf("MapStoreError(%v) status = %d, want %d", tt.err, status, tt.wantStatus)
			}
			if msg != tt.wantMsg {
				t.Errorf("MapStoreError(%v) msg = %q, want %q", tt.err, msg, tt.wantMsg)
			}
		})
	}
}

func TestMapStoreError_WrappedErrors(t *testing.T) {
	wrapped := errors.Join(errors.New("context"), models.ErrUserNotFound)
	status, msg := MapStoreError(wrapped)
	if status != http.StatusNotFound {
		t.Errorf("MapStoreError(wrapped) status = %d, want %d", status, http.StatusNotFound)
	}
	if msg != "User not found" {
		t.Errorf("MapStoreError(wrapped) msg = %q, want %q", msg, "User not found")
	}
}

func TestHandleStoreError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
		wantDetail string
	}{
		{
			name:       "not found",
			err:        models.ErrUserNotFound,
			wantStatus: http.StatusNotFound,
			wantTitle:  "Not Found",
			wantDetail: "User not found",
		},
		{
			name:       "conflict",
			err:        models.ErrDuplicateUser,
			wantStatus: http.StatusConflict,
			wantTitle:  "Conflict",
			wantDetail: "User already exists",
		},
		{
			name:       "unknown",
			err:        errors.New("boom"),
			wantStatus: http.StatusInternalServerError,
			wantTitle:  "Internal Server Error",
			wantDetail: "Internal server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			HandleStoreError(w, tt.err)

			if w.Code != tt.wantStatus {
				t.Errorf("HandleStoreError status = %d, want %d", w.Code, tt.wantStatus)
			}

			ct := w.Header().Get("Content-Type")
			if ct != ContentTypeProblemJSON {
				t.Errorf("Content-Type = %q, want %q", ct, ContentTypeProblemJSON)
			}

			var p Problem
			if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
				t.Fatalf("failed to decode problem response: %v", err)
			}
			if p.Title != tt.wantTitle {
				t.Errorf("problem.Title = %q, want %q", p.Title, tt.wantTitle)
			}
			if p.Detail != tt.wantDetail {
				t.Errorf("problem.Detail = %q, want %q", p.Detail, tt.wantDetail)
			}
			if p.Status != tt.wantStatus {
				t.Errorf("problem.Status = %d, want %d", p.Status, tt.wantStatus)
			}
		})
	}
}
