package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Common errors for JWT operations.
var (
	ErrInvalidToken        = errors.New("invalid token")
	ErrExpiredToken        = errors.New("token has expired")
	ErrInvalidTokenType    = errors.New("invalid token type")
	ErrTokenSigningFailed  = errors.New("failed to sign token")
	ErrInvalidSecretLength = errors.New("JWT secret must be at least 32 characters")
)

// JWTConfig holds configuration for JWT token generation.
type JWTConfig struct {
	// Secret is the HMAC signing key. Must be at least 32 characters.
	Secret string

	// Issuer is the token issuer claim. Default: "dittofs"
	Issuer string

	// AccessTokenDuration is the lifetime of access tokens. Default: 15 minutes.
	AccessTokenDuration time.Duration

	// RefreshTokenDuration is the lifetime of refresh tokens. Default: 7 days.
	RefreshTokenDuration time.Duration
}

// JWTService handles JWT token generation and validation.
type JWTService struct {
	config JWTConfig
}

// TokenPair contains both access and refresh tokens.
type TokenPair struct {
	// AccessToken is the short-lived token for API authorization.
	AccessToken string `json:"access_token"`

	// RefreshToken is the long-lived token for obtaining new access tokens.
	RefreshToken string `json:"refresh_token"`

	// TokenType is always "Bearer".
	TokenType string `json:"token_type"`

	// ExpiresIn is the access token lifetime in seconds.
	ExpiresIn int64 `json:"expires_in"`

	// ExpiresAt is the access token expiration time.
	ExpiresAt time.Time `json:"expires_at"`
}

// NewJWTService creates a new JWT service with the given configuration.
func NewJWTService(config JWTConfig) (*JWTService, error) {
	if len(config.Secret) < 32 {
		return nil, ErrInvalidSecretLength
	}

	// Apply defaults
	if config.Issuer == "" {
		config.Issuer = "dittofs"
	}
	if config.AccessTokenDuration == 0 {
		config.AccessTokenDuration = 15 * time.Minute
	}
	if config.RefreshTokenDuration == 0 {
		config.RefreshTokenDuration = 7 * 24 * time.Hour
	}

	return &JWTService{config: config}, nil
}

// GenerateTokenPair creates a new access/refresh token pair for the given user.
func (s *JWTService) GenerateTokenPair(user *models.User) (*TokenPair, error) {
	now := time.Now()
	accessExpiry := now.Add(s.config.AccessTokenDuration)
	refreshExpiry := now.Add(s.config.RefreshTokenDuration)

	// Generate access token
	accessToken, err := s.generateToken(user, TokenTypeAccess, now, accessExpiry)
	if err != nil {
		return nil, fmt.Errorf("failed to generate access token: %w", err)
	}

	// Generate refresh token
	refreshToken, err := s.generateToken(user, TokenTypeRefresh, now, refreshExpiry)
	if err != nil {
		return nil, fmt.Errorf("failed to generate refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int64(s.config.AccessTokenDuration.Seconds()),
		ExpiresAt:    accessExpiry,
	}, nil
}

// generateToken creates a single JWT token.
func (s *JWTService) generateToken(user *models.User, tokenType TokenType, issuedAt, expiresAt time.Time) (string, error) {
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.config.Issuer,
			Subject:   user.Username,
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		UserID:             user.ID,
		Username:           user.Username,
		Role:               string(user.Role),
		Groups:             user.GetGroupNames(),
		TokenType:          tokenType,
		MustChangePassword: user.MustChangePassword,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signedToken, err := token.SignedString([]byte(s.config.Secret))
	if err != nil {
		return "", ErrTokenSigningFailed
	}

	return signedToken, nil
}

// ValidateToken validates a JWT token and returns the claims.
// Returns an error if the token is invalid, expired, or has wrong type.
func (s *JWTService) ValidateToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		// Verify signing method
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(s.config.Secret), nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	return claims, nil
}

// ValidateAccessToken validates a token and ensures it's an access token.
func (s *JWTService) ValidateAccessToken(tokenString string) (*Claims, error) {
	claims, err := s.ValidateToken(tokenString)
	if err != nil {
		return nil, err
	}

	if !claims.IsAccessToken() {
		return nil, ErrInvalidTokenType
	}

	return claims, nil
}

// ValidateRefreshToken validates a token and ensures it's a refresh token.
func (s *JWTService) ValidateRefreshToken(tokenString string) (*Claims, error) {
	claims, err := s.ValidateToken(tokenString)
	if err != nil {
		return nil, err
	}

	if !claims.IsRefreshToken() {
		return nil, ErrInvalidTokenType
	}

	return claims, nil
}

// GetAccessTokenDuration returns the configured access token duration.
func (s *JWTService) GetAccessTokenDuration() time.Duration {
	return s.config.AccessTokenDuration
}

// GetRefreshTokenDuration returns the configured refresh token duration.
func (s *JWTService) GetRefreshTokenDuration() time.Duration {
	return s.config.RefreshTokenDuration
}
