// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Package agent provides network probing interfaces and types.
package agent

import (
	"context"
	"net"
	"time"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
)

// Prober sends network probes and returns timing information.
//
// Implementations must be safe for concurrent use by multiple goroutines.
//
// Example usage:
//
//	prober, err := NewCaracalProber(cfg)
//	if err != nil {
//	    return err
//	}
//	defer prober.Close()
//
//	result, err := prober.Probe(ctx, directive, 10)
//	if err != nil {
//	    return fmt.Errorf("probe failed: %w", err)
//	}
//	if result.TimedOut {
//	    log.Printf("Probe timed out")
//	} else {
//	    rtt := result.ReceivedTime.Sub(result.SentTime)
//	    log.Printf("Reply from %s, RTT: %v", result.ReplyAddress, rtt)
//	}
type Prober interface {
	// Probe sends a network probe with the specified TTL and blocks until complete.
	//
	// Returns a ProbeResult indicating success or timeout. A timeout (no reply within
	// the configured probe timeout) is not an error - it returns a ProbeResult with
	// TimedOut=true and error=nil.
	//
	// Returns an error if:
	//   - ctx is cancelled (returns ctx.Err())
	//   - probe operation fails (e.g., permission denied, invalid parameters)
	//   - prober is closed or not properly initialized
	//
	// The method respects context cancellation and returns immediately if ctx is done.
	Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error)

	// Close releases resources held by the prober.
	// Should be called when done using the prober, typically via defer.
	// Safe to call multiple times.
	Close() error
}

// ProbeResult contains the outcome and timing information for a single probe.
type ProbeResult struct {
	// ReplyAddress is the IP address that sent the reply packet.
	// Nil if the probe timed out (no reply received).
	ReplyAddress net.IP

	// SentTime is when the probe packet was sent.
	SentTime time.Time

	// ReceivedTime is when the reply packet was received.
	// Zero value if the probe timed out.
	ReceivedTime time.Time

	// TimedOut indicates whether the probe timed out (no reply received).
	// When true, ReplyAddress will be nil.
	TimedOut bool
}
