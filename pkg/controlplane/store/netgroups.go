package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func (s *GORMStore) GetNetgroup(ctx context.Context, name string) (*models.Netgroup, error) {
	return getByField[models.Netgroup](s.db, ctx, "name", name, models.ErrNetgroupNotFound, "Members")
}

func (s *GORMStore) GetNetgroupByID(ctx context.Context, id string) (*models.Netgroup, error) {
	return getByField[models.Netgroup](s.db, ctx, "id", id, models.ErrNetgroupNotFound, "Members")
}

func (s *GORMStore) ListNetgroups(ctx context.Context) ([]*models.Netgroup, error) {
	return listAll[models.Netgroup](s.db, ctx, "Members")
}

func (s *GORMStore) CreateNetgroup(ctx context.Context, netgroup *models.Netgroup) (string, error) {
	now := time.Now()
	netgroup.CreatedAt = now
	netgroup.UpdatedAt = now
	return createWithID(s.db, ctx, netgroup, func(n *models.Netgroup, id string) { n.ID = id }, netgroup.ID, models.ErrDuplicateNetgroup)
}

func (s *GORMStore) DeleteNetgroup(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var netgroup models.Netgroup
		if err := tx.Where("name = ?", name).First(&netgroup).Error; err != nil {
			return convertNotFoundError(err, models.ErrNetgroupNotFound)
		}

		// Check if any NFS adapter configs reference this netgroup ID in their JSON config.
		// The netgroup_id is stored inside share_adapter_configs.config JSON blob.
		var refCount int64
		if err := tx.Model(&models.ShareAdapterConfig{}).
			Where("adapter_type = ? AND config LIKE ?", "nfs", "%"+netgroup.ID+"%").
			Count(&refCount).Error; err != nil {
			return err
		}
		if refCount > 0 {
			return models.ErrNetgroupInUse
		}

		// Delete all members first
		if err := tx.Where("netgroup_id = ?", netgroup.ID).Delete(&models.NetgroupMember{}).Error; err != nil {
			return err
		}

		// Delete the netgroup
		return tx.Delete(&netgroup).Error
	})
}

func (s *GORMStore) AddNetgroupMember(ctx context.Context, netgroupName string, member *models.NetgroupMember) error {
	// Validate member type
	if !models.ValidateMemberType(member.Type) {
		return fmt.Errorf("invalid member type: %s", member.Type)
	}

	// Validate member value
	if err := models.ValidateMemberValue(member.Type, member.Value); err != nil {
		return err
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var netgroup models.Netgroup
		if err := tx.Where("name = ?", netgroupName).First(&netgroup).Error; err != nil {
			return convertNotFoundError(err, models.ErrNetgroupNotFound)
		}

		if member.ID == "" {
			member.ID = uuid.New().String()
		}
		member.NetgroupID = netgroup.ID
		member.CreatedAt = time.Now()

		return tx.Create(member).Error
	})
}

func (s *GORMStore) RemoveNetgroupMember(ctx context.Context, netgroupName, memberID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var netgroup models.Netgroup
		if err := tx.Where("name = ?", netgroupName).First(&netgroup).Error; err != nil {
			return convertNotFoundError(err, models.ErrNetgroupNotFound)
		}

		return tx.Where("id = ? AND netgroup_id = ?", memberID, netgroup.ID).Delete(&models.NetgroupMember{}).Error
	})
}

func (s *GORMStore) GetNetgroupMembers(ctx context.Context, netgroupName string) ([]*models.NetgroupMember, error) {
	netgroup, err := s.GetNetgroup(ctx, netgroupName)
	if err != nil {
		return nil, err
	}

	members := make([]*models.NetgroupMember, len(netgroup.Members))
	for i := range netgroup.Members {
		members[i] = &netgroup.Members[i]
	}
	return members, nil
}

func (s *GORMStore) GetSharesByNetgroup(ctx context.Context, netgroupName string) ([]*models.Share, error) {
	netgroup, err := s.GetNetgroup(ctx, netgroupName)
	if err != nil {
		return nil, err
	}

	// Find share IDs from NFS adapter configs that reference this netgroup in their JSON config.
	var configs []models.ShareAdapterConfig
	if err := s.db.WithContext(ctx).
		Where("adapter_type = ? AND config LIKE ?", "nfs", "%"+netgroup.ID+"%").
		Find(&configs).Error; err != nil {
		return nil, err
	}

	if len(configs) == 0 {
		return nil, nil
	}

	shareIDs := make([]string, len(configs))
	for i, cfg := range configs {
		shareIDs[i] = cfg.ShareID
	}

	var shares []*models.Share
	if err := s.db.WithContext(ctx).
		Preload("MetadataStore").
		Preload("PayloadStore").
		Where("id IN ?", shareIDs).
		Find(&shares).Error; err != nil {
		return nil, err
	}
	return shares, nil
}
