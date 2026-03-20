package main

import (
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
	scaleway "github.com/pulumiverse/pulumi-scaleway/sdk/go/scaleway"
)

// deployBench provisions an ephemeral server VM with a 150 GB block storage
// volume and exports its install script and connection details.
func deployBench(ctx *pulumi.Context) error {
	stackCfg := config.New(ctx, "dittofs-bench")
	cfg := loadConfig(stackCfg)

	// Which competitor to install (e.g., "kernel-nfs", "dittofs-badger-s3").
	systemName := stackCfg.Require("system")
	system := FindSystem(systemName)
	if system == nil {
		return fmt.Errorf("unknown system %q — see systems.go for available options", systemName)
	}

	// Private network ID from the base stack.
	privateNetworkID := stackCfg.Require("privateNetworkID")

	// 150 GB block storage volume for benchmark data (/data, /export).
	dataVolume, err := scaleway.NewBlockVolume(ctx, "data-volume", &scaleway.BlockVolumeArgs{
		SizeInGb: pulumi.Int(150),
		Iops:     pulumi.Int(5000),
		Tags:     pulumi.StringArray{pulumi.String("dittofs-bench"), pulumi.String(systemName)},
	})
	if err != nil {
		return err
	}

	// Flexible IP for server VM — SSH access.
	serverIP, err := scaleway.NewInstanceIp(ctx, "server-ip", &scaleway.InstanceIpArgs{
		Tags: pulumi.StringArray{pulumi.String("dittofs-bench"), pulumi.String(systemName)},
	})
	if err != nil {
		return err
	}

	// Ephemeral server VM from standard image with block volume attached.
	server, err := scaleway.NewInstanceServer(ctx, "bench-server", &scaleway.InstanceServerArgs{
		Name:  pulumi.Sprintf("bench-%s", systemName),
		Type:  pulumi.String(cfg.VMType),
		Image: pulumi.String(cfg.Image),
		IpId:  serverIP.ID(),
		Tags:  pulumi.StringArray{pulumi.String("dittofs-bench"), pulumi.String(systemName)},
		AdditionalVolumeIds: pulumi.StringArray{
			dataVolume.ID(),
		},
	})
	if err != nil {
		return err
	}

	// Attach to the private network.
	_, err = scaleway.NewInstancePrivateNic(ctx, "server-pn", &scaleway.InstancePrivateNicArgs{
		ServerId:         server.ID(),
		PrivateNetworkId: pulumi.String(privateNetworkID),
	})
	if err != nil {
		return err
	}

	// Exports for the orchestrator script.
	ctx.Export("serverIP", serverIP.Address)
	ctx.Export("system", pulumi.String(systemName))
	ctx.Export("protocol", pulumi.String(system.Protocol))
	ctx.Export("port", pulumi.Int(system.Port))
	ctx.Export("mountOpts", pulumi.String(system.MountOpts))
	ctx.Export("installScript", pulumi.String(system.InstallScript))

	return nil
}
