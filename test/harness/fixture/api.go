package fixture

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// configure sets the server up through its API: its stores on the fixture's
// profile, with the blocks of an S3 profile in b, the users, the shares with
// their settings and grants, and the adapter settings, all before dt enables
// the adapters, which read them as they start. The server's answer to the NFS
// settings must hold them as set. Users get password.
func (s *Server) configure(c *apiclient.Client, b *bucket, password string) error {
	f := s.f
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
		meta = map[string]any{"path": filepath.Join(s.data, "metadata")}
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
	// The users come before the shares, since {user} owns a shares file's
	// shares.
	for _, u := range f.Users {
		uid, gid := ids(u)
		if _, err := c.CreateUser(&apiclient.CreateUserRequest{Username: userName(u), Password: password, UID: &uid, GID: &gid}); err != nil {
			return fmt.Errorf("user %s: %w", u, err)
		}
	}
	// decision: {user} and {user2} have UIDs and GIDs of their own, the first
	// two above the range users may name, since a user without one maps to
	// 1000, which a users entry or the developer may hold.
	for _, u := range []struct {
		name string
		id   uint32
	}{{User, UserUID}, {User2, User2UID}} {
		if _, err := c.CreateUser(&apiclient.CreateUserRequest{Username: u.name, Password: password, UID: &u.id, GID: &u.id}); err != nil {
			return fmt.Errorf("the test user %s: %w", u.name, err)
		}
	}
	// {user}'s principal maps to {user}, as the scripts' bootstrap maps it: a
	// client given the principal sends it as its NTLM user name too, which
	// the server looks up through the mapping.
	if f.Kerberos {
		if _, err := c.CreateIdentityMapping("kerberos", User+"@"+Realm, User); err != nil {
			return fmt.Errorf("the identity mapping of %s@%s: %w", User, Realm, err)
		}
	}
	for _, share := range s.shares {
		if err := addShare(c, f, share); err != nil {
			return err
		}
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
		if r := settings.PortmapperRegisterWithSystem; r != nil && (!got.PortmapperRegisterWithSystem || !got.UDPEnabled) {
			return errors.New("the server reports it doesn't register with the host's rpcbind, or serves no UDP, though dt set both")
		}
	}
	if f.SMB.Encryption == "preferred" {
		if _, err := c.PatchSMBSettings(&apiclient.PatchSMBSettingsRequest{EnableEncryption: &on}); err != nil {
			return fmt.Errorf("the SMB settings: %w", err)
		}
	}
	for _, a := range []struct {
		name string
		port int
	}{{"nfs", s.nfsPort}, {"smb", s.smbPort}} {
		if a.port == 0 {
			continue
		}
		ad, err := c.UpdateAdapter(a.name, &apiclient.UpdateAdapterRequest{Port: &a.port, Enabled: &on})
		if err != nil {
			return fmt.Errorf("enabling %s on port %d: %w", strings.ToUpper(a.name), a.port, err)
		}
		if !ad.Running {
			return fmt.Errorf("%s is enabled on port %d, but its listener isn't running", strings.ToUpper(a.name), a.port)
		}
	}
	return nil
}

// addShare creates share on the default stores, with fixture f's squash
// and each of its users' read-write grant. A shares file's share has its root
// owned by {user}, who holds the admin grant on it: ownership lets {user}
// write at the root, and admin lets it take ownership of a file. On the
// default share {user} has no grant.
func addShare(c *apiclient.Client, f registry.Fixture, share apiclient.CreateShareRequest) error {
	share.MetadataStoreID, share.BlockStore = "default", "default"
	if f.Shares != "" {
		share.Owner = User
	}
	if _, err := c.CreateShare(&share); err != nil {
		return fmt.Errorf("the share %s: %w", share.Name, err)
	}
	if f.Shares != "" {
		if err := c.SetUserSharePermission(share.Name, User, "admin"); err != nil {
			return fmt.Errorf("%s's grant on %s: %w", User, share.Name, err)
		}
	}
	if f.Squash != "" {
		if _, err := c.PatchShareNFSConfig(share.Name, &apiclient.PatchShareNFSConfigRequest{Squash: &f.Squash}); err != nil {
			return fmt.Errorf("squash %s on %s: %w", f.Squash, share.Name, err)
		}
	}
	for _, u := range f.Users {
		if err := c.SetUserSharePermission(share.Name, userName(u), "read-write"); err != nil {
			return fmt.Errorf("user %s's grant on %s: %w", u, share.Name, err)
		}
	}
	return nil
}

// ids gives a fixture user's UID and GID: self is the user dt runs as, in its
// own group, and a UID's GID is the UID, as the suites' scripts set them up
// today.
func ids(user string) (uid, gid uint32) {
	if user == "self" {
		return uint32(os.Getuid()), uint32(os.Getgid())
	}
	n, _ := strconv.ParseUint(user, 10, 32) // the registry checks it's a UID
	return uint32(n), uint32(n)
}

// userName is a fixture user's name on the server.
func userName(user string) string {
	uid, _ := ids(user)
	return fmt.Sprintf("dt-%d", uid)
}

// nfsSettings is the NFS adapter settings fixture f sets, or nil for none.
// With rpcbind: host the server registers its services with the host's
// rpcbind, and serves MOUNT, NLM and NSM over UDP too, as NSM needs.
func nfsSettings(f registry.Fixture) (*apiclient.PatchNFSSettingsRequest, error) {
	if f.Delegations == nil && f.Lease == "" && f.RPCBind == "" {
		return nil, nil
	}
	req := &apiclient.PatchNFSSettingsRequest{DelegationsEnabled: f.Delegations}
	if f.RPCBind == "host" {
		on := true
		req.PortmapperRegisterWithSystem, req.UDPEnabled = &on, &on
	}
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
// answers: an authenticated call answers, and shows NFS and SMB each served
// on its port when the fixture asks for it, and no adapter it doesn't ask for.
// With Kerberos, {server}, a name then, must lead to SMB's port. With an S3
// profile's bucket b, the server's own check of its block store must find it
// healthy too.
func (s *Server) check(c *apiclient.Client, b *bucket) error {
	adapters, err := c.ListAdapters()
	if err != nil {
		return fmt.Errorf("an authenticated call: %w", err)
	}
	ports := map[string]int{"nfs": s.nfsPort, "smb": s.smbPort}
	served := map[string]bool{}
	for _, a := range adapters {
		switch {
		case ports[a.Type] > 0:
			served[a.Type] = a.Running && a.Port == ports[a.Type]
		case a.Running:
			return fmt.Errorf("the %s adapter runs, which the fixture doesn't ask for", a.Type)
		}
	}
	for _, a := range []string{"nfs", "smb"} {
		if ports[a] > 0 && !served[a] {
			return fmt.Errorf("%s isn't served on port %d", strings.ToUpper(a), ports[a])
		}
	}
	if s.kdc != nil {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(s.kdc.host, strconv.Itoa(s.smbPort)), 5*time.Second)
		if err != nil {
			return fmt.Errorf("{server}, the name %s, doesn't lead to SMB's port: %w", s.kdc.host, err)
		}
		_ = conn.Close()
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

// registered waits until the host's rpcbind lists the server's lock manager
// and MOUNT on its NFS port, which rpcbind: host has the server register.
//
// decision: the host has one NLM and one MOUNT registration, and a server
// takes them over from another that registered them as it does, so two
// fixtures with rpcbind: host at once would take each other's. dt runs such
// cells one at a time, as those whose fixtures set the same port.
func (s *Server) registered(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, clientWaits[0])
	defer cancel()
	for {
		table, err := exec.CommandContext(ctx, "rpcinfo", "-p").Output()
		missing, nlm := unregistered(string(table), s.nfsPort)
		if err == nil && len(missing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("the host's rpcbind: rpcinfo -p: %w", err)
			}
			err = fmt.Errorf("the host's rpcbind doesn't list the server's %s on port %d", strings.Join(missing, " and "), s.nfsPort)
			if nlm != "" {
				err = fmt.Errorf("%w: it gives NLM to port %s, held by a lock manager the server can't take it from, such as the host's own lockd, which runs while the host serves NFS or has an NFSv3 mount that locks", err, nlm)
			}
			return err
		case <-time.After(clientWaits[1]):
		}
	}
}

// unregistered names what of NLM 4 and MOUNT 3 over TCP on port an rpcinfo
// -p table doesn't list, and the port it gives NLM 4 to instead, if any.
func unregistered(table string, port int) (missing []string, nlm string) {
	want := map[string]string{"100021 4": "lock manager", "100005 3": "MOUNT"}
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) < 4 || f[2] != "tcp":
		case f[3] == strconv.Itoa(port):
			delete(want, f[0]+" "+f[1])
		case f[0]+" "+f[1] == "100021 4":
			nlm = f[3]
		}
	}
	return slices.Sorted(maps.Values(want)), nlm
}
