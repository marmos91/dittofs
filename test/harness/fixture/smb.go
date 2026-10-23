package fixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// smbDialect is the dialect an SMB mount must report: SMB 3.1.1, the newest,
// which mount.cifs is asked for, and mount_smbfs and New-SmbMapping
// negotiate.
const smbDialect = "3.1.1"

// goos is the system dt runs on, whose own SMB client mounts the share; the
// tests replace it.
var goos = runtime.GOOS

// mountSMB mounts the first share from server over SMB as {user}, with the
// machine's own client, on the server's SMB port, and checks that the client
// reports it mounted at smbDialect: with mount.cifs or mount_smbfs at a new
// directory, {mount}, or with New-SmbMapping on a free drive, which is then
// {mount}. The server's own port is what lets New-SmbMapping's client reach
// it where the system's own SMB server holds 445.
func (s *Server) mountSMB(ctx context.Context, server, password string) error {
	var dialect string
	var err error
	switch goos {
	case "darwin":
		dialect, err = s.mountSMBFS(ctx, server, password)
	case "windows":
		dialect, err = s.mapDrive(ctx, server, password)
	default:
		dialect, err = s.mountCIFS(ctx, server, password)
	}
	if err != nil {
		return err
	}
	if dialect != smbDialect {
		return fmt.Errorf("%s is mounted at SMB %s, not %s", s.mount, dialect, smbDialect)
	}
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: SMB %s mounted at %s, as %s\n", dialect, s.mount, User)
	return nil
}

// mountCIFS mounts the share with the kernel's client, as root, asked for
// smbDialect, and returns the dialect the kernel reports. The password is in
// a credentials file of the server's data, which only the user dt runs as
// and root can read, so it never shows on mount's command line.
func (s *Server) mountCIFS(ctx context.Context, server, password string) (string, error) {
	creds := filepath.Join(s.data, "smb-credentials")
	if err := os.WriteFile(creds, []byte("username="+User+"\npassword="+password+"\n"), 0o600); err != nil {
		return "", err
	}
	opts, err := s.mountAt(ctx, "cifs", fmt.Sprintf("vers=%s,port=%d,credentials=%s", smbDialect, s.smbPort, creds), "//"+server+s.shares[0].Name)
	return versOf(opts), err
}

// mountSMBFS mounts the share with mount_smbfs as the user dt runs as, whose
// mount it is then, as the suite's commands run as that user, and returns the
// dialect smbutil reports for it. nobrowse keeps the share off a developer's
// desktop.
//
// decision: mount_smbfs takes a password only in the share's URL or at a
// terminal's prompt, so the password is on its command line while it runs.
// It is the fixture's own, made for this server alone, and stays out of dt's
// lines.
func (s *Server) mountSMBFS(ctx context.Context, server, password string) (string, error) {
	dir, err := mountDir()
	if err != nil {
		return "", err
	}
	s.mount = dir
	url := fmt.Sprintf("//%s:%s@%s:%d%s", User, password, server, s.smbPort, s.shares[0].Name)
	argv := []string{"mount_smbfs", "-N", "-o", "nobrowse", url, dir}
	hide := strings.NewReplacer(password, "{password}").Replace
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: $ %s\n", hide(strings.Join(argv, " ")))
	if err := runFor(ctx, 0, argv); err != nil {
		return "", fmt.Errorf("mounting %s at %s: %s", hide(url), dir, hide(err.Error()))
	}
	out, err := exec.CommandContext(ctx, "smbutil", "statshares", "-m", dir, "-f", "json").Output()
	if err != nil {
		return "", fmt.Errorf("smbutil statshares -m %s: %w", dir, err)
	}
	return statsharesDialect(out)
}

// statsharesDialect is the dialect smbutil statshares's JSON gives the one
// share it lists, without its SMB_ prefix.
func statsharesDialect(out []byte) (string, error) {
	var shares []struct {
		Version string `json:"SMB_VERSION"`
	}
	if err := json.Unmarshal(out, &shares); err != nil {
		return "", fmt.Errorf("smbutil statshares: %w: %s", err, out)
	}
	if len(shares) != 1 {
		return "", fmt.Errorf("smbutil statshares lists %d shares mounted there, not 1", len(shares))
	}
	return strings.TrimPrefix(shares[0].Version, "SMB_"), nil
}

// mapScript maps the share on the last free drive letter, from Z: down, with
// New-SmbMapping, on the server's SMB port, and prints the drive and the
// dialect its connection negotiated. Its values come from the environment, so
// the password never shows on a command line.
const mapScript = `$ErrorActionPreference = 'Stop'; ` +
	`$drive = @(68..90 | ForEach-Object { [string][char]$_ + ':' } | Where-Object { -not (Test-Path ($_ + '\')) })[-1]; ` +
	`New-SmbMapping -LocalPath $drive -RemotePath $env:DT_SMB_PATH -TcpPort $env:DT_SMB_PORT -UserName $env:DT_SMB_USER -Password $env:DT_SMB_PASSWORD | Out-Null; ` +
	`$drive + ' ' + (Get-SmbConnection -ServerName $env:DT_SMB_SERVER | Where-Object ShareName -EQ $env:DT_SMB_SHARE).Dialect`

// mapDrive maps the share on a free drive, {mount}, as the user dt runs as,
// and returns the dialect the system reports for the connection.
func (s *Server) mapDrive(ctx context.Context, server, password string) (string, error) {
	share := strings.TrimPrefix(s.shares[0].Name, "/")
	path := `\\` + server + `\` + share
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: New-SmbMapping -RemotePath %s -TcpPort %d -UserName %s\n", path, s.smbPort, User)
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", mapScript)
	cmd.Env = append(os.Environ(), "DT_SMB_PATH="+path, fmt.Sprintf("DT_SMB_PORT=%d", s.smbPort),
		"DT_SMB_USER="+User, "DT_SMB_PASSWORD="+password, "DT_SMB_SERVER="+server, "DT_SMB_SHARE="+share)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mapping %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) > 0 {
		s.mount = f[0] // mapped already: Stop removes it even when the dialect is missing
	}
	if len(f) < 2 {
		return "", fmt.Errorf("mapping %s printed no drive and dialect: %s", path, out)
	}
	return f[1], nil
}

// unmountSMB unmounts a share mount_smbfs mounted and removes its directory,
// or removes a drive New-SmbMapping mapped, never waiting long, as unmount
// does. A share that stays mounted keeps its directory, and dt says so, as it
// does for a drive that stays mapped.
func (s *Server) unmountSMB() {
	ctx := context.Background()
	if goos == "windows" {
		if err := runFor(ctx, unmountWaits[0], []string{"net", "use", s.mount, "/delete", "/y"}); err != nil {
			_, _ = fmt.Fprintf(s.out, "[dt] fixture: the share stays mapped at %s (%v); remove it with: net use %s /delete /y\n", s.mount, err, s.mount)
		}
		return
	}
	// decision: a umount that fails is forced, as unmount forces it, since
	// the server stops next. One that fails because nothing is mounted, as when a
	// workload's own mount of the share failed, leaves the directory to
	// remove.
	if runFor(ctx, unmountWaits[0], []string{"umount", s.mount}) != nil {
		_ = runFor(ctx, unmountWaits[1], []string{"umount", "-f", s.mount})
	}
	if err := os.Remove(s.mount); err != nil && !errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintf(s.out, "[dt] fixture: the share stays mounted at %s; unmount it with: umount -f %s\n", s.mount, s.mount)
	}
}
