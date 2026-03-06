package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumiverse/pulumi-scaleway/sdk/go/scaleway/network"
)

// networkResources holds the VPC resources shared by both stacks.
type networkResources struct {
	VPC        *network.Vpc
	PrivateNet *network.PrivateNetwork
}

// createNetwork provisions the VPC and private network.
func createNetwork(ctx *pulumi.Context, _ infraConfig) (*networkResources, error) {
	vpc, err := network.NewVpc(ctx, "bench-vpc", &network.VpcArgs{
		Name: pulumi.String("dittofs-bench"),
		Tags: pulumi.StringArray{pulumi.String("dittofs-bench")},
	})
	if err != nil {
		return nil, err
	}

	pn, err := network.NewPrivateNetwork(ctx, "bench-pn", &network.PrivateNetworkArgs{
		Name:  pulumi.String("dittofs-bench-pn"),
		VpcId: vpc.ID(),
		Tags:  pulumi.StringArray{pulumi.String("dittofs-bench")},
	})
	if err != nil {
		return nil, err
	}

	return &networkResources{VPC: vpc, PrivateNet: pn}, nil
}
