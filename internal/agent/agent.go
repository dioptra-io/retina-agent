// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Package agent implements a network probing agent that connects to an orchestrator
// via TCP, receives probing directives (PDs), sends network probes and collects
// replies, then returns forwarding information elements (FIEs) derived from the
// probe responses.
//
// # Architecture
//
// The agent uses a three-stage pipeline with separate goroutines:
//   - Reader: Receives ProbingDirective messages from orchestrator
//   - Processor: Sends probes, collects replies, and constructs FIEs from responses
//   - Writer: Sends ForwardingInfoElement results to orchestrator
//
// These goroutines communicate via buffered channels and are coordinated by errgroup,
// which handles error propagation and graceful shutdown.
//
// # Resiliency
//
// Reconnection is handled by the caller (see cmd/retina-agent/main.go).
// The agent respects context cancellation and shuts down gracefully without
// leaking goroutines.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
)

var ErrInvalidDirective = errors.New("invalid probing directive")

type agent struct {
	config *Config
	prober Prober
}

// Run starts the agent and blocks until context is cancelled or an error occurs.
//
// The function establishes a TCP connection to the orchestrator and spawns three
// goroutines (reader, processor, writer) that communicate via buffered channels.
// All goroutines are coordinated by errgroup for proper error propagation and
// graceful shutdown.
//
// Returns nil on clean shutdown (context cancelled), or an error if the connection
// is lost or another failure occurs.
func Run(ctx context.Context, cfg *Config) error {
	if cfg == nil {
		cfg = DefaultConfig()
	}

	prober, err := createProber(cfg)
	if err != nil {
		return fmt.Errorf("failed to create prober: %w", err)
	}
	defer func() {
		if err := prober.Close(); err != nil {
			log.Printf("failed to close prober: %v", err)
		}
	}()

	a := &agent{
		config: cfg,
		prober: prober,
	}

	conn, err := net.Dial("tcp", a.config.OrchestratorAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to orchestrator: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("failed to close connection: %v", err)
		}
	}()

	log.Printf("Agent %s: Connected to orchestrator at %s", a.config.AgentID, a.config.OrchestratorAddr)

	pds := make(chan *api.ProbingDirective, a.config.DirectivesBufferSize)
	fies := make(chan *api.ForwardingInfoElement, a.config.FIEsBufferSize)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return a.readerLoop(ctx, conn, pds) })
	g.Go(func() error { return a.processorLoop(ctx, pds, fies) })
	g.Go(func() error { return a.writerLoop(ctx, conn, fies) })

	if err := g.Wait(); err != nil && err != ctx.Err() {
		log.Printf("Agent %s: Connection terminated with error: %v", a.config.AgentID, err)
		return err
	}

	log.Printf("Agent %s: Shut down gracefully", a.config.AgentID)
	return nil
}

// readerLoop receives ProbingDirective messages from orchestrator via TCP.
// Each message is a newline-delimited JSON object. Network errors trigger
// reconnection; malformed JSON is logged and skipped to handle transient
// corruption. After MaxConsecutiveDecodeErrors consecutive failures, the
// connection is terminated (set to 0 to disable this check).
func (a *agent) readerLoop(ctx context.Context, conn net.Conn, pds chan<- *api.ProbingDirective) error {
	defer close(pds)
	decoder := json.NewDecoder(conn)
	consecutiveDecodeErrors := 0

	for {
		if err := conn.SetReadDeadline(time.Now().Add(a.config.ReadDeadline)); err != nil {
			log.Printf("Agent %s: Failed to set read deadline: %v", a.config.AgentID, err)
			return fmt.Errorf("failed to set read deadline: %w", err)
		}

		var directive api.ProbingDirective
		if err := decoder.Decode(&directive); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if isNetworkError(err) {
				return fmt.Errorf("connection lost while reading: %w", err)
			}

			consecutiveDecodeErrors++

			if a.config.MaxConsecutiveDecodeErrors > 0 {
				log.Printf("Agent %s: Failed to decode directive (attempt %d/%d, skipping): %v",
					a.config.AgentID, consecutiveDecodeErrors, a.config.MaxConsecutiveDecodeErrors, err)

				if consecutiveDecodeErrors >= a.config.MaxConsecutiveDecodeErrors {
					return fmt.Errorf("too many consecutive decode errors (%d), protocol may be broken: %w",
						consecutiveDecodeErrors, err)
				}
			} else {
				log.Printf("Agent %s: Failed to decode directive (skipping): %v", a.config.AgentID, err)
			}

			continue
		}

		consecutiveDecodeErrors = 0

		// Log directive received
		log.Printf("Agent %s: ← Directive for %s (TTL %d → %d)",
			a.config.AgentID,
			directive.DestinationAddress,
			directive.NearTTL,
			directive.NearTTL+1)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case pds <- &directive:
		}
	}
}

// processorLoop executes probes and generates FIEs.
// For each directive, spawns one goroutine that executes both probes
// (near and far TTL) in parallel and sends the FIE when both complete.
func (a *agent) processorLoop(ctx context.Context, pds <-chan *api.ProbingDirective, fies chan<- *api.ForwardingInfoElement) error {
	defer close(fies)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pd, ok := <-pds:
			if !ok {
				return nil
			}

			if err := validateDirective(pd); err != nil {
				log.Printf("Agent %s: Invalid directive: %v", a.config.AgentID, err)
				continue
			}

			// Launch a single goroutine that handles both probes
			go a.processDirective(ctx, pd, fies)
		}
	}
}

// writerLoop sends ForwardingInfoElement results to orchestrator via TCP.
// Each result is encoded as a newline-delimited JSON object. Network errors
// trigger reconnection; encoding errors indicate a bug and also terminate
// the connection.
func (a *agent) writerLoop(ctx context.Context, conn net.Conn, fies <-chan *api.ForwardingInfoElement) error {
	encoder := json.NewEncoder(conn)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fie, ok := <-fies:
			if !ok {
				return nil
			}

			if err := conn.SetWriteDeadline(time.Now().Add(a.config.WriteDeadline)); err != nil {
				log.Printf("Agent %s: Failed to set write deadline: %v", a.config.AgentID, err)
				return fmt.Errorf("failed to set write deadline: %w", err)
			}

			if err := encoder.Encode(fie); err != nil {
				if isNetworkError(err) {
					return fmt.Errorf("connection lost while writing: %w", err)
				}
				return fmt.Errorf("failed to encode FIE: %w", err)
			}

			// Log FIE sent
			nearRTT := fie.NearInfo.ReceivedTimestamp.Sub(fie.NearInfo.SentTimestamp)
			farRTT := fie.FarInfo.ReceivedTimestamp.Sub(fie.FarInfo.SentTimestamp)
			log.Printf("Agent %s: → FIE for %s | Near(TTL%d): %v | Far(TTL%d): %v",
				a.config.AgentID,
				fie.DestinationAddress,
				fie.NearInfo.ProbeTTL,
				nearRTT,
				fie.FarInfo.ProbeTTL,
				farRTT)
		}
	}
}

// processDirective executes near and far probes for a directive and sends the FIE
func (a *agent) processDirective(ctx context.Context, pd *api.ProbingDirective, fies chan<- *api.ForwardingInfoElement) {
	type result struct {
		probe *ProbeResult
		err   error
	}

	nearCh := make(chan result, 1)
	farCh := make(chan result, 1)

	// Launch both probes in parallel
	go func() {
		probe, err := a.prober.Probe(ctx, pd, pd.NearTTL)
		nearCh <- result{probe, err}
	}()

	go func() {
		probe, err := a.prober.Probe(ctx, pd, pd.NearTTL+1)
		farCh <- result{probe, err}
	}()

	// Wait for both results
	nearRes := <-nearCh
	farRes := <-farCh

	// Handle errors
	if nearRes.err != nil {
		log.Printf("Agent %s: Near probe failed for %s (TTL %d): %v",
			a.config.AgentID, pd.DestinationAddress, pd.NearTTL, nearRes.err)
		return
	}
	if farRes.err != nil {
		log.Printf("Agent %s: Far probe failed for %s (TTL %d): %v",
			a.config.AgentID, pd.DestinationAddress, pd.NearTTL+1, farRes.err)
		return
	}

	// Skip if either timed out (expected, no logging needed)
	if nearRes.probe.TimedOut || farRes.probe.TimedOut {
		return
	}

	// Build and send FIE
	fie := &api.ForwardingInfoElement{
		Agent: api.Agent{
			AgentID: a.config.AgentID,
		},
		IPVersion:           pd.IPVersion,
		Protocol:            pd.Protocol,
		DestinationAddress:  pd.DestinationAddress,
		NearInfo:            probeResultToInfo(nearRes.probe, pd.NearTTL),
		FarInfo:             probeResultToInfo(farRes.probe, pd.NearTTL+1),
		ProductionTimestamp: time.Now().UTC(),
	}

	select {
	case fies <- fie:
	case <-ctx.Done():
	}
}

// createProber instantiates a prober based on cfg.ProberType.
//
// To add a new prober type:
//  1. Implement the Prober interface
//  2. Add a case to this switch
//  3. Update ProberType documentation in config.go
func createProber(cfg *Config) (Prober, error) {
	switch cfg.ProberType {
	case "mock":
		return NewMockProber(cfg), nil
	case "caracal":
		return NewCaracalProber(cfg)
	default:
		return nil, fmt.Errorf("unknown prober type: %q (valid: mock, caracal)", cfg.ProberType)
	}
}

// validateDirective checks that a directive has all required fields and
// protocol-specific headers.
func validateDirective(pd *api.ProbingDirective) error {
	if pd == nil {
		return fmt.Errorf("%w: directive is nil", ErrInvalidDirective)
	}
	if pd.AgentID == "" {
		return fmt.Errorf("%w: agent ID is empty", ErrInvalidDirective)
	}
	if pd.DestinationAddress == nil {
		return fmt.Errorf("%w: destination address is nil", ErrInvalidDirective)
	}
	if pd.NearTTL == 0 {
		return fmt.Errorf("%w: TTL cannot be zero", ErrInvalidDirective)
	}

	switch pd.Protocol {
	case api.ICMP, api.ICMPv6:
		if pd.NextHeader.ICMPNextHeader == nil && pd.NextHeader.ICMPv6NextHeader == nil {
			return fmt.Errorf("%w: ICMP directive missing next header", ErrInvalidDirective)
		}
	case api.UDP:
		if pd.NextHeader.UDPNextHeader == nil {
			return fmt.Errorf("%w: UDP directive missing next header", ErrInvalidDirective)
		}
	default:
		return fmt.Errorf("%w: unsupported protocol %d", ErrInvalidDirective, pd.Protocol)
	}

	return nil
}

// probeResultToInfo converts a ProbeResult into an Info structure for FIE generation.
func probeResultToInfo(result *ProbeResult, ttl uint8) api.Info {
	return api.Info{
		ProbeTTL:          ttl,
		ReplyAddress:      result.ReplyAddress,
		SentTimestamp:     result.SentTime,
		ReceivedTimestamp: result.ReceivedTime,
	}
}

// isNetworkError returns true if err indicates a network/connection failure
// (EOF, timeout, connection reset) rather than a JSON decoding error.
//
// Network errors should trigger reconnection, while decoding errors should
// be logged and skipped.
func isNetworkError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
