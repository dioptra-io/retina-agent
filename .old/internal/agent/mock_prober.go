// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

// MockProber simulates network probing for testing without sending real packets.
//
// Thread-safe for concurrent use by multiple goroutines.
type MockProber struct {
	rng      *rand.Rand
	mu       sync.Mutex // protects rng and nextSend
	nextSend time.Time  // earliest send time of the next probe when MockProbingRate is set
	config   *Config
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
// With MockProbingRate set, probes are sent one at a time at that rate and a
// probe first waits for its turn, like a probe queued behind caracal's rate
// limit. With MockAlwaysTimeout set, every probe waits the full ProbeTimeout
// and reports a timeout, which is the slowest a real probe can be.
//
// Respects context cancellation during the simulated delay.
func (m *MockProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	m.mu.Lock()
	var sendWait time.Duration
	if m.config.MockProbingRate > 0 {
		now := time.Now()
		if m.nextSend.Before(now) {
			m.nextSend = now
		}
		sendWait = m.nextSend.Sub(now)
		m.nextSend = m.nextSend.Add(time.Second / time.Duration(m.config.MockProbingRate))
	}
	delay := time.Duration(10+m.rng.Intn(90)) * time.Millisecond
	shouldTimeout := m.rng.Float32() < 0.1
	m.mu.Unlock()

	if m.config.MockAlwaysTimeout {
		delay = m.config.ProbeTimeout
		shouldTimeout = true
	}

	if sendWait > 0 {
		if err := sleepContext(ctx, sendWait); err != nil {
			return nil, err
		}
	}
	sentTime := time.Now()

	if err := sleepContext(ctx, delay); err != nil {
		return nil, err
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

// sleepContext waits for d, or returns the context's error if it ends first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close is a no-op for MockProber.
func (m *MockProber) Close() error {
	return nil
}
