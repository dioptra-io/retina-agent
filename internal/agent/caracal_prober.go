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
//   - Correlates results with pending probes using a shared map
//   - Delivers results to waiting caller goroutines
//
// 4. cleanupLoop goroutine (one):
//   - Periodically removes stale/timed-out probes from the pending map
//
// 5. logStderr goroutine (one):
//   - Logs caracal's stderr output for debugging
//
// Correlation mechanism:
//   - Each probe is identified by (dst_addr, dst_port, ttl, time_second)
//   - When Probe() is called, an entry is added to the pending map with this key
//   - When a result arrives, the key is reconstructed from the result
//   - A ±1 second time tolerance handles clock drift
//   - ASSUMPTION: Probes are sent within ~1 second of being queued
//     (if writeQueue backpressure causes >1 second delay, correlation may fail)
//
// Deduplication:
//   - If multiple callers request identical probes in the same second,
//     only one probe is sent and all callers share the result
package agent

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
	"golang.org/x/sync/errgroup"
)

// CaracalProber implements high-throughput probing using caracal.
// It uses a pipelined architecture for maximum throughput.
type CaracalProber struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *csv.Reader
	stderr io.ReadCloser

	// Pending probes - unified for all protocols
	// Keyed by tuple (dst_addr + dst_port + ttl + time_second)
	pending   map[probeKey]*pendingProbe
	pendingMu sync.RWMutex

	// Writer queue
	writeQueue chan *probeRequest

	config *Config
	cancel context.CancelFunc
	g      *errgroup.Group
}

// probeKey uniquely identifies a probe within a time window.
type probeKey struct {
	dstAddr    string
	dstPort    uint16 // 0 for ICMP
	ttl        uint8
	timeSecond int64 // Unix timestamp in seconds
}

// pendingProbe tracks a probe waiting for a result.
type pendingProbe struct {
	resultCh chan *ProbeResult
	sentTime time.Time
}

// probeRequest represents a single probe to be sent.
type probeRequest struct {
	directive *api.ProbingDirective
	ttl       uint8
	resultCh  chan *ProbeResult
}

// NewCaracalProber creates and starts a caracal prober.
func NewCaracalProber(cfg *Config) (*CaracalProber, error) {
	cmd, stdin, stdout, stderr, err := setupCaracalProcess(cfg)
	if err != nil {
		return nil, err
	}

	// Create context and errgroup
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	p := &CaracalProber{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     csv.NewReader(stdout),
		stderr:     stderr,
		pending:    make(map[probeKey]*pendingProbe),
		writeQueue: make(chan *probeRequest, 1000),
		config:     cfg,
		cancel:     cancel,
		g:          g,
	}

	// Skip CSV header
	if _, err := p.stdout.Read(); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.Printf("failed to kill caracal: %v", killErr)
		}
		cancel()
		return nil, fmt.Errorf("failed to read CSV header: %w", err)
	}

	// Start all workers in errgroup
	g.Go(func() error { return p.writerLoop(ctx) })
	g.Go(func() error { return p.readerLoop(ctx) })
	g.Go(func() error { return p.logStderr(ctx) })
	g.Go(func() error { return p.cleanupLoop(ctx) })

	return p, nil
}

// setupCaracalProcess creates and starts the caracal subprocess with all pipes configured.
func setupCaracalProcess(cfg *Config) (*exec.Cmd, io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	// Use ProberPath as caracal executable path
	caracalPath := cfg.ProberPath
	if caracalPath == "" {
		caracalPath = "caracal" // Default: search in PATH
	}

	cmd := exec.Command(caracalPath,
		"--probing-rate", "100000",
		"--sniffer-wait-time", fmt.Sprintf("%d", int(cfg.ProbeTimeout.Seconds())),
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			log.Printf("failed to close stdin: %v", closeErr)
		}
		return nil, nil, nil, nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			log.Printf("failed to close stdin: %v", closeErr)
		}
		return nil, nil, nil, nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			log.Printf("failed to close stdin: %v", closeErr)
		}
		return nil, nil, nil, nil, fmt.Errorf("failed to start caracal: %w", err)
	}

	return cmd, stdin, stdoutPipe, stderr, nil
}

// Probe sends a probe and waits for the result.
func (p *CaracalProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	resultCh := make(chan *ProbeResult, 1)
	now := time.Now()

	// Build key for correlation
	key := probeKey{
		dstAddr:    pd.DestinationAddress.String(),
		ttl:        ttl,
		timeSecond: now.Unix(),
	}

	// Add destination port for UDP
	if pd.Protocol == api.UDP && pd.NextHeader.UDPNextHeader != nil {
		key.dstPort = pd.NextHeader.UDPNextHeader.DestinationPort
	}

	p.pendingMu.Lock()

	// Check if duplicate probe already pending in same second
	if existing, exists := p.pending[key]; exists {
		p.pendingMu.Unlock()
		// Wait for existing probe to complete (share result)
		return <-existing.resultCh, nil
	}

	p.pending[key] = &pendingProbe{
		resultCh: resultCh,
		sentTime: now,
	}
	p.pendingMu.Unlock()

	// Cleanup function
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, key)
		p.pendingMu.Unlock()
	}()

	// Queue probe for writing
	req := &probeRequest{
		directive: pd,
		ttl:       ttl,
		resultCh:  resultCh,
	}

	select {
	case p.writeQueue <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Wait for result with timeout
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
			log.Printf("failed to close stdin: %v", err)
		}
	}()

	for {
		select {
		case req := <-p.writeQueue:
			if err := p.encodeAndSendProbe(req); err != nil {
				log.Printf("writer error: %v", err)
				return fmt.Errorf("write error: %w", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// encodeAndSendProbe formats a probe request as CSV and writes it to caracal's stdin.
func (p *CaracalProber) encodeAndSendProbe(req *probeRequest) error {
	pd := req.directive

	// CSV format: dst_addr,src_port,dst_port,ttl,protocol
	var csvLine string

	switch pd.Protocol {
	case api.ICMP, api.ICMPv6:
		csvLine = fmt.Sprintf("%s,0,0,%d,%s\n",
			pd.DestinationAddress.String(),
			req.ttl,
			protocolToString(pd.Protocol),
		)

	case api.UDP:
		var srcPort, dstPort uint16

		if pd.NextHeader.UDPNextHeader != nil {
			srcPort = pd.NextHeader.UDPNextHeader.SourcePort
			dstPort = pd.NextHeader.UDPNextHeader.DestinationPort
		}

		// Defaults
		if srcPort == 0 {
			srcPort = 50000
		}
		if dstPort == 0 {
			dstPort = 33434
		}

		csvLine = fmt.Sprintf("%s,%d,%d,%d,UDP\n",
			pd.DestinationAddress.String(),
			srcPort,
			dstPort,
			req.ttl,
		)

	default:
		return fmt.Errorf("unsupported protocol: %d", pd.Protocol)
	}

	_, err := p.stdin.Write([]byte(csvLine))
	return err
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
			log.Printf("failed to handle result: %v", err)
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

	protocol, err := strconv.ParseUint(record[1], 10, 8)
	if err != nil {
		return fmt.Errorf("invalid protocol: %s", record[1])
	}

	result := parseProbeResult(record)

	key, err := buildProbeKey(record, protocol, result)
	if err != nil {
		return err
	}

	p.matchAndDeliverResult(key, result)
	return nil
}

// parseProbeResult extracts timestamps and reply information from a CSV record.
func parseProbeResult(record []string) *ProbeResult {
	result := &ProbeResult{}

	// Parse timestamps
	if captureTS := record[0]; captureTS != "" {
		if ts, err := strconv.ParseInt(captureTS, 10, 64); err == nil {
			// Timestamp in microseconds
			result.ReceivedTime = time.Unix(0, ts*1000)
		}
	}

	// Parse RTT to calculate sent time
	if rttStr := record[15]; rttStr != "" {
		if rtt, err := strconv.ParseInt(rttStr, 10, 64); err == nil {
			// RTT in tenths of milliseconds = 100 microseconds
			rttMicros := rtt * 100
			result.SentTime = result.ReceivedTime.Add(-time.Duration(rttMicros) * time.Microsecond)
		}
	}

	// Parse reply address
	replyAddrStr := record[8]
	if replyAddrStr == "" {
		result.TimedOut = true
	} else {
		result.ReplyAddress = net.ParseIP(replyAddrStr)
	}

	// Fallback timestamps
	if result.ReceivedTime.IsZero() {
		result.ReceivedTime = time.Now()
	}
	if result.SentTime.IsZero() {
		result.SentTime = result.ReceivedTime.Add(-100 * time.Millisecond)
	}

	return result
}

// buildProbeKey constructs a correlation key from the CSV record.
func buildProbeKey(record []string, protocol uint64, result *ProbeResult) (probeKey, error) {
	dstAddr := record[3]
	ttl, err := strconv.ParseUint(record[6], 10, 8)
	if err != nil {
		return probeKey{}, fmt.Errorf("invalid TTL: %s", record[6])
	}

	key := probeKey{
		dstAddr:    dstAddr,
		ttl:        uint8(ttl),
		timeSecond: result.SentTime.Unix(),
	}

	// Add destination port for UDP
	if protocol == 17 { // UDP
		dstPort, err := strconv.ParseUint(record[5], 10, 16)
		if err != nil {
			return probeKey{}, fmt.Errorf("invalid UDP destination port: %s", record[5])
		}
		key.dstPort = uint16(dstPort)
	}

	return key, nil
}

// matchAndDeliverResult attempts to match the result with a pending probe and deliver it.
func (p *CaracalProber) matchAndDeliverResult(key probeKey, result *ProbeResult) {
	// Try to match (with ±1 second tolerance for clock drift)
	for _, ts := range []int64{key.timeSecond, key.timeSecond - 1} {
		key.timeSecond = ts

		p.pendingMu.RLock()
		probe, exists := p.pending[key]
		p.pendingMu.RUnlock()

		if exists {
			select {
			case probe.resultCh <- result:
			default:
			}
			return
		}
	}
}

// cleanupLoop periodically removes stale probes.
func (p *CaracalProber) cleanupLoop(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Second)
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

	p.pendingMu.Lock()
	for key, probe := range p.pending {
		if probe.sentTime.Before(cutoff) {
			delete(p.pending, key)
		}
	}
	p.pendingMu.Unlock()
}

// logStderr logs caracal's stderr.
func (p *CaracalProber) logStderr(ctx context.Context) error {
	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		log.Printf("caracal: %s", scanner.Text())
	}

	// EOF is normal when caracal exits
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stderr scan error: %w", err)
	}
	return nil
}

// Close stops caracal and cleans up.
func (p *CaracalProber) Close() error {
	// Cancel context (stops all goroutines)
	p.cancel()

	// Wait for all goroutines to finish
	err := p.g.Wait()

	// Kill caracal process
	if killErr := p.cmd.Process.Kill(); killErr != nil {
		log.Printf("failed to kill caracal: %v", killErr)
	}

	return err
}

// protocolToString converts api.Protocol to caracal's protocol string.
func protocolToString(protocol api.Protocol) string {
	switch protocol {
	case api.ICMP:
		return "ICMP"
	case api.ICMPv6:
		return "ICMPv6"
	case api.UDP:
		return "UDP"
	default:
		return fmt.Sprintf("%d", protocol)
	}
}
