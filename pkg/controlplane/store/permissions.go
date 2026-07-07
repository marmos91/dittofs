package store

import (
	"context"
	"errors"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func (s *GORMStore) GetUserSharePermission(ctx context.Context, username, shareName string) (*models.UserSharePermission, error) {
	// Get user and share IDs
	user, err := s.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}

	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	var perm models.UserSharePermission
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND share_id = ?", user.ID, share.ID).
		First(&perm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil // No permission is not an error
		}
		return nil, err
	}
	return &perm, nil
}

func (s *GORMStore) SetUserSharePermission(ctx context.Context, perm *models.UserSharePermission) error {
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "share_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"share_name", "permission"}),
		}).
		Create(perm).Error
}

func (s *GORMStore) DeleteUserSharePermission(ctx context.Context, username, shareName string) error {
	// Get user and share IDs
	user, err := s.GetUser(ctx, username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			return nil // Not an error if user doesn't exist
		}
		return err
	}

	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			return nil // Not an error if share doesn't exist
		}
		return err
	}

	return s.db.WithContext(ctx).
		Where("user_id = ? AND share_id = ?", user.ID, share.ID).
		Delete(&models.UserSharePermission{}).Error
}

func (s *GORMStore) GetUserSharePermissions(ctx context.Context, username string) ([]*models.UserSharePermission, error) {
	user, err := s.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}

	var perms []*models.UserSharePermission
	if err := s.db.WithContext(ctx).
		Where("user_id = ?", user.ID).
		Find(&perms).Error; err != nil {
		return nil, err
	}
	return perms, nil
}

func (s *GORMStore) GetGroupSharePermission(ctx context.Context, groupName, shareName string) (*models.GroupSharePermission, error) {
	group, err := s.GetGroup(ctx, groupName)
	if err != nil {
		return nil, err
	}

	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	var perm models.GroupSharePermission
	if err := s.db.WithContext(ctx).
		Where("group_id = ? AND share_id = ?", group.ID, share.ID).
		First(&perm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &perm, nil
}

func (s *GORMStore) SetGroupSharePermission(ctx context.Context, perm *models.GroupSharePermission) error {
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "group_id"}, {Name: "share_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"share_name", "permission"}),
		}).
		Create(perm).Error
}

func (s *GORMStore) DeleteGroupSharePermission(ctx context.Context, groupName, shareName string) error {
	group, err := s.GetGroup(ctx, groupName)
	if err != nil {
		if errors.Is(err, models.ErrGroupNotFound) {
			return nil
		}
		return err
	}

	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			return nil
		}
		return err
	}

	return s.db.WithContext(ctx).
		Where("group_id = ? AND share_id = ?", group.ID, share.ID).
		Delete(&models.GroupSharePermission{}).Error
}

func (s *GORMStore) GetGroupSharePermissions(ctx context.Context, groupName string) ([]*models.GroupSharePermission, error) {
	group, err := s.GetGroup(ctx, groupName)
	if err != nil {
		return nil, err
	}

	var perms []*models.GroupSharePermission
	if err := s.db.WithContext(ctx).
		Where("group_id = ?", group.ID).
		Find(&perms).Error; err != nil {
		return nil, err
	}
	return perms, nil
}

func (s *GORMStore) GetShareUserPermissions(ctx context.Context, shareName string) ([]*models.UserSharePermission, error) {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	var perms []*models.UserSharePermission
	if err := s.db.WithContext(ctx).
		Where("share_id = ?", share.ID).
		Find(&perms).Error; err != nil {
		return nil, err
	}
	return perms, nil
}

func (s *GORMStore) GetShareGroupPermissions(ctx context.Context, shareName string) ([]*models.GroupSharePermission, error) {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	var perms []*models.GroupSharePermission
	if err := s.db.WithContext(ctx).
		Where("share_id = ?", share.ID).
		Find(&perms).Error; err != nil {
		return nil, err
	}
	return perms, nil
}

func (s *GORMStore) ResolveSharePermission(ctx context.Context, user *models.User, shareName string) (models.SharePermission, error) {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return models.PermissionNone, err
	}

	defaultPerm := models.ParseSharePermission(share.DefaultPermission)

	if user == nil {
		return defaultPerm, nil
	}

	// Check user explicit permission first
	for _, perm := range user.SharePermissions {
		if perm.ShareID == share.ID {
			return models.ParseSharePermission(perm.Permission), nil
		}
	}

	// Check group permissions (highest wins)
	highestPerm := defaultPerm
	for _, group := range user.Groups {
		for _, perm := range share.GroupPermissions {
			if perm.GroupID == group.ID {
				p := models.ParseSharePermission(perm.Permission)
				if p.Level() > highestPerm.Level() {
					highestPerm = p
				}
			}
		}
	}

	return highestPerm, nil
}

func (s *GORMStore) SetSIDSharePermission(ctx context.Context, perm *models.SIDSharePermission) error {
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "sid"}, {Name: "share_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"share_name", "permission", "is_group", "display_name", "unix_id"}),
		}).
		Create(perm).Error
}

func (s *GORMStore) DeleteSIDSharePermission(ctx context.Context, sid, shareName string) error {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			return nil // Not an error if share doesn't exist
		}
		return err
	}

	return s.db.WithContext(ctx).
		Where("sid = ? AND share_id = ?", sid, share.ID).
		Delete(&models.SIDSharePermission{}).Error
}

// DeleteSIDSharePermissionsByDisplayName removes SID grants on a share whose
// stored DisplayName (the principal's sAMAccountName, captured at grant time)
// matches, so a name-based revoke works without re-resolving the name through
// LDAP. isGroup disambiguates a user grant from a group grant of the same name.
func (s *GORMStore) DeleteSIDSharePermissionsByDisplayName(ctx context.Context, shareName, displayName string, isGroup bool) error {
	if displayName == "" {
		return nil
	}
	return s.db.WithContext(ctx).
		Where("share_name = ? AND display_name = ? AND is_group = ?", shareName, displayName, isGroup).
		Delete(&models.SIDSharePermission{}).Error
}

func (s *GORMStore) GetShareSIDPermissions(ctx context.Context, shareName string) ([]*models.SIDSharePermission, error) {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	var perms []*models.SIDSharePermission
	if err := s.db.WithContext(ctx).
		Where("share_id = ?", share.ID).
		Find(&perms).Error; err != nil {
		return nil, err
	}
	return perms, nil
}

// ResolveSharePermissionForSIDs is on the SMB TREE_CONNECT hot path, so it
// queries the SID grant table directly by the denormalized share_name (unique)
// rather than loading the share with all its associations first.
func (s *GORMStore) ResolveSharePermissionForSIDs(ctx context.Context, sids []string, shareName string) (models.SharePermission, error) {
	if len(sids) == 0 {
		return models.PermissionNone, nil
	}

	var perms []models.SIDSharePermission
	if err := s.db.WithContext(ctx).
		Where("share_name = ? AND sid IN ?", shareName, sids).
		Find(&perms).Error; err != nil {
		return models.PermissionNone, err
	}

	highest := models.PermissionNone
	for _, perm := range perms {
		p := models.ParseSharePermission(perm.Permission)
		if p.Level() > highest.Level() {
			highest = p
		}
	}
	return highest, nil
}

// ResolveSharePermissionForUnixIDs is on the NFS auth hot path; like
// ResolveSharePermissionForSIDs it queries by share_name to avoid a full share
// load. Only SID grants that carry a resolved numeric id can match an NFS login,
// which has no SID on the wire.
//
// CAVEAT: NFS matching is by MAPPED Unix id, not the SID itself (the SID is not
// on the NFS wire), so it inherits the idmap's range contract: a local principal
// whose UID/GID collides with a foreign SID's mapped id would match the grant.
// This is safe only when AD ids do not overlap the local id space — the same
// requirement Samba meets with dedicated idmap ranges. The SMB path
// (ResolveSharePermissionForSIDs) is always SID-exact and not subject to this.
func (s *GORMStore) ResolveSharePermissionForUnixIDs(ctx context.Context, uid uint32, gids []uint32, shareName string) (models.SharePermission, error) {
	// Restrict the query to the login's candidate ids (uid + gids) so the scan
	// is bounded by the caller's group count, not the total number of grants on
	// the share. The in-memory pass below still applies the user-vs-group rule.
	candidates := append(make([]uint32, 0, len(gids)+1), uid)
	candidates = append(candidates, gids...)

	var perms []models.SIDSharePermission
	if err := s.db.WithContext(ctx).
		Where("share_name = ? AND unix_id <> 0 AND unix_id IN ?", shareName, candidates).
		Find(&perms).Error; err != nil {
		return models.PermissionNone, err
	}

	highest := models.PermissionNone
	for _, perm := range perms {
		matched := perm.UnixID == uid
		if perm.IsGroup {
			matched = slices.Contains(gids, perm.UnixID)
		}
		if !matched {
			continue
		}
		if p := models.ParseSharePermission(perm.Permission); p.Level() > highest.Level() {
			highest = p
		}
	}
	return highest, nil
}
