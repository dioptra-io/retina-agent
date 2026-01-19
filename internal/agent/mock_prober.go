// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
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
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
		config: cfg,
	}
}

// Probe simulates sending a network probe with artificial delay and random outcomes.
//
// Returns a timeout 10% of the time, otherwise returns a successful probe result
// after a simulated delay of 10-100ms. Respects context cancellation.
//
// This method blocks as required by the Prober interface.
func (m *MockProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
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
			SentTime: time.Now().Add(-delay),
		}, nil
	}

	// Generate fake successful reply
	now := time.Now()
	return &ProbeResult{
		ReplyAddress: pd.DestinationAddress,
		SentTime:     now.Add(-delay),
		ReceivedTime: now,
		TimedOut:     false,
	}, nil
}

// Close releases resources. No-op for MockProber.
func (m *MockProber) Close() error {
	return nil
}
