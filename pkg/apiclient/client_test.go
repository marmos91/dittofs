package apiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	client := New("http://localhost:8080")
	assert.NotNil(t, client)
	assert.Equal(t, "http://localhost:8080", client.baseURL)
}

func TestWithToken(t *testing.T) {
	client := New("http://localhost:8080")
	tokenClient := client.WithToken("test-token")

	// Original client should not have token
	assert.Empty(t, client.token)

	// New client should have token
	assert.Equal(t, "test-token", tokenClient.token)
	assert.Equal(t, "http://localhost:8080", tokenClient.baseURL)
}

func TestSetToken(t *testing.T) {
	client := New("http://localhost:8080")
	client.SetToken("my-token")
	assert.Equal(t, "my-token", client.token)
}

func TestDoWithSuccess(t *testing.T) {
	type Response struct {
		Message string `json:"message"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Response{Message: "success"})
	}))
	defer server.Close()

	client := New(server.URL)

	var resp Response
	err := client.get("/test", &resp)
	require.NoError(t, err)
	assert.Equal(t, "success", resp.Message)
}

func TestDoWithAuthHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	err := client.get("/test", nil)
	require.NoError(t, err)
}

func TestDoWithAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(APIError{
			Code:    "UNAUTHORIZED",
			Message: "Invalid credentials",
		})
	}))
	defer server.Close()

	client := New(server.URL)
	err := client.get("/test", nil)
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok)
	assert.Equal(t, "UNAUTHORIZED", apiErr.Code)
	assert.Equal(t, "Invalid credentials", apiErr.Message)
	assert.True(t, apiErr.IsAuthError())
}

func TestDoWithPost(t *testing.T) {
	type Request struct {
		Name string `json:"name"`
	}
	type Response struct {
		ID int `json:"id"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)

		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		assert.Equal(t, "test", req.Name)

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Response{ID: 123})
	}))
	defer server.Close()

	client := New(server.URL)

	var resp Response
	err := client.post("/test", Request{Name: "test"}, &resp)
	require.NoError(t, err)
	assert.Equal(t, 123, resp.ID)
}
