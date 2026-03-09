package main

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"

// infraConfig holds all configurable parameters for the benchmark infrastructure.
type infraConfig struct {
	Zone     string
	VMType   string
	Image    string
	SSHKeyID string
}

// loadConfig reads Pulumi stack configuration.
func loadConfig(ctx *config.Config) infraConfig {
	return infraConfig{
		Zone:     ctx.Get("zone"),
		VMType:   ctx.Get("vmType"),
		Image:    ctx.Get("image"),
		SSHKeyID: ctx.Get("sshKeyId"),
	}
}

// withDefaults fills in default values for any unset config fields.
func (c infraConfig) withDefaults() infraConfig {
	if c.Zone == "" {
		c.Zone = "fr-par-1"
	}
	if c.VMType == "" {
		c.VMType = "PLAY2-MICRO"
	}
	if c.Image == "" {
		c.Image = "ubuntu_noble"
	}
	return c
}
