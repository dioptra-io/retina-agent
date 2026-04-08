// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// caracal_prober.go
//
// CaracalProber uses a pipelined architecture: caller goroutines queue probe
// requests via writeQueue, writerLoop serializes them to caracal's stdin, and
// readerLoop reads results from stdout and delivers them back to callers.
//
// Correlation: each probe is keyed by (dst_addr, first_half_word, second_half_word,
// ttl, protocol, unix_second). Results are matched with ±2 seconds tolerance to
// handle queue delays and clock variations. Probes must be sent within ~2 seconds
// of being queued or correlation may fail.
//
// Deduplication: a duplicate probe (same key, already in-flight) is rejected and
// returns nil. With proper directive randomization, duplicates should be rare.
package agent

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
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

// caracalRTTUnit is the duration of one RTT unit as reported by caracal (1/10 of a millisecond).
// See: caracal documentation, field `rtt` is a 16-bit integer in units of 0.1ms.
const caracalRTTUnit = 100 * time.Microsecond

type caracalProber struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	csvWriter *csv.Writer
	stdout    *csv.Reader
	stderr    io.ReadCloser

	// inFlight maps queued probes to their result channels; also enforces deduplication.
	inFlight   map[probeKey]*inFlightProbe
	inFlightMu sync.RWMutex

	writeQueue chan *probeRequest

	config  *Config
	logger  *slog.Logger
	metrics *Metrics
	cancel  context.CancelFunc
	g       *errgroup.Group
}

var _ (Prober) = (*caracalProber)(nil)

// probeKey uniquely identifies a probe within a time window.
type probeKey struct {
	dstAddr           string
	firstHalfWord     uint16 // FirstHalfWord for ICMP/ICMPv6, SourcePort for UDP
	secondHalfWord    uint16 // SecondHalfWord for ICMP/ICMPv6, DestinationPort for UDP
	ttl               uint8
	protocol          api.Protocol
	correlationSecond int64 // Unix timestamp truncated to seconds; matched with ±2s tolerance in readerLoop
}

type inFlightProbe struct {
	resultCh   chan *ProbeResult
	queuedTime time.Time
}

type probeRequest struct {
	pd       *api.ProbingDirective
	ttl      uint8
	resultCh chan *ProbeResult
}

// NewCaracalProber is a package-level variable to allow overriding in tests.
// Note: tests that override this cannot run in parallel.
var NewCaracalProber = func(cfg *Config, logger *slog.Logger, metrics *Metrics) (*caracalProber, error) {
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

	p := &caracalProber{
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

	if _, err := p.stdout.Read(); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			logger.Error("Failed to kill caracal", slog.Any("err", killErr))
		}
		cancel()
		return nil, fmt.Errorf("failed to read CSV header: %w", err)
	}

	g.Go(func() error { return p.writerLoop(ctx) })
	g.Go(func() error { return p.readerLoop() })
	g.Go(func() error { return p.logStderr() })
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

	cmd = exec.Command(caracalPath, cfg.ProberArgs...) // #nosec G204 -- caracalPath is user-controlled by design (ProberPath config)

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

func closePipe(pipe io.Closer, name string, logger *slog.Logger) {
	if pipe != nil {
		if err := pipe.Close(); err != nil {
			logger.Error("Failed to close pipe", slog.String("name", name), slog.Any("err", err))
		}
	}
}

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

// Probe sends a single probe and blocks until a result arrives, the probe times
// out, or ctx is cancelled. The actual send happens asynchronously via
// writerLoop → caracal → network.
//
// Returns nil, nil if an identical probe is already in-flight (deduplication).
// Returns a ProbeResult with TimedOut=true on timeout; SentTime is approximated
// as queue time and may differ from actual send time under backpressure.
// Returns a non-nil error only if ctx is cancelled.
func (p *caracalProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	resultCh := make(chan *ProbeResult, 1)
	now := time.Now()
	key := buildProbeKeyFromDirective(pd, ttl, now.Unix())

	// Register in-flight entry, rejecting duplicates.
	p.inFlightMu.Lock()
	if _, exists := p.inFlight[key]; exists {
		p.inFlightMu.Unlock()
		p.logger.Warn("Duplicate probe rejected",
			slog.String("dest", pd.DestinationAddress.String()),
			slog.Int("ttl", int(ttl)))
		p.metrics.DuplicateProbesTotal.Inc()
		return nil, ErrDuplicatePD
	}
	p.inFlight[key] = &inFlightProbe{resultCh: resultCh, queuedTime: now}
	p.metrics.InFlightProbes.Inc()
	p.inFlightMu.Unlock()

	defer func() {
		p.inFlightMu.Lock()
		_, stillPresent := p.inFlight[key]
		delete(p.inFlight, key)
		p.inFlightMu.Unlock()
		// Only decrement if we are the ones removing the key;
		// cleanupStaleProbes may have already removed and decremented it.
		if stillPresent {
			p.metrics.InFlightProbes.Dec()
		}
	}()

	// Queue the probe for writerLoop to send.
	select {
	case p.writeQueue <- &probeRequest{pd: pd, ttl: ttl, resultCh: resultCh}:
		p.metrics.WriteQueueDepth.Set(float64(len(p.writeQueue)))
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Wait for readerLoop to deliver the result.
	timeout := time.NewTimer(p.config.ProbeTimeout)
	defer timeout.Stop()

	select {
	case result := <-resultCh:
		return result, nil
	case <-timeout.C:
		// SentTime is approximated as queue time; actual send time may be
		// later under writerLoop backpressure.
		return &ProbeResult{SentTime: now, TimedOut: true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *caracalProber) writerLoop(ctx context.Context) error {
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

func (p *caracalProber) encodeAndSendProbe(req *probeRequest) error {
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

// Cancellation is handled by Close() killing the caracal process, which causes
// stdout.Read() to return EOF.
func (p *caracalProber) readerLoop() error {
	for {
		record, err := p.stdout.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("caracal stdout closed")
			}
			return fmt.Errorf("read error: %w", err)
		}

		if err := p.handleResult(record); err != nil {
			p.logger.Error("Failed to handle result", slog.Any("err", err))
		}
	}
}

// handleResult processes a single caracal CSV output record and delivers the
// parsed probe result to the waiting Probe() goroutine.
//
// Expected CSV fields (0-indexed):
//
//	[0]  capture_timestamp   - Unix timestamp of reply capture
//	[1]  probe_protocol      - IP protocol number (1=ICMP, 17=UDP, 58=ICMPv6)
//	[2]  probe_src_addr      - Source address of the probe
//	[3]  probe_dst_addr      - Destination address of the probe
//	[4]  probe_src_port      - Source port (or ICMP first half-word)
//	[5]  probe_dst_port      - Destination port (or ICMP second half-word)
//	[6]  probe_ttl           - TTL of the probe
//	[7]  quoted_ttl          - TTL quoted back in ICMP reply
//	[8]  reply_src_addr      - Address that sent the reply
//	[9]  reply_protocol      - Protocol of the reply
//	[10] reply_icmp_type     - ICMP type of the reply
//	[11] reply_icmp_code     - ICMP code of the reply
//	[12] reply_ttl           - TTL of the reply packet
//	[13] reply_size          - Size of the reply packet
//	[14] reply_mpls_labels   - MPLS labels (may be empty)
//	[15] rtt                 - Round-trip time in units of 0.1ms
//	[16] round               - Probe round number
func (p *caracalProber) handleResult(record []string) error {
	if len(record) < 17 {
		return fmt.Errorf("invalid CSV record: expected 17 fields, got %d", len(record))
	}

	result, err := parseProbeResult(record)
	if err != nil {
		p.logger.Warn("Skipping probe result", slog.Any("err", err))
		return nil
	}

	key, err := buildProbeKey(record, result.SentTime)
	if err != nil {
		return err
	}

	p.metrics.ICMPReplyTotal.WithLabelValues(record[10], record[11]).Inc()
	p.matchAndDeliverResult(key, result)
	return nil
}

func parseProbeResult(record []string) (*ProbeResult, error) {
	result := &ProbeResult{}

	if captureTS := record[0]; captureTS != "" {
		if ts, err := strconv.ParseInt(captureTS, 10, 64); err == nil {
			result.ReceivedTime = time.Unix(ts, 0)
		}
	}

	if rttStr := record[15]; rttStr != "" {
		if rtt, err := strconv.ParseInt(rttStr, 10, 64); err == nil {
			result.SentTime = result.ReceivedTime.Add(-time.Duration(rtt) * caracalRTTUnit)
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

// buildProbeKey constructs a correlation key from the CSV record.
// Note: caracal accepts protocol as a string (e.g. "icmp") but reports it back
// as a numeric IP protocol number (1=ICMP, 17=UDP, 58=ICMPv6).
func buildProbeKey(record []string, sentTime time.Time) (probeKey, error) {
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

// matchAndDeliverResult finds the in-flight probe matching key and sends it the
// result. It searches ±2 seconds around the correlation timestamp to tolerate
// clock skew between Go's system clock and caracal's reported timestamps.
// If the channel is full or no match is found, the result is dropped.
func (p *caracalProber) matchAndDeliverResult(key probeKey, result *ProbeResult) {
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

func (p *caracalProber) cleanupLoop(ctx context.Context) error {
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

func (p *caracalProber) cleanupStaleProbes() {
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

// Cancellation is handled by Close() killing the caracal process, which causes
// the scanner to stop.
func (p *caracalProber) logStderr() error {
	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		p.logger.Info(scanner.Text(), slog.String("source", "caracal"))
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stderr scan error: %w", err)
	}
	return nil
}

func (p *caracalProber) Close() error {
	p.cancel()
	if p.cmd != nil && p.cmd.Process != nil {
		if killErr := p.cmd.Process.Kill(); killErr != nil {
			p.logger.Error("Failed to kill caracal", slog.Any("err", killErr))
		}
	}
	return p.g.Wait()
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
		return strconv.Itoa(int(protocol))
	}
}
