package models

import "errors"

// Common errors for identity and control plane operations.
var (
	// User errors
	ErrUserNotFound  = errors.New("user not found")
	ErrDuplicateUser = errors.New("user already exists")
	ErrUserDisabled  = errors.New("user account is disabled")

	// Group errors
	ErrGroupNotFound  = errors.New("group not found")
	ErrDuplicateGroup = errors.New("group already exists")

	// Share errors
	ErrShareNotFound  = errors.New("share not found")
	ErrDuplicateShare = errors.New("share already exists")

	// Store errors
	ErrStoreNotFound  = errors.New("store not found")
	ErrDuplicateStore = errors.New("store already exists")
	ErrStoreInUse     = errors.New("store is referenced by shares")

	// Adapter errors
	ErrAdapterNotFound  = errors.New("adapter not found")
	ErrDuplicateAdapter = errors.New("adapter already exists")

	// Setting errors
	ErrSettingNotFound = errors.New("setting not found")

	// Netgroup errors
	ErrNetgroupNotFound  = errors.New("netgroup not found")
	ErrDuplicateNetgroup = errors.New("netgroup already exists")
	ErrNetgroupInUse     = errors.New("netgroup is referenced by shares")

	// Guest errors
	ErrGuestDisabled = errors.New("guest access is disabled")
)
