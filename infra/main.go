// Package main implements the Pulumi program for DittoFS infrastructure
// (benchmarks and edge tests).
//
// Two stacks are used:
//
//   - "base": Creates VPC, persistent client VM, and S3 bucket.
//     Run once before any test session.
//
//   - "bench": Creates an ephemeral server VM, provisions it with the selected
//     system's install script, and exports connection details.
//     Created per test, destroyed after for clean isolation.
//
// # Authentication
//
// Scaleway credentials are read from environment variables (never committed):
//
//	SCW_ACCESS_KEY, SCW_SECRET_KEY, SCW_DEFAULT_PROJECT_ID
//
// S3 credentials for backends that need Object Storage are set via:
//
//	pulumi config set --secret s3AccessKey <key>
//	pulumi config set --secret s3SecretKey <key>
package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		switch ctx.Stack() {
		case "bench":
			return deployBench(ctx)
		default:
			return deployBase(ctx)
		}
	})
}
