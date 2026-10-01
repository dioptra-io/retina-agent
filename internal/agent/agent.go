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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/dioptra-io/retina-commons/api/v1"
)

var ErrInvalidDirective = errors.New("invalid probing directive")

// TCP keepalive settings for the orchestrator connection, matching the
// orchestrator side. The orchestrator may legitimately stay silent for long
// periods, so silence is never treated as failure; instead the kernel probes
// the peer after orchestratorKeepaliveIdle without traffic, and a dead peer
// (host down, network partition) is detected after orchestratorKeepaliveCount
// unanswered probes (~60s), surfacing as a read error that triggers reconnection.
const (
	orchestratorKeepaliveIdle     = 30 * time.Second
	orchestratorKeepaliveInterval = 10 * time.Second
	orchestratorKeepaliveCount    = 3
)

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

	tcpConn, err := net.Dial("tcp", a.config.OrchestratorAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to orchestrator: %w", err)
	}
	conn := tcpConn.(*net.TCPConn)
	defer func() {
		if err := conn.Close(); err != nil {
			a.logger.Debug("Failed to close connection", slog.Any("err", err))
		}
	}()

	if err := conn.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     orchestratorKeepaliveIdle,
		Interval: orchestratorKeepaliveInterval,
		Count:    orchestratorKeepaliveCount,
	}); err != nil {
		return fmt.Errorf("failed to configure keepalive: %w", err)
	}

	a.logger.Info("Connected to orchestrator",
		slog.String("address", a.config.OrchestratorAddr))

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	if err := a.authenticateIO(conn, reader, writer); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	if a.config.Secret == "" {
		a.logger.Warn("Authentication disabled — not recommended for production")
	} else {
		a.logger.Info("Authentication enabled — authenticated successfully")
	}

	pds := make(chan *api.ProbingDirective, a.config.PDsBufferSize)
	fies := make(chan *api.ForwardingInfoElement, a.config.FIEsBufferSize)

	g, ctx := errgroup.WithContext(ctx)

	// FIE writes have no deadline, so that the writer waits for as long as
	// the orchestrator applies backpressure. Closing the connection is what
	// unblocks it on shutdown.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	g.Go(func() error { return a.readerLoopWithReader(ctx, conn, reader, pds) })
	g.Go(func() error { return a.processorLoop(ctx, pds, fies) })
	g.Go(func() error { return a.writerLoopWithWriter(ctx, conn, writer, fies) })

	if err := g.Wait(); err != nil && err != ctx.Err() {
		a.logger.Error("Connection terminated", slog.Any("err", err))
		return err
	}

	a.logger.Info("Shut down gracefully")
	return nil
}

// authenticate must be called immediately after connecting, before any other messages.
func (a *agent) authenticate(conn net.Conn) error {
	return a.authenticateIO(conn, bufio.NewReader(conn), bufio.NewWriter(conn))
}

func (a *agent) authenticateIO(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer) error {
	encoder := json.NewEncoder(writer)
	decoder := json.NewDecoder(singleByteReader{reader: reader})

	authReq := &api.AuthRequest{
		AgentID: a.config.AgentID,
		Secret:  a.config.Secret,
	}

	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("failed to set write deadline: %w", err)
	}

	if err := encoder.Encode(authReq); err != nil { //nolint:gosec // G117: secret field is intentionally included in auth request
		return fmt.Errorf("failed to send auth request: %w", err)
	}
	if err := writer.Flush(); err != nil {
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

	// Intentionally ignore errors: if the connection is already broken,
	// the next read/write operation will surface the failure.
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})

	return nil
}

// singleByteReader prevents the JSON handshake decoder from reading ahead into
// the first CSV record, which belongs to the post-handshake protocol.
type singleByteReader struct {
	reader io.Reader
}

func (r singleByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.reader.Read(p)
}

// readerLoop receives and validates ProbingDirective messages from the orchestrator.
// After MaxConsecutiveDecodeErrors consecutive decoding failures the connection is
// terminated (set to 0 to disable). Read timeouts are not errors: ReadDeadline
// only wakes the loop periodically to observe ctx cancellation, and a dead
// orchestrator is detected by TCP keepalive, which surfaces as a read error.
// Closing pds on return signals processorLoop to drain in-flight goroutines and exit.
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
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				decoder = json.NewDecoder(io.MultiReader(decoder.Buffered(), conn))
				continue
			}
			shouldContinue, newCount, handledErr := a.handleDecodeError(ctx, err, consecutiveDecodeErrors)
			consecutiveDecodeErrors = newCount
			if !shouldContinue {
				return handledErr
			}
			continue
		}
		consecutiveDecodeErrors = 0
		a.metrics.PDsReceivedTotal.Inc()
		if err := validatePD(&pd); err != nil {
			a.logger.Warn("Invalid directive", slog.Any("err", err))
			a.metrics.PDsInvalidTotal.Inc()
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pds <- &pd:
			a.pdsDepth.Add(1)
			a.metrics.ChannelDepth.WithLabelValues("pds").Set(float64(a.pdsDepth.Load()))
		}
	}
}

func (a *agent) readerLoopWithReader(ctx context.Context, conn net.Conn, reader *bufio.Reader, pds chan<- *api.ProbingDirective) error {
	defer close(pds)
	consecutiveDecodeErrors := 0
	var pending []byte

	for {
		if err := conn.SetReadDeadline(time.Now().Add(a.config.ReadDeadline)); err != nil {
			return fmt.Errorf("failed to set read deadline: %w", err)
		}

		fragment, err := reader.ReadSlice('\n')
		pending = append(pending, fragment...)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			shouldContinue, newCount, handledErr := a.handleDecodeError(ctx, err, consecutiveDecodeErrors)
			consecutiveDecodeErrors = newCount
			if !shouldContinue {
				return handledErr
			}
			continue
		}
		if len(bytes.TrimSpace(pending)) == 0 {
			pending = pending[:0]
			continue
		}

		pd, err := decodePDRecord(string(pending), a.config.AgentID)
		pending = pending[:0]
		if err != nil {
			shouldContinue, newCount, handledErr := a.handleDecodeError(ctx, err, consecutiveDecodeErrors)
			consecutiveDecodeErrors = newCount
			if !shouldContinue {
				return handledErr
			}
			continue
		}

		consecutiveDecodeErrors = 0
		a.metrics.PDsReceivedTotal.Inc()

		if err := validatePD(pd); err != nil {
			a.logger.Warn("Invalid directive", slog.Any("err", err))
			a.metrics.PDsInvalidTotal.Inc()
			continue
		}

		a.logger.Debug("← PD received",
			slog.Uint64("pd_id", uint64(pd.ProbingDirectiveID)),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("near_ttl", int(pd.NearTTL)))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case pds <- pd:
			a.pdsDepth.Add(1)
			a.metrics.ChannelDepth.WithLabelValues("pds").Set(float64(a.pdsDepth.Load()))
		}
	}
}

// handleDecodeError classifies a wire decode error and decides whether to retry.
// Timeouts are handled by the caller. Returns (shouldContinue, updatedErrorCount, errorToReturn).
func (a *agent) handleDecodeError(ctx context.Context, err error, consecutiveErrors int) (bool, int, error) {
	// Check for context cancellation first, regardless of error type.
	if ctx.Err() != nil {
		return false, consecutiveErrors, ctx.Err()
	}

	// Check for network errors (trigger reconnection)
	if isNetworkError(err) {
		return false, consecutiveErrors, fmt.Errorf("connection lost while reading: %w", err)
	}

	// Malformed record — log and potentially skip
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

// processorLoop dispatches incoming directives to processPD goroutines, at
// most MaxInflightPDs at a time when that limit is set.
// A WaitGroup ensures all in-flight goroutines finish before fies is closed,
// preventing a send-on-closed-channel panic.
func (a *agent) processorLoop(ctx context.Context, pds <-chan *api.ProbingDirective, fies chan<- *api.ForwardingInfoElement) error {
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		close(fies)
	}()

	// slots bounds the number of in-flight PDs. While it is full this loop
	// stops taking PDs, readerLoop blocks on the pds channel, and the
	// orchestrator sees the backpressure on its side of the connection.
	var slots chan struct{}
	if a.config.MaxInflightPDs > 0 {
		slots = make(chan struct{}, a.config.MaxInflightPDs)
	}

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
			if slots != nil {
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			a.metrics.PDGoroutines.Inc()
			wg.Go(func() {
				if slots != nil {
					defer func() { <-slots }()
				}
				a.processPD(ctx, pd, fies)
			})
		}
	}
}

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
			a.fiesDepth.Add(-1)
			a.metrics.ChannelDepth.WithLabelValues("fies").Set(float64(a.fiesDepth.Load()))
			if err := encoder.Encode(fie); err != nil {
				a.metrics.WriteErrorsTotal.Inc()
				if isNetworkError(err) {
					return fmt.Errorf("connection lost while writing: %w", err)
				}
				return fmt.Errorf("failed to encode FIE: %w", err)
			}
			a.metrics.FIEsSentTotal.Inc()
		}
	}
}

func (a *agent) writerLoopWithWriter(ctx context.Context, conn net.Conn, writer *bufio.Writer, fies <-chan *api.ForwardingInfoElement) error {

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

			_, err := writer.WriteString(encodeFIERecord(fie))
			if err == nil {
				err = writer.Flush()
			}
			if err != nil {
				a.metrics.WriteErrorsTotal.Inc()
				if isNetworkError(err) {
					return fmt.Errorf("connection lost while writing: %w", err)
				}
				return fmt.Errorf("failed to encode FIE: %w", err)
			}

			a.metrics.FIEsSentTotal.Inc()
			a.logger.Debug("→ FIE sent",
				slog.Uint64("pd_id", uint64(fie.ProbingDirectiveID)),
				slog.String("dest", fie.DestinationAddress.String()),
				slog.Bool("near_timeout", fie.NearInfo == nil),
				slog.Bool("far_timeout", fie.FarInfo == nil))
		}
	}
}

// processPD executes near and far probes in parallel and sends an FIE on success.
// A nil probe result means the probe was already in-flight and the PD is silently dropped.
// Timed-out probes produce a nil NearInfo or FarInfo in the FIE.
func (a *agent) processPD(ctx context.Context, pd *api.ProbingDirective, fies chan<- *api.ForwardingInfoElement) {
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
			slog.Uint64("pd_id", uint64(pd.ProbingDirectiveID)),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("ttl", int(nearTTL)),
			slog.Any("err", nearRes.err))
		a.metrics.ProbesTotal.WithLabelValues("error").Inc()
		return
	}
	a.recordProbeOutcome(nearRes.result)

	if farRes.err != nil {
		if errors.Is(farRes.err, ErrDuplicatePD) {
			return // probe already in-flight for this destination/TTL/second
		}
		a.logger.Error("Far probe failed",
			slog.Uint64("pd_id", uint64(pd.ProbingDirectiveID)),
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("ttl", int(farTTL)),
			slog.Any("err", farRes.err))
		a.metrics.ProbesTotal.WithLabelValues("error").Inc()
		return
	}
	a.recordProbeOutcome(farRes.result)

	select {
	case fies <- a.buildFIE(pd, nearRes.result, farRes.result, nearTTL, farTTL):
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

// buildFIE constructs a FIE from probe results. Timed-out probes produce a nil NearInfo or FarInfo.
func (a *agent) buildFIE(pd *api.ProbingDirective, nearRes, farRes *ProbeResult, nearTTL, farTTL uint8) *api.ForwardingInfoElement {
	var nearInfo *api.Info
	if !nearRes.TimedOut {
		nearInfo = probeResultToInfo(nearRes, nearTTL)
	}

	var farInfo *api.Info
	if !farRes.TimedOut {
		farInfo = probeResultToInfo(farRes, farTTL)
	}

	return &api.ForwardingInfoElement{
		Agent:               api.Agent{AgentID: a.config.AgentID},
		ProbingDirectiveID:  pd.ProbingDirectiveID,
		IPVersion:           pd.IPVersion,
		Protocol:            pd.Protocol,
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
	if pd.NearTTL == 255 {
		return fmt.Errorf("%w: NearTTL 255 would overflow farTTL", ErrInvalidDirective)
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

func probeResultToInfo(result *ProbeResult, ttl uint8) *api.Info {
	return &api.Info{
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
