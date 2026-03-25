package store

import (
	"context"

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
	var result T
	q := db.WithContext(ctx)
	for _, p := range preloads {
		q = q.Preload(p)
	}
	if err := q.Where(field+" = ?", value).First(&result).Error; err != nil {
		return nil, convertNotFoundError(err, notFoundErr)
	}
	return &result, nil
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
