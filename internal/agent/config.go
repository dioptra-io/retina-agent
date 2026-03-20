// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	ProberTypeCaracal = "caracal"
	ProberTypeMock    = "mock"
)

// Config holds agent configuration.
type Config struct {
	AgentID string
	// OrchestratorAddr is the TCP address of the orchestrator (host:port).
	OrchestratorAddr string
	// Secret is the authentication credential shared between agent and orchestrator.
	// If empty, authentication is disabled.
	Secret string
	// ReadDeadline is the timeout for receiving messages from orchestrator.
	// Should be longer than expected message intervals.
	ReadDeadline time.Duration
	// WriteDeadline is the timeout for sending messages to orchestrator.
	WriteDeadline       time.Duration
	MaxReconnectBackoff time.Duration
	// MaxConsecutiveDecodeErrors is the maximum number of consecutive
	// JSON decoding errors before terminating the connection to the orchestrator.
	// Set to 0 to never terminate on decode errors (always skip and log).
	MaxConsecutiveDecodeErrors int

	ProberType string
	// ProberPath is the filesystem path to the prober executable.
	// If empty, searches PATH. Only required for caracal prober type.
	ProberPath string
	// ProberArgs contains additional command-line arguments to pass to the prober.
	// Only applies to caracal prober type.
	// Example: []string{"--probing-rate", "100000", "--n-packets", "3"}
	ProberArgs []string
	// WriteQueueSize is the buffer size for the caracal prober's write queue.
	// Larger buffers provide more tolerance for burst traffic.
	WriteQueueSize  int
	CleanupInterval time.Duration
	// ProbeTimeout is the maximum time to wait for a probe response.
	// Longer timeouts reduce false negatives but slow processing.
	ProbeTimeout time.Duration

	PDsBufferSize  int
	FIEsBufferSize int
}

// DefaultConfig returns a configuration with sensible defaults for production use.
func DefaultConfig() *Config {
	return &Config{
		AgentID:                    "agent-1",
		OrchestratorAddr:           "localhost:50050",
		ReadDeadline:               10 * time.Second,
		WriteDeadline:              5 * time.Second,
		MaxReconnectBackoff:        5 * time.Minute,
		MaxConsecutiveDecodeErrors: 3,

		ProberType:      ProberTypeMock,
		WriteQueueSize:  1000,
		CleanupInterval: 10 * time.Second,
		ProbeTimeout:    5 * time.Second,

		PDsBufferSize:  100,
		FIEsBufferSize: 100,
	}
}

// Validate checks that all configuration values are valid.
// Returns an error describing the first invalid field encountered.
func (c *Config) Validate() error {
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

func (c *Config) validateConnection() error {
	if c.AgentID == "" {
		return errors.New("agent ID cannot be empty")
	}
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

func (c *Config) validateSecret() error {
	if c.Secret == "" {
		return nil
	}

	weakSecrets := []string{"test", "secret", "password", "123456", "abc123", "changeme"}
	for _, weak := range weakSecrets {
		if c.Secret == weak {
			return fmt.Errorf("secret '%s' is a known weak/test value; use a strong randomly-generated secret", weak)
		}
	}

	if len(c.Secret) < 16 {
		return fmt.Errorf("secret is too short (%d chars); use at least 16 characters for security (generate with: openssl rand -hex 32)", len(c.Secret))
	}

	return nil
}

func (c *Config) validateProber() error {
	if c.ProberType != ProberTypeCaracal && c.ProberType != ProberTypeMock {
		return fmt.Errorf("prober type must be %q or %q, got: %q",
			ProberTypeCaracal, ProberTypeMock, c.ProberType)
	}
	if c.ProberPath != "" {
		info, err := os.Stat(c.ProberPath)
		if os.IsNotExist(err) {
			return fmt.Errorf("prober path does not exist: %s", c.ProberPath)
		}
		if err == nil && info.IsDir() {
			return fmt.Errorf("prober path is a directory, not an executable: %s", c.ProberPath)
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

func (c *Config) validateBuffers() error {
	if c.PDsBufferSize <= 0 {
		return fmt.Errorf("PDs buffer size must be positive, got: %d", c.PDsBufferSize)
	}
	if c.FIEsBufferSize <= 0 {
		return fmt.Errorf("FIEs buffer size must be positive, got: %d", c.FIEsBufferSize)
	}
	return nil
}
