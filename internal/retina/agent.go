// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

// Package retina implements the Retina agent.
package retina

import (
	"context"
	"fmt"
	"log/slog"
)

// Config is the configuration of the agent.
type Config struct {
	// ID is the agent ID presented to the orchestrator.
	ID           string             `json:"id"`
	Orchestrator OrchestratorConfig `json:"orchestrator"`
}

// OrchestratorConfig configures the connection to the orchestrator.
type OrchestratorConfig struct {
	// Address is the TCP address of the orchestrator, in the form "host:port".
	Address string `json:"address"`
	// Secret is the shared secret presented in the handshake.
	Secret string `json:"-"`
}

// Validate reports whether the configuration is usable.
func (c *Config) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("id cannot be empty")
	}
	if c.Orchestrator.Address == "" {
		return fmt.Errorf("orchestrator: address cannot be empty")
	}
	return nil
}

// Agent receives PDs from the orchestrator, probes, and reports FIEs.
type Agent struct {
	config *Config
	logger *slog.Logger
}

// NewAgent creates an agent from the given configuration.
func NewAgent(config *Config, logger *slog.Logger) (*Agent, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Agent{
		config: config,
		logger: logger,
	}, nil
}

// Run runs the agent until ctx is done or a component fails. It returns nil
// on a clean shutdown.
func (a *Agent) Run(ctx context.Context) error {
	a.logger.Info("Agent started", slog.String("agent_id", a.config.ID))

	// TODO: connect to the orchestrator, start the prober, and run the PD
	// and FIE loops.
	<-ctx.Done()

	a.logger.Info("Shutting down")
	return nil
}
