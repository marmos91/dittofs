# DittoFS REST API Authentication

This guide explains how to authenticate with the DittoFS REST API using JWT tokens.

## Overview

DittoFS provides a REST API for managing users, shares, and other resources. The API uses JWT (JSON Web Token) authentication with access and refresh token pairs.

**Default API Port:** 8080

## Prerequisites

Before using the API, ensure:

1. The API is enabled in your configuration:
   ```yaml
   server:
     api:
       enabled: true
       jwt:
         secret: "your-32-character-secret-key-here"
   ```

2. You have valid user credentials (see [Admin Setup](#admin-user))

## Authentication Flow

### 1. Login to Get Tokens

**Endpoint:** `POST /api/v1/auth/login`

**Request:**
```bash
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "username": "admin",
    "password": "your-password"
  }'
```

**Response:**
```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "refresh_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "token_type": "Bearer",
  "expires_in": 900,
  "expires_at": "2024-01-15T10:45:00Z",
  "user": {
    "id": "admin",
    "username": "admin",
    "role": "admin",
    "must_change_password": false
  }
}
```

### 2. Use Access Token for Requests

Include the access token in the `Authorization` header:

```bash
curl http://localhost:8080/api/v1/auth/me \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

### 3. Refresh Token When Expired

When your access token expires, use the refresh token to get a new pair:

**Endpoint:** `POST /api/v1/auth/refresh`

```bash
curl -X POST http://localhost:8080/api/v1/auth/refresh \
  -H "Content-Type: application/json" \
  -d '{
    "refresh_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
  }'
```

## API Endpoints

### Public Endpoints (No Authentication)

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/health` | Liveness probe |
| GET | `/health/ready` | Readiness probe |
| GET | `/health/stores` | Detailed store health |
| POST | `/api/v1/auth/login` | Authenticate and get tokens |
| POST | `/api/v1/auth/refresh` | Refresh token pair |

### Authenticated Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/auth/me` | Get current user info |
| POST | `/api/v1/users/me/password` | Change own password |

### Admin-Only Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/users/` | Create a new user |
| GET | `/api/v1/users/` | List all users |
| GET | `/api/v1/users/{username}` | Get user details |
| PUT | `/api/v1/users/{username}` | Update user |
| DELETE | `/api/v1/users/{username}` | Delete user |
| POST | `/api/v1/users/{username}/password` | Reset user password |
| GET | `/api/v1/users/{username}/shares` | List user's share mappings |
| GET | `/api/v1/users/{username}/shares/{share}` | Get specific share mapping |
| PUT | `/api/v1/users/{username}/shares/{share}` | Set share mapping |
| DELETE | `/api/v1/users/{username}/shares/{share}` | Delete share mapping |

## Admin User

### Initial Setup

When DittoFS starts for the first time with the API enabled, an admin user is created automatically. If no admin password is configured, a random password is generated and logged at startup.

**Check startup logs for:**
```
Admin user created with generated password: <random-password>
```

### Configuring Admin Credentials

You can configure the admin password in your config:

```yaml
identity:
  type: memory
  admin:
    username: admin
    password: "your-secure-password"
```

Or use environment variables:
```bash
DITTOFS_IDENTITY_ADMIN_PASSWORD="your-secure-password"
```

## Token Configuration

### JWT Settings

Configure token durations in your config:

```yaml
server:
  api:
    enabled: true
    jwt:
      # Required: 32+ character secret for signing tokens
      secret: "your-32-character-secret-key-here"

      # Optional: Token issuer claim
      issuer: "dittofs"

      # Optional: Access token duration (default: 15m)
      access_token_duration: "15m"

      # Optional: Refresh token duration (default: 168h / 7 days)
      refresh_token_duration: "168h"
```

## Example: Complete Workflow

```bash
# 1. Login and save tokens
LOGIN_RESPONSE=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "password": "your-password"}')

ACCESS_TOKEN=$(echo $LOGIN_RESPONSE | jq -r '.access_token')
REFRESH_TOKEN=$(echo $LOGIN_RESPONSE | jq -r '.refresh_token')

# 2. Create a new user (admin only)
curl -X POST http://localhost:8080/api/v1/users/ \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "username": "alice",
    "password": "alice-password",
    "uid": 1001,
    "gid": 1001,
    "role": "user"
  }'

# 3. List all users
curl http://localhost:8080/api/v1/users/ \
  -H "Authorization: Bearer $ACCESS_TOKEN"

# 4. Set share permission for user
curl -X PUT http://localhost:8080/api/v1/users/alice/shares/export \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"permission": "read-write"}'

# 5. When access token expires, refresh it
NEW_TOKENS=$(curl -s -X POST http://localhost:8080/api/v1/auth/refresh \
  -H "Content-Type: application/json" \
  -d "{\"refresh_token\": \"$REFRESH_TOKEN\"}")

ACCESS_TOKEN=$(echo $NEW_TOKENS | jq -r '.access_token')
```

## Error Responses

### 401 Unauthorized

```json
{
  "error": "Invalid username or password"
}
```

Possible causes:
- Invalid credentials
- Missing or invalid `Authorization` header
- Expired access token (use refresh endpoint)

### 403 Forbidden

```json
{
  "error": "User account is disabled"
}
```

Possible causes:
- User account is disabled
- User must change password but trying to access protected endpoint
- Non-admin user trying to access admin-only endpoint

### 400 Bad Request

```json
{
  "error": "Username and password are required"
}
```

Possible causes:
- Missing required fields in request body
- Invalid JSON format

## Security Best Practices

1. **Use HTTPS in production** - Always use TLS to encrypt API traffic
2. **Protect the JWT secret** - Use a strong, unique secret and store it securely
3. **Short access token duration** - Default 15 minutes is recommended
4. **Rotate refresh tokens** - Consider implementing token rotation
5. **Secure password storage** - Passwords are stored using bcrypt
6. **Change default admin password** - Always change the generated admin password

## Kubernetes Operator

When using the DittoFS Kubernetes Operator, configure JWT authentication via the CRD:

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: my-dittofs
spec:
  identity:
    type: memory
    jwt:
      secretRef:
        name: dittofs-jwt-secret
        key: secret
    admin:
      username: admin
      passwordSecretRef:
        name: dittofs-admin-secret
        key: password
```

Create the secrets:
```bash
# Generate a 32-character secret for JWT signing
kubectl create secret generic dittofs-jwt-secret \
  --from-literal=secret=$(openssl rand -base64 32)

# Create admin password secret
kubectl create secret generic dittofs-admin-secret \
  --from-literal=password='your-secure-admin-password'
```
