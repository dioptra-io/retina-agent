// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// caracal_prober.go
//
// CaracalProber Architecture:
//
// This implements a high-throughput network prober using the caracal tool as a
// subprocess. It uses a pipelined architecture with multiple goroutines for
// non-blocking operation:
//
// 1. Caller goroutines (multiple):
//   - Call Probe() to request a network probe
//   - Queue probe request to writeQueue channel
//   - Wait on a result channel for the probe result
//
// 2. writerLoop goroutine (one):
//   - Continuously reads from writeQueue
//   - Formats probe specs as CSV
//   - Writes to caracal's stdin
//
// 3. readerLoop goroutine (one):
//   - Continuously reads CSV results from caracal's stdout
//   - Correlates results with in-flight probes using a shared map
//   - Delivers results to waiting caller goroutines
//
// 4. cleanupLoop goroutine (one):
//   - Periodically removes stale/timed-out probes from the in-flight map
//
// 5. logStderr goroutine (one):
//   - Logs caracal's stderr output for debugging
//
// Correlation mechanism:
//   - Each probe is identified by (dst_addr, first_half_word, second_half_word, ttl, protocol, correlation_second)
//   - When Probe() is called, an entry is added to the in-flight map with this key
//   - When a result arrives, the key is reconstructed from the result
//   - A ±2 second time tolerance handles queue delays and clock variations
//   - ASSUMPTION: Probes are sent within ~2 seconds of being queued
//     (if writeQueue backpressure causes >2 second delay, correlation may fail)
//
// Deduplication:
//   - Only one probe per unique (destination, ports/fields, TTL, protocol, second)
//   - If a duplicate is requested while a probe is in-flight, it returns nil
//   - This prevents sending redundant probes and ensures correlation works correctly
//   - With proper directive randomization, duplicates should be rare
package agent

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
	"golang.org/x/sync/errgroup"
)

// CaracalProber implements high-throughput probing using caracal.
// It uses a pipelined architecture for maximum throughput.
type CaracalProber struct {
	// Caracal subprocess management
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	csvWriter *csv.Writer
	stdout    *csv.Reader
	stderr    io.ReadCloser

	// Probe correlation - maps in-flight probes to waiting goroutines
	inFlight   map[probeKey]*inFlightProbe
	inFlightMu sync.RWMutex

	// Non-blocking write pipeline
	writeQueue chan *probeRequest

	// Configuration and lifecycle management
	config  *Config
	logger  *slog.Logger
	metrics *Metrics
	cancel  context.CancelFunc
	g       *errgroup.Group
}

// probeKey uniquely identifies a probe within a time window.
type probeKey struct {
	dstAddr           string
	firstHalfWord     uint16 // FirstHalfWord for ICMP/ICMPv6, SourcePort for UDP
	secondHalfWord    uint16 // SecondHalfWord for ICMP/ICMPv6, DestinationPort for UDP
	ttl               uint8
	protocol          api.Protocol // Protocol type (ICMP, ICMPv6, UDP)
	correlationSecond int64        // Unix timestamp for correlation (1-second window)
}

// inFlightProbe tracks a probe waiting for a result.
type inFlightProbe struct {
	resultCh   chan *ProbeResult
	queuedTime time.Time
}

// probeRequest represents a single probe to be sent to caracal.
type probeRequest struct {
	pd       *api.ProbingDirective
	ttl      uint8
	resultCh chan *ProbeResult
}

// NewCaracalProber creates and starts a caracal prober.
var NewCaracalProber = func(cfg *Config, logger *slog.Logger, metrics *Metrics) (*CaracalProber, error) {
	cmd, stdin, stdout, stderr, err := setupCaracalProcess(cfg, logger)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	queueSize := cfg.WriteQueueSize
	if queueSize <= 0 {
		queueSize = 1000
	}

	p := &CaracalProber{
		cmd:        cmd,
		stdin:      stdin,
		csvWriter:  csv.NewWriter(stdin),
		stdout:     csv.NewReader(stdout),
		stderr:     stderr,
		inFlight:   make(map[probeKey]*inFlightProbe),
		writeQueue: make(chan *probeRequest, queueSize),
		config:     cfg,
		logger:     logger,
		metrics:    metrics,
		cancel:     cancel,
		g:          g,
	}

	// Skip CSV header
	if _, err := p.stdout.Read(); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			logger.Error("Failed to kill caracal", slog.Any("err", killErr))
		}
		cancel()
		return nil, fmt.Errorf("failed to read CSV header: %w", err)
	}

	g.Go(func() error { return p.writerLoop(ctx) })
	g.Go(func() error { return p.readerLoop(ctx) })
	g.Go(func() error { return p.logStderr(ctx) })
	g.Go(func() error { return p.cleanupLoop(ctx) })

	return p, nil
}

// setupCaracalProcess creates and starts the caracal subprocess with all pipes configured.
//
// Uses cfg.ProberPath to locate the caracal executable (defaults to searching PATH).
// Custom caracal arguments are specified via cfg.ProberArgs.
// Example: cfg.ProberArgs = []string{"--probing-rate", "100000", "--n-packets", "3"}
func setupCaracalProcess(cfg *Config, logger *slog.Logger) (cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser, err error) {
	caracalPath := cfg.ProberPath
	if caracalPath == "" {
		caracalPath = "caracal"
	}

	args := cfg.ProberArgs

	cmd = exec.Command(caracalPath, args...) // #nosec G204 -- caracalPath is user-controlled by design (ProberPath config)

	success := false

	defer func() {
		if !success {
			closePipe(stdin, "stdin", logger)
			closePipe(stdout, "stdout", logger)
			closePipe(stderr, "stderr", logger)
		}
	}()

	stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err = cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err = cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err = cmd.Start(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to start caracal: %w", err)
	}

	success = true
	return cmd, stdin, stdout, stderr, nil
}

// closePipe safely closes a pipe and logs any error.
func closePipe(pipe io.Closer, name string, logger *slog.Logger) {
	if pipe != nil {
		if err := pipe.Close(); err != nil {
			logger.Error("Failed to close pipe", slog.String("name", name), slog.Any("err", err))
		}
	}
}

// extractHalfWords returns the firstHalfWord and secondHalfWord from a directive.
func extractHalfWords(pd *api.ProbingDirective) (uint16, uint16) {
	switch pd.Protocol {
	case api.ICMP:
		if pd.NextHeader.ICMPNextHeader != nil {
			return pd.NextHeader.ICMPNextHeader.FirstHalfWord,
				pd.NextHeader.ICMPNextHeader.SecondHalfWord
		}
	case api.ICMPv6:
		if pd.NextHeader.ICMPv6NextHeader != nil {
			return pd.NextHeader.ICMPv6NextHeader.FirstHalfWord,
				pd.NextHeader.ICMPv6NextHeader.SecondHalfWord
		}
	case api.UDP:
		if pd.NextHeader.UDPNextHeader != nil {
			return pd.NextHeader.UDPNextHeader.SourcePort,
				pd.NextHeader.UDPNextHeader.DestinationPort
		}
	}
	return 0, 0
}

// buildProbeKeyFromDirective creates a correlation key from a probe directive.
func buildProbeKeyFromDirective(pd *api.ProbingDirective, ttl uint8, timestamp int64) probeKey {
	firstHalf, secondHalf := extractHalfWords(pd)
	return probeKey{
		dstAddr:           normalizeIPAddress(pd.DestinationAddress.String()),
		firstHalfWord:     firstHalf,
		secondHalfWord:    secondHalf,
		ttl:               ttl,
		protocol:          pd.Protocol,
		correlationSecond: timestamp,
	}
}

// Probe queues a probe request and waits for the result.
//
// This function does not directly send the probe - instead it:
//  1. Registers the request in the in-flight map
//  2. Queues it to the write queue (non-blocking)
//  3. Waits for the result from the reader loop (or timeout)
//
// The actual probing happens asynchronously via writerLoop → caracal → network.
func (p *CaracalProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	resultCh := make(chan *ProbeResult, 1)
	now := time.Now()

	key := buildProbeKeyFromDirective(pd, ttl, now.Unix())

	p.inFlightMu.Lock()
	if _, exists := p.inFlight[key]; exists {
		p.inFlightMu.Unlock()
		p.logger.Warn("Duplicate probe rejected",
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("ttl", int(ttl)))
		p.metrics.DuplicateProbesTotal.Inc()
		return nil, nil
	}
	p.inFlight[key] = &inFlightProbe{
		resultCh:   resultCh,
		queuedTime: now,
	}
	p.metrics.InFlightProbes.Inc()
	p.inFlightMu.Unlock()

	defer func() {
		p.inFlightMu.Lock()
		delete(p.inFlight, key)
		p.inFlightMu.Unlock()
		p.metrics.InFlightProbes.Dec()
	}()

	req := &probeRequest{pd: pd, ttl: ttl, resultCh: resultCh}
	select {
	case p.writeQueue <- req:
		p.metrics.WriteQueueDepth.Set(float64(len(p.writeQueue)))
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	timeout := time.NewTimer(p.config.ProbeTimeout)
	defer timeout.Stop()

	select {
	case result := <-resultCh:
		return result, nil
	case <-timeout.C:
		return &ProbeResult{
			SentTime: time.Now().Add(-p.config.ProbeTimeout),
			TimedOut: true,
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// writerLoop continuously processes the write queue and sends probes to caracal.
func (p *CaracalProber) writerLoop(ctx context.Context) error {
	defer func() {
		if err := p.stdin.Close(); err != nil {
			p.logger.Error("Failed to close caracal stdin", slog.Any("err", err))
		}
	}()

	for {
		select {
		case req := <-p.writeQueue:
			if err := p.encodeAndSendProbe(req); err != nil {
				return fmt.Errorf("write error: %w", err)
			}
			p.metrics.WriteQueueDepth.Set(float64(len(p.writeQueue)))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// encodeAndSendProbe formats a probe request as CSV and writes it to caracal's stdin.
func (p *CaracalProber) encodeAndSendProbe(req *probeRequest) error {
	pd := req.pd

	firstHalfWord, secondHalfWord := extractHalfWords(pd)

	record := []string{
		pd.DestinationAddress.String(),
		strconv.Itoa(int(firstHalfWord)),
		strconv.Itoa(int(secondHalfWord)),
		strconv.Itoa(int(req.ttl)),
		protocolToString(pd.Protocol),
	}

	if err := p.csvWriter.Write(record); err != nil {
		return err
	}
	p.csvWriter.Flush()
	return p.csvWriter.Error()
}

// readerLoop continuously reads results from caracal's stdout.
func (p *CaracalProber) readerLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		record, err := p.stdout.Read()
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("caracal stdout closed")
			}
			return fmt.Errorf("read error: %w", err)
		}

		if err := p.handleResult(record); err != nil {
			p.logger.Error("Failed to handle result", slog.Any("err", err))
		}
	}
}

// handleResult parses a result and sends it to the waiting goroutine.
func (p *CaracalProber) handleResult(record []string) error {
	// CSV fields: capture_timestamp, probe_protocol, probe_src_addr, probe_dst_addr,
	// probe_src_port, probe_dst_port, probe_ttl, quoted_ttl, reply_src_addr,
	// reply_protocol, reply_icmp_type, reply_icmp_code, reply_ttl, reply_size,
	// reply_mpls_labels, rtt, round

	if len(record) < 17 {
		return fmt.Errorf("invalid CSV record: expected 17 fields, got %d", len(record))
	}

	result, err := parseProbeResult(record)
	if err != nil {
		p.logger.Warn("Skipping probe result", slog.Any("err", err))
		return nil
	}

	key, err := buildProbeKey(record)
	if err != nil {
		return err
	}

	p.metrics.ICMPReplyTotal.WithLabelValues(record[10], record[11]).Inc()
	p.matchAndDeliverResult(key, result)
	return nil
}

// parseProbeResult extracts timestamps and reply information from a CSV record.
func parseProbeResult(record []string) (*ProbeResult, error) {
	result := &ProbeResult{}

	if captureTS := record[0]; captureTS != "" {
		if ts, err := strconv.ParseInt(captureTS, 10, 64); err == nil {
			result.ReceivedTime = time.Unix(ts, 0)
		}
	}

	if rttStr := record[15]; rttStr != "" {
		if rtt, err := strconv.ParseInt(rttStr, 10, 64); err == nil {
			rttMicros := rtt * 100
			result.SentTime = result.ReceivedTime.Add(-time.Duration(rttMicros) * time.Microsecond)
		}
	}

	result.ReplyAddress = net.ParseIP(record[8])

	if result.ReceivedTime.IsZero() || result.SentTime.IsZero() {
		return nil, fmt.Errorf("missing timestamps (caracal malfunction)")
	}

	return result, nil
}

// normalizeIPAddress converts IPv4-mapped IPv6 addresses (::ffff:x.x.x.x) back to IPv4.
// This ensures consistent key matching between stored probes and caracal results.
func normalizeIPAddress(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}

	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String()
	}

	return ip.String()
}

// parseSentTime extracts the sent timestamp from a CSV record for correlation.
func parseSentTime(record []string) (time.Time, error) {
	var receivedTime time.Time
	if captureTS := record[0]; captureTS != "" {
		if ts, err := strconv.ParseInt(captureTS, 10, 64); err == nil {
			receivedTime = time.Unix(ts, 0)
		}
	}

	if receivedTime.IsZero() {
		return time.Time{}, fmt.Errorf("missing received time for correlation")
	}

	var sentTime time.Time
	if rttStr := record[15]; rttStr != "" {
		if rtt, err := strconv.ParseInt(rttStr, 10, 64); err == nil {
			rttMicros := rtt * 100
			sentTime = receivedTime.Add(-time.Duration(rttMicros) * time.Microsecond)
		}
	}

	if sentTime.IsZero() {
		return time.Time{}, fmt.Errorf("missing sent time for correlation")
	}

	return sentTime, nil
}

// buildProbeKey constructs a correlation key from the CSV record.
func buildProbeKey(record []string) (probeKey, error) {
	protocol, err := strconv.ParseUint(record[1], 10, 8)
	if err != nil {
		return probeKey{}, fmt.Errorf("invalid protocol: %s", record[1])
	}

	dstAddr := normalizeIPAddress(record[3])

	ttl, err := strconv.ParseUint(record[6], 10, 8)
	if err != nil {
		return probeKey{}, fmt.Errorf("invalid TTL: %s", record[6])
	}

	firstHalfWord, err := strconv.ParseUint(record[4], 10, 16)
	if err != nil {
		return probeKey{}, fmt.Errorf("invalid first half word: %s", record[4])
	}

	secondHalfWord, err := strconv.ParseUint(record[5], 10, 16)
	if err != nil {
		return probeKey{}, fmt.Errorf("invalid second half word: %s", record[5])
	}

	sentTime, err := parseSentTime(record)
	if err != nil {
		return probeKey{}, err
	}

	var protoType api.Protocol
	switch protocol {
	case 1:
		protoType = api.ICMP
	case 17:
		protoType = api.UDP
	case 58:
		protoType = api.ICMPv6
	default:
		return probeKey{}, fmt.Errorf("unsupported protocol: %d", protocol)
	}

	return probeKey{
		dstAddr:           dstAddr,
		firstHalfWord:     uint16(firstHalfWord),
		secondHalfWord:    uint16(secondHalfWord),
		ttl:               uint8(ttl),
		protocol:          protoType,
		correlationSecond: sentTime.Unix(),
	}, nil
}

// matchAndDeliverResult attempts to match the result with an in-flight probe and deliver it.
func (p *CaracalProber) matchAndDeliverResult(key probeKey, result *ProbeResult) {
	// Try to match with up to 2 seconds tolerance for timing variations.
	// The result has correlationSecond from caracal's timestamps (sent time).
	// The in-flight map has correlationSecond from Go's system clock (queued time).
	// Due to clock skew, NTP adjustments, and timestamp calculation differences,
	// these can differ in either direction. Search ±2 seconds to handle this.
	sentTime := key.correlationSecond

	for _, offset := range []int64{0, -1, 1, -2, 2} {
		searchKey := key
		searchKey.correlationSecond = sentTime + offset

		p.inFlightMu.RLock()
		probe, exists := p.inFlight[searchKey]
		p.inFlightMu.RUnlock()

		if exists {
			select {
			case probe.resultCh <- result:
			default:
			}
			return
		}
	}

	p.metrics.CorrelationFailuresTotal.Inc()
	p.logger.Warn("No in-flight probe found for result",
		slog.String("dest", key.dstAddr),
		slog.Int("ttl", int(key.ttl)),
		slog.String("protocol", protocolToString(key.protocol)),
		slog.Int64("sent_time", sentTime))
}

// cleanupLoop periodically removes stale probes.
func (p *CaracalProber) cleanupLoop(ctx context.Context) error {
	interval := p.config.CleanupInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.cleanupStaleProbes()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// cleanupStaleProbes removes probes older than timeout.
func (p *CaracalProber) cleanupStaleProbes() {
	now := time.Now()
	cutoff := now.Add(-p.config.ProbeTimeout - 5*time.Second)

	var cleaned int

	p.inFlightMu.Lock()
	for key, probe := range p.inFlight {
		if probe.queuedTime.Before(cutoff) {
			delete(p.inFlight, key)
			cleaned++
		}
	}
	p.inFlightMu.Unlock()

	if cleaned > 0 {
		p.metrics.StaleProbesCleanedTotal.Add(float64(cleaned))
		p.metrics.InFlightProbes.Add(float64(-cleaned))
	}
}

// logStderr logs caracal's stderr output at DEBUG level.
func (p *CaracalProber) logStderr(ctx context.Context) error {
	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		p.logger.Info(scanner.Text(), slog.String("source", "caracal"))
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stderr scan error: %w", err)
	}
	return nil
}

// Close stops caracal and cleans up.
func (p *CaracalProber) Close() error {
	p.cancel()

	err := p.g.Wait()

	if p.cmd != nil && p.cmd.Process != nil {
		if killErr := p.cmd.Process.Kill(); killErr != nil {
			p.logger.Error("Failed to kill caracal", slog.Any("err", killErr))
		}
	}

	return err
}

// protocolToString converts api.Protocol to caracal's protocol string.
func protocolToString(protocol api.Protocol) string {
	switch protocol {
	case api.ICMP:
		return "icmp"
	case api.ICMPv6:
		return "icmp6"
	case api.UDP:
		return "udp"
	default:
		return fmt.Sprintf("%d", protocol)
	}
}
