package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// A Provider provisions and tears down one disposable bench VM on some cloud.
// SCW is the only implementation today; add another by implementing this
// interface and wiring it into newProvider. The rest of the remote runner (ssh,
// push, detached driver, poll, pull) is provider-agnostic and works off VM.
type Provider interface {
	// Provision creates a VM and returns its handle once it has a public IP.
	Provision(ctx context.Context) (VM, error)
	// Terminate deletes the VM and its attached resources.
	Terminate(ctx context.Context, vm VM) error
}

// newProvider returns the provider selected by name. Empty defaults to scw.
func newProvider(name string) (Provider, error) {
	switch name {
	case "", "scw":
		return scwProvider{}, nil
	default:
		return nil, fmt.Errorf("unknown --provider %q (supported: scw)", name)
	}
}

const vmStateFile = ".bench-vm.json"

// VM is the persisted handle to a provisioned bench VM. Provider records which
// cloud created it so teardown routes to the right implementation.
type VM struct {
	Provider string `json:"provider"`
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

// LoadVM reads the persisted bench-VM handle written by setup.
func LoadVM() (VM, error) {
	data, err := os.ReadFile(vmStateFile)
	if err != nil {
		return VM{}, fmt.Errorf("no bench VM (run `dfsbench setup` first): %w", err)
	}
	var vm VM
	if err := json.Unmarshal(data, &vm); err != nil {
		return VM{}, err
	}
	return vm, nil
}

func clearVMState() error {
	err := os.Remove(vmStateFile)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
