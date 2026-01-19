// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// agent_test.go
package agent

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
)

func TestAgentPipeline(t *testing.T) {
	// Setup mock orchestrator
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to start mock orchestrator: %v", err)
	}
	defer listener.Close()

	orchestratorAddr := listener.Addr().String()

	// Channel to collect errors from orchestrator goroutine
	orchestratorErr := make(chan error, 1)

	// Channel to signal test completion
	done := make(chan struct{})

	// Mock orchestrator goroutine
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			orchestratorErr <- err
			return
		}
		defer conn.Close()

		encoder := json.NewEncoder(conn)
		decoder := json.NewDecoder(conn)

		// Send one directive
		directive := &api.ProbingDirective{
			AgentID:            "test-agent",
			IPVersion:          4, // ✅ Just use integer 4 or 6
			Protocol:           api.UDP,
			DestinationAddress: net.ParseIP("8.8.8.8"),
			NearTTL:            10,
			NextHeader: api.NextHeader{
				UDPNextHeader: &api.UDPNextHeader{
					// ✅ Try these field name variations:
					// Option 1: Full names
					SourcePort:      24000,
					DestinationPort: 33434,
					// Option 2: Abbreviated
					// Src: 24000,
					// Dst: 33434,
				},
			},
		}

		if err := encoder.Encode(directive); err != nil {
			orchestratorErr <- err
			return
		}

		// Receive FIE
		var fie api.ForwardingInfoElement
		if err := decoder.Decode(&fie); err != nil {
			orchestratorErr <- err
			return
		}

		// Verify FIE
		if fie.Agent.AgentID != "test-agent" {
			orchestratorErr <- err
			return
		}
		if fie.DestinationAddress.String() != "8.8.8.8" {
			orchestratorErr <- err
			return
		}
		if fie.NearInfo.ReplyAddress == nil {
			orchestratorErr <- err
			return
		}
		if fie.FarInfo.ReplyAddress == nil {
			orchestratorErr <- err
			return
		}

		// Success!
		close(done)
	}()

	// Configure agent
	cfg := &Config{
		AgentID:                    "test-agent",
		OrchestratorAddr:           orchestratorAddr,
		ProberType:                 "mock",
		DirectivesBufferSize:       10,
		FIEsBufferSize:             10,
		ReadDeadline:               5 * time.Second,
		WriteDeadline:              5 * time.Second,
		ProbeTimeout:               1 * time.Second,
		MaxReconnectBackoff:        5 * time.Minute,
		MaxConsecutiveDecodeErrors: 3,
	}

	// Run agent with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentDone := make(chan error, 1)
	go func() {
		agentDone <- Run(ctx, cfg)
	}()

	// Wait for test to complete or timeout
	select {
	case <-done:
		// Success! Cancel agent and wait for clean shutdown
		cancel()
		<-agentDone
		t.Log("Pipeline test passed: directive → probes → FIE")

	case err := <-orchestratorErr:
		cancel()
		<-agentDone
		t.Fatalf("Orchestrator error: %v", err)

	case <-time.After(5 * time.Second):
		cancel()
		<-agentDone
		t.Fatal("Test timeout: agent did not complete pipeline")

	case err := <-agentDone:
		if err != context.Canceled && err != context.DeadlineExceeded {
			t.Fatalf("Agent stopped with unexpected error: %v", err)
		}
	}
}
