// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

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
	// Probe blocks until a reply is received or the probe times out.
	// TimedOut=true in the result is not an error — only ctx cancellation
	// or probe operation failures return a non-nil error.
	Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error)

	// Close releases resources. Safe to call multiple times.
	Close() error
}

type ProbeResult struct {
	ReplyAddress net.IP
	SentTime     time.Time
	ReceivedTime time.Time
	TimedOut     bool
}

func (r *ProbeResult) Success() bool {
	return !r.TimedOut
}

// RTT returns the round-trip time, or 0 on timeout.
func (r *ProbeResult) RTT() time.Duration {
	if r.TimedOut {
		return 0
	}
	return r.ReceivedTime.Sub(r.SentTime)
}

func (r *ProbeResult) String() string {
	if r.TimedOut {
		return fmt.Sprintf("TIMEOUT (sent at %s)", r.SentTime.Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("SUCCESS from %s, RTT=%v", r.ReplyAddress, r.RTT())
}
