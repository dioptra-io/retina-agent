// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Package agent implements the network probing pipeline for retina-agent.
//
// The Prober interface abstracts probe execution with implementations for
// production (CaracalProber) and testing (MockProber).
package agent

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

// Prober sends network probes and returns timing information.
// Implementations must be safe for concurrent use.
type Prober interface {
	// Probe sends a network probe with the specified TTL and blocks until complete.
	// Returns ProbeResult with TimedOut=true if no reply received (not an error).
	// Returns error only for ctx cancellation or probe operation failures.
	Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error)

	// Close releases resources. Safe to call multiple times.
	Close() error
}

// ProbeResult contains the outcome and timing for a single probe.
type ProbeResult struct {
	ReplyAddress net.IP
	SentTime     time.Time
	ReceivedTime time.Time
	TimedOut     bool
}

// Success returns true if the probe received a reply.
func (r *ProbeResult) Success() bool {
	return !r.TimedOut
}

// RTT returns the round-trip time (0 if timeout).
func (r *ProbeResult) RTT() time.Duration {
	if r.TimedOut {
		return 0
	}
	return r.ReceivedTime.Sub(r.SentTime)
}

// String returns a human-readable representation.
func (r *ProbeResult) String() string {
	if r.TimedOut {
		return fmt.Sprintf("TIMEOUT (sent at %s)", r.SentTime.Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("SUCCESS from %s, RTT=%v", r.ReplyAddress, r.RTT())
}
