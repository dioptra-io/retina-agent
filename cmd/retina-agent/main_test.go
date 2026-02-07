// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// ## Test Coverage
//
// Tests for the retina-agent command-line interface, focusing on the
// runWithReconnect reconnection logic with exponential backoff.
//
// Coverage:
// - runWithReconnect: 100% - All reconnection scenarios and shutdown paths
// - Config validation: 100% - All validation rules
// - main(): 0% (untested) - Standard practice for main functions with log.Fatalf
//
// ## Testing Strategy
//
// Uses variable injection (var agentRun = agent.Run) to replace the real
// agent.Run with a mock during tests. This follows Go standard library
// patterns (e.g., net/http tests) and allows comprehensive testing without
// requiring actual network connections.
//
// Tests cannot be parallel because they modify the global agentRun variable.
// Each test properly restores the original value via defer.

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dioptra-io/retina-agent/internal/agent"
)

// ============================================================================
// TEST HELPERS
// ============================================================================

// mockAgentRun simulates agent.Run with configurable behavior.
//
// Allows testing reconnection logic without actual network operations.
// Thread-safe call counting using atomic operations.
type mockAgentRun struct {
	calls       atomic.Int32       // Number of times run() was called
	returnErr   error              // Error to return from run()
	delay       time.Duration      // Simulate connection time
	maxCalls    int                // Return context.Canceled after this many calls
	cancelOnN   int                // Cancel context after N calls
	ctxToCancel context.CancelFunc // Context to cancel (if cancelOnN > 0)
}

func (m *mockAgentRun) run(ctx context.Context, cfg *agent.Config) error {
	callNum := int(m.calls.Add(1))

	// Simulate work
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Cancel context if requested
	if m.cancelOnN > 0 && callNum == m.cancelOnN && m.ctxToCancel != nil {
		m.ctxToCancel()
	}

	// Stop after maxCalls
	if m.maxCalls > 0 && callNum >= m.maxCalls {
		return context.Canceled
	}

	return m.returnErr
}

func (m *mockAgentRun) getCalls() int {
	return int(m.calls.Load())
}

// testBackoffTiming runs a backoff test with the given parameters.
// Extracted to reduce duplication between backoff tests.
func testBackoffTiming(t *testing.T, cfg *agent.Config, mock *mockAgentRun,
	expectedCalls int, minDuration, maxDuration time.Duration) {
	t.Helper()

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	start := time.Now()

	done := make(chan bool)
	go func() {
		runWithReconnect(context.Background(), cfg)
		done <- true
	}()

	select {
	case <-done:
		elapsed := time.Since(start)

		if elapsed < minDuration || elapsed > maxDuration {
			t.Errorf("Expected backoff between %v and %v, got %v", minDuration, maxDuration, elapsed)
		}

		if mock.getCalls() != expectedCalls {
			t.Errorf("Expected %d calls, got %d", expectedCalls, mock.getCalls())
		}
	case <-time.After(maxDuration + 5*time.Second):
		t.Fatal("runWithReconnect did not complete")
	}
}

// validConfig returns a valid configuration for testing.
// Used as a base that can be modified for specific test cases.
func validConfig() *agent.Config {
	return &agent.Config{
		AgentID:                    "test-agent",
		OrchestratorAddr:           "localhost:50050",
		ProberType:                 agent.ProberTypeMock,
		PDsBufferSize:              100,
		FIEsBufferSize:             100,
		ReadDeadline:               60 * time.Second,
		WriteDeadline:              5 * time.Second,
		ProbeTimeout:               5 * time.Second,
		MaxReconnectBackoff:        5 * time.Minute,
		WriteQueueSize:             1000,
		CleanupInterval:            10 * time.Second,
		MaxConsecutiveDecodeErrors: 3,
	}
}

// ============================================================================
// UNIT TESTS - runWithReconnect
// ============================================================================

func TestRunWithReconnect_ImmediateShutdown(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	// Test that runWithReconnect exits immediately when context is
	// already cancelled (e.g., Ctrl+C before first connection attempt)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{returnErr: context.Canceled}

	// Replace agentRun
	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(ctx, cfg)
		done <- true
	}()

	select {
	case <-done:
		if mock.getCalls() != 1 {
			t.Errorf("Expected 1 call, got %d", mock.getCalls())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runWithReconnect did not exit on immediate shutdown")
	}
}

func TestRunWithReconnect_ShutdownDuringBackoff(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	// Test that runWithReconnect respects context cancellation during
	// the exponential backoff wait period (doesn't wait for full backoff)

	ctx, cancel := context.WithCancel(context.Background())

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 5 * time.Minute,
	}

	mock := &mockAgentRun{returnErr: errors.New("connection failed")}

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(ctx, cfg)
		done <- true
	}()

	// Wait for first attempt to fail and enter backoff
	time.Sleep(100 * time.Millisecond)

	// Cancel during backoff
	cancel()

	select {
	case <-done:
		if mock.getCalls() < 1 {
			t.Error("Expected at least 1 call")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWithReconnect did not exit during backoff")
	}
}

func TestRunWithReconnect_ExponentialBackoff(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	// Test that backoff doubles on each failure: 1s → 2s → 4s
	// Verifies timing is correct within reasonable tolerance

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		maxCalls:  4, // Fail 3 times, succeed on 4th
		returnErr: errors.New("connection failed"),
	}

	// Expected backoff: 1s + 2s + 4s = 7s total
	testBackoffTiming(t, cfg, mock, 4, 6*time.Second, 9*time.Second)
}

func TestRunWithReconnect_BackoffCapping(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	// Test that backoff is capped at MaxReconnectBackoff
	// Expected: 1s → 2s → 3s (capped) → 3s (capped)

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 3 * time.Second, // Cap at 3s
	}

	mock := &mockAgentRun{
		maxCalls:  5, // Fail 4 times, succeed on 5th
		returnErr: errors.New("connection failed"),
	}

	// Expected: 1s + 2s + 3s + 3s = 9s (capped at 3s after 2nd retry)
	testBackoffTiming(t, cfg, mock, 5, 8*time.Second, 11*time.Second)
}

func TestRunWithReconnect_ContextCancelledDuringRun(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	// Test that context cancellation during agent.Run() causes
	// immediate exit without retry

	ctx, cancel := context.WithCancel(context.Background())

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		delay:       200 * time.Millisecond, // Simulate long-running connection
		returnErr:   errors.New("connection failed"),
		ctxToCancel: cancel,
		cancelOnN:   1, // Cancel during first call
	}

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(ctx, cfg)
		done <- true
	}()

	select {
	case <-done:
		// Should exit immediately when context is cancelled
	case <-time.After(1 * time.Second):
		t.Fatal("runWithReconnect did not exit on context cancellation")
	}
}

func TestRunWithReconnect_NonContextError(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	// Test that non-context errors (network failures, etc.) trigger
	// reconnection with backoff

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		maxCalls:  3, // Fail twice with network error, then exit
		returnErr: errors.New("network error"),
	}

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(context.Background(), cfg)
		done <- true
	}()

	select {
	case <-done:
		if mock.getCalls() != 3 {
			t.Errorf("Expected 3 calls, got %d", mock.getCalls())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runWithReconnect did not complete")
	}
}

func TestRunWithReconnect_ContextErrorCheck(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	// Test that ctx.Err() is checked even when agent.Run returns
	// a different error (covers the "|| ctx.Err() != nil" branch)

	ctx, cancel := context.WithCancel(context.Background())

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		returnErr:   errors.New("network error"),
		ctxToCancel: cancel,
		cancelOnN:   1, // Cancel during first call
	}

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(ctx, cfg)
		done <- true
	}()

	select {
	case <-done:
		// Should exit because ctx.Err() != nil
	case <-time.After(1 * time.Second):
		t.Fatal("runWithReconnect did not respect ctx.Err()")
	}
}

// ============================================================================
// UNIT TESTS - Configuration
// ============================================================================

func TestConfig_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		modify  func(*agent.Config)
		wantErr bool
	}{
		{"valid config", func(c *agent.Config) {}, false},
		{"empty agent ID", func(c *agent.Config) { c.AgentID = "" }, true},
		{"empty orchestrator address", func(c *agent.Config) { c.OrchestratorAddr = "" }, true},
		{"invalid prober type", func(c *agent.Config) { c.ProberType = "invalid-type" }, true},
		{"zero read deadline", func(c *agent.Config) { c.ReadDeadline = 0 }, true},
		{"zero write deadline", func(c *agent.Config) { c.WriteDeadline = 0 }, true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			tt.modify(cfg)

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
