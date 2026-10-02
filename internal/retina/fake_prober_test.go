// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"context"
	"net/netip"
	"time"
)

// fakeTick is how often the fake prober looks for PDs whose delay has passed.
const fakeTick = 10 * time.Millisecond

var (
	// fakeNear and fakeFar are the reply addresses of every fake FIE.
	fakeNear = netip.MustParseAddr("192.0.2.1")
	fakeFar  = netip.MustParseAddr("192.0.2.2")
)

// fakeProberConfig configures the fake prober.
type fakeProberConfig struct {
	// Delay is the time between taking a PD and producing its FIE, rounded
	// up to the next tick of 10 ms.
	Delay time.Duration
	// MaxInflight is how many PDs may be waiting for their delay at once.
	MaxInflight int
	// NoReply makes every FIE report that neither probe was answered.
	NoReply bool
}

// fakeProber is a prober that sends nothing: it answers every PD with a made
// up FIE after a fixed delay. It stands in for the caracal prober in the
// tests of the agent, which need no caracal process.
type fakeProber struct {
	config *fakeProberConfig
}

// newFakeProber creates a fake prober.
func newFakeProber(config *fakeProberConfig) *fakeProber {
	return &fakeProber{config: config}
}

// fakePending is a PD waiting for its delay to pass.
type fakePending struct {
	pd  PD
	due time.Time
}

// Run implements Prober.
func (p *fakeProber) Run(ctx context.Context, pds <-chan PD, fies chan<- FIE) error {
	// The pending PDs, in a ring. The delay is the same for all of them, so
	// they are due in the order they arrived.
	pending := make([]fakePending, p.config.MaxInflight)
	head, count := 0, 0

	ticker := time.NewTicker(fakeTick)
	defer ticker.Stop()

	for {
		// A nil channel is never ready: no PD is taken while at the limit.
		in := pds
		if count == len(pending) {
			in = nil
		}

		select {
		case pd := <-in:
			pending[(head+count)%len(pending)] = fakePending{pd: pd, due: time.Now().Add(p.config.Delay)}
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
func (p *fakeProber) fie(pd PD, now time.Time) FIE {
	fie := FIE{PDID: pd.ID, CaptureUnix: now.Unix()}
	if !p.config.NoReply {
		fie.Near, fie.Far = fakeNear, fakeFar
	}
	return fie
}
