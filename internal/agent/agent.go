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
	"log/slog"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/dioptra-io/retina-commons/api/v1"
)

var ErrInvalidDirective = errors.New("invalid probing directive")

type agent struct {
	config *Config
	prober Prober
	logger *slog.Logger
}

// probeResult wraps a probe result with its error for channel communication.
type probeResult struct {
	result *ProbeResult
	err    error
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
func Run(ctx context.Context, cfg *Config, logger *slog.Logger) error {
	if cfg == nil {
		cfg = DefaultConfig()
	}

	prober, err := createProber(cfg)
	if err != nil {
		return fmt.Errorf("failed to create prober: %w", err)
	}
	defer func() {
		if err := prober.Close(); err != nil {
			logger.Error("Failed to close prober", slog.Any("err", err))
		}
	}()

	a := &agent{
		config: cfg,
		prober: prober,
		logger: logger.With(slog.String("agent_id", cfg.AgentID)),
	}

	conn, err := net.Dial("tcp", a.config.OrchestratorAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to orchestrator: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			a.logger.Error("Failed to close connection", slog.Any("err", err))
		}
	}()

	a.logger.Info("Connected to orchestrator",
		slog.String("address", a.config.OrchestratorAddr))

	// Authenticate if secret is configured
	if a.config.Secret != "" {
		if err := a.authenticate(conn); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}
		a.logger.Info("Authentication enabled — authenticated successfully")
	} else {
		a.logger.Warn("Authentication disabled — not recommended for production")
	}

	pds := make(chan *api.ProbingDirective, a.config.PDsBufferSize)
	fies := make(chan *api.ForwardingInfoElement, a.config.FIEsBufferSize)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return a.readerLoop(ctx, conn, pds) })
	g.Go(func() error { return a.processorLoop(ctx, pds, fies) })
	g.Go(func() error { return a.writerLoop(ctx, conn, fies) })

	if err := g.Wait(); err != nil && err != ctx.Err() {
		a.logger.Error("Connection terminated", slog.Any("err", err))
		return err
	}

	a.logger.Info("Shut down gracefully")
	return nil
}

// authenticate sends authentication request and waits for orchestrator's response.
// This must be called immediately after connection, before any other messages.
// Returns error if authentication fails or times out.
func (a *agent) authenticate(conn net.Conn) error {
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	authReq := &api.AuthRequest{
		AgentID: a.config.AgentID,
		Secret:  a.config.Secret,
	}

	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("failed to set write deadline: %w", err)
	}

	if err := encoder.Encode(authReq); err != nil {
		return fmt.Errorf("failed to send auth request: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("failed to set read deadline: %w", err)
	}

	var authResp api.AuthResponse
	if err := decoder.Decode(&authResp); err != nil {
		return fmt.Errorf("failed to receive auth response: %w", err)
	}

	if !authResp.Authenticated {
		return fmt.Errorf("authentication rejected: %s", authResp.Message)
	}

	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})

	return nil
}

// readerLoop receives and validates ProbingDirective messages from orchestrator via TCP.
// Each message is a newline-delimited JSON object. Read timeouts are logged and retried.
// Network errors trigger reconnection; malformed JSON is logged and skipped to handle
// transient corruption. After MaxConsecutiveDecodeErrors consecutive failures, the
// connection is terminated (set to 0 to disable this check). Invalid directives are
// logged and skipped without terminating the connection.
func (a *agent) readerLoop(ctx context.Context, conn net.Conn, pds chan<- *api.ProbingDirective) error {
	defer close(pds)
	decoder := json.NewDecoder(conn)
	consecutiveDecodeErrors := 0

	for {
		if err := conn.SetReadDeadline(time.Now().Add(a.config.ReadDeadline)); err != nil {
			return fmt.Errorf("failed to set read deadline: %w", err)
		}

		var pd api.ProbingDirective
		if err := decoder.Decode(&pd); err != nil {
			shouldContinue, newCount, handledErr := a.handleDecodeError(ctx, err, consecutiveDecodeErrors)
			consecutiveDecodeErrors = newCount
			if !shouldContinue {
				return handledErr
			}
			continue
		}

		consecutiveDecodeErrors = 0

		if err := validatePD(&pd); err != nil {
			a.logger.Warn("Invalid directive", slog.Any("err", err))
			continue
		}

		a.logger.Debug("← PD received",
			slog.Uint64("pd_id", pd.ProbingDirectiveID),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("near_ttl", int(pd.NearTTL)))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case pds <- &pd:
		}
	}
}

// handleDecodeError processes JSON decode errors during directive reception.
// Returns (shouldContinue, newErrorCount, wrappedError) where shouldContinue indicates
// whether to retry reading, newErrorCount is the updated consecutive error count,
// and wrappedError is the error to return if shouldContinue is false.
func (a *agent) handleDecodeError(ctx context.Context, err error, consecutiveErrors int) (bool, int, error) {
	// Check for read timeout (expected, just retry)
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		a.logger.Debug("Read timeout, no data received")
		return true, consecutiveErrors, nil
	}

	// Check for context cancellation (clean shutdown)
	if ctx.Err() != nil {
		return false, consecutiveErrors, ctx.Err()
	}

	// Check for network errors (trigger reconnection)
	if isNetworkError(err) {
		return false, consecutiveErrors, fmt.Errorf("connection lost while reading: %w", err)
	}

	// Malformed JSON — log and potentially skip
	consecutiveErrors++

	if a.config.MaxConsecutiveDecodeErrors > 0 {
		a.logger.Warn("Failed to decode directive",
			slog.Int("attempt", consecutiveErrors),
			slog.Int("max", a.config.MaxConsecutiveDecodeErrors),
			slog.Any("err", err))

		if consecutiveErrors >= a.config.MaxConsecutiveDecodeErrors {
			return false, consecutiveErrors, fmt.Errorf("too many consecutive decode errors (%d), protocol may be broken: %w",
				consecutiveErrors, err)
		}
	} else {
		a.logger.Warn("Failed to decode directive (skipping)", slog.Any("err", err))
	}

	return true, consecutiveErrors, nil
}

// processorLoop receives directives and dispatches them for processing.
// For each directive, spawns a goroutine running processPD to execute probes.
// Exits when the directive channel closes or context is cancelled.
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
			go a.processPD(ctx, pd, fies)
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
				return fmt.Errorf("failed to set write deadline: %w", err)
			}

			if err := encoder.Encode(fie); err != nil {
				if isNetworkError(err) {
					return fmt.Errorf("connection lost while writing: %w", err)
				}
				return fmt.Errorf("failed to encode FIE: %w", err)
			}

			if fie.NearInfo == nil || fie.FarInfo == nil {
				a.logger.Debug("→ FIE sent (no probe response)",
					slog.Uint64("pd_id", fie.ProbingDirectiveID),
					slog.String("dest", fie.DestinationAddress.String()))
			} else {
				a.logger.Debug("→ FIE sent",
					slog.Uint64("pd_id", fie.ProbingDirectiveID),
					slog.String("dest", fie.DestinationAddress.String()),
					slog.Int("near_ttl", int(fie.NearInfo.ProbeTTL)),
					slog.Int("far_ttl", int(fie.FarInfo.ProbeTTL)))
			}
		}
	}
}

// processPD executes near and far probes in parallel and sends an FIE regardless of timeouts.
// Failed probes are logged and cause the FIE to be dropped.
// Timed-out probes result in a nil NearInfo or FarInfo in the FIE.
func (a *agent) processPD(ctx context.Context, pd *api.ProbingDirective, fies chan<- *api.ForwardingInfoElement) {
	nearTTL := pd.NearTTL
	farTTL := pd.NearTTL + 1
	nearCh := make(chan probeResult, 1)
	farCh := make(chan probeResult, 1)

	probe := func(ttl uint8, ch chan<- probeResult) {
		result, err := a.prober.Probe(ctx, pd, ttl)
		ch <- probeResult{result, err}
	}

	go probe(nearTTL, nearCh)
	go probe(farTTL, farCh)

	nearRes := <-nearCh
	farRes := <-farCh

	if nearRes.err != nil {
		a.logger.Error("Near probe failed",
			slog.Uint64("pd_id", pd.ProbingDirectiveID),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("ttl", int(nearTTL)),
			slog.Any("err", nearRes.err))
		return
	}
	if farRes.err != nil {
		a.logger.Error("Far probe failed",
			slog.Uint64("pd_id", pd.ProbingDirectiveID),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("ttl", int(farTTL)),
			slog.Any("err", farRes.err))
		return
	}

	var nearInfo *api.Info
	if !nearRes.result.TimedOut {
		nearInfo = probeResultToInfo(nearRes.result, nearTTL)
	}

	var farInfo *api.Info
	if !farRes.result.TimedOut {
		farInfo = probeResultToInfo(farRes.result, farTTL)
	}

	fie := &api.ForwardingInfoElement{
		Agent:               api.Agent{AgentID: a.config.AgentID},
		ProbingDirectiveID:  pd.ProbingDirectiveID,
		IPVersion:           pd.IPVersion,
		Protocol:            pd.Protocol,
		DestinationAddress:  pd.DestinationAddress,
		NearInfo:            nearInfo,
		FarInfo:             farInfo,
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
//
// This is a var (not func) to allow mocking in tests.
var createProber = func(cfg *Config) (Prober, error) {
	switch cfg.ProberType {
	case "mock":
		return NewMockProber(cfg), nil
	case "caracal":
		return NewCaracalProber(cfg)
	default:
		return nil, fmt.Errorf("unknown prober type: %q (valid: mock, caracal)", cfg.ProberType)
	}
}

// validatePD checks that a directive has all required fields and
// protocol-specific headers.
func validatePD(pd *api.ProbingDirective) error {
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
func probeResultToInfo(result *ProbeResult, ttl uint8) *api.Info {
	return &api.Info{
		ProbeTTL:          ttl,
		ReplyAddress:      result.ReplyAddress,
		SentTimestamp:     result.SentTime,
		ReceivedTimestamp: result.ReceivedTime,
	}
}

// isNetworkError returns true if err indicates a network/connection failure
// (EOF, timeout, connection reset) rather than a JSON decoding error.
func isNetworkError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
