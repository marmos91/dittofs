package apiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListUsers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/users", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]User{
			{ID: "1", Username: "user1", Role: "user", Enabled: true},
			{ID: "2", Username: "user2", Role: "admin", Enabled: true},
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	users, err := client.ListUsers()

	require.NoError(t, err)
	assert.Len(t, users, 2)
	assert.Equal(t, "user1", users[0].Username)
	assert.Equal(t, "user2", users[1].Username)
}

func TestGetUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/users/testuser", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(User{
			ID:          "user-123",
			Username:    "testuser",
			DisplayName: "Test User",
			Email:       "test@example.com",
			Role:        "user",
			Enabled:     true,
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	user, err := client.GetUser("testuser")

	require.NoError(t, err)
	assert.Equal(t, "user-123", user.ID)
	assert.Equal(t, "testuser", user.Username)
	assert.Equal(t, "Test User", user.DisplayName)
}

func TestGetUser_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(APIError{
			Code:    "NOT_FOUND",
			Message: "User not found",
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	user, err := client.GetUser("nonexistent")

	assert.Nil(t, user)
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok)
	assert.Equal(t, "NOT_FOUND", apiErr.Code)
	assert.True(t, apiErr.IsNotFound())
}

func TestCreateUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/users", r.URL.Path)

		var req CreateUserRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.Equal(t, "newuser", req.Username)
		assert.Equal(t, "password123", req.Password)
		assert.Equal(t, "New User", req.DisplayName)

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(User{
			ID:                 "new-user-123",
			Username:           req.Username,
			DisplayName:        req.DisplayName,
			Role:               "user",
			Enabled:            true,
			MustChangePassword: true,
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	user, err := client.CreateUser(&CreateUserRequest{
		Username:    "newuser",
		Password:    "password123",
		DisplayName: "New User",
	})

	require.NoError(t, err)
	assert.Equal(t, "new-user-123", user.ID)
	assert.Equal(t, "newuser", user.Username)
	assert.True(t, user.MustChangePassword)
}

func TestCreateUser_Duplicate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(APIError{
			Code:    "CONFLICT",
			Message: "User already exists",
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	user, err := client.CreateUser(&CreateUserRequest{
		Username: "existinguser",
		Password: "password123",
	})

	assert.Nil(t, user)
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok)
	assert.Equal(t, "CONFLICT", apiErr.Code)
}

func TestUpdateUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/api/v1/users/testuser", r.URL.Path)

		var req UpdateUserRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.NotNil(t, req.DisplayName)
		assert.Equal(t, "Updated Name", *req.DisplayName)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(User{
			ID:          "user-123",
			Username:    "testuser",
			DisplayName: "Updated Name",
			Role:        "user",
			Enabled:     true,
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	newDisplayName := "Updated Name"
	user, err := client.UpdateUser("testuser", &UpdateUserRequest{
		DisplayName: &newDisplayName,
	})

	require.NoError(t, err)
	assert.Equal(t, "Updated Name", user.DisplayName)
}

func TestDeleteUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/api/v1/users/deleteuser", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	err := client.DeleteUser("deleteuser")

	require.NoError(t, err)
}

func TestResetUserPassword(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/users/testuser/password", r.URL.Path)

		var req ChangePasswordRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.Equal(t, "newpassword123", req.NewPassword)
		assert.Empty(t, req.CurrentPassword) // Admin reset doesn't need current password

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := New(server.URL).WithToken("admin-token")
	err := client.ResetUserPassword("testuser", "newpassword123")

	require.NoError(t, err)
}

func TestChangeOwnPassword(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/users/me/password", r.URL.Path)

		var req ChangePasswordRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.Equal(t, "currentpassword", req.CurrentPassword)
		assert.Equal(t, "newpassword123", req.NewPassword)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TokenResponse{
			AccessToken:  "new-access-token",
			RefreshToken: "new-refresh-token",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	resp, err := client.ChangeOwnPassword("currentpassword", "newpassword123")

	require.NoError(t, err)
	assert.Equal(t, "new-access-token", resp.AccessToken)
}

func TestGetCurrentUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/auth/me", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(User{
			ID:       "current-user-123",
			Username: "currentuser",
			Role:     "user",
			Enabled:  true,
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	user, err := client.GetCurrentUser()

	require.NoError(t, err)
	assert.Equal(t, "current-user-123", user.ID)
	assert.Equal(t, "currentuser", user.Username)
}
