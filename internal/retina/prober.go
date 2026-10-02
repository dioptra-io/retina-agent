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

	// MaxPDRate is the most PDs per second the agent takes from the
	// orchestrator and probes. PDs that come faster are left with the
	// orchestrator, which is slowed down, so that no PD waits to be sent
	// while its probe timeout runs. After a pause, up to a tenth of a second
	// of PDs are taken at once.
	MaxPDRate int `json:"max_pd_rate"`
	// MaxInFlightPDs is the most PDs the agent holds at once, from when a PD
	// is received to when its FIE is written to the orchestrator. At the
	// limit the agent stops receiving PDs, which slows the orchestrator
	// down: this is what bounds the agent's memory when the orchestrator
	// reads FIEs slower than it sends PDs. Zero means no limit.
	MaxInFlightPDs int `json:"max_in_flight_pds"`

	// Caracal configures the caracal prober.
	Caracal CaracalProberConfig `json:"caracal"`
}

func (c *ProberConfig) validate() error {
	if c.PDQueueSize < 0 {
		return fmt.Errorf("PD queue size cannot be negative: got %d", c.PDQueueSize)
	}
	if c.FIEQueueSize < 0 {
		return fmt.Errorf("FIE queue size cannot be negative: got %d", c.FIEQueueSize)
	}
	if c.MaxPDRate < 1 {
		return fmt.Errorf("max PD rate must be at least 1: got %d", c.MaxPDRate)
	}
	if c.MaxInFlightPDs < 0 {
		return fmt.Errorf("max in-flight PDs cannot be negative: got %d", c.MaxInFlightPDs)
	}
	if err := c.Caracal.validate(); err != nil {
		return fmt.Errorf("caracal: %w", err)
	}
	return nil
}

// Prober turns PDs into FIEs.
type Prober interface {
	// Run takes PDs from pds, probes, and writes one FIE per PD to fies,
	// until ctx is done or the prober fails. It outlives the connections to
	// the orchestrator: neither channel is ever closed.
	//
	// The agent bounds the rate of the PDs and the number in flight. A PD
	// that gets no FIE is never counted out of those in flight, so every PD
	// taken must get its FIE.
	Run(ctx context.Context, pds <-chan PD, fies chan<- FIE) error
}

// statsProber is a prober that has counters for the agent's stats log line.
type statsProber interface {
	// stats returns the counters, as totals since the prober started.
	stats() []slog.Attr
}
