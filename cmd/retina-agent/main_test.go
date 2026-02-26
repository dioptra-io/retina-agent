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
// - newLogger: 100% - All log levels including invalid fallback
// - main(): 0% (untested) - Standard practice for main functions with os.Exit
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
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dioptra-io/retina-agent/internal/agent"
)

// testLogger returns a logger that discards all output, keeping test output clean.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

func (m *mockAgentRun) run(ctx context.Context, cfg *agent.Config, logger *slog.Logger) error {
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
		runWithReconnect(context.Background(), cfg, testLogger())
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{returnErr: context.Canceled}

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(ctx, cfg, testLogger())
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
		runWithReconnect(ctx, cfg, testLogger())
		done <- true
	}()

	// Wait for first attempt to fail and enter backoff
	time.Sleep(100 * time.Millisecond)
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

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		maxCalls:  4,
		returnErr: errors.New("connection failed"),
	}

	// Expected backoff: 1s + 2s + 4s = 7s total
	testBackoffTiming(t, cfg, mock, 4, 6*time.Second, 9*time.Second)
}

func TestRunWithReconnect_BackoffCapping(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 3 * time.Second,
	}

	mock := &mockAgentRun{
		maxCalls:  5,
		returnErr: errors.New("connection failed"),
	}

	// Expected: 1s + 2s + 3s + 3s = 9s (capped at 3s after 2nd retry)
	testBackoffTiming(t, cfg, mock, 5, 8*time.Second, 11*time.Second)
}

func TestRunWithReconnect_ContextCancelledDuringRun(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	ctx, cancel := context.WithCancel(context.Background())

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		delay:       200 * time.Millisecond,
		returnErr:   errors.New("connection failed"),
		ctxToCancel: cancel,
		cancelOnN:   1,
	}

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(ctx, cfg, testLogger())
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("runWithReconnect did not exit on context cancellation")
	}
}

func TestRunWithReconnect_NonContextError(t *testing.T) {
	// Note: Not parallel - modifies global agentRun variable

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		maxCalls:  3,
		returnErr: errors.New("network error"),
	}

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(context.Background(), cfg, testLogger())
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

	ctx, cancel := context.WithCancel(context.Background())

	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		returnErr:   errors.New("network error"),
		ctxToCancel: cancel,
		cancelOnN:   1,
	}

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	done := make(chan bool)
	go func() {
		runWithReconnect(ctx, cfg, testLogger())
		done <- true
	}()

	select {
	case <-done:
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

// ============================================================================
// UNIT TESTS - Logging
// ============================================================================
func TestNewLogger_Levels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input     string
		wantLevel slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"invalid", slog.LevelInfo}, // fallback to info
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			logger := newLogger(tt.input)
			if logger == nil {
				t.Fatal("newLogger returned nil")
			}
			if !logger.Enabled(context.Background(), tt.wantLevel) {
				t.Errorf("newLogger(%q): level %v should be enabled", tt.input, tt.wantLevel)
			}
			if tt.wantLevel > slog.LevelDebug {
				if logger.Enabled(context.Background(), tt.wantLevel-1) {
					t.Errorf("newLogger(%q): level below %v should not be enabled", tt.input, tt.wantLevel)
				}
			}
		})
	}
}
