package apiclient

import "fmt"

// ============================================================================
// Generic API Client Helpers
// ============================================================================
//
// These helpers reduce repetitive HTTP boilerplate across API client resource
// files. Each helper wraps the underlying Client.get/post/put/delete methods
// with type-safe generics for request/response handling. They are unexported
// (package-internal).

// getResource performs a GET request to the given path and decodes the response
// body into a value of type T. Returns a pointer to the decoded value.
//
// Example:
//
//	user, err := getResource[User](c, "/api/v1/users/alice")
func getResource[T any](c *Client, path string) (*T, error) {
	var result T
	if err := c.get(path, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// listResources performs a GET request to the given path and decodes the response
// body into a slice of type T.
//
// Example:
//
//	users, err := listResources[User](c, "/api/v1/users")
func listResources[T any](c *Client, path string) ([]T, error) {
	var results []T
	if err := c.get(path, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// createResource performs a POST request to the given path with the provided body
// and decodes the response into a value of type T. Returns a pointer to the decoded
// value.
//
// Example:
//
//	user, err := createResource[User](c, "/api/v1/users", req)
func createResource[T any](c *Client, path string, body any) (*T, error) {
	var result T
	if err := c.post(path, body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// updateResource performs a PUT request to the given path with the provided body
// and decodes the response into a value of type T. Returns a pointer to the decoded
// value.
//
// Example:
//
//	user, err := updateResource[User](c, "/api/v1/users/alice", req)
func updateResource[T any](c *Client, path string, body any) (*T, error) {
	var result T
	if err := c.put(path, body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// deleteResource performs a DELETE request to the given path.
//
// Example:
//
//	err := deleteResource(c, "/api/v1/users/alice")
func deleteResource(c *Client, path string) error {
	return c.delete(path, nil)
}

// resourcePath builds a resource path by formatting a path template with the given
// arguments using fmt.Sprintf.
//
// Example:
//
//	path := resourcePath("/api/v1/users/%s", "alice")
func resourcePath(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
