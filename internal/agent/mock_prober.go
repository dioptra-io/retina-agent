// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/dioptra-io/retina-commons/model"
)

// MockProber simulates network probing for testing without sending real packets.
//
// Thread-safe for concurrent use by multiple goroutines.
type MockProber struct {
	rng    *rand.Rand
	mu     sync.Mutex // protects rng
	config *Config
}

var _ (Prober) = (*MockProber)(nil)

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
func (m *MockProber) Probe(ctx context.Context, pd *model.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	sentTime := time.Now()

	m.mu.Lock()
	delay := time.Duration(10+m.rng.Intn(90)) * time.Millisecond
	shouldTimeout := m.rng.Float32() < 0.1
	m.mu.Unlock()

	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if shouldTimeout {
		return &ProbeResult{
			TimedOut: true,
			SentTime: sentTime,
		}, nil
	}

	return &ProbeResult{
		ReplyAddress: pd.DestinationAddress,
		SentTime:     sentTime,
		ReceivedTime: time.Now(),
		TimedOut:     false,
	}, nil
}

// Close is a no-op for MockProber.
func (m *MockProber) Close() error {
	return nil
}
