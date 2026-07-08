package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Provisioning shells the `scw` CLI from Go, reusing the validated
// parity-scw.sh robustness (IP-poll, transient-aware terminate) but parsing
// JSON with encoding/json instead of python3. One disposable VM per run; its
// handle is persisted to .bench-vm.json so run/teardown reattach.

const vmStateFile = ".bench-vm.json"

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

// VM is the persisted handle to a provisioned bench VM.
type VM struct {
	ServerID string `json:"server_id"`
	IP       string `json:"ip"`
	Zone     string `json:"zone"`
}

func saveVM(vm VM) error {
	data, err := json.MarshalIndent(vm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(vmStateFile, data, 0o644)
}

func loadVM() (VM, error) {
	data, err := os.ReadFile(vmStateFile)
	if err != nil {
		return VM{}, fmt.Errorf("no bench VM (run `dfsbench setup` first): %w", err)
	}
	var vm VM
	return vm, json.Unmarshal(data, &vm)
}

func clearVMState() error {
	err := os.Remove(vmStateFile)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// scwJSON runs a scw command with `-o json` and decodes stdout into v.
func scwJSON(ctx context.Context, v any, args ...string) error {
	out, err := exec.CommandContext(ctx, "scw", append(args, "-o", "json")...).Output()
	if err != nil {
		return fmt.Errorf("scw %v: %w", args, err)
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
	vm := VM{ServerID: created.ID, Zone: spec.Zone}
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

// terminateVM deletes the server, its IP, and its block volume, retrying to ride
// out transient states (30 × 4s).
func terminateVM(ctx context.Context, vm VM) error {
	var last error
	for i := 0; i < 30; i++ {
		if err := exec.CommandContext(ctx, "scw", "instance", "server", "terminate",
			vm.ServerID, "zone="+vm.Zone, "with-ip=true", "with-block=true").Run(); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(4 * time.Second)
	}
	return fmt.Errorf("terminate %s failed after retries: %w", vm.ServerID, last)
}
