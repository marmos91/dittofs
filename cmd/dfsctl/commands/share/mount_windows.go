//go:build windows

package share

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

const defaultNFSPortStandard = 2049

func getDefaultModeForPlatform() (string, string) {
	return "", "File permissions (not applicable on Windows)"
}

func validatePlatform() error {
	return nil
}

func validateMountPoint(mountPoint string) error {
	// On Windows, SMB mount points are drive letters (e.g., Z:).
	// NFS mount can use drive letters or directory paths.
	if len(mountPoint) == 2 && mountPoint[1] == ':' {
		letter := mountPoint[0]
		if (letter >= 'A' && letter <= 'Z') || (letter >= 'a' && letter <= 'z') {
			return nil
		}
		return fmt.Errorf("invalid drive letter: %s\nHint: Use a drive letter like Z: or a directory path", mountPoint)
	}

	info, err := os.Stat(mountPoint)
	if os.IsNotExist(err) {
		return fmt.Errorf("mount point does not exist: %s\nHint: Use a drive letter (e.g., Z:) or create the directory first", mountPoint)
	}
	if err != nil {
		return fmt.Errorf("failed to access mount point: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mount point is not a directory: %s\nHint: Specify a directory path or drive letter as the mount point", mountPoint)
	}

	return nil
}

func checkMountPrivileges(mountPoint, protocol, sharePath string) error {
	return nil
}

func mountNFS(sharePath, mountPoint string, adapters []apiclient.Adapter) error {
	port := getAdapterPort(adapters, "nfs", defaultNFSPort)

	// The Windows NFS mount command does NOT support specifying a custom port via -o options.
	// Supported -o options are: rsize, wsize, timeout, retry, mtype, lang, fileaccess, anon,
	// nolock, casesensitive, sec. There is no port option.
	// See: https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/mount
	//
	// If the NFS adapter is on a non-standard port, the user must either:
	//   1. Configure the NFS adapter to use the standard port (2049)
	//   2. Set up port forwarding from 2049 to the actual port
	if port != defaultNFSPortStandard {
		return fmt.Errorf(
			"the Windows NFS client does not support mounting from a custom port (server is using port %d)\n\n"+
				"The Windows 'mount' command only connects to the standard NFS port (2049).\n"+
				"To resolve this, either:\n\n"+
				"  1. Configure the NFS adapter to use port 2049 (requires Administrator on the server):\n"+
				"     dfsctl adapter edit nfs --config '{\"port\": 2049}'\n\n"+
				"  2. Set up port forwarding from 2049 to %d (requires Administrator):\n"+
				"     netsh interface portproxy add v4tov4 listenport=2049 listenaddress=127.0.0.1 connectport=%d connectaddress=127.0.0.1\n\n"+
				"     Then retry the mount command.",
			port, port, port,
		)
	}

	fmt.Println(`Note: NFS mounting on Windows requires the 'Client for NFS' feature to be installed.

To enable it:
  1. Open 'Turn Windows features on or off' (Win+R > optionalfeatures.exe)
  2. Expand 'Services for NFS' and check 'Client for NFS'
  3. Click OK and restart your computer if prompted

Alternatively, install via PowerShell (run as Administrator):
  Enable-WindowsOptionalFeature -FeatureName ServicesForNFS-ClientOnly -Online`)

	source := fmt.Sprintf("\\\\localhost%s", strings.ReplaceAll(sharePath, "/", "\\"))
	cmd := exec.Command("mount",
		"-o", "mtype=hard,nolock",
		source,
		mountPoint,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return formatMountError(err, string(output), "NFS", port)
	}

	fmt.Printf("Mounted %s at %s (NFS)\n", sharePath, mountPoint)
	return nil
}

func mountSMB(sharePath, mountPoint string, adapters []apiclient.Adapter) error {
	port := getAdapterPort(adapters, "smb", defaultSMBPort)

	username, err := resolveSMBUsername()
	if err != nil {
		return err
	}

	password, err := resolveSMBPassword(username)
	if err != nil {
		return err
	}

	shareName := strings.TrimPrefix(sharePath, "/")
	uncPath := fmt.Sprintf("\\\\localhost\\%s", shareName)

	args := []string{"use", mountPoint, uncPath, fmt.Sprintf("/user:%s", username), password}

	// The standard 'net use' command does not support custom SMB ports.
	// Windows Insider builds (25992+) added /TCPPORT: support to net use.
	if port != 445 {
		fmt.Printf("Note: Server is using non-standard SMB port %d.\n", port)
		fmt.Printf("Attempting 'net use' with /TCPPORT:%d (requires Windows 11 Insider Build 25992+ or Windows Server 2025+).\n\n", port)
		args = append(args, fmt.Sprintf("/TCPPORT:%d", port))
	}

	cmd := exec.Command("net", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if port != 445 {
			return fmt.Errorf(
				"mount failed: %w\nOutput: %s\n\n"+
					"The /TCPPORT flag requires Windows 11 Insider Build 25992+ or Windows Server 2025+.\n"+
					"On older Windows versions, you can either:\n\n"+
					"  1. Configure the SMB adapter to use port 445 (requires Administrator on the server):\n"+
					"     dfsctl adapter edit smb --config '{\"port\": 445}'\n\n"+
					"  2. Set up port forwarding from 445 to %d (requires Administrator):\n"+
					"     netsh interface portproxy add v4tov4 listenport=445 listenaddress=127.0.0.1 connectport=%d connectaddress=127.0.0.1\n"+
					"     Then retry: dfsctl share mount --protocol smb %s %s",
				err, output, port, port, sharePath, mountPoint,
			)
		}
		return formatMountError(err, string(output), "SMB", port)
	}

	fmt.Printf("Mounted %s at %s (SMB)\n", sharePath, mountPoint)
	return nil
}
