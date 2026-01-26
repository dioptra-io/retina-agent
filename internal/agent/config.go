// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Package agent provides configuration for the retina network measurement agent.
package agent

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Config holds agent configuration.
type Config struct {
	// AgentID is the unique identifier for this agent instance.
	// Used in logging and for the orchestrator to track agents.
	AgentID string

	// OrchestratorAddr is the TCP address of the orchestrator (host:port).
	OrchestratorAddr string

	// ProberType specifies which prober implementation to use.
	// Valid values: "caracal", "mock"
	ProberType string

	// ProberPath is the filesystem path to the prober executable.
	// If empty, searches PATH. Only required for "caracal" prober type.
	ProberPath string

	// ProberArgs contains additional command-line arguments to pass to the prober.
	// Optional. Only applies to caracal prober type.
	// Example: []string{"--n-packets", "3", "--interface", "eth0"}
	ProberArgs []string

	// DirectivesBufferSize is the channel buffer for incoming directives.
	// Larger buffers provide more tolerance for processing delays.
	DirectivesBufferSize int

	// FIEsBufferSize is the channel buffer for outgoing FIEs.
	// Should match expected probe completion rate.
	FIEsBufferSize int

	// ReadDeadline is the timeout for receiving messages from orchestrator.
	// Should be longer than expected message intervals.
	ReadDeadline time.Duration

	// WriteDeadline is the timeout for sending messages to orchestrator.
	WriteDeadline time.Duration

	// ProbeTimeout is the maximum time to wait for a probe response.
	// Longer timeouts reduce false negatives but slow processing.
	ProbeTimeout time.Duration

	// MaxReconnectBackoff is the maximum wait time between reconnection attempts.
	// Actual backoff uses exponential backoff up to this limit.
	MaxReconnectBackoff time.Duration

	// MaxConsecutiveDecodeErrors is the maximum number of consecutive
	// JSON decoding errors before terminating the connection.
	// Set to 0 to never terminate on decode errors (always skip and log).
	MaxConsecutiveDecodeErrors int
}

// DefaultConfig returns a configuration with sensible defaults for production use.
//
// Defaults:
//   - AgentID: "agent-1" (should be overridden per instance)
//   - OrchestratorAddr: "localhost:50050" (local development)
//   - ProberType: "mock" (for testing; use "caracal" in production)
//   - ProberArgs: nil (no additional arguments)
//   - Buffer sizes: 100 (balances memory vs. throughput)
//   - ReadDeadline: 60s (tolerates slow networks)
//   - WriteDeadline: 5s (fail fast on write issues)
//   - ProbeTimeout: 5s (standard timeout for network probes)
//   - MaxReconnectBackoff: 5m (caps exponential backoff)
//   - MaxConsecutiveDecodeErrors: 3 (tolerates transient corruption)
func DefaultConfig() *Config {
	return &Config{
		AgentID:                    "agent-1",
		OrchestratorAddr:           "localhost:50050",
		ProberType:                 "mock",
		ProberPath:                 "",
		ProberArgs:                 nil,
		DirectivesBufferSize:       100,
		FIEsBufferSize:             100,
		ReadDeadline:               60 * time.Second,
		WriteDeadline:              5 * time.Second,
		ProbeTimeout:               5 * time.Second,
		MaxReconnectBackoff:        5 * time.Minute,
		MaxConsecutiveDecodeErrors: 3,
	}
}

// Validate checks that all configuration values are valid.
// Returns an error describing the first invalid field encountered.
func (c *Config) Validate() error {
	// Validate identity and connection
	if c.AgentID == "" {
		return errors.New("agent ID cannot be empty")
	}
	if c.OrchestratorAddr == "" {
		return errors.New("orchestrator address cannot be empty")
	}

	// Validate prober configuration
	validProbers := map[string]bool{"caracal": true, "mock": true}
	if !validProbers[c.ProberType] {
		return fmt.Errorf("prober type must be 'caracal' or 'mock', got: %q", c.ProberType)
	}
	if c.ProberPath != "" {
		if _, err := os.Stat(c.ProberPath); os.IsNotExist(err) {
			return fmt.Errorf("prober path does not exist: %s", c.ProberPath)
		}
	}

	// Validate channel buffers
	if c.DirectivesBufferSize <= 0 {
		return fmt.Errorf("directives buffer size must be positive, got: %d", c.DirectivesBufferSize)
	}
	if c.FIEsBufferSize <= 0 {
		return fmt.Errorf("FIEs buffer size must be positive, got: %d", c.FIEsBufferSize)
	}

	// Validate timeouts
	if c.ReadDeadline <= 0 {
		return fmt.Errorf("read deadline must be positive, got: %v", c.ReadDeadline)
	}
	if c.WriteDeadline <= 0 {
		return fmt.Errorf("write deadline must be positive, got: %v", c.WriteDeadline)
	}
	if c.ProbeTimeout <= 0 {
		return fmt.Errorf("probe timeout must be positive, got: %v", c.ProbeTimeout)
	}
	if c.MaxReconnectBackoff <= 0 {
		return fmt.Errorf("max reconnect backoff must be positive, got: %v", c.MaxReconnectBackoff)
	}

	// Validate error handling
	if c.MaxConsecutiveDecodeErrors < 0 {
		return fmt.Errorf("max consecutive decode errors cannot be negative, got: %d", c.MaxConsecutiveDecodeErrors)
	}

	return nil
}
