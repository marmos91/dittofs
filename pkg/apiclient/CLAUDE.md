# pkg/apiclient

REST API client library for communicating with the DittoFS control plane server.

## Architecture

```
Client (client.go)
    │
    ├─► Auth methods (auth.go)
    │   └─► Login, RefreshToken, Logout
    │
    ├─► User methods (users.go)
    │   └─► ListUsers, GetUser, CreateUser, UpdateUser, DeleteUser
    │
    ├─► Group methods (groups.go)
    │   └─► ListGroups, GetGroup, CreateGroup, UpdateGroup, DeleteGroup
    │
    ├─► Share methods (shares.go)
    │   └─► ListShares, GetShare, CreateShare, UpdateShare, DeleteShare
    │
    ├─► Store methods (stores.go)
    │   └─► Metadata and Payload store management
    │
    ├─► Adapter methods (adapters.go)
    │   └─► ListAdapters, GetAdapter, EnableAdapter, DisableAdapter
    │
    └─► Settings methods (settings.go)
        └─► GetSettings, UpdateSettings
```

## Usage Pattern

```go
// Create client
client := apiclient.New("http://localhost:8080")

// Login to get token
tokens, err := client.Login("username", "password")
if err != nil {
    // Handle error
}

// Use token for authenticated requests
authClient := client.WithToken(tokens.AccessToken)

// Make authenticated requests
users, err := authClient.ListUsers()
```

## Key Conventions

### Token Management
- `WithToken()` returns a new client with the token set (immutable pattern)
- `SetToken()` modifies the client in place
- Token is sent as `Authorization: Bearer <token>` header

### Error Handling
- API errors are returned as `*APIError` type
- Check with `err.(*APIError)` type assertion
- Helper methods: `IsAuthError()`, `IsNotFound()`, `IsConflict()`

### Request/Response Types
- Request types use pointers for optional fields (e.g., `*string`, `*bool`)
- Response types use value types with `omitempty` JSON tags
- All methods return `(*Type, error)` or `error` for void operations

## Testing

Tests use `httptest.NewServer` to mock server responses:

```go
server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    // Verify request
    assert.Equal(t, "/api/v1/users", r.URL.Path)

    // Return mock response
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode([]User{...})
}))
defer server.Close()

client := New(server.URL)
users, err := client.ListUsers()
```
