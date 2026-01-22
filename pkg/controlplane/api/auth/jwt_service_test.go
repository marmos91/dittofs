package auth

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestNewJWTService_ValidConfig(t *testing.T) {
	config := JWTConfig{
		Secret:               "test-secret-key-must-be-32-chars!",
		Issuer:               "test-issuer",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	}

	service, err := NewJWTService(config)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if service == nil {
		t.Fatal("Expected service to be non-nil")
	}
}

func TestNewJWTService_EmptySecret(t *testing.T) {
	config := JWTConfig{
		Secret: "",
		Issuer: "test-issuer",
	}

	_, err := NewJWTService(config)
	if err == nil {
		t.Fatal("Expected error for empty secret")
	}
}

func TestNewJWTService_ShortSecret(t *testing.T) {
	config := JWTConfig{
		Secret: "short",
		Issuer: "test-issuer",
	}

	_, err := NewJWTService(config)
	if err == nil {
		t.Fatal("Expected error for short secret")
	}
}

func TestGenerateTokenPair(t *testing.T) {
	config := JWTConfig{
		Secret:               "test-secret-key-must-be-32-chars!",
		Issuer:               "test-issuer",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	}

	service, _ := NewJWTService(config)

	user := &models.User{
		ID:                 "test-uuid",
		Username:           "testuser",
		Role:               string(models.RoleUser),
		Groups:             []models.Group{{Name: "developers"}},
		MustChangePassword: false,
	}

	tokenPair, err := service.GenerateTokenPair(user)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if tokenPair.AccessToken == "" {
		t.Error("Expected non-empty access token")
	}
	if tokenPair.RefreshToken == "" {
		t.Error("Expected non-empty refresh token")
	}
	if tokenPair.TokenType != "Bearer" {
		t.Errorf("Expected TokenType 'Bearer', got '%s'", tokenPair.TokenType)
	}
	if tokenPair.ExpiresIn != int64(15*time.Minute/time.Second) {
		t.Errorf("Expected ExpiresIn %d, got %d", int64(15*time.Minute/time.Second), tokenPair.ExpiresIn)
	}
}

func TestValidateAccessToken(t *testing.T) {
	config := JWTConfig{
		Secret:               "test-secret-key-must-be-32-chars!",
		Issuer:               "test-issuer",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	}

	service, _ := NewJWTService(config)

	user := &models.User{
		ID:                 "test-uuid",
		Username:           "testuser",
		Role:               string(models.RoleAdmin),
		Groups:             []models.Group{{Name: "admins"}},
		MustChangePassword: true,
	}

	tokenPair, _ := service.GenerateTokenPair(user)

	// Validate the access token
	claims, err := service.ValidateAccessToken(tokenPair.AccessToken)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if claims.Username != "testuser" {
		t.Errorf("Expected username 'testuser', got '%s'", claims.Username)
	}
	if claims.UserID != "test-uuid" {
		t.Errorf("Expected UserID 'test-uuid', got '%s'", claims.UserID)
	}
	if claims.Role != "admin" {
		t.Errorf("Expected role 'admin', got '%s'", claims.Role)
	}
	if !claims.IsAdmin() {
		t.Error("Expected IsAdmin() to return true")
	}
	if !claims.MustChangePassword {
		t.Error("Expected MustChangePassword to be true")
	}
	if len(claims.Groups) != 1 || claims.Groups[0] != "admins" {
		t.Errorf("Expected groups ['admins'], got %v", claims.Groups)
	}
}

func TestValidateAccessToken_InvalidToken(t *testing.T) {
	config := JWTConfig{
		Secret:               "test-secret-key-must-be-32-chars!",
		Issuer:               "test-issuer",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	}

	service, _ := NewJWTService(config)

	_, err := service.ValidateAccessToken("invalid-token")
	if err == nil {
		t.Fatal("Expected error for invalid token")
	}
}

func TestValidateAccessToken_WrongTokenType(t *testing.T) {
	config := JWTConfig{
		Secret:               "test-secret-key-must-be-32-chars!",
		Issuer:               "test-issuer",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	}

	service, _ := NewJWTService(config)

	user := &models.User{
		ID:       "test-uuid",
		Username: "testuser",
		Role:     string(models.RoleUser),
	}

	tokenPair, _ := service.GenerateTokenPair(user)

	// Try to validate refresh token as access token
	_, err := service.ValidateAccessToken(tokenPair.RefreshToken)
	if err != ErrInvalidTokenType {
		t.Errorf("Expected ErrInvalidTokenType, got: %v", err)
	}
}

func TestValidateRefreshToken(t *testing.T) {
	config := JWTConfig{
		Secret:               "test-secret-key-must-be-32-chars!",
		Issuer:               "test-issuer",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	}

	service, _ := NewJWTService(config)

	user := &models.User{
		ID:       "test-uuid",
		Username: "testuser",
		Role:     string(models.RoleUser),
	}

	tokenPair, _ := service.GenerateTokenPair(user)

	// Validate the refresh token
	claims, err := service.ValidateRefreshToken(tokenPair.RefreshToken)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if claims.Username != "testuser" {
		t.Errorf("Expected username 'testuser', got '%s'", claims.Username)
	}
	if claims.TokenType != TokenTypeRefresh {
		t.Errorf("Expected token type 'refresh', got '%s'", claims.TokenType)
	}
}

func TestValidateRefreshToken_WrongTokenType(t *testing.T) {
	config := JWTConfig{
		Secret:               "test-secret-key-must-be-32-chars!",
		Issuer:               "test-issuer",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	}

	service, _ := NewJWTService(config)

	user := &models.User{
		ID:       "test-uuid",
		Username: "testuser",
		Role:     string(models.RoleUser),
	}

	tokenPair, _ := service.GenerateTokenPair(user)

	// Try to validate access token as refresh token
	_, err := service.ValidateRefreshToken(tokenPair.AccessToken)
	if err != ErrInvalidTokenType {
		t.Errorf("Expected ErrInvalidTokenType, got: %v", err)
	}
}

func TestClaims_IsAdmin(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"admin", true},
		{"user", false},
		{"", false},
		{"Admin", false}, // Case-sensitive
	}

	for _, tc := range tests {
		claims := &Claims{Role: tc.role}
		if claims.IsAdmin() != tc.expected {
			t.Errorf("IsAdmin() for role '%s': expected %v, got %v", tc.role, tc.expected, claims.IsAdmin())
		}
	}
}
