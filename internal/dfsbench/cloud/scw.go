package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// scwProvider provisions bench VMs on Scaleway by shelling the `scw` CLI,
// parsing JSON with encoding/json (not python3). It implements Provider.
type scwProvider struct{}

func (scwProvider) Provision(ctx context.Context) (VM, error) {
	return provisionVM(ctx, defaultVMSpec())
}

func (scwProvider) Terminate(ctx context.Context, vm VM) error { return terminateVM(ctx, vm) }

// vmSpec is the create-time shape; every field has an env override matching the
// old script's knobs.
type vmSpec struct {
	Zone, Type, Image, RootVol, Name string
}

func defaultVMSpec() vmSpec {
	env := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	return vmSpec{
		Zone:    env("SCW_ZONE", "fr-par-1"),
		Type:    env("SCW_INSTANCE_TYPE", "POP2-8C-32G"), // the #1432 baseline shape
		Image:   env("SCW_IMAGE", "ubuntu_noble"),
		RootVol: env("SCW_ROOT_VOLUME", "sbs:100GB:5000"), // ample scratch
		Name:    env("SCW_NAME", ""),                      // timestamped if empty
	}
}

// scwJSON runs a scw command with `-o json` and decodes stdout into v. It
// captures stderr into the error so auth/quota/arg failures stay diagnosable.
func scwJSON(ctx context.Context, v any, args ...string) error {
	cmd := exec.CommandContext(ctx, "scw", append(args, "-o", "json")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("scw %v: %w: %s", args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return json.Unmarshal(out, v)
}

// provisionVM creates a server and polls until it has a public IP (30 × 4s).
func provisionVM(ctx context.Context, spec vmSpec) (VM, error) {
	if spec.Name == "" {
		spec.Name = "dfsbench-" + time.Now().Format("20060102-150405")
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := scwJSON(ctx, &created, "instance", "server", "create",
		"type="+spec.Type, "zone="+spec.Zone, "image="+spec.Image,
		"name="+spec.Name, "ip=new", "root-volume="+spec.RootVol); err != nil {
		return VM{}, fmt.Errorf("scw create: %w", err)
	}
	vm := VM{Provider: "scw", ServerID: created.ID, Zone: spec.Zone}
	for i := 0; i < 30; i++ {
		if ip, err := serverIP(ctx, vm); err == nil && ip != "" {
			vm.IP = ip
			return vm, nil
		}
		time.Sleep(4 * time.Second)
	}
	return vm, fmt.Errorf("server %s: timed out waiting for public IP", vm.ServerID)
}

// serverIP reads the current public IP, tolerating both the single public_ip
// and the newer public_ips[] shapes the old script handled.
func serverIP(ctx context.Context, vm VM) (string, error) {
	var d struct {
		PublicIP struct {
			Address string `json:"address"`
		} `json:"public_ip"`
		PublicIPs []struct {
			Address string `json:"address"`
		} `json:"public_ips"`
	}
	if err := scwJSON(ctx, &d, "instance", "server", "get", vm.ServerID, "zone="+vm.Zone); err != nil {
		return "", err
	}
	if d.PublicIP.Address != "" {
		return d.PublicIP.Address, nil
	}
	if len(d.PublicIPs) > 0 {
		return d.PublicIPs[0].Address, nil
	}
	return "", nil
}

// terminateVM deletes the server, its IP, and its root block volume, then the
// separate bench-data volume (if any), retrying to ride out transient states.
func terminateVM(ctx context.Context, vm VM) error {
	var last error
	terminated := false
	for i := 0; i < 30; i++ {
		if err := exec.CommandContext(ctx, "scw", "instance", "server", "terminate",
			vm.ServerID, "zone="+vm.Zone, "with-ip=true", "with-block=true").Run(); err == nil {
			terminated = true
			break
		} else {
			last = err
		}
		time.Sleep(4 * time.Second)
	}
	// Always attempt the data-volume delete, even if terminate kept erroring: the
	// server may already be gone (a transient CLI error), and a leaked data volume
	// bills indefinitely. deleteDataVolume tolerates a not-found volume.
	var delErr error
	if vm.VolumeID != "" {
		delErr = deleteDataVolume(ctx, vm)
	}
	if !terminated {
		return fmt.Errorf("terminate %s failed after retries: %w", vm.ServerID, last)
	}
	return delErr
}

// createDataVolume creates an SBS block volume of gb GB in the VM's zone and
// returns its ID (same 5000-IOPS class as the default root volume).
func createDataVolume(ctx context.Context, vm VM, gb int) (string, error) {
	if gb <= 0 {
		return "", fmt.Errorf("data volume size must be > 0 GB, got %d", gb)
	}
	var v struct {
		ID string `json:"id"`
	}
	name := "dfsbench-data-" + time.Now().Format("20060102-150405")
	// scw's block API requires the size with an explicit G/GB unit — a bare
	// byte count is rejected ("size must be defined using the G or GB unit").
	if err := scwJSON(ctx, &v, "block", "volume", "create",
		"name="+name, "perf-iops=5000",
		fmt.Sprintf("from-empty.size=%dGB", gb),
		"zone="+vm.Zone); err != nil {
		return "", fmt.Errorf("scw block volume create: %w", err)
	}
	if v.ID == "" {
		return "", fmt.Errorf("scw block volume create returned no id")
	}
	return v.ID, nil
}

// attachDataVolume attaches vol to the server, retrying while the freshly-created
// volume settles into an attachable state (30 × 4s).
func attachDataVolume(ctx context.Context, vm VM, vol string) error {
	var last error
	for i := 0; i < 30; i++ {
		cmd := exec.CommandContext(ctx, "scw", "instance", "server", "attach-volume",
			"server-id="+vm.ServerID, "volume-id="+vol, "volume-type=sbs_volume", "zone="+vm.Zone)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		time.Sleep(4 * time.Second)
	}
	return fmt.Errorf("attach volume %s to %s failed after retries: %w", vol, vm.ServerID, last)
}

// deleteDataVolume deletes the bench-data volume once the server is gone (a
// volume can't be deleted while in_use). Tolerates a not-found — with-block on
// terminate may already have removed it, which is the goal either way.
func deleteDataVolume(ctx context.Context, vm VM) error {
	var last error
	for i := 0; i < 30; i++ {
		// CombinedOutput so a "not found" printed to stdout (not just stderr) is
		// still recognised as an already-deleted volume rather than a hard error.
		out, err := exec.CommandContext(ctx, "scw", "block", "volume", "delete", vm.VolumeID, "zone="+vm.Zone).CombinedOutput()
		if err == nil {
			return nil
		}
		low := bytes.ToLower(out)
		if bytes.Contains(low, []byte("not found")) || bytes.Contains(low, []byte("does not exist")) ||
			bytes.Contains(low, []byte("cannot find resource")) {
			return nil
		}
		last = fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		time.Sleep(4 * time.Second)
	}
	return fmt.Errorf("delete data volume %s failed after retries: %w", vm.VolumeID, last)
}
