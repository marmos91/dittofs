// Package kvstoretest is the metadata store's conformance suite: every case
// drives the store through its public views only and never reads a key, so
// any KV backend that passes kv/kvtest can run it as it is.
package kvstoretest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// Opener returns a fresh, empty KV for one case, closed when the case ends.
type Opener func(tb testing.TB) kv.KV

// backends is the one backend a race case runs on: the suite's.
func (o Opener) backends() []backend { return []backend{{name: "kv", open: o}} }

// Run runs every case against stores over KVs from open.
func Run(t *testing.T, open Opener) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T, Opener)
	}{
		{"SetAttrsRights", testSetAttrsRights},
		{"Times", testTimes},
		{"SetAttrsClearsVerifier", testSetAttrsClearsVerifier},
		{"ACLs", testACLs},
		{"Xattrs", testXattrs},
		{"ImmutableAndAppendOnly", testImmutableAndAppendOnly},
		{"StoredACLDecides", testStoredACLDecides},
		{"RevokedGrantRefusesNextCall", testRevokedGrantRefusesNextCall},
		{"CookieSurvivesDeletes", testCookieSurvivesDeletes},
		{"ReadCosts", testReadCosts},
		{"ParallelCreatesInOneDirectory", testParallelCreatesInOneDirectory},
		{"UserRenameAndIDsMoveTheirRows", testUserRenameAndIDsMoveTheirRows},
		{"PrincipalUniqueness", testPrincipalUniqueness},
		{"DeleteRemovesMembershipsAndGrants", testDeleteRemovesMembershipsAndGrants},
		{"Shares", testShares},
		{"SetShareState", testSetShareState},
		{"GrantVersions", testGrantVersions},
		{"Quotas", testQuotas},
		{"QuotaSoftChange", testQuotaSoftChange},
		{"ModeBitRights", testModeBitRights},
		{"ExportPolicy", testExportPolicy},
		{"NetgroupsAndSettings", testNetgroupsAndSettings},
		{"ListingPages", testListingPages},
		{"ShareBoundSettings", testShareBoundSettings},
		{"Costs", testCosts},
		{"RootAndResolve", testRootAndResolve},
		{"Entries", testEntries},
		{"CreateAndLookup", testCreateAndLookup},
		{"CaseInsensitiveShare", testCaseInsensitiveShare},
		{"OwnerAndGroupAtCreate", testOwnerAndGroupAtCreate},
		{"ExclusiveCreate", testExclusiveCreate},
		{"CreateCost", testCreateCost},
		{"GetAndGrant", testGetAndGrant},
		{"UniqueClaimsRace", testUniqueClaimsRace},
		{"DeleteRacesAdd", testDeleteRacesAdd},
		{"ShareStateRace", testShareStateRace},
		{"UnlinkAndLink", testUnlinkAndLink},
		{"RemoveDirectory", testRemoveDirectory},
		{"RemoveDirectoryRacesCreate", testRemoveDirectoryRacesCreate},
		{"RemovalRights", testRemovalRights},
		{"NlinkIsExactlyItsEntries", testNlinkIsExactlyItsEntries},
		{"Rename", testRename},
		{"RenameOver", testRenameOver},
		{"RenameUnderItself", testRenameUnderItself},
		{"RenameLoopRace", testRenameLoopRace},
		{"RenameOverRacesCreate", testRenameOverRacesCreate},
		{"DeleteShareMark", testDeleteShareMark},
		{"DeleteShareRacesRename", testDeleteShareRacesRename},
		{"ShareTags", testShareTags},
		{"VersionFloor", testVersionFloor},
		{"UsageFollowsOwner", testUsageFollowsOwner},
		{"FoldDirectory", testFoldDirectory},
		{"DirectoryTimeSet", testDirectoryTimeSet},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, open) })
	}
}
