package fixture

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// configure sets the server up through its API: its stores on the fixture's
// profile, with the blocks of an S3 profile in b, the share, its squash, the
// users, and the NFS settings, all before dt enables the NFS adapter, which
// reads them as it starts. The server's answer to the settings must hold
// them as set. Users get password, and each UID in the fixture's users a
// read-write grant on the share.
func configure(c *apiclient.Client, f registry.Fixture, b *bucket, data, password string, nfsPort int) error {
	off, on := false, true
	// decision: on a fresh database the server creates its NFS and SMB
	// adapters enabled, on 12049 and 12445, and starts them; no setting stops
	// that. So between the server's start and this step the fixture may hold
	// those ports, or fail to bind them where another server holds them, which
	// changes nothing here. A developer's own dfs that starts in that moment
	// finds the port taken and runs without that adapter, logging it, until
	// the adapter is enabled again. Withdraw this when the server can start
	// without its default adapters.
	for _, a := range []string{"nfs", "smb"} {
		if _, err := c.UpdateAdapter(a, &apiclient.UpdateAdapterRequest{Enabled: &off}); err != nil {
			return fmt.Errorf("turning off the %s adapter the server starts with: %w", a, err)
		}
	}
	meta := map[string]any{"in_memory": true}
	if f.Profile != "memory" { // badger and badger-s3 keep it on disk
		meta = map[string]any{"path": filepath.Join(data, "metadata")}
	}
	if _, err := c.CreateMetadataStore(&apiclient.CreateStoreRequest{Name: "default", Type: "badger", Config: meta}); err != nil {
		return fmt.Errorf("the metadata store: %w", err)
	}
	blocks := &apiclient.CreateStoreRequest{Name: "default", Type: "memory"}
	if b != nil {
		blocks.Type, blocks.Config = "s3", b.storeConfig()
	}
	if _, err := c.CreateBlockStore(blocks); err != nil {
		return fmt.Errorf("the block store: %w", err)
	}
	if _, err := c.CreateShare(&apiclient.CreateShareRequest{Name: share, MetadataStoreID: "default", BlockStore: "default"}); err != nil {
		return fmt.Errorf("the share %s: %w", share, err)
	}
	if f.Squash != "" {
		if _, err := c.PatchShareNFSConfig(share, &apiclient.PatchShareNFSConfigRequest{Squash: &f.Squash}); err != nil {
			return fmt.Errorf("squash %s: %w", f.Squash, err)
		}
	}
	for _, uid := range f.Users {
		// decision: each user's GID is its UID, as the suites' scripts set
		// them up today.
		id, name := uint32(uid), fmt.Sprintf("dt-%d", uid)
		if _, err := c.CreateUser(&apiclient.CreateUserRequest{Username: name, Password: password, UID: &id, GID: &id}); err != nil {
			return fmt.Errorf("user %d: %w", uid, err)
		}
		if err := c.SetUserSharePermission(share, name, "read-write"); err != nil {
			return fmt.Errorf("user %d's grant: %w", uid, err)
		}
	}
	// decision: {user} has a UID and GID of its own, the first above the
	// range users may name, since a user without one maps to 1000, which a
	// users entry or the developer may hold. It has no grant on the default
	// share: the shares-file rules give it the admin grant and the root on a
	// shares file's shares, and nothing on the default one yet.
	id := userUID
	if _, err := c.CreateUser(&apiclient.CreateUserRequest{Username: user, Password: password, UID: &id, GID: &id}); err != nil {
		return fmt.Errorf("the test user: %w", err)
	}
	settings, err := nfsSettings(f)
	if err != nil {
		return err
	}
	if settings != nil {
		got, err := c.PatchNFSSettings(settings)
		if err != nil {
			return fmt.Errorf("the NFS settings: %w", err)
		}
		if l := settings.LeaseTime; l != nil && got.LeaseTime != *l {
			return fmt.Errorf("the server reports a lease of %ds, not the %ds dt set", got.LeaseTime, *l)
		}
		if d := settings.DelegationsEnabled; d != nil && got.DelegationsEnabled != *d {
			return fmt.Errorf("the server reports delegations %t, not the %t dt set", got.DelegationsEnabled, *d)
		}
	}
	if nfsPort > 0 {
		if _, err := c.UpdateAdapter("nfs", &apiclient.UpdateAdapterRequest{Port: &nfsPort, Enabled: &on}); err != nil {
			return fmt.Errorf("enabling NFS on port %d: %w", nfsPort, err)
		}
	}
	return nil
}

// nfsSettings is the NFS adapter settings fixture f sets, or nil for none.
func nfsSettings(f registry.Fixture) (*apiclient.PatchNFSSettingsRequest, error) {
	if f.Delegations == nil && f.Lease == "" {
		return nil, nil
	}
	req := &apiclient.PatchNFSSettingsRequest{DelegationsEnabled: f.Delegations}
	if f.Lease != "" {
		d, err := time.ParseDuration(f.Lease)
		if err != nil || d <= 0 || d%time.Second != 0 {
			return nil, fmt.Errorf("lease %q: a whole number of seconds, as 30s", f.Lease)
		}
		secs := int(d / time.Second)
		req.LeaseTime = &secs
	}
	return req, nil
}

// check is the fixture's check before the first test that the server
// answers: an authenticated call answers, and shows NFS served on its port
// when the fixture asks for it, and no adapter it doesn't ask for. With an S3
// profile's bucket b, the server's own check of its block store must find it
// healthy too.
func check(c *apiclient.Client, b *bucket, nfsPort int) error {
	adapters, err := c.ListAdapters()
	if err != nil {
		return fmt.Errorf("an authenticated call: %w", err)
	}
	served := false
	for _, a := range adapters {
		switch {
		case a.Type == "nfs" && nfsPort > 0:
			served = a.Running && a.Port == nfsPort
		case a.Running:
			return fmt.Errorf("the %s adapter runs, which the fixture doesn't ask for", a.Type)
		}
	}
	if nfsPort > 0 && !served {
		return fmt.Errorf("NFS isn't served on port %d", nfsPort)
	}
	if b == nil {
		return nil
	}
	h, err := c.BlockStoreHealth("default")
	if err != nil {
		return fmt.Errorf("the block store's health: %w", err)
	}
	if !h.Healthy {
		return fmt.Errorf("the server can't reach the bucket %s at %s: %s", bucketName, b.endpoint, h.Details)
	}
	return nil
}
