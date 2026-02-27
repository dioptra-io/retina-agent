// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Package agent provides configuration for the retina network measurement agent.
package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	// Prober types
	ProberTypeCaracal = "caracal"
	ProberTypeMock    = "mock"
)

// Config holds agent configuration.
type Config struct {
	// ===== Agent Identity =====

	// AgentID is the unique identifier for this agent instance.
	// Used in logging and for the orchestrator to track agents.
	AgentID string

	// ===== Orchestrator Connection =====

	// OrchestratorAddr is the TCP address of the orchestrator (host:port).
	OrchestratorAddr string

	// Secret is the authentication credential shared between agent and orchestrator.
	// If empty, authentication is disabled.
	// Both agent and orchestrator must know this value for authentication to succeed.
	Secret string

	// ReadDeadline is the timeout for receiving messages from orchestrator.
	// Should be longer than expected message intervals.
	ReadDeadline time.Duration

	// WriteDeadline is the timeout for sending messages to orchestrator.
	WriteDeadline time.Duration

	// MaxReconnectBackoff is the maximum wait time between reconnection attempts.
	// Actual backoff uses exponential backoff up to this limit.
	MaxReconnectBackoff time.Duration

	// MaxConsecutiveDecodeErrors is the maximum number of consecutive
	// JSON decoding errors before terminating the connection.
	// Set to 0 to never terminate on decode errors (always skip and log).
	MaxConsecutiveDecodeErrors int

	// ===== Prober Configuration =====

	// ProberType specifies which prober implementation to use.
	// Valid values: ProberTypeCaracal, ProberTypeMock
	ProberType string

	// ProberPath is the filesystem path to the prober executable.
	// If empty, searches PATH. Only required for caracal prober type.
	ProberPath string

	// ProberArgs contains additional command-line arguments to pass to the prober.
	// Optional. Only applies to caracal prober type.
	// Example: []string{"--probing-rate", "100000", "--n-packets", "3"}
	ProberArgs []string

	// WriteQueueSize is the buffer size for the caracal prober's write queue.
	// Larger buffers provide more tolerance for burst traffic.
	WriteQueueSize int

	// CleanupInterval is how often to clean up stale probes in the caracal prober.
	CleanupInterval time.Duration

	// ProbeTimeout is the maximum time to wait for a probe response.
	// Longer timeouts reduce false negatives but slow processing.
	ProbeTimeout time.Duration

	// ===== Pipeline Buffers =====

	// PDsBufferSize is the channel buffer for incoming directives.
	// Larger buffers provide more tolerance for processing delays.
	PDsBufferSize int

	// FIEsBufferSize is the channel buffer for outgoing FIEs.
	// Should match expected probe completion rate.
	FIEsBufferSize int
}

// DefaultConfig returns a configuration with sensible defaults for production use.
//
// Defaults:
//   - AgentID: "agent-1" (should be overridden per instance)
//   - OrchestratorAddr: "localhost:50050" (local development)
//   - Secret: "" (authentication disabled; set via RETINA_SECRET environment variable)
//   - ProberType: ProberTypeMock (for testing; use ProberTypeCaracal in production)
//   - ProberArgs: nil (no additional arguments)
//   - WriteQueueSize: 1000 (balances memory vs. throughput)
//   - CleanupInterval: 10s (removes stale probes periodically)
//   - Buffer sizes: 100 (balances memory vs. throughput)
//   - ReadDeadline: 60s (tolerates slow networks)
//   - WriteDeadline: 5s (fail fast on write issues)
//   - ProbeTimeout: 5s (standard timeout for network probes)
//   - MaxReconnectBackoff: 5m (caps exponential backoff)
//   - MaxConsecutiveDecodeErrors: 3 (tolerates transient corruption)
func DefaultConfig() *Config {
	return &Config{
		// Agent Identity
		AgentID: "agent-1",

		// Orchestrator Connection
		OrchestratorAddr:           "localhost:50050",
		Secret:                     "", // Empty = no authentication
		ReadDeadline:               10 * time.Second,
		WriteDeadline:              5 * time.Second,
		MaxReconnectBackoff:        5 * time.Minute,
		MaxConsecutiveDecodeErrors: 3,

		// Prober Configuration
		ProberType:      ProberTypeMock,
		ProberPath:      "",
		ProberArgs:      nil,
		WriteQueueSize:  1000,
		CleanupInterval: 10 * time.Second,
		ProbeTimeout:    5 * time.Second,

		// Pipeline Buffers
		PDsBufferSize:  100,
		FIEsBufferSize: 100,
	}
}

// Validate checks that all configuration values are valid.
// Returns an error describing the first invalid field encountered.
func (c *Config) Validate() error {
	if err := c.validateAgentIdentity(); err != nil {
		return err
	}
	if err := c.validateConnection(); err != nil {
		return err
	}
	if err := c.validateProber(); err != nil {
		return err
	}
	if err := c.validateBuffers(); err != nil {
		return err
	}
	return nil
}

// validateAgentIdentity checks agent identity fields.
func (c *Config) validateAgentIdentity() error {
	if c.AgentID == "" {
		return errors.New("agent ID cannot be empty")
	}
	return nil
}

// validateConnection checks orchestrator connection and authentication fields.
func (c *Config) validateConnection() error {
	if c.OrchestratorAddr == "" {
		return errors.New("orchestrator address cannot be empty")
	}
	if _, _, err := net.SplitHostPort(c.OrchestratorAddr); err != nil {
		return fmt.Errorf("orchestrator address must be in host:port format: %w", err)
	}

	if err := c.validateSecret(); err != nil {
		return err
	}

	if c.ReadDeadline <= 0 {
		return fmt.Errorf("read deadline must be positive, got: %v", c.ReadDeadline)
	}
	if c.WriteDeadline <= 0 {
		return fmt.Errorf("write deadline must be positive, got: %v", c.WriteDeadline)
	}
	if c.MaxReconnectBackoff <= 0 {
		return fmt.Errorf("max reconnect backoff must be positive, got: %v", c.MaxReconnectBackoff)
	}
	if c.MaxConsecutiveDecodeErrors < 0 {
		return fmt.Errorf("max consecutive decode errors cannot be negative, got: %d", c.MaxConsecutiveDecodeErrors)
	}
	return nil
}

// validateSecret checks that the secret meets security requirements if provided.
func (c *Config) validateSecret() error {
	if c.Secret == "" {
		return nil // Empty is valid (no authentication)
	}

	// Check for obviously weak/test secrets FIRST (even if they're short)
	weakSecrets := []string{"test", "secret", "password", "123456", "abc123", "changeme"}
	for _, weak := range weakSecrets {
		if c.Secret == weak {
			return fmt.Errorf("secret '%s' is a known weak/test value; use a strong randomly-generated secret", weak)
		}
	}

	// Check minimum length (at least 16 characters for security)
	if len(c.Secret) < 16 {
		return fmt.Errorf("secret is too short (%d chars); use at least 16 characters for security (generate with: openssl rand -hex 32)", len(c.Secret))
	}

	return nil
}

// validateProber checks prober configuration fields.
func (c *Config) validateProber() error {
	validProbers := map[string]bool{
		ProberTypeCaracal: true,
		ProberTypeMock:    true,
	}
	if !validProbers[c.ProberType] {
		return fmt.Errorf("prober type must be %q or %q, got: %q",
			ProberTypeCaracal, ProberTypeMock, c.ProberType)
	}
	if c.ProberPath != "" {
		if _, err := os.Stat(c.ProberPath); os.IsNotExist(err) {
			return fmt.Errorf("prober path does not exist: %s", c.ProberPath)
		}
	}
	if c.WriteQueueSize <= 0 {
		return fmt.Errorf("write queue size must be positive, got: %d", c.WriteQueueSize)
	}
	if c.CleanupInterval <= 0 {
		return fmt.Errorf("cleanup interval must be positive, got: %v", c.CleanupInterval)
	}
	if c.ProbeTimeout <= 0 {
		return fmt.Errorf("probe timeout must be positive, got: %v", c.ProbeTimeout)
	}
	return nil
}

// validateBuffers checks pipeline buffer size fields.
func (c *Config) validateBuffers() error {
	if c.PDsBufferSize <= 0 {
		return fmt.Errorf("PDs buffer size must be positive, got: %d", c.PDsBufferSize)
	}
	if c.FIEsBufferSize <= 0 {
		return fmt.Errorf("FIEs buffer size must be positive, got: %d", c.FIEsBufferSize)
	}
	return nil
}
