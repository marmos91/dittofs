package store

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Generic GORM helpers that reduce repetitive CRUD boilerplate across store
// implementation files. Unexported and operate on raw *gorm.DB to avoid coupling
// to GORMStore.

// getByField retrieves a single record of type T by matching field=value.
// It applies optional GORM Preload clauses and converts gorm.ErrRecordNotFound
// to the provided notFoundErr for consistent domain error mapping.
//
// Example:
//
//	user, err := getByField[models.User](db, ctx, "username", "alice", models.ErrUserNotFound, "Groups", "SharePermissions")
func getByField[T any](db *gorm.DB, ctx context.Context, field string, value any, notFoundErr error, preloads ...string) (*T, error) {
	return getByFieldScoped[T](db, ctx, field, value, nil, notFoundErr, preloads...)
}

// fieldEq is an extra column=value equality constraint ANDed into a lookup. It
// lets partitioned tables (e.g. block stores, which are scoped by kind) resolve
// only within their partition.
type fieldEq struct {
	column string
	value  any
}

// getByFieldScoped is getByField with additional equality constraints ANDed into
// the query. Pass a nil scope for an unscoped lookup.
func getByFieldScoped[T any](db *gorm.DB, ctx context.Context, field string, value any, scope []fieldEq, notFoundErr error, preloads ...string) (*T, error) {
	var result T
	q := db.WithContext(ctx)
	for _, p := range preloads {
		q = q.Preload(p)
	}
	for _, c := range scope {
		q = q.Where(c.column+" = ?", c.value)
	}
	if err := q.Where(field+" = ?", value).First(&result).Error; err != nil {
		return nil, convertNotFoundError(err, notFoundErr)
	}
	return &result, nil
}

// getByNameOrIDWithin retrieves a single record of type T by its natural-key
// column (name, username, type, ...), falling back to its opaque UUID "id" when
// no natural-key match exists. This lets API consumers address a record by
// either its human-readable name or its ID (docker-style). The natural key is
// matched first and the ID lookup only runs on a genuine not-found, so a name
// that happens to look like a UUID still resolves to the named record.
//
// scope confines resolution to one partition (e.g. block stores within a kind):
// its constraints are ANDed into every attempt. Pass a nil scope for an
// unpartitioned lookup, or use getByNameOrID.
//
// Share names are stored with a leading "/" (the API layer prepends it), so a
// raw ID arrives here as "/<id>"; the final attempt strips that prefix. IDs
// never contain a slash, so this is a no-op for every other caller. Each
// fallback is gated on notFoundErr so a genuine DB error is never masked.
func getByNameOrIDWithin[T any](db *gorm.DB, ctx context.Context, naturalKey, token string, notFoundErr error, scope []fieldEq, preloads ...string) (*T, error) {
	result, err := getByFieldScoped[T](db, ctx, naturalKey, token, scope, notFoundErr, preloads...)
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, notFoundErr) {
		return nil, err
	}

	result, err = getByFieldScoped[T](db, ctx, "id", token, scope, notFoundErr, preloads...)
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, notFoundErr) {
		return nil, err
	}
	if trimmed := strings.TrimPrefix(token, "/"); trimmed != token {
		return getByFieldScoped[T](db, ctx, "id", trimmed, scope, notFoundErr, preloads...)
	}
	return nil, err
}

// getByNameOrID is getByNameOrIDWithin for unpartitioned entities (nil scope).
func getByNameOrID[T any](db *gorm.DB, ctx context.Context, naturalKey, token string, notFoundErr error, preloads ...string) (*T, error) {
	return getByNameOrIDWithin[T](db, ctx, naturalKey, token, notFoundErr, nil, preloads...)
}

// listAll retrieves all records of type T, applying optional GORM Preload clauses.
// Returns an empty slice (not nil) on success with no records.
//
// Example:
//
//	users, err := listAll[models.User](db, ctx, "Groups", "SharePermissions")
func listAll[T any](db *gorm.DB, ctx context.Context, preloads ...string) ([]*T, error) {
	var results []*T
	q := db.WithContext(ctx)
	for _, p := range preloads {
		q = q.Preload(p)
	}
	if err := q.Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

// createWithID generates a UUID for the entity if it has no ID, then creates
// it in the database. The idSetter callback sets the generated ID on the entity.
// Unique constraint violations are converted to dupErr for consistent error handling.
//
// Example:
//
//	id, err := createWithID[models.User](db, ctx, user, func(u *models.User, id string) { u.ID = id }, models.ErrDuplicateUser)
func createWithID[T any](db *gorm.DB, ctx context.Context, entity *T, idSetter func(*T, string), currentID string, dupErr error) (string, error) {
	id := currentID
	if id == "" {
		id = uuid.New().String()
		idSetter(entity, id)
	}
	if err := db.WithContext(ctx).Create(entity).Error; err != nil {
		if isUniqueConstraintError(err) {
			return "", dupErr
		}
		return "", err
	}
	return id, nil
}

// dedup returns a new slice with duplicate strings removed, preserving order.
func dedup(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	result := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			result = append(result, v)
		}
	}
	return result
}

// deleteByField deletes records of type T matching field=value.
// Returns notFoundErr if no rows were affected.
//
// Example:
//
//	err := deleteByField[models.AdapterConfig](db, ctx, "type", "nfs", models.ErrAdapterNotFound)
func deleteByField[T any](db *gorm.DB, ctx context.Context, field string, value any, notFoundErr error) error {
	var zero T
	result := db.WithContext(ctx).Where(field+" = ?", value).Delete(&zero)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return notFoundErr
	}
	return nil
}
