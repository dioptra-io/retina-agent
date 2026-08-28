// Copyright (c) 2025 Sorbonne Université
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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/dioptra-io/retina-commons/framing"
	"github.com/dioptra-io/retina-commons/model"
	wire "github.com/dioptra-io/retina-commons/wire/v2"
)

var ErrInvalidDirective = errors.New("invalid probing directive")

// orchestratorKeepalivePeriod is the interval between TCP keepalive probes
// for the orchestrator connection. Keepalives ensure that a dead connection is
// detected even when no data is being exchanged, triggering reconnection instead
// of blocking indefinitely on a read timeout.
const orchestratorKeepalivePeriod = 10 * time.Second

// maxConsecutiveReadTimeouts is the number of consecutive read timeouts before
// the connection is considered dead and reconnection is triggered. With the
// default ReadDeadline of 10s, a dead connection is detected in ~60s. Kept as
// a constant rather than a config field since operators should tune ReadDeadline
// instead.
const maxConsecutiveReadTimeouts = 6

type agent struct {
	config  *Config
	prober  Prober
	logger  *slog.Logger
	metrics *Metrics

	pdsDepth  atomic.Int64
	fiesDepth atomic.Int64
}

// Run starts the agent and blocks until the context is canceled or an error occurs.
func Run(ctx context.Context, cfg *Config, logger *slog.Logger, metrics *Metrics) error {
	if cfg == nil {
		cfg = DefaultConfig()
	}

	prober, err := createProber(cfg, logger, metrics)
	if err != nil {
		return fmt.Errorf("failed to create prober: %w", err)
	}
	defer func() {
		if err := prober.Close(); err != nil {
			logger.Debug("Failed to close prober", slog.Any("err", err))
		}
	}()

	a := &agent{
		config:  cfg,
		prober:  prober,
		logger:  logger,
		metrics: metrics,
	}

	dialer := net.Dialer{KeepAlive: orchestratorKeepalivePeriod}
	tcpConn, err := dialer.DialContext(ctx, "tcp", a.config.OrchestratorAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to orchestrator: %w", err)
	}
	conn := tcpConn.(*net.TCPConn)
	defer func() {
		if err := conn.Close(); err != nil {
			a.logger.Debug("Failed to close connection", slog.Any("err", err))
		}
	}()

	if err := conn.SetKeepAlive(true); err != nil {
		return fmt.Errorf("failed to enable keepalive: %w", err)
	}
	if err := conn.SetKeepAlivePeriod(orchestratorKeepalivePeriod); err != nil {
		return fmt.Errorf("failed to set keepalive period: %w", err)
	}

	// Created before authentication (not just before the pipeline loops)
	// so cancellation during the handshake is also honored promptly —
	// this derived ctx cancels both on external shutdown and if any of
	// the three loops below errors, and closing conn immediately unblocks
	// whatever blocking read/write is in flight at that moment, rather
	// than waiting for it to time out or for g.Wait() to return naturally.
	g, ctx := errgroup.WithContext(ctx)
	stopClose := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	defer stopClose()

	a.logger.Info("Connected to orchestrator",
		slog.String("address", a.config.OrchestratorAddr))

	if err := a.authenticate(conn); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	if a.config.Secret == "" {
		a.logger.Warn("Authentication disabled — not recommended for production")
	} else {
		a.logger.Info("Authentication enabled — authenticated successfully")
	}

	pds := make(chan *model.ProbingDirective, a.config.PDsBufferSize)
	fies := make(chan *model.ForwardingInfoElement, a.config.FIEsBufferSize)

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

// authenticate must be called immediately after connecting, before any other messages.
func (a *agent) authenticate(conn net.Conn) error {
	authReq := &wire.AuthRequest{
		AgentId: a.config.AgentID,
		Secret:  a.config.Secret, //nolint:gosec // G117: secret field is intentionally included in auth request
	}

	if err := framing.Send(conn, 5*time.Second, authReq); err != nil {
		return fmt.Errorf("failed to send auth request: %w", err)
	}

	var authResp wire.AuthResponse
	if err := framing.Receive(conn, 5*time.Second, &authResp); err != nil {
		return fmt.Errorf("failed to receive auth response: %w", err)
	}

	if !authResp.Authenticated {
		return fmt.Errorf("authentication rejected: %s", authResp.Message)
	}

	// Intentionally ignore errors: if the connection is already broken,
	// the next read/write operation will surface the failure.
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})

	return nil
}

// readerLoop receives and validates ProbingDirective messages from the orchestrator.
// After MaxConsecutiveDecodeErrors consecutive decode failures the connection is
// terminated (set to 0 to disable). After maxConsecutiveReadTimeouts consecutive
// read timeouts the connection is considered dead and reconnection is triggered.
// Closing pds on return signals processorLoop to drain in-flight goroutines and exit.
func (a *agent) readerLoop(ctx context.Context, conn net.Conn, pds chan<- *model.ProbingDirective) error {
	defer close(pds)
	consecutiveDecodeErrors := 0
	consecutiveTimeouts := 0

	for {
		var wirePD wire.ProbingDirective
		if err := framing.Receive(conn, a.config.ReadDeadline, &wirePD); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// errors.As, not a direct type assertion: framing.Receive's
			// clean pre-frame timeout is returned unwrapped in practice, so
			// this matters mainly for a mid-payload timeout, which gets
			// wrapped in ErrProtocolViolation — that case is already
			// correctly fatal either way (a partial frame desyncs the
			// stream regardless of why the read stopped), but errors.As
			// is the strictly more correct check regardless.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				consecutiveTimeouts++
				a.logger.Debug("Read timeout, no data received",
					slog.Int("consecutive", consecutiveTimeouts),
					slog.Int("max", maxConsecutiveReadTimeouts))
				if consecutiveTimeouts >= maxConsecutiveReadTimeouts {
					return fmt.Errorf("connection timed out after %d consecutive read timeouts", consecutiveTimeouts)
				}
				continue
			}
			consecutiveTimeouts = 0
			shouldContinue, newCount, handledErr := a.handleDecodeError(ctx, err, consecutiveDecodeErrors)
			consecutiveDecodeErrors = newCount
			if !shouldContinue {
				return handledErr
			}
			continue
		}

		pd, err := model.ProbingDirectiveFromProto(&wirePD)
		if err != nil {
			a.logger.Warn("Invalid directive", slog.Any("err", err))
			a.metrics.PDsInvalidTotal.Inc()
			continue
		}

		if err := validatePD(&pd); err != nil {
			a.logger.Warn("Invalid directive", slog.Any("err", err))
			a.metrics.PDsInvalidTotal.Inc()
			continue
		}

		consecutiveDecodeErrors = 0
		consecutiveTimeouts = 0
		a.metrics.PDsReceivedTotal.Inc()

		a.logger.Debug("← PD received",
			slog.Uint64("pd_id", pd.ProbingDirectiveID),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("near_ttl", int(pd.NearTTL)))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case pds <- &pd:
			a.pdsDepth.Add(1)
			a.metrics.ChannelDepth.WithLabelValues("pds").Set(float64(a.pdsDepth.Load()))
		}
	}
}

// handleDecodeError classifies a decode error and decides whether to retry.
// Timeouts are handled by the caller. Returns (shouldContinue, updatedErrorCount, errorToReturn).
func (a *agent) handleDecodeError(ctx context.Context, err error, consecutiveErrors int) (bool, int, error) {
	// Check for context cancellation first, regardless of error type.
	if ctx.Err() != nil {
		return false, consecutiveErrors, ctx.Err()
	}

	// Check for network errors (trigger reconnection). This also covers
	// framing.ErrProtocolViolation for a partial-frame read (n > 0): the
	// stream is desynchronized in that case regardless of whether the
	// underlying cause was a timeout, so it's treated as fatal here too,
	// not retried like the clean/no-bytes-consumed timeout case above.
	if isNetworkError(err) || errors.Is(err, framing.ErrProtocolViolation) {
		return false, consecutiveErrors, fmt.Errorf("connection lost while reading: %w", err)
	}

	// Malformed frame — log and potentially skip
	consecutiveErrors++
	a.metrics.DecodeErrorsTotal.Inc()

	if a.config.MaxConsecutiveDecodeErrors > 0 {
		a.logger.Error("Failed to decode directive",
			slog.Int("attempt", consecutiveErrors),
			slog.Int("max", a.config.MaxConsecutiveDecodeErrors),
			slog.Any("err", err))

		if consecutiveErrors >= a.config.MaxConsecutiveDecodeErrors {
			return false, consecutiveErrors, fmt.Errorf("too many consecutive decode errors (%d), protocol may be broken: %w",
				consecutiveErrors, err)
		}
	} else {
		a.logger.Error("Failed to decode directive (skipping)", slog.Any("err", err))
	}

	return true, consecutiveErrors, nil
}

// processorLoop dispatches incoming directives to processPD goroutines.
// A WaitGroup ensures all in-flight goroutines finish before fies is closed,
// preventing a send-on-closed-channel panic.
func (a *agent) processorLoop(ctx context.Context, pds <-chan *model.ProbingDirective, fies chan<- *model.ForwardingInfoElement) error {
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		close(fies)
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pd, ok := <-pds:
			if !ok {
				return nil
			}
			a.pdsDepth.Add(-1)
			a.metrics.ChannelDepth.WithLabelValues("pds").Set(float64(a.pdsDepth.Load()))
			a.metrics.PDGoroutines.Inc()
			wg.Go(func() {
				a.processPD(ctx, pd, fies)
			})
		}
	}
}

func (a *agent) writerLoop(ctx context.Context, conn net.Conn, fies <-chan *model.ForwardingInfoElement) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fie, ok := <-fies:
			if !ok {
				return nil
			}

			a.fiesDepth.Add(-1)
			a.metrics.ChannelDepth.WithLabelValues("fies").Set(float64(a.fiesDepth.Load()))

			wireFIE, err := fie.ToProto()
			if err != nil {
				a.metrics.WriteErrorsTotal.Inc()
				return fmt.Errorf("failed to convert FIE to wire format: %w", err)
			}

			if err := framing.Send(conn, a.config.WriteDeadline, wireFIE); err != nil {
				a.metrics.WriteErrorsTotal.Inc()
				if isNetworkError(err) {
					return fmt.Errorf("connection lost while writing: %w", err)
				}
				return fmt.Errorf("failed to encode FIE: %w", err)
			}

			a.metrics.FIEsSentTotal.Inc()
			a.logger.Debug("→ FIE sent",
				slog.Uint64("pd_id", fie.ProbingDirectiveID),
				slog.String("dest", fie.DestinationAddress.String()),
				slog.Bool("near_timeout", fie.NearInfo == nil),
				slog.Bool("far_timeout", fie.FarInfo == nil))
		}
	}
}

// processPD executes near and far probes in parallel and sends an FIE on success.
// A nil probe result means the probe was already in-flight and the PD is silently dropped.
// Timed-out probes produce a nil NearInfo or FarInfo in the FIE.
func (a *agent) processPD(ctx context.Context, pd *model.ProbingDirective, fies chan<- *model.ForwardingInfoElement) {
	defer a.metrics.PDGoroutines.Dec()

	type probeResult struct {
		result *ProbeResult
		err    error
	}

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
		if errors.Is(nearRes.err, ErrDuplicatePD) {
			return // probe already in-flight for this destination/TTL/second
		}
		a.logger.Error("Near probe failed",
			slog.Uint64("pd_id", pd.ProbingDirectiveID),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("ttl", int(nearTTL)),
			slog.Any("err", nearRes.err))
		a.metrics.ProbesTotal.WithLabelValues("error").Inc()
		return
	}
	// Guard against a Prober implementation returning (nil, nil) instead
	// of (nil, ErrDuplicatePD) for "already in-flight" — recordProbeOutcome
	// dereferences unconditionally, and this runs under wg.Go, whose
	// contract requires the function not to panic.
	if nearRes.result == nil {
		a.logger.Warn("Near probe returned no result", slog.Uint64("pd_id", pd.ProbingDirectiveID))
		return
	}
	a.recordProbeOutcome(nearRes.result)

	if farRes.err != nil {
		if errors.Is(farRes.err, ErrDuplicatePD) {
			return // probe already in-flight for this destination/TTL/second
		}
		a.logger.Error("Far probe failed",
			slog.Uint64("pd_id", pd.ProbingDirectiveID),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("ttl", int(farTTL)),
			slog.Any("err", farRes.err))
		a.metrics.ProbesTotal.WithLabelValues("error").Inc()
		return
	}
	if farRes.result == nil {
		a.logger.Warn("Far probe returned no result", slog.Uint64("pd_id", pd.ProbingDirectiveID))
		return
	}
	a.recordProbeOutcome(farRes.result)

	fie := a.buildFIE(pd, nearRes.result, farRes.result, nearTTL, farTTL)

	select {
	case fies <- fie:
		a.fiesDepth.Add(1)
		a.metrics.ChannelDepth.WithLabelValues("fies").Set(float64(a.fiesDepth.Load()))
	case <-ctx.Done():
	}
}

func (a *agent) recordProbeOutcome(result *ProbeResult) {
	if result.TimedOut {
		a.metrics.ProbesTotal.WithLabelValues("timeout").Inc()
		return
	}

	a.metrics.ProbesTotal.WithLabelValues("success").Inc()
	a.metrics.ProbeRTTSeconds.Observe(result.ReceivedTime.Sub(result.SentTime).Seconds())

	if result.ReplyAddress != nil {
		a.metrics.ReplyAddressTypeTotal.WithLabelValues(classifyIP(result.ReplyAddress)).Inc()
	}
}

func classifyIP(ip net.IP) string {
	if ip.IsLoopback() {
		return "loopback"
	}
	if ip.IsMulticast() {
		return "multicast"
	}
	if ip.IsPrivate() {
		return "private"
	}
	return "public"
}

// buildFIE constructs a FIE from probe results. Timed-out probes produce a
// nil NearInfo or FarInfo. SourceAddress comes from whichever of
// nearRes/farRes succeeded (ProbeResult.SourceAddress, from caracal's CSV
// output — see prober.go's doc comment); if both time out, SourceAddress
// is nil — that's a legitimate, expected value, not an error, since
// model.ForwardingInfoElement.SourceAddress is optional precisely for
// this case (a probe that gets no reply at all has no source address to
// report).
func (a *agent) buildFIE(pd *model.ProbingDirective, nearRes, farRes *ProbeResult, nearTTL, farTTL uint8) *model.ForwardingInfoElement {
	var nearInfo *model.Info
	if !nearRes.TimedOut {
		nearInfo = probeResultToInfo(nearRes, nearTTL)
	}

	var farInfo *model.Info
	if !farRes.TimedOut {
		farInfo = probeResultToInfo(farRes, farTTL)
	}

	// Only from a genuinely successful probe — a timed-out ProbeResult
	// shouldn't carry a source address at all in practice, but this
	// guards against one that does (matches the "from whichever succeeded"
	// intent stated above, which the bare field reads below didn't).
	var sourceAddress net.IP
	if !nearRes.TimedOut {
		sourceAddress = nearRes.SourceAddress
	}
	if sourceAddress == nil && !farRes.TimedOut {
		sourceAddress = farRes.SourceAddress
	}

	return &model.ForwardingInfoElement{
		Agent:               model.Agent{ID: a.config.AgentID},
		ProbingDirectiveID:  pd.ProbingDirectiveID,
		IPVersion:           pd.IPVersion,
		Protocol:            pd.Protocol,
		SourceAddress:       sourceAddress,
		DestinationAddress:  pd.DestinationAddress,
		NearInfo:            nearInfo,
		FarInfo:             farInfo,
		ProductionTimestamp: time.Now().UTC(),
	}
}

// createProber instantiates a prober based on cfg.ProberType.
//
// To add a new prober type:
//  1. Implement the Prober interface
//  2. Add a case to this switch
//  3. Update ProberType documentation in config.go
//
// This is a package-level variable to allow overriding in tests.
// Tests that override this cannot run in parallel.
var createProber = func(cfg *Config, logger *slog.Logger, metrics *Metrics) (Prober, error) {
	switch cfg.ProberType {
	case "mock":
		return NewMockProber(cfg), nil
	case "caracal":
		return NewCaracalProber(cfg, logger, metrics)
	default:
		return nil, fmt.Errorf("unknown prober type: %q (valid: mock, caracal)", cfg.ProberType)
	}
}

// validatePD checks fields model.ProbingDirectiveFromProto doesn't already
// validate: AgentID non-empty, NearTTL isn't 0 or 255 (0 is meaningless;
// 255 would silently wrap farTTL := pd.NearTTL+1 to 0, since NearTTL is
// uint8 — model's own TTL check only rejects values that don't fit in a
// uint8 at all, i.e. >255, not these two specific in-range values), and
// NextHeader present for the given protocol. DestinationAddress validity
// is enforced by ProbingDirectiveFromProto itself before this is called.
func validatePD(pd *model.ProbingDirective) error {
	if pd == nil {
		return fmt.Errorf("%w: directive is nil", ErrInvalidDirective)
	}
	if pd.AgentID == "" {
		return fmt.Errorf("%w: agent ID is empty", ErrInvalidDirective)
	}
	if pd.NearTTL == 0 {
		return fmt.Errorf("%w: TTL cannot be zero", ErrInvalidDirective)
	}
	if pd.NearTTL == 255 {
		return fmt.Errorf("%w: NearTTL 255 would overflow farTTL", ErrInvalidDirective)
	}

	// Separate ICMP/ICMPv6 cases (not combined) — a combined check would
	// accept an ICMP-protocol PD carrying only an ICMPv6NextHeader, or vice
	// versa. This exact bug predates the migration (same combined-case
	// pattern existed in v1's validatePD) but is worth fixing regardless
	// of origin.
	switch pd.Protocol {
	case wire.Protocol_PROTOCOL_UNSPECIFIED:
		return fmt.Errorf("%w: protocol not set", ErrInvalidDirective)
	case wire.Protocol_PROTOCOL_ICMP:
		if pd.NextHeader.GetIcmpNextHeader() == nil {
			return fmt.Errorf("%w: ICMP directive missing next header", ErrInvalidDirective)
		}
	case wire.Protocol_PROTOCOL_ICMPV6:
		if pd.NextHeader.GetIcmpv6NextHeader() == nil {
			return fmt.Errorf("%w: ICMPv6 directive missing next header", ErrInvalidDirective)
		}
	case wire.Protocol_PROTOCOL_UDP:
		if pd.NextHeader.GetUdpNextHeader() == nil {
			return fmt.Errorf("%w: UDP directive missing next header", ErrInvalidDirective)
		}
	default:
		return fmt.Errorf("%w: unsupported protocol %d", ErrInvalidDirective, pd.Protocol)
	}

	return nil
}

func probeResultToInfo(result *ProbeResult, ttl uint8) *model.Info {
	return &model.Info{
		ProbeTTL:          ttl,
		ReplyAddress:      result.ReplyAddress,
		SentTimestamp:     result.SentTime,
		ReceivedTimestamp: result.ReceivedTime,
	}
}

// isNetworkError returns true for connection failures, including EOF and unexpected EOF.
func isNetworkError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
