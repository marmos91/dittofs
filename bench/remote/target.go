package remote

import (
	"context"
	"fmt"
)

// Stack-output keys the bench Pulumi stack exports. serverIP is the public
// flexible IP (SSH). serverPrivateIP is the private-network address the bench
// serves on; privateNetworkID is the VPC private network the server attaches
// to (used to derive/confirm the private IP). See bench/infra/bench.go.
const (
	OutputServerIP        = "serverIP"
	OutputServerPrivateIP = "serverPrivateIP"
	OutputPrivateNetID    = "privateNetworkID"
)

// ResolveTarget builds a Target from a Pulumi stack's outputs. The public IP
// (SSH) comes from serverIP; the bench-mount address comes from
// serverPrivateIP. privateOverride, when non-empty, wins over the stack's
// serverPrivateIP — useful when the private IP is assigned by DHCP and not yet
// surfaced as a stack output.
//
// requirePrivate enforces that a private address was resolved before any mount
// runs (failure mode B5: the bench must not mount over the public IP).
func ResolveTarget(ctx context.Context, reader StackReader, stack, user, privateOverride string, requirePrivate bool) (Target, error) {
	if reader == nil {
		return Target{}, fmt.Errorf("ResolveTarget: stack reader is nil")
	}
	outs, err := reader.Outputs(ctx, stack)
	if err != nil {
		return Target{}, fmt.Errorf("read stack %q outputs: %w", stack, err)
	}

	public := outs[OutputServerIP]
	if public == "" {
		return Target{}, fmt.Errorf("stack %q has no %q output (is the bench server provisioned?)", stack, OutputServerIP)
	}

	private := privateOverride
	if private == "" {
		private = outs[OutputServerPrivateIP]
	}
	// privateNetworkID being present but no private IP is a clear misconfig: the
	// NIC is attached but the address wasn't surfaced.
	if private == "" && outs[OutputPrivateNetID] != "" && requirePrivate {
		return Target{}, fmt.Errorf("stack %q attaches a private network (%s=%s) but exports no %q; "+
			"pass --private-ip with the server's private-network address",
			stack, OutputPrivateNetID, outs[OutputPrivateNetID], OutputServerPrivateIP)
	}

	t := Target{PublicIP: public, PrivateIP: private, User: user}
	if err := t.Validate(requirePrivate); err != nil {
		return Target{}, err
	}
	return t, nil
}
