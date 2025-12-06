package importer

import (
	"fmt"
)

const (
	PluginName = "localnet_algod"
)

// Config specific to the localnet importer
type Config struct {
	// LeadNodeURL is the URL of the lead algod node to wait for round availability
	LeadNodeURL string `yaml:"lead-node-url"`
	// FollowerNodeURL is the follower Algod network address (must be http or https URL)
	FollowerNodeURL string `yaml:"follower-node-url"`
	// Token is the default API token used for both nodes if specific tokens are not provided
	Token string `yaml:"token"`
	// FollowerNodeToken is the API token for the follower node (defaults to Token if not specified)
	FollowerNodeToken string `yaml:"follower-node-token"`
	// LeadNodeToken is the API token for the lead node (defaults to Token if not specified)
	LeadNodeToken string `yaml:"lead-node-token"`
}

// validateConfig validates required configuration fields
func (c *Config) validateRequired() error {
	if c.LeadNodeURL == "" {
		return fmt.Errorf("lead-node-url is required")
	}
	if c.FollowerNodeURL == "" {
		return fmt.Errorf("follower-node-url is required")
	}
	return nil
}
