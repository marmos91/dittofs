package main

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"

// infraConfig holds all configurable parameters for the infrastructure.
type infraConfig struct {
	Zone     string
	VMType   string
	Image    string
	SSHKeyID string
}

// loadConfig reads Pulumi stack configuration and applies defaults for unset fields.
func loadConfig(cfg *config.Config) infraConfig {
	return infraConfig{
		Zone:     getOrDefault(cfg, "zone", "fr-par-1"),
		VMType:   getOrDefault(cfg, "vmType", "PLAY2-MICRO"),
		Image:    getOrDefault(cfg, "image", "ubuntu_noble"),
		SSHKeyID: cfg.Get("sshKeyId"),
	}
}

// getOrDefault returns the config value for key, or fallback if unset.
func getOrDefault(cfg *config.Config, key, fallback string) string {
	if v := cfg.Get(key); v != "" {
		return v
	}
	return fallback
}
