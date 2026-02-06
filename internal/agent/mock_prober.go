// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// MockProber provides a simple network probe simulator for testing the agent
// pipeline without requiring real network access or the caracal prober.
// It simulates realistic timing and occasional timeouts for comprehensive
// testing of directive processing, probe correlation, and FIE generation.

package agent

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

// MockProber simulates network probing for testing without sending real packets.
// Not intended for production use.
//
// Thread-safe for concurrent use by multiple goroutines.
type MockProber struct {
	rng    *rand.Rand
	mu     sync.Mutex // protects rng
	config *Config
}

// NewMockProber creates a new MockProber for testing.
func NewMockProber(cfg *Config) *MockProber {
	return &MockProber{
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())), // #nosec G404 -- crypto/rand not needed for mock prober testing
		config: cfg,
	}
}

// Probe simulates sending a network probe with artificial delay and random outcomes.
//
// Simulates a 10-100ms network delay. Returns a timeout 10% of the time.
// When successful, returns a reply from the destination address.
//
// Respects context cancellation during the simulated delay.
func (m *MockProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	sentTime := time.Now() // Record when "probe sent"

	// Generate random values under lock (rand.Rand is not thread-safe)
	m.mu.Lock()
	delay := time.Duration(10+m.rng.Intn(90)) * time.Millisecond
	shouldTimeout := m.rng.Float32() < 0.1
	m.mu.Unlock()

	// Simulate network delay
	select {
	case <-time.After(delay):
		// Continue after delay
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Simulate 10% timeout rate
	if shouldTimeout {
		return &ProbeResult{
			TimedOut: true,
			SentTime: sentTime,
		}, nil
	}

	// Generate fake successful reply
	return &ProbeResult{
		ReplyAddress: pd.DestinationAddress,
		SentTime:     sentTime,
		ReceivedTime: time.Now(),
		TimedOut:     false,
	}, nil
}

// Close releases resources. No-op for MockProber.
func (m *MockProber) Close() error {
	return nil
}
