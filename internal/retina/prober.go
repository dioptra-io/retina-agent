// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"context"
	"fmt"
	"log/slog"
)

// ProberConfig configures the prober and the queues between it and the
// orchestrator connection.
type ProberConfig struct {
	// PDQueueSize is how many received PDs may wait for the prober. The
	// orchestrator is slowed down, not dropped, while the queue is full.
	PDQueueSize int `json:"pd_queue_size"`
	// FIEQueueSize is how many FIEs may wait to be sent to the orchestrator.
	// The prober pauses while the queue is full.
	FIEQueueSize int `json:"fie_queue_size"`
	// DiscardQueuedOnDisconnect empties both queues when the connection to the
	// orchestrator is lost, and again when the next one is made. When false,
	// what the queues hold is handled on the next connection.
	//
	// PDs the prober has already taken are not discarded: their FIEs are sent
	// if they are produced after the next connection is made.
	DiscardQueuedOnDisconnect bool `json:"discard_queued_on_disconnect"`

	// Caracal configures the caracal prober. When it is nil the mock prober
	// is used, with the Mock configuration.
	Caracal *CaracalProberConfig `json:"caracal"`
	Mock    MockProberConfig     `json:"mock"`
}

func (c *ProberConfig) validate() error {
	if c.PDQueueSize < 0 {
		return fmt.Errorf("PD queue size cannot be negative: got %d", c.PDQueueSize)
	}
	if c.FIEQueueSize < 0 {
		return fmt.Errorf("FIE queue size cannot be negative: got %d", c.FIEQueueSize)
	}
	if c.Caracal != nil {
		if err := c.Caracal.validate(); err != nil {
			return fmt.Errorf("caracal: %w", err)
		}
		return nil
	}
	if err := c.Mock.validate(); err != nil {
		return fmt.Errorf("mock: %w", err)
	}
	return nil
}

// Prober turns PDs into FIEs.
type Prober interface {
	// Run takes PDs from pds, probes, and writes one FIE per PD to fies,
	// until ctx is done or the prober fails. It outlives the connections to
	// the orchestrator: neither channel is ever closed.
	//
	// The prober bounds its own PDs in flight. While it is at its limit, or
	// while fies is full, it stops taking from pds.
	Run(ctx context.Context, pds <-chan PD, fies chan<- FIE) error
}

// statsProber is a prober that has counters for the agent's stats log line.
type statsProber interface {
	// stats returns the counters, as totals since the prober started.
	stats() []slog.Attr
}
