package identity

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"time"

	"github.com/google/uuid"
)

const (
	// AdminUsername is the reserved username for the system administrator.
	AdminUsername = "admin"

	// EnvAdminInitialPassword is the environment variable that can be used
	// to set the initial admin password. If not set, a random password is generated.
	EnvAdminInitialPassword = "DITTOFS_ADMIN_INITIAL_PASSWORD"

	// DefaultAdminDisplayName is the display name for the admin user.
	DefaultAdminDisplayName = "Administrator"
)

// DefaultAdminUser creates a new admin user with the given password hashes.
// The user will have MustChangePassword set to true, requiring a password
// change on first login.
func DefaultAdminUser(passwordHash, ntHash string) *User {
	return &User{
		ID:                 uuid.New().String(),
		Username:           AdminUsername,
		PasswordHash:       passwordHash,
		NTHash:             ntHash,
		Enabled:            true,
		MustChangePassword: true,
		Role:               RoleAdmin,
		DisplayName:        DefaultAdminDisplayName,
		CreatedAt:          time.Now(),
	}
}

// GetOrGenerateAdminPassword returns the admin password from the environment
// variable if set, otherwise generates a cryptographically secure random password.
// The generated password is 24 characters of URL-safe base64.
// Returns an error if random password generation fails.
func GetOrGenerateAdminPassword() (string, error) {
	if pw := os.Getenv(EnvAdminInitialPassword); pw != "" {
		return pw, nil
	}
	return GenerateRandomPassword()
}

// GenerateRandomPassword generates a cryptographically secure random password.
// Returns a 24-character URL-safe base64 string (18 bytes of randomness).
// Returns an error if the system's random number generator fails.
func GenerateRandomPassword() (string, error) {
	b := make([]byte, 18)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// IsAdminUsername checks if the given username is the reserved admin username.
func IsAdminUsername(username string) bool {
	return username == AdminUsername
}
