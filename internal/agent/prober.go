// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/dioptra-io/retina-commons/model"
)

var ErrDuplicatePD = errors.New("probing directive is already in-flight")

// Prober sends network probes and returns timing information.
// Implementations must be safe for concurrent use.
type Prober interface {
	// Probe sends a network probe and blocks until one of:
	//   - a reply is received: returns (result with TimedOut=false, nil)
	//   - the implementation's internal timeout fires: returns (result with TimedOut=true, nil)
	//   - a duplicate probe is already in-flight: returns (nil, ErrDuplicateProbe)
	//   - ctx is canceled or a probe operation failure occurs: returns (nil, non-nil error)
	//
	// Implementations are responsible for enforcing their own probe timeout.
	// Probe must not block indefinitely — callers do not add a deadline to ctx.
	Probe(ctx context.Context, pd *model.ProbingDirective, ttl uint8) (*ProbeResult, error)

	// Close releases resources. Safe to call multiple times.
	Close() error
}

// ProbeResult carries the outcome of a single probe.
//
// SourceAddress is only populated on a successful (non-timed-out) probe:
// it comes from caracal's CSV output (probe_src_addr), which only exists
// for rows caracal emits when a reply is received — a genuine timeout
// produces no CSV row at all, so a timed-out ProbeResult has no source
// address available this way. model.ForwardingInfoElement requires a
// SourceAddress to send, so a PD whose near AND far probes both time out
// currently has no source to build one from — see buildFIE in agent.go.
type ProbeResult struct {
	SourceAddress net.IP
	ReplyAddress  net.IP
	SentTime      time.Time
	ReceivedTime  time.Time
	TimedOut      bool
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
