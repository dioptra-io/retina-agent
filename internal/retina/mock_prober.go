// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// mockTick is how often the mock prober looks for PDs whose delay has passed.
const mockTick = 10 * time.Millisecond

var (
	// mockNear and mockFar are the reply addresses of every mock FIE.
	mockNear = netip.MustParseAddr("192.0.2.1")
	mockFar  = netip.MustParseAddr("192.0.2.2")
)

// MockProberConfig configures the mock prober.
type MockProberConfig struct {
	// Delay is the time between taking a PD and producing its FIE, rounded
	// up to the next tick of 10 ms.
	Delay time.Duration `json:"delay"`
	// MaxInflight is how many PDs may be waiting for their delay at once.
	MaxInflight int `json:"max_inflight"`
	// NoReply makes every FIE report that neither probe was answered.
	NoReply bool `json:"no_reply"`
}

func (c *MockProberConfig) validate() error {
	if c.Delay < 0 {
		return fmt.Errorf("delay cannot be negative: got %v", c.Delay)
	}
	if c.MaxInflight < 1 {
		return fmt.Errorf("max inflight must be at least 1: got %d", c.MaxInflight)
	}
	return nil
}

// MockProber is a prober that sends nothing: it answers every PD with a made
// up FIE after a fixed delay.
type MockProber struct {
	config *MockProberConfig
}

// NewMockProber creates a mock prober.
func NewMockProber(config *MockProberConfig) *MockProber {
	return &MockProber{config: config}
}

// mockPending is a PD waiting for its delay to pass.
type mockPending struct {
	pd  PD
	due time.Time
}

// Run implements Prober.
func (p *MockProber) Run(ctx context.Context, pds <-chan PD, fies chan<- FIE) error {
	// The pending PDs, in a ring. The delay is the same for all of them, so
	// they are due in the order they arrived.
	pending := make([]mockPending, p.config.MaxInflight)
	head, count := 0, 0

	ticker := time.NewTicker(mockTick)
	defer ticker.Stop()

	for {
		// A nil channel is never ready: no PD is taken while at the limit.
		in := pds
		if count == len(pending) {
			in = nil
		}

		select {
		case pd := <-in:
			pending[(head+count)%len(pending)] = mockPending{pd: pd, due: time.Now().Add(p.config.Delay)}
			count++
		case now := <-ticker.C:
			for count > 0 && !pending[head].due.After(now) {
				select {
				case fies <- p.fie(pending[head].pd, now):
				case <-ctx.Done():
					return nil
				}
				head = (head + 1) % len(pending)
				count--
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// fie makes up the FIE of a PD.
func (p *MockProber) fie(pd PD, now time.Time) FIE {
	fie := FIE{PDID: pd.ID, CaptureUnix: now.Unix()}
	if !p.config.NoReply {
		fie.Near, fie.Far = mockNear, mockFar
	}
	return fie
}
