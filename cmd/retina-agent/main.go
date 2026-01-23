// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Command retina-agent is a network measurement agent that connects to
// an orchestrator to receive probing directives and return measurements.
//
// Usage:
//
//	retina-agent [flags]
//
// Example:
//
//	retina-agent -id agent-1 -address orchestrator.example.com:50050 -prober-type caracal
//
// The agent automatically reconnects on connection loss using exponential
// backoff. Press Ctrl+C for graceful shutdown.
//
// Available prober types: caracal (default), mock
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/dioptra-io/retina-agent/internal/agent"
)

var (
	// Agent identity and connection
	agentID          = flag.String("id", "agent-1", "Unique identifier for this agent")
	orchestratorAddr = flag.String("address", "localhost:50050", "Orchestrator address (host:port)")

	// Prober configuration
	proberType = flag.String("prober-type", "caracal", "Prober implementation (caracal, mock)")
	proberPath = flag.String("prober-path", "", "Path to prober executable (searches PATH if empty)")

	// Buffer sizes
	directivesBufferSize = flag.Int("directives-buffer", 100, "Directives channel buffer size")
	fiesBufferSize       = flag.Int("fies-buffer", 100, "FIEs channel buffer size")

	// Timeouts and deadlines
	readDeadline        = flag.Duration("read-deadline", 60*time.Second, "Read timeout for orchestrator connection")
	writeDeadline       = flag.Duration("write-deadline", 5*time.Second, "Write timeout for orchestrator connection")
	probeTimeout        = flag.Duration("probe-timeout", 5*time.Second, "Timeout for individual probe responses")
	maxReconnectBackoff = flag.Duration("max-reconnect-backoff", 5*time.Minute, "Maximum wait time between reconnection attempts")

	// Error handling
	maxConsecutiveDecodeErrors = flag.Int("max-consecutive-decode-errors", 3, "Maximum consecutive decode errors before reconnecting (0 to disable)")
)

func main() {
	flag.Parse()

	cfg := &agent.Config{
		AgentID:                    *agentID,
		OrchestratorAddr:           *orchestratorAddr,
		ProberType:                 *proberType,
		ProberPath:                 *proberPath,
		DirectivesBufferSize:       *directivesBufferSize,
		FIEsBufferSize:             *fiesBufferSize,
		ReadDeadline:               *readDeadline,
		WriteDeadline:              *writeDeadline,
		ProbeTimeout:               *probeTimeout,
		MaxReconnectBackoff:        *maxReconnectBackoff,
		MaxConsecutiveDecodeErrors: *maxConsecutiveDecodeErrors,
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runWithReconnect(ctx, cfg)
}

// runWithReconnect wraps agent.Run with exponential backoff reconnection.
//
// The backoff starts at 1 second and doubles on each failure, up to
// MaxReconnectBackoff. On intentional shutdown (Ctrl+C), the function
// returns immediately without retrying.
func runWithReconnect(ctx context.Context, cfg *agent.Config) {
	const (
		initialBackoff = 1 * time.Second
		backoffFactor  = 2
	)

	backoff := initialBackoff
	for {
		log.Printf("Agent %s: Connecting to %s", cfg.AgentID, cfg.OrchestratorAddr)

		err := agent.Run(ctx, cfg)

		// Distinguish intentional shutdown from connection failure
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			log.Printf("Agent %s: Shutdown complete", cfg.AgentID)
			return
		}

		log.Printf("Agent %s: Connection lost: %v", cfg.AgentID, err)
		log.Printf("Agent %s: Reconnecting in %v", cfg.AgentID, backoff)

		select {
		case <-time.After(backoff):
			backoff *= backoffFactor
			if backoff > cfg.MaxReconnectBackoff {
				backoff = cfg.MaxReconnectBackoff
			}
		case <-ctx.Done():
			log.Printf("Agent %s: Shutdown during reconnect backoff", cfg.AgentID)
			return
		}
	}
}
