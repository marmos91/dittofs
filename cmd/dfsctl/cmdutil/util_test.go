package cmdutil

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

func TestParseCommaSeparatedList(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "single item",
			input:    "foo",
			expected: []string{"foo"},
		},
		{
			name:     "multiple items",
			input:    "foo,bar,baz",
			expected: []string{"foo", "bar", "baz"},
		},
		{
			name:     "items with spaces",
			input:    "foo, bar , baz",
			expected: []string{"foo", "bar", "baz"},
		},
		{
			name:     "empty items filtered out",
			input:    "foo,,bar,",
			expected: []string{"foo", "bar"},
		},
		{
			name:     "only whitespace filtered out",
			input:    "foo, , bar",
			expected: []string{"foo", "bar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseCommaSeparatedList(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("ParseCommaSeparatedList(%q) = %v, want %v", tt.input, result, tt.expected)
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("ParseCommaSeparatedList(%q)[%d] = %q, want %q", tt.input, i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestBoolToYesNo(t *testing.T) {
	tests := []struct {
		input    bool
		expected string
	}{
		{true, "yes"},
		{false, "no"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := BoolToYesNo(tt.input)
			if result != tt.expected {
				t.Errorf("BoolToYesNo(%v) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// testTableRenderer implements output.TableRenderer for testing
type testTableRenderer struct {
	headers []string
	rows    [][]string
}

func (t testTableRenderer) Headers() []string {
	return t.headers
}

func (t testTableRenderer) Rows() [][]string {
	return t.rows
}

func TestPrintOutput_JSON(t *testing.T) {
	// Set flags to JSON format
	Flags.Output = "json"

	var buf bytes.Buffer
	data := []string{"foo", "bar"}
	renderer := testTableRenderer{
		headers: []string{"NAME"},
		rows:    [][]string{{"foo"}, {"bar"}},
	}

	err := PrintOutput(&buf, data, false, "No items", renderer)
	if err != nil {
		t.Fatalf("PrintOutput() error = %v", err)
	}

	// JSON output is indented
	result := buf.String()
	if len(result) == 0 {
		t.Error("PrintOutput() returned empty output for JSON")
	}
	// Check that it contains the expected data
	if !bytes.Contains(buf.Bytes(), []byte("foo")) || !bytes.Contains(buf.Bytes(), []byte("bar")) {
		t.Errorf("PrintOutput() = %q, missing expected data", result)
	}
}

func TestPrintOutput_YAML(t *testing.T) {
	// Set flags to YAML format
	Flags.Output = "yaml"

	var buf bytes.Buffer
	data := []string{"foo", "bar"}
	renderer := testTableRenderer{
		headers: []string{"NAME"},
		rows:    [][]string{{"foo"}, {"bar"}},
	}

	err := PrintOutput(&buf, data, false, "No items", renderer)
	if err != nil {
		t.Fatalf("PrintOutput() error = %v", err)
	}

	expected := "- foo\n- bar\n"
	if buf.String() != expected {
		t.Errorf("PrintOutput() = %q, want %q", buf.String(), expected)
	}
}

func TestPrintOutput_Table_Empty(t *testing.T) {
	// Set flags to table format
	Flags.Output = "table"

	var buf bytes.Buffer
	data := []string{}
	renderer := testTableRenderer{
		headers: []string{"NAME"},
		rows:    [][]string{},
	}

	err := PrintOutput(&buf, data, true, "No items found.", renderer)
	if err != nil {
		t.Fatalf("PrintOutput() error = %v", err)
	}

	expected := "No items found.\n"
	if buf.String() != expected {
		t.Errorf("PrintOutput() = %q, want %q", buf.String(), expected)
	}
}

func TestPrintOutput_Table_WithData(t *testing.T) {
	// Set flags to table format
	Flags.Output = "table"

	var buf bytes.Buffer
	data := []string{"foo", "bar"}
	renderer := testTableRenderer{
		headers: []string{"NAME"},
		rows:    [][]string{{"foo"}, {"bar"}},
	}

	err := PrintOutput(&buf, data, false, "No items found.", renderer)
	if err != nil {
		t.Fatalf("PrintOutput() error = %v", err)
	}

	// Table output should contain headers and rows
	result := buf.String()
	if len(result) == 0 {
		t.Errorf("PrintOutput() returned empty output for table")
	}
}

func TestGetOutputFormatParsed(t *testing.T) {
	tests := []struct {
		flagValue string
		expected  output.Format
		wantErr   bool
	}{
		{"table", output.FormatTable, false},
		{"json", output.FormatJSON, false},
		{"yaml", output.FormatYAML, false},
		{"invalid", output.FormatTable, true},
	}

	for _, tt := range tests {
		t.Run(tt.flagValue, func(t *testing.T) {
			Flags.Output = tt.flagValue
			result, err := GetOutputFormatParsed()
			if (err != nil) != tt.wantErr {
				t.Errorf("GetOutputFormatParsed() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result != tt.expected {
				t.Errorf("GetOutputFormatParsed() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestIsColorDisabled(t *testing.T) {
	Flags.NoColor = true
	if !IsColorDisabled() {
		t.Error("IsColorDisabled() = false, want true")
	}

	Flags.NoColor = false
	if IsColorDisabled() {
		t.Error("IsColorDisabled() = true, want false")
	}
}

func TestIsVerbose(t *testing.T) {
	Flags.Verbose = true
	if !IsVerbose() {
		t.Error("IsVerbose() = false, want true")
	}

	Flags.Verbose = false
	if IsVerbose() {
		t.Error("IsVerbose() = true, want false")
	}
}

// seedExpiredContext writes a config.json with one expired-but-refreshable
// context pointing at serverURL, using an isolated config dir. credentials.Store
// reads APPDATA on Windows and XDG_CONFIG_HOME elsewhere.
func seedExpiredContext(t *testing.T, serverURL string) {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", dir)
	} else {
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
	store, err := credentials.NewStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := &credentials.Context{
		ServerURL:    serverURL,
		Username:     "admin",
		AccessToken:  "stale-access",
		RefreshToken: "valid-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	if err := store.SetContext("test-ctx", ctx); err != nil {
		t.Fatalf("set context: %v", err)
	}
	if err := store.UseContext("test-ctx"); err != nil {
		t.Fatalf("use context: %v", err)
	}
}

// newRefreshServer answers /api/v1/auth/refresh with the given response.
func newRefreshServer(t *testing.T, resp apiclient.TokenResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/refresh" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// clearGlobalFlags prevents state bleed between tests that mutate Flags.
func clearGlobalFlags(t *testing.T) {
	t.Helper()
	orig := *Flags
	Flags.ServerURL = ""
	Flags.Token = ""
	t.Cleanup(func() { *Flags = orig })
}

func TestGetAuthenticatedClient_RefreshRejectsEmptyRefreshToken(t *testing.T) {
	// Server refreshes access but omits the new refresh token — if we let this
	// through, store.UpdateTokens would wipe out the caller's existing refresh.
	server := newRefreshServer(t, apiclient.TokenResponse{
		AccessToken:  "new-access",
		RefreshToken: "",
		TokenType:    "Bearer",
		ExpiresIn:    900,
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	defer server.Close()

	clearGlobalFlags(t)
	seedExpiredContext(t, server.URL)

	_, err := GetAuthenticatedClient()
	if err == nil {
		t.Fatal("expected error when refresh returns empty refresh_token, got nil")
	}
	if !strings.Contains(err.Error(), "incomplete tokens") {
		t.Errorf("error should mention incomplete tokens, got: %v", err)
	}
}

func TestGetAuthenticatedClient_RefreshRejectsEmptyAccessToken(t *testing.T) {
	server := newRefreshServer(t, apiclient.TokenResponse{
		AccessToken:  "",
		RefreshToken: "new-refresh",
		TokenType:    "Bearer",
		ExpiresIn:    900,
	})
	defer server.Close()

	clearGlobalFlags(t)
	seedExpiredContext(t, server.URL)

	_, err := GetAuthenticatedClient()
	if err == nil {
		t.Fatal("expected error when refresh returns empty access_token, got nil")
	}
	if !strings.Contains(err.Error(), "incomplete tokens") {
		t.Errorf("error should mention incomplete tokens, got: %v", err)
	}
}
