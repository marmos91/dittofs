package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
	scaleway "github.com/pulumiverse/pulumi-scaleway/sdk/go/scaleway"
)

// deployBase provisions the one-time base infrastructure:
//  1. VPC + private network
//  2. Persistent client VM with public IP (stays alive across all competitor tests)
func deployBase(ctx *pulumi.Context) error {
	cfg := loadConfig(config.New(ctx, "dittofs-bench"))

	net, err := createNetwork(ctx)
	if err != nil {
		return err
	}

	// Flexible IP for client VM — SSH access.
	clientIP, err := scaleway.NewInstanceIp(ctx, "client-ip", &scaleway.InstanceIpArgs{
		Tags: pulumi.StringArray{pulumi.String("dittofs-bench"), pulumi.String("client")},
	})
	if err != nil {
		return err
	}

	// Client VM — persistent, used by all benchmark runs.
	clientVM, err := scaleway.NewInstanceServer(ctx, "bench-client", &scaleway.InstanceServerArgs{
		Name:  pulumi.String("dittofs-bench-client"),
		Type:  pulumi.String(cfg.VMType),
		Image: pulumi.String(cfg.Image),
		IpId:  clientIP.ID(),
		Tags:  pulumi.StringArray{pulumi.String("dittofs-bench"), pulumi.String("client")},
	})
	if err != nil {
		return err
	}

	// Attach client to private network.
	_, err = scaleway.NewInstancePrivateNic(ctx, "client-pn", &scaleway.InstancePrivateNicArgs{
		ServerId:         clientVM.ID(),
		PrivateNetworkId: net.PrivateNet.ID(),
	})
	if err != nil {
		return err
	}

	// S3 bucket for S3-backed benchmarks.
	bucket, err := scaleway.NewObjectBucket(ctx, "bench-bucket", &scaleway.ObjectBucketArgs{
		Name:   pulumi.String("dittofs-bench"),
		Region: pulumi.String("fr-par"),
		Tags: pulumi.StringMap{
			"project": pulumi.String("dittofs-bench"),
		},
	})
	if err != nil {
		return err
	}

	// Exports for the bench stack and orchestrator.
	ctx.Export("clientIP", clientIP.Address)
	ctx.Export("privateNetworkID", net.PrivateNet.ID())
	ctx.Export("s3Bucket", bucket.Name)
	ctx.Export("s3Region", pulumi.String("fr-par"))
	ctx.Export("s3Endpoint", pulumi.String("s3.fr-par.scw.cloud"))

	return nil
}
