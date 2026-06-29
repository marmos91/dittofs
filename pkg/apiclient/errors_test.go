package apiclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAPIError_Error_RendersFieldErrors(t *testing.T) {
	t.Run("with field errors", func(t *testing.T) {
		e := &APIError{
			Detail:     "One or more settings are outside valid range",
			StatusCode: 422,
			Errors: map[string]string{
				"blocked_operations": "unknown NFS operation: INVALID_OP",
				"portmapper_port":    "must be >= 1",
			},
		}
		got := e.Error()
		// Base detail is preserved.
		if !strings.Contains(got, "One or more settings are outside valid range") {
			t.Fatalf("missing base detail: %q", got)
		}
		// Both field messages are surfaced, in stable (sorted) order.
		if !strings.Contains(got, "blocked_operations: unknown NFS operation: INVALID_OP") {
			t.Fatalf("missing blocked_operations field error: %q", got)
		}
		if !strings.Contains(got, "portmapper_port: must be >= 1") {
			t.Fatalf("missing portmapper_port field error: %q", got)
		}
		if idx1, idx2 := strings.Index(got, "blocked_operations"), strings.Index(got, "portmapper_port"); idx1 > idx2 {
			t.Fatalf("field errors not in sorted order: %q", got)
		}
	})

	t.Run("without field errors falls back to detail", func(t *testing.T) {
		e := &APIError{Detail: "boom", StatusCode: 400}
		if got := e.Error(); got != "boom" {
			t.Fatalf("want %q, got %q", "boom", got)
		}
	})

	t.Run("empty falls back to title then status", func(t *testing.T) {
		if got := (&APIError{Title: "Not Found", StatusCode: 404}).Error(); got != "Not Found" {
			t.Fatalf("want title, got %q", got)
		}
		if got := (&APIError{StatusCode: 503}).Error(); !strings.Contains(got, "503") {
			t.Fatalf("want status in message, got %q", got)
		}
	})

	t.Run("errors map decodes from problem+json", func(t *testing.T) {
		body := `{"title":"Validation Failed","detail":"bad","errors":{"x":"too big"}}`
		var e APIError
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Errors["x"] != "too big" {
			t.Fatalf("errors map not decoded: %+v", e.Errors)
		}
		if !strings.Contains(e.Error(), "x: too big") {
			t.Fatalf("decoded field error not rendered: %q", e.Error())
		}
	})
}
