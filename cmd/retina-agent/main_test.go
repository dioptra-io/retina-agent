// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Tests for the retina-agent CLI, focusing on runWithReconnect reconnection
// logic. Uses variable injection (agentRun) to avoid real network connections.
//
// Tests that modify agentRun are not parallel; each restores the original
// value via defer.
//
// All functions reach 100% coverage except:
//   - startMetricsServer: goroutine error path after Serve failure requires
//     closing the listener mid-serve, which is inherently racy.
//   - main(): standard practice for functions with os.Exit.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dioptra-io/retina-agent/internal/agent"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMetrics() *agent.Metrics {
	return agent.NewMetrics(prometheus.NewRegistry(), "test-agent")
}

// mockAgentRun simulates agent.Run with configurable behavior.
// Thread-safe call counting using atomic operations.
type mockAgentRun struct {
	calls       atomic.Int32
	returnErr   error
	delay       time.Duration
	maxCalls    int                // return context.Canceled after this many calls
	cancelOnN   int                // cancel context after N calls
	ctxToCancel context.CancelFunc // context to cancel (if cancelOnN > 0)
}

func (m *mockAgentRun) run(ctx context.Context, cfg *agent.Config, logger *slog.Logger, metrics *agent.Metrics) error {
	callNum := int(m.calls.Add(1))

	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if m.cancelOnN > 0 && callNum == m.cancelOnN && m.ctxToCancel != nil {
		m.ctxToCancel()
	}

	if m.maxCalls > 0 && callNum >= m.maxCalls {
		return context.Canceled
	}

	return m.returnErr
}

func (m *mockAgentRun) getCalls() int {
	return int(m.calls.Load())
}

func testBackoffTiming(t *testing.T, cfg *agent.Config, mock *mockAgentRun,
	expectedCalls int, minDuration, maxDuration time.Duration) {
	t.Helper()

	oldRun := agentRun
	agentRun = mock.run
	defer func() { agentRun = oldRun }()

	start := time.Now()

	done := make(chan bool)
	go func() {
		runWithReconnect(context.Background(), cfg, testLogger(), testMetrics())
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

// -- runWithReconnect ---------------------------------------------------------

func TestRunWithReconnect_ImmediateShutdown(t *testing.T) {
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
		runWithReconnect(ctx, cfg, testLogger(), testMetrics())
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
		runWithReconnect(ctx, cfg, testLogger(), testMetrics())
		done <- true
	}()

	// Wait for first attempt to fail and enter backoff.
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

func TestRunWithReconnect_BackoffReset(t *testing.T) {
	cfg := &agent.Config{
		AgentID:             "test-agent",
		OrchestratorAddr:    "localhost:50050",
		MaxReconnectBackoff: 1 * time.Minute,
	}

	mock := &mockAgentRun{
		delay:     1100 * time.Millisecond,
		maxCalls:  2,
		returnErr: errors.New("connection dropped"),
	}

	// Each call takes 1.1s due to delay.
	// With reset:    1.1s (run1) + 1s (backoff) + 1.1s (run2) ≈ 3.2s
	// Without reset: 1.1s (run1) + 2s (backoff) + 1.1s (run2) ≈ 4.2s
	testBackoffTiming(t, cfg, mock, 2, 3*time.Second, 4*time.Second)
}

func TestRunWithReconnect_ContextCancelledDuringRun(t *testing.T) {
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
		runWithReconnect(ctx, cfg, testLogger(), testMetrics())
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("runWithReconnect did not exit on context cancellation")
	}
}

func TestRunWithReconnect_NonContextError(t *testing.T) {
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
		runWithReconnect(context.Background(), cfg, testLogger(), testMetrics())
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
		runWithReconnect(ctx, cfg, testLogger(), testMetrics())
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("runWithReconnect did not respect ctx.Err()")
	}
}

// -- Config -------------------------------------------------------------------

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

// -- newLogger ----------------------------------------------------------------

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

// -- startMetricsServer -------------------------------------------------------

func TestStartMetricsServer(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	registry := prometheus.NewRegistry()
	srv, err := startMetricsServer(testLogger(), registry, addr)
	if err != nil {
		t.Fatalf("startMetricsServer returned unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	// Port is bound before startMetricsServer returns, so the server is
	// reachable immediately — no retry loop needed.
	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", addr)) //nolint:noctx
	if err != nil {
		t.Fatalf("failed to reach metrics server: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("metrics server returned %d, want 200", resp.StatusCode)
	}
}

func TestStartMetricsServer_InvalidAddr(t *testing.T) {
	t.Parallel()

	// With eager net.Listen, an invalid address is rejected immediately.
	_, err := startMetricsServer(testLogger(), prometheus.NewRegistry(), "invalid-addr")
	if err == nil {
		t.Error("startMetricsServer: expected error for invalid address, got nil")
	}
}

// -- multiFlag ----------------------------------------------------------------

func TestMultiFlag(t *testing.T) {
	t.Parallel()

	var f multiFlag
	if err := f.Set("--probing-rate"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := f.Set("100000"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f) != 2 {
		t.Errorf("expected 2 values, got %d", len(f))
	}
	if f.String() != "--probing-rate, 100000" {
		t.Errorf("unexpected String(): %s", f.String())
	}
}
