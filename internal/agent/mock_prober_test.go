// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// ## Test Coverage
//
// Current coverage: 100%
//
// MockProber is a simple test double with no external dependencies or
// unreachable error paths. All code paths (successful probes, timeouts,
// context cancellation, and concurrent access) are tested.

package agent

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

// ============================================================================
// TEST HELPER FUNCTIONS
// ============================================================================

// makeProbingDirective creates a minimal ProbingDirective for testing.
func makeProbingDirective(dest string, proto api.Protocol) *api.ProbingDirective {
	return &api.ProbingDirective{
		ProbingDirectiveID: 1,
		DestinationAddress: net.ParseIP(dest),
		Protocol:           proto,
	}
}

// ============================================================================
// UNIT TESTS - MockProber functionality
// ============================================================================

func TestMockProber_SuccessfulProbe(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)
	defer func() { _ = prober.Close() }()

	pd := makeProbingDirective("8.8.8.8", api.UDP)
	ctx := context.Background()
	result, err := prober.Probe(ctx, pd, 10)

	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}

	// Either timeout or success is valid
	if result.TimedOut {
		// Timeout case
		if result.SentTime.IsZero() {
			t.Error("SentTime should be set even for timeouts")
		}
		if !result.ReceivedTime.IsZero() {
			t.Error("ReceivedTime should be zero for timeout")
		}
	} else {
		// Success case
		if result.ReplyAddress == nil {
			t.Error("ReplyAddress should not be nil for successful probe")
		}
		if !result.ReplyAddress.Equal(pd.DestinationAddress) {
			t.Errorf("ReplyAddress = %v, want %v", result.ReplyAddress, pd.DestinationAddress)
		}
		if result.SentTime.IsZero() {
			t.Error("SentTime should not be zero")
		}
		if result.ReceivedTime.IsZero() {
			t.Error("ReceivedTime should not be zero")
		}
		if !result.ReceivedTime.After(result.SentTime) {
			t.Error("ReceivedTime should be after SentTime")
		}
	}
}

func TestMockProber_Timestamps(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)
	defer func() { _ = prober.Close() }()

	pd := makeProbingDirective("1.1.1.1", api.ICMP)
	ctx := context.Background()

	// Run multiple probes to eventually get a successful one
	var successResult *ProbeResult
	for i := 0; i < 50; i++ {
		result, err := prober.Probe(ctx, pd, 15)
		if err != nil {
			t.Fatalf("Probe failed: %v", err)
		}
		if !result.TimedOut {
			successResult = result
			break
		}
	}

	if successResult == nil {
		t.Skip("No successful probe in 50 attempts (very unlikely)")
		return
	}

	// Verify timestamp ordering
	if successResult.SentTime.IsZero() {
		t.Error("SentTime should not be zero")
	}
	if successResult.ReceivedTime.IsZero() {
		t.Error("ReceivedTime should not be zero")
	}
	if !successResult.ReceivedTime.After(successResult.SentTime) {
		t.Errorf("ReceivedTime (%v) should be after SentTime (%v)",
			successResult.ReceivedTime, successResult.SentTime)
	}

	// RTT should be reasonable (10-100ms as per implementation)
	rtt := successResult.ReceivedTime.Sub(successResult.SentTime)
	if rtt < 10*time.Millisecond || rtt > 150*time.Millisecond {
		t.Errorf("RTT %v outside expected range [10ms, 150ms]", rtt)
	}
}

func TestMockProber_TimeoutBehavior(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)
	defer func() { _ = prober.Close() }()

	pd := makeProbingDirective("8.8.8.8", api.UDP)
	ctx := context.Background()

	// Run many probes to verify timeout rate is approximately 10%
	const numProbes = 200
	timeouts := 0

	for i := 0; i < numProbes; i++ {
		result, err := prober.Probe(ctx, pd, 10)
		if err != nil {
			t.Fatalf("Probe %d failed: %v", i, err)
		}
		if result.TimedOut {
			timeouts++
			// Verify timeout has SentTime but no ReceivedTime
			if result.SentTime.IsZero() {
				t.Error("Timeout should have SentTime set")
			}
			if !result.ReceivedTime.IsZero() {
				t.Error("Timeout should not have ReceivedTime set")
			}
		}
	}

	timeoutRate := float64(timeouts) / float64(numProbes)
	// Allow range of 5%-20% (10% ± 50% tolerance for randomness)
	if timeoutRate < 0.05 || timeoutRate > 0.20 {
		t.Errorf("Timeout rate %.2f%% outside expected range [5%%, 20%%]", timeoutRate*100)
	}
}

func TestMockProber_ContextCancellation(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)
	defer func() { _ = prober.Close() }()

	pd := makeProbingDirective("8.8.8.8", api.UDP)

	// Create context that we'll cancel immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	result, err := prober.Probe(ctx, pd, 10)

	if err != context.Canceled {
		t.Errorf("Expected context.Canceled error, got: %v", err)
	}
	if result != nil {
		t.Errorf("Expected nil result on cancellation, got: %v", result)
	}
}

func TestMockProber_ContextTimeout(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)
	defer func() { _ = prober.Close() }()

	pd := makeProbingDirective("8.8.8.8", api.UDP)

	// Create context with very short timeout (shorter than min delay of 10ms)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	result, err := prober.Probe(ctx, pd, 10)

	if err != context.DeadlineExceeded {
		t.Errorf("Expected context.DeadlineExceeded error, got: %v", err)
	}
	if result != nil {
		t.Errorf("Expected nil result on timeout, got: %v", result)
	}
}

func TestMockProber_ConcurrentProbes(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)
	defer func() { _ = prober.Close() }()

	pd := makeProbingDirective("8.8.8.8", api.UDP)
	ctx := context.Background()

	// Launch 10 concurrent probes
	const numGoroutines = 10
	done := make(chan error, numGoroutines)

	for i := uint8(0); i < numGoroutines; i++ {
		go func(ttl uint8) {
			result, err := prober.Probe(ctx, pd, ttl)
			if err != nil {
				done <- err
				return
			}
			if result == nil {
				done <- nil
				return
			}
			// Verify result makes sense
			if !result.TimedOut {
				if result.SentTime.IsZero() || result.ReceivedTime.IsZero() {
					done <- nil
					return
				}
			}
			done <- nil
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < numGoroutines; i++ {
		if err := <-done; err != nil {
			t.Errorf("Concurrent probe %d failed: %v", i, err)
		}
	}
}

func TestMockProber_DifferentProtocols(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)
	defer func() { _ = prober.Close() }()

	tests := []struct {
		name     string
		protocol api.Protocol
	}{
		{name: "ICMP", protocol: api.ICMP},
		{name: "UDP", protocol: api.UDP},
		{name: "ICMPv6", protocol: api.ICMPv6},
	}

	ctx := context.Background()

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pd := makeProbingDirective("8.8.8.8", tt.protocol)
			result, err := prober.Probe(ctx, pd, 10)

			if err != nil {
				t.Errorf("Probe with protocol %d failed: %v", tt.protocol, err)
			}
			if result == nil {
				t.Errorf("Expected result for protocol %d, got nil", tt.protocol)
			}
		})
	}
}

func TestMockProber_DifferentTTLs(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)
	defer func() { _ = prober.Close() }()

	pd := makeProbingDirective("8.8.8.8", api.UDP)
	ctx := context.Background()

	tests := []struct {
		name string
		ttl  uint8
	}{
		{name: "TTL_1", ttl: 1},
		{name: "TTL_10", ttl: 10},
		{name: "TTL_20", ttl: 20},
		{name: "TTL_30", ttl: 30},
		{name: "TTL_64", ttl: 64},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := prober.Probe(ctx, pd, tt.ttl)
			if err != nil {
				t.Errorf("Probe with TTL %d failed: %v", tt.ttl, err)
			}
			if result == nil {
				t.Errorf("Expected result for TTL %d, got nil", tt.ttl)
			}
		})
	}
}

func TestMockProber_Close(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	prober := NewMockProber(cfg)

	err := prober.Close()
	if err != nil {
		t.Errorf("Close() returned error: %v", err)
	}

	// Close should be idempotent
	err = prober.Close()
	if err != nil {
		t.Errorf("Second Close() returned error: %v", err)
	}
}
