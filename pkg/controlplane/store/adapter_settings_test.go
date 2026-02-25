//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func createTestStoreWithNFSAdapter(t *testing.T) (*GORMStore, *models.AdapterConfig) {
	t.Helper()
	s := createTestStore(t)
	ctx := context.Background()

	adapter := &models.AdapterConfig{
		ID:      uuid.New().String(),
		Type:    "nfs",
		Enabled: true,
		Port:    12049,
	}
	if _, err := s.CreateAdapter(ctx, adapter); err != nil {
		t.Fatalf("Failed to create NFS adapter: %v", err)
	}

	// EnsureAdapterSettings is called in New(), but we created adapter after,
	// so call it explicitly
	if err := s.EnsureAdapterSettings(ctx); err != nil {
		t.Fatalf("Failed to ensure adapter settings: %v", err)
	}

	return s, adapter
}

func createTestStoreWithSMBAdapter(t *testing.T) (*GORMStore, *models.AdapterConfig) {
	t.Helper()
	s := createTestStore(t)
	ctx := context.Background()

	adapter := &models.AdapterConfig{
		ID:      uuid.New().String(),
		Type:    "smb",
		Enabled: true,
		Port:    12445,
	}
	if _, err := s.CreateAdapter(ctx, adapter); err != nil {
		t.Fatalf("Failed to create SMB adapter: %v", err)
	}

	if err := s.EnsureAdapterSettings(ctx); err != nil {
		t.Fatalf("Failed to ensure adapter settings: %v", err)
	}

	return s, adapter
}

func TestEnsureAdapterSettings_CreatesDefaults(t *testing.T) {
	s, adapter := createTestStoreWithNFSAdapter(t)
	ctx := context.Background()

	settings, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings failed: %v", err)
	}

	if settings.AdapterID != adapter.ID {
		t.Errorf("AdapterID = %s, want %s", settings.AdapterID, adapter.ID)
	}
	if settings.Version != 1 {
		t.Errorf("Version = %d, want 1", settings.Version)
	}
}

func TestGetNFSAdapterSettings_ReturnsDefaults(t *testing.T) {
	s, adapter := createTestStoreWithNFSAdapter(t)
	ctx := context.Background()

	settings, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings failed: %v", err)
	}

	defaults := models.NewDefaultNFSSettings("")

	if settings.MinVersion != defaults.MinVersion {
		t.Errorf("MinVersion = %s, want %s", settings.MinVersion, defaults.MinVersion)
	}
	if settings.MaxVersion != defaults.MaxVersion {
		t.Errorf("MaxVersion = %s, want %s", settings.MaxVersion, defaults.MaxVersion)
	}
	if settings.LeaseTime != defaults.LeaseTime {
		t.Errorf("LeaseTime = %d, want %d", settings.LeaseTime, defaults.LeaseTime)
	}
	if settings.GracePeriod != defaults.GracePeriod {
		t.Errorf("GracePeriod = %d, want %d", settings.GracePeriod, defaults.GracePeriod)
	}
	if settings.DelegationRecallTimeout != defaults.DelegationRecallTimeout {
		t.Errorf("DelegationRecallTimeout = %d, want %d", settings.DelegationRecallTimeout, defaults.DelegationRecallTimeout)
	}
	if settings.CallbackTimeout != defaults.CallbackTimeout {
		t.Errorf("CallbackTimeout = %d, want %d", settings.CallbackTimeout, defaults.CallbackTimeout)
	}
	if settings.LeaseBreakTimeout != defaults.LeaseBreakTimeout {
		t.Errorf("LeaseBreakTimeout = %d, want %d", settings.LeaseBreakTimeout, defaults.LeaseBreakTimeout)
	}
	if settings.MaxConnections != defaults.MaxConnections {
		t.Errorf("MaxConnections = %d, want %d", settings.MaxConnections, defaults.MaxConnections)
	}
	if settings.MaxClients != defaults.MaxClients {
		t.Errorf("MaxClients = %d, want %d", settings.MaxClients, defaults.MaxClients)
	}
	if settings.MaxCompoundOps != defaults.MaxCompoundOps {
		t.Errorf("MaxCompoundOps = %d, want %d", settings.MaxCompoundOps, defaults.MaxCompoundOps)
	}
	if settings.MaxReadSize != defaults.MaxReadSize {
		t.Errorf("MaxReadSize = %d, want %d", settings.MaxReadSize, defaults.MaxReadSize)
	}
	if settings.MaxWriteSize != defaults.MaxWriteSize {
		t.Errorf("MaxWriteSize = %d, want %d", settings.MaxWriteSize, defaults.MaxWriteSize)
	}
	if settings.PreferredTransferSize != defaults.PreferredTransferSize {
		t.Errorf("PreferredTransferSize = %d, want %d", settings.PreferredTransferSize, defaults.PreferredTransferSize)
	}
	if settings.DelegationsEnabled != defaults.DelegationsEnabled {
		t.Errorf("DelegationsEnabled = %v, want %v", settings.DelegationsEnabled, defaults.DelegationsEnabled)
	}
}

func TestUpdateNFSAdapterSettings_IncrementsVersion(t *testing.T) {
	s, adapter := createTestStoreWithNFSAdapter(t)
	ctx := context.Background()

	settings, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings failed: %v", err)
	}

	originalVersion := settings.Version
	settings.LeaseTime = 120

	if err := s.UpdateNFSAdapterSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateNFSAdapterSettings failed: %v", err)
	}

	updated, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings after update failed: %v", err)
	}

	if updated.Version != originalVersion+1 {
		t.Errorf("Version = %d, want %d (incremented)", updated.Version, originalVersion+1)
	}
	if updated.LeaseTime != 120 {
		t.Errorf("LeaseTime = %d, want 120", updated.LeaseTime)
	}
}

func TestUpdateNFSAdapterSettings_PreservesUnchanged(t *testing.T) {
	s, adapter := createTestStoreWithNFSAdapter(t)
	ctx := context.Background()

	settings, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings failed: %v", err)
	}

	originalGracePeriod := settings.GracePeriod
	originalMaxClients := settings.MaxClients
	settings.LeaseTime = 200

	if err := s.UpdateNFSAdapterSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateNFSAdapterSettings failed: %v", err)
	}

	updated, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings after update failed: %v", err)
	}

	if updated.GracePeriod != originalGracePeriod {
		t.Errorf("GracePeriod = %d, want %d (unchanged)", updated.GracePeriod, originalGracePeriod)
	}
	if updated.MaxClients != originalMaxClients {
		t.Errorf("MaxClients = %d, want %d (unchanged)", updated.MaxClients, originalMaxClients)
	}
	if updated.LeaseTime != 200 {
		t.Errorf("LeaseTime = %d, want 200", updated.LeaseTime)
	}
}

func TestResetNFSAdapterSettings_RestoresDefaults(t *testing.T) {
	s, adapter := createTestStoreWithNFSAdapter(t)
	ctx := context.Background()

	// Modify settings
	settings, _ := s.GetNFSAdapterSettings(ctx, adapter.ID)
	settings.LeaseTime = 500
	settings.GracePeriod = 500
	settings.MaxClients = 999
	if err := s.UpdateNFSAdapterSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateNFSAdapterSettings failed: %v", err)
	}

	// Reset
	if err := s.ResetNFSAdapterSettings(ctx, adapter.ID); err != nil {
		t.Fatalf("ResetNFSAdapterSettings failed: %v", err)
	}

	// Verify defaults restored
	restored, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings after reset failed: %v", err)
	}

	defaults := models.NewDefaultNFSSettings("")
	if restored.LeaseTime != defaults.LeaseTime {
		t.Errorf("LeaseTime = %d, want %d (default)", restored.LeaseTime, defaults.LeaseTime)
	}
	if restored.GracePeriod != defaults.GracePeriod {
		t.Errorf("GracePeriod = %d, want %d (default)", restored.GracePeriod, defaults.GracePeriod)
	}
	if restored.MaxClients != defaults.MaxClients {
		t.Errorf("MaxClients = %d, want %d (default)", restored.MaxClients, defaults.MaxClients)
	}
}

func TestGetSMBAdapterSettings(t *testing.T) {
	s, adapter := createTestStoreWithSMBAdapter(t)
	ctx := context.Background()

	settings, err := s.GetSMBAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetSMBAdapterSettings failed: %v", err)
	}

	defaults := models.NewDefaultSMBSettings("")
	if settings.MinDialect != defaults.MinDialect {
		t.Errorf("MinDialect = %s, want %s", settings.MinDialect, defaults.MinDialect)
	}
	if settings.MaxDialect != defaults.MaxDialect {
		t.Errorf("MaxDialect = %s, want %s", settings.MaxDialect, defaults.MaxDialect)
	}
	if settings.SessionTimeout != defaults.SessionTimeout {
		t.Errorf("SessionTimeout = %d, want %d", settings.SessionTimeout, defaults.SessionTimeout)
	}
	if settings.OplockBreakTimeout != defaults.OplockBreakTimeout {
		t.Errorf("OplockBreakTimeout = %d, want %d", settings.OplockBreakTimeout, defaults.OplockBreakTimeout)
	}
	if settings.Version != 1 {
		t.Errorf("Version = %d, want 1", settings.Version)
	}
}

func TestEnsureAdapterSettings_Idempotent(t *testing.T) {
	s, adapter := createTestStoreWithNFSAdapter(t)
	ctx := context.Background()

	// Get initial settings
	settings1, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings failed: %v", err)
	}

	// Call EnsureAdapterSettings again
	if err := s.EnsureAdapterSettings(ctx); err != nil {
		t.Fatalf("Second EnsureAdapterSettings failed: %v", err)
	}

	// Verify settings are unchanged
	settings2, err := s.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		t.Fatalf("GetNFSAdapterSettings after second ensure failed: %v", err)
	}

	if settings1.ID != settings2.ID {
		t.Errorf("Settings ID changed: %s -> %s", settings1.ID, settings2.ID)
	}
	if settings1.Version != settings2.Version {
		t.Errorf("Settings Version changed: %d -> %d", settings1.Version, settings2.Version)
	}
}

func TestUpdateSMBAdapterSettings_IncrementsVersion(t *testing.T) {
	s, adapter := createTestStoreWithSMBAdapter(t)
	ctx := context.Background()

	settings, _ := s.GetSMBAdapterSettings(ctx, adapter.ID)
	originalVersion := settings.Version
	settings.SessionTimeout = 1800

	if err := s.UpdateSMBAdapterSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSMBAdapterSettings failed: %v", err)
	}

	updated, _ := s.GetSMBAdapterSettings(ctx, adapter.ID)
	if updated.Version != originalVersion+1 {
		t.Errorf("Version = %d, want %d", updated.Version, originalVersion+1)
	}
	if updated.SessionTimeout != 1800 {
		t.Errorf("SessionTimeout = %d, want 1800", updated.SessionTimeout)
	}
}

func TestResetSMBAdapterSettings_RestoresDefaults(t *testing.T) {
	s, adapter := createTestStoreWithSMBAdapter(t)
	ctx := context.Background()

	// Modify
	settings, _ := s.GetSMBAdapterSettings(ctx, adapter.ID)
	settings.SessionTimeout = 9999
	s.UpdateSMBAdapterSettings(ctx, settings)

	// Reset
	if err := s.ResetSMBAdapterSettings(ctx, adapter.ID); err != nil {
		t.Fatalf("ResetSMBAdapterSettings failed: %v", err)
	}

	restored, _ := s.GetSMBAdapterSettings(ctx, adapter.ID)
	defaults := models.NewDefaultSMBSettings("")
	if restored.SessionTimeout != defaults.SessionTimeout {
		t.Errorf("SessionTimeout = %d, want %d (default)", restored.SessionTimeout, defaults.SessionTimeout)
	}
}

func TestNFSAdapterSettings_BlockedOperations(t *testing.T) {
	s, adapter := createTestStoreWithNFSAdapter(t)
	ctx := context.Background()

	settings, _ := s.GetNFSAdapterSettings(ctx, adapter.ID)

	// Set blocked operations
	settings.SetBlockedOperations([]string{"WRITE", "REMOVE"})
	if err := s.UpdateNFSAdapterSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateNFSAdapterSettings failed: %v", err)
	}

	updated, _ := s.GetNFSAdapterSettings(ctx, adapter.ID)
	ops := updated.GetBlockedOperations()
	if len(ops) != 2 {
		t.Fatalf("BlockedOperations count = %d, want 2", len(ops))
	}
	if ops[0] != "WRITE" || ops[1] != "REMOVE" {
		t.Errorf("BlockedOperations = %v, want [WRITE REMOVE]", ops)
	}
}

// ============================================
// NETGROUP STORE TESTS
// ============================================

func TestCreateNetgroup(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{
		Name: "office-network",
	}

	id, err := s.CreateNetgroup(ctx, ng)
	if err != nil {
		t.Fatalf("CreateNetgroup failed: %v", err)
	}
	if id == "" {
		t.Error("Expected non-empty ID")
	}

	// Verify in DB
	retrieved, err := s.GetNetgroup(ctx, "office-network")
	if err != nil {
		t.Fatalf("GetNetgroup failed: %v", err)
	}
	if retrieved.Name != "office-network" {
		t.Errorf("Name = %s, want office-network", retrieved.Name)
	}
	if retrieved.ID == "" {
		t.Error("Expected non-empty ID on retrieved netgroup")
	}
}

func TestCreateNetgroup_DuplicateName(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "duplicate-test"}
	if _, err := s.CreateNetgroup(ctx, ng); err != nil {
		t.Fatalf("First CreateNetgroup failed: %v", err)
	}

	ng2 := &models.Netgroup{Name: "duplicate-test"}
	_, err := s.CreateNetgroup(ctx, ng2)
	if err != models.ErrDuplicateNetgroup {
		t.Errorf("Expected ErrDuplicateNetgroup, got %v", err)
	}
}

func TestDeleteNetgroup(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "delete-test"}
	s.CreateNetgroup(ctx, ng)

	if err := s.DeleteNetgroup(ctx, "delete-test"); err != nil {
		t.Fatalf("DeleteNetgroup failed: %v", err)
	}

	_, err := s.GetNetgroup(ctx, "delete-test")
	if err != models.ErrNetgroupNotFound {
		t.Errorf("Expected ErrNetgroupNotFound after delete, got %v", err)
	}
}

func TestDeleteNetgroup_InUse(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	// Create netgroup
	ng := &models.Netgroup{Name: "in-use-test"}
	ngID, _ := s.CreateNetgroup(ctx, ng)

	// Need metadata and payload stores for a share
	metaStore := &models.MetadataStoreConfig{
		ID:   uuid.New().String(),
		Name: "test-meta",
		Type: "memory",
	}
	s.CreateMetadataStore(ctx, metaStore)

	payloadStore := &models.PayloadStoreConfig{
		ID:   uuid.New().String(),
		Name: "test-payload",
		Type: "memory",
	}
	s.CreatePayloadStore(ctx, payloadStore)

	// Create share and associate netgroup via NFS adapter config
	share := &models.Share{
		ID:              uuid.New().String(),
		Name:            "/test-share",
		MetadataStoreID: metaStore.ID,
		PayloadStoreID:  payloadStore.ID,
		CreatedAt:       time.Now(),
	}
	s.CreateShare(ctx, share)

	// Create NFS adapter config referencing the netgroup
	nfsOpts := models.DefaultNFSExportOptions()
	nfsOpts.NetgroupID = &ngID
	nfsCfg := &models.ShareAdapterConfig{ShareID: share.ID, AdapterType: "nfs"}
	if err := nfsCfg.SetConfig(nfsOpts); err != nil {
		t.Fatalf("Failed to set NFS config: %v", err)
	}
	if err := s.SetShareAdapterConfig(ctx, nfsCfg); err != nil {
		t.Fatalf("Failed to set adapter config: %v", err)
	}

	// Try to delete - should fail
	err := s.DeleteNetgroup(ctx, "in-use-test")
	if err != models.ErrNetgroupInUse {
		t.Errorf("Expected ErrNetgroupInUse, got %v", err)
	}
}

func TestAddNetgroupMember_IP(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "ip-test"}
	s.CreateNetgroup(ctx, ng)

	member := &models.NetgroupMember{
		Type:  "ip",
		Value: "192.168.1.100",
	}
	if err := s.AddNetgroupMember(ctx, "ip-test", member); err != nil {
		t.Fatalf("AddNetgroupMember failed: %v", err)
	}

	members, err := s.GetNetgroupMembers(ctx, "ip-test")
	if err != nil {
		t.Fatalf("GetNetgroupMembers failed: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("Expected 1 member, got %d", len(members))
	}
	if members[0].Value != "192.168.1.100" {
		t.Errorf("Member value = %s, want 192.168.1.100", members[0].Value)
	}
	if members[0].Type != "ip" {
		t.Errorf("Member type = %s, want ip", members[0].Type)
	}
}

func TestAddNetgroupMember_CIDR(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "cidr-test"}
	s.CreateNetgroup(ctx, ng)

	member := &models.NetgroupMember{
		Type:  "cidr",
		Value: "10.0.0.0/8",
	}
	if err := s.AddNetgroupMember(ctx, "cidr-test", member); err != nil {
		t.Fatalf("AddNetgroupMember failed: %v", err)
	}

	members, _ := s.GetNetgroupMembers(ctx, "cidr-test")
	if len(members) != 1 || members[0].Value != "10.0.0.0/8" {
		t.Errorf("Expected CIDR member 10.0.0.0/8, got %v", members)
	}
}

func TestAddNetgroupMember_Hostname(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "hostname-test"}
	s.CreateNetgroup(ctx, ng)

	member := &models.NetgroupMember{
		Type:  "hostname",
		Value: "*.example.com",
	}
	if err := s.AddNetgroupMember(ctx, "hostname-test", member); err != nil {
		t.Fatalf("AddNetgroupMember failed: %v", err)
	}

	members, _ := s.GetNetgroupMembers(ctx, "hostname-test")
	if len(members) != 1 || members[0].Value != "*.example.com" {
		t.Errorf("Expected hostname member *.example.com, got %v", members)
	}
}

func TestRemoveNetgroupMember(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "remove-member-test"}
	s.CreateNetgroup(ctx, ng)

	member := &models.NetgroupMember{
		Type:  "ip",
		Value: "10.0.0.1",
	}
	s.AddNetgroupMember(ctx, "remove-member-test", member)

	members, _ := s.GetNetgroupMembers(ctx, "remove-member-test")
	if len(members) != 1 {
		t.Fatalf("Expected 1 member before remove, got %d", len(members))
	}

	if err := s.RemoveNetgroupMember(ctx, "remove-member-test", members[0].ID); err != nil {
		t.Fatalf("RemoveNetgroupMember failed: %v", err)
	}

	members, _ = s.GetNetgroupMembers(ctx, "remove-member-test")
	if len(members) != 0 {
		t.Errorf("Expected 0 members after remove, got %d", len(members))
	}
}

func TestGetNetgroupMembers_Multiple(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "multi-member-test"}
	s.CreateNetgroup(ctx, ng)

	for _, val := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		s.AddNetgroupMember(ctx, "multi-member-test", &models.NetgroupMember{
			Type:  "ip",
			Value: val,
		})
	}

	members, err := s.GetNetgroupMembers(ctx, "multi-member-test")
	if err != nil {
		t.Fatalf("GetNetgroupMembers failed: %v", err)
	}
	if len(members) != 3 {
		t.Errorf("Expected 3 members, got %d", len(members))
	}
}

func TestGetNetgroup_WithMembers(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "preload-test"}
	s.CreateNetgroup(ctx, ng)

	s.AddNetgroupMember(ctx, "preload-test", &models.NetgroupMember{Type: "ip", Value: "1.2.3.4"})
	s.AddNetgroupMember(ctx, "preload-test", &models.NetgroupMember{Type: "cidr", Value: "10.0.0.0/24"})

	retrieved, err := s.GetNetgroup(ctx, "preload-test")
	if err != nil {
		t.Fatalf("GetNetgroup failed: %v", err)
	}

	if len(retrieved.Members) != 2 {
		t.Errorf("Expected 2 preloaded members, got %d", len(retrieved.Members))
	}
}

func TestListNetgroups(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	s.CreateNetgroup(ctx, &models.Netgroup{Name: "ng-1"})
	s.CreateNetgroup(ctx, &models.Netgroup{Name: "ng-2"})
	s.CreateNetgroup(ctx, &models.Netgroup{Name: "ng-3"})

	netgroups, err := s.ListNetgroups(ctx)
	if err != nil {
		t.Fatalf("ListNetgroups failed: %v", err)
	}
	if len(netgroups) != 3 {
		t.Errorf("Expected 3 netgroups, got %d", len(netgroups))
	}
}

func TestAddNetgroupMember_InvalidType(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "invalid-type-test"}
	s.CreateNetgroup(ctx, ng)

	member := &models.NetgroupMember{
		Type:  "foobar",
		Value: "something",
	}
	err := s.AddNetgroupMember(ctx, "invalid-type-test", member)
	if err == nil {
		t.Error("Expected error for invalid member type")
	}
}

func TestAddNetgroupMember_InvalidCIDR(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	ng := &models.Netgroup{Name: "invalid-cidr-test"}
	s.CreateNetgroup(ctx, ng)

	member := &models.NetgroupMember{
		Type:  "cidr",
		Value: "not-a-cidr",
	}
	err := s.AddNetgroupMember(ctx, "invalid-cidr-test", member)
	if err == nil {
		t.Error("Expected error for invalid CIDR")
	}
}
