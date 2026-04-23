// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

var ErrDuplicatePD = errors.New("probing directive is already in-flight")

// Prober sends network probes and returns timing information.
// Implementations must be safe for concurrent use.
type Prober interface {
	// Probe sends a network probe and blocks until one of:
	//   - a reply is received: returns (result with TimedOut=false, nil)
	//   - the implementation's internal timeout fires: returns (result with TimedOut=true, nil)
	//   - a duplicate probe is already in-flight: returns (nil, ErrDuplicateProbe)
	//   - ctx is cancelled or a probe operation failure occurs: returns (nil, non-nil error)
	//
	// Implementations are responsible for enforcing their own probe timeout.
	// Probe must not block indefinitely — callers do not add a deadline to ctx.
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
