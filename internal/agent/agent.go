// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Package agent implements a network probing agent that connects to an orchestrator
// via TCP, receives probing directives, executes network probes, and returns
// forwarding information elements.
//
// # Architecture
//
// The agent uses a three-stage pipeline with separate goroutines:
//   - Reader: Receives ProbingDirective messages from orchestrator
//   - Processor: Executes network probes based on directives
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
	"sync"
	"sync/atomic"
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
	defer prober.Close()

	a := &agent{
		config: cfg,
		prober: prober,
	}

	conn, err := net.Dial("tcp", a.config.OrchestratorAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	log.Printf("Agent %s: Connected to orchestrator at %s", a.config.AgentID, a.config.OrchestratorAddr)

	directives := make(chan *api.ProbingDirective, a.config.DirectivesBufferSize)
	fies := make(chan *api.ForwardingInfoElement, a.config.FIEsBufferSize)

	g, ctx := errgroup.WithContext(ctx)

	// Reader: receives ProbingDirective messages from orchestrator via TCP.
	// Each message is a newline-delimited JSON object. Network errors trigger
	// reconnection; malformed JSON is logged and skipped to handle transient
	// corruption. After MaxConsecutiveDecodeErrors consecutive failures, the
	// connection is terminated (set to 0 to disable this check).
	g.Go(func() error {
		defer close(directives)
		decoder := json.NewDecoder(conn)
		consecutiveDecodeErrors := 0

		for {
			conn.SetReadDeadline(time.Now().Add(a.config.ReadDeadline))

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
			case directives <- &directive:
			}
		}
	})

	// Processor: execute probes and generate FIEs using map-based correlation.
	// For each directive, two probes are launched concurrently (near and far TTL).
	// Results are correlated by directive ID and assembled into FIEs when both complete.
	g.Go(func() error {
		defer close(fies)

		// directiveState tracks probe results for a single directive
		type directiveState struct {
			pd         *api.ProbingDirective
			nearResult *ProbeResult
			farResult  *ProbeResult
		}

		pending := make(map[uint64]*directiveState)
		var mu sync.Mutex

		// completion represents a probe result ready for correlation
		type completion struct {
			id     uint64
			isNear bool
			result *ProbeResult
			err    error
		}
		completions := make(chan *completion, 200)

		// Completion handler: correlates probe results and builds FIEs
		go func() {
			for c := range completions {
				var fieToSend *api.ForwardingInfoElement

				mu.Lock()
				state := pending[c.id]
				if state != nil {
					// Handle errors
					if c.err != nil {
						if c.isNear {
							log.Printf("Agent %s: Near probe failed for destination %s (TTL %d): %v",
								a.config.AgentID, state.pd.DestinationAddress, state.pd.NearTTL, c.err)
						} else {
							log.Printf("Agent %s: Far probe failed for destination %s (TTL %d): %v",
								a.config.AgentID, state.pd.DestinationAddress, state.pd.NearTTL+1, c.err)
						}
						delete(pending, c.id)
						mu.Unlock()
						continue
					}

					// Update result
					if c.isNear {
						state.nearResult = c.result
					} else {
						state.farResult = c.result
					}

					// Both probes complete?
					if state.nearResult != nil && state.farResult != nil {
						// Both succeeded (no timeouts)?
						if !state.nearResult.TimedOut && !state.farResult.TimedOut {
							fieToSend = &api.ForwardingInfoElement{
								Agent: api.Agent{
									AgentID: a.config.AgentID,
								},
								IPVersion:           state.pd.IPVersion,
								Protocol:            state.pd.Protocol,
								DestinationAddress:  state.pd.DestinationAddress,
								NearInfo:            probeResultToInfo(state.nearResult, state.pd.NearTTL),
								FarInfo:             probeResultToInfo(state.farResult, state.pd.NearTTL+1),
								ProductionTimestamp: time.Now().UTC(),
							}
						}
						// No logging for timeouts - they're expected
						delete(pending, c.id)
					}
				}
				mu.Unlock()

				// Send FIE outside lock
				if fieToSend != nil {
					select {
					case fies <- fieToSend:
					case <-ctx.Done():
						return
					}
				}
			}
		}()

		var counter uint64

		// Main loop: receive directives and launch probes
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case pd, ok := <-directives:
				if !ok {
					return nil
				}

				if err := validateDirective(pd); err != nil {
					log.Printf("Agent %s: Invalid directive: %v", a.config.AgentID, err)
					continue
				}

				id := atomic.AddUint64(&counter, 1)

				mu.Lock()
				pending[id] = &directiveState{pd: pd}
				mu.Unlock()

				// Launch near probe
				go func(probeID uint64, directive *api.ProbingDirective) {
					result, err := a.prober.Probe(ctx, directive, directive.NearTTL)
					completions <- &completion{
						id:     probeID,
						isNear: true,
						result: result,
						err:    err,
					}
				}(id, pd)

				// Launch far probe
				go func(probeID uint64, directive *api.ProbingDirective) {
					result, err := a.prober.Probe(ctx, directive, directive.NearTTL+1)
					completions <- &completion{
						id:     probeID,
						isNear: false,
						result: result,
						err:    err,
					}
				}(id, pd)
			}
		}
	})

	// Writer: sends ForwardingInfoElement results to orchestrator via TCP.
	// Each result is encoded as a newline-delimited JSON object. Network errors
	// trigger reconnection; encoding errors indicate a bug and also terminate
	// the connection.
	g.Go(func() error {
		encoder := json.NewEncoder(conn)

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case fie, ok := <-fies:
				if !ok {
					return nil
				}

				conn.SetWriteDeadline(time.Now().Add(a.config.WriteDeadline))

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
	})

	if err := g.Wait(); err != nil && err != ctx.Err() {
		log.Printf("Agent %s: Connection terminated with error: %v", a.config.AgentID, err)
		return err
	}

	log.Printf("Agent %s: Shut down gracefully", a.config.AgentID)
	return nil
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
	if errors.As(err, &netErr) {
		return true
	}
	return false
}
