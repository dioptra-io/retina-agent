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
//   - Each probe is identified by (dst_addr, first_half_word, second_half_word, ttl, protocol, time_second)
//   - When Probe() is called, an entry is added to the pending map with this key
//   - When a result arrives, the key is reconstructed from the result
//   - A ±2 second time tolerance handles queue delays and clock variations
//   - ASSUMPTION: Probes are sent within ~2 seconds of being queued
//     (if writeQueue backpressure causes >2 second delay, correlation may fail)
//
// Deduplication:
//   - If multiple callers request identical probes in the same second,
//     the first request wins and subsequent requests return an error
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
	// Caracal subprocess management.
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	csvWriter *csv.Writer
	stdout    *csv.Reader
	stderr    io.ReadCloser

	// Probe correlation - maps sent probes to waiting goroutines.
	pending   map[probeKey]*pendingProbe
	pendingMu sync.RWMutex

	// Non-blocking write pipeline.
	writeQueue chan *probeRequest

	// Configuration and lifecycle management.
	config *Config
	cancel context.CancelFunc
	g      *errgroup.Group
}

// probeKey uniquely identifies a probe within a time window.
type probeKey struct {
	dstAddr        string
	firstHalfWord  uint16 // FirstHalfWord for ICMP/ICMPv6, SourcePort for UDP
	secondHalfWord uint16 // SecondHalfWord for ICMP/ICMPv6, DestinationPort for UDP
	ttl            uint8
	protocol       api.Protocol // Protocol type (ICMP, ICMPv6, UDP)
	timeSecond     int64        // Unix timestamp in seconds
}

// pendingProbe tracks a probe waiting for a result.
type pendingProbe struct {
	resultCh chan *ProbeResult
	sentTime time.Time
}

// probeRequest represents a single probe to be sent to caracal.
type probeRequest struct {
	pd       *api.ProbingDirective
	ttl      uint8
	resultCh chan *ProbeResult
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
		csvWriter:  csv.NewWriter(stdin),
		stdout:     csv.NewReader(stdout),
		stderr:     stderr,
		pending:    make(map[probeKey]*pendingProbe),
		writeQueue: make(chan *probeRequest, 1000),
		config:     cfg,
		cancel:     cancel,
		g:          g,
	}

	// Skip CSV header.
	if _, err := p.stdout.Read(); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.Printf("failed to kill caracal: %v", killErr)
		}
		cancel()
		return nil, fmt.Errorf("failed to read CSV header: %w", err)
	}

	// Start all workers in errgroup.
	g.Go(func() error { return p.writerLoop(ctx) })
	g.Go(func() error { return p.readerLoop(ctx) })
	g.Go(func() error { return p.logStderr(ctx) })
	g.Go(func() error { return p.cleanupLoop(ctx) })

	return p, nil
}

// setupCaracalProcess creates and starts the caracal subprocess with all pipes configured.
//
// Uses cfg.ProberPath to locate the caracal executable (defaults to searching PATH).
// Runs caracal with --probing-rate 100000 by default.
//
// Custom caracal arguments can be added via cfg.ProberArgs.
// Example: cfg.ProberArgs = []string{"--n-packets", "3", "--interface", "eth0"}
// Note: ProberArgs is not exposed as a command-line flag; set it programmatically if needed.
func setupCaracalProcess(cfg *Config) (*exec.Cmd, io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	// Use ProberPath as caracal executable path.
	caracalPath := cfg.ProberPath
	if caracalPath == "" {
		caracalPath = "caracal" // Default: search in PATH.
	}

	// Base args.
	args := []string{"--probing-rate", "100000"}

	// Add custom args if provided.
	if len(cfg.ProberArgs) > 0 {
		args = append(args, cfg.ProberArgs...)
	}

	cmd := exec.Command(caracalPath, args...) // #nosec G204 -- caracalPath is user-controlled by design (ProberPath config)

	// Track pipes for cleanup on error.
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	var stderr io.ReadCloser
	success := false

	// Cleanup pipes if function fails.
	defer func() {
		if !success {
			closePipe(stdin, "stdin")
			closePipe(stdout, "stdout")
			closePipe(stderr, "stderr")
		}
	}()

	// Create pipes.
	var err error
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

	// Start caracal.
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to start caracal: %w", err)
	}

	success = true
	return cmd, stdin, stdout, stderr, nil
}

// closePipe safely closes a pipe and logs errors.
func closePipe(pipe io.Closer, name string) {
	if pipe != nil {
		if err := pipe.Close(); err != nil {
			log.Printf("Failed to close %s: %v", name, err)
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

// Probe queues a probe request and waits for the result.
//
// This function does not directly send the probe - instead it:
//  1. Registers the request in the pending map
//  2. Queues it to the write queue (non-blocking)
//  3. Waits for the result from the reader loop (or timeout)
//
// The actual probing happens asynchronously via writerLoop → caracal → network.
func (p *CaracalProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	resultCh := make(chan *ProbeResult, 1)
	now := time.Now()

	// Build key for correlation.
	firstHalf, secondHalf := extractHalfWords(pd)
	key := probeKey{
		dstAddr:        normalizeIPAddress(pd.DestinationAddress.String()),
		firstHalfWord:  firstHalf,
		secondHalfWord: secondHalf,
		ttl:            ttl,
		protocol:       pd.Protocol,
		timeSecond:     now.Unix(),
	}

	p.pendingMu.Lock()

	// Check for duplicate probe already pending.
	if _, exists := p.pending[key]; exists {
		p.pendingMu.Unlock()
		return nil, fmt.Errorf("duplicate probe already pending for %s (TTL %d)",
			pd.DestinationAddress.String(), ttl)
	}

	// Register probe.
	p.pending[key] = &pendingProbe{
		resultCh: resultCh,
		sentTime: now,
	}

	p.pendingMu.Unlock()

	// Cleanup function.
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, key)
		p.pendingMu.Unlock()
	}()

	// Queue probe for writing.
	req := &probeRequest{
		pd:       pd,
		ttl:      ttl,
		resultCh: resultCh,
	}

	select {
	case p.writeQueue <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Wait for result with timeout.
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
	pd := req.pd

	// CSV format: dst_addr,src_port,dst_port,ttl,protocol
	firstHalfWord, secondHalfWord := extractHalfWords(pd)

	// Apply UDP defaults if needed.
	if pd.Protocol == api.UDP {
		if firstHalfWord == 0 {
			firstHalfWord = 50000
		}
		if secondHalfWord == 0 {
			secondHalfWord = 33434
		}
	}

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

	// Skip protocol 0 (caracal sometimes outputs this for invalid/malformed packets).
	if protocol == 0 {
		log.Printf("Skipping result with protocol 0 (likely invalid packet)")
		return nil
	}

	result, err := parseProbeResult(record)
	if err != nil {
		log.Printf("Skipping probe result: %v", err)
		return nil // Skip this result but continue processing others
	}

	key, err := buildProbeKey(record, protocol, result)
	if err != nil {
		return err
	}

	p.matchAndDeliverResult(key, result)
	return nil
}

// parseProbeResult extracts timestamps and reply information from a CSV record.
func parseProbeResult(record []string) (*ProbeResult, error) {
	result := &ProbeResult{}

	// Parse timestamps.
	if captureTS := record[0]; captureTS != "" {
		if ts, err := strconv.ParseInt(captureTS, 10, 64); err == nil {
			// Timestamp in seconds (Unix timestamp).
			result.ReceivedTime = time.Unix(ts, 0)
		}
	}

	// Parse RTT to calculate sent time.
	if rttStr := record[15]; rttStr != "" {
		if rtt, err := strconv.ParseInt(rttStr, 10, 64); err == nil {
			// RTT in tenths of milliseconds = 100 microseconds.
			rttMicros := rtt * 100
			result.SentTime = result.ReceivedTime.Add(-time.Duration(rttMicros) * time.Microsecond)
		}
	}

	// Parse reply address (always present if caracal outputs the line).
	result.ReplyAddress = net.ParseIP(record[8])

	// Validate timestamps - missing timestamps indicate caracal malfunction.
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
		return addr // Return as-is if not parseable
	}

	// Convert IPv4-mapped IPv6 to IPv4
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String()
	}

	return ip.String()
}

// buildProbeKey constructs a correlation key from the CSV record.
func buildProbeKey(record []string, protocol uint64, result *ProbeResult) (probeKey, error) {
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

	// Convert protocol number to api.Protocol type.
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

	key := probeKey{
		dstAddr:        dstAddr,
		firstHalfWord:  uint16(firstHalfWord),
		secondHalfWord: uint16(secondHalfWord),
		ttl:            uint8(ttl),
		protocol:       protoType,
		timeSecond:     result.SentTime.Unix(),
	}

	return key, nil
}

// matchAndDeliverResult attempts to match the result with a pending probe and deliver it.
func (p *CaracalProber) matchAndDeliverResult(key probeKey, result *ProbeResult) {
	// Try to match with up to 2 seconds tolerance for queue delay.
	// Result has timeSecond from when probe was sent (e.g., 100).
	// Pending map has timeSecond from when probe was queued (e.g., 98-102).
	// Try exact match, then check up to 2 seconds in either direction.
	originalTime := key.timeSecond
	for _, offset := range []int64{0, -1, 1, -2, 2} {
		key.timeSecond = originalTime + offset

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

	// Log when we can't match the result (correlation failure).
	log.Printf("No pending probe found for result: dst=%s ttl=%d protocol=%d time=%d",
		key.dstAddr, key.ttl, key.protocol, originalTime)
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

	// EOF is normal when caracal exits.
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stderr scan error: %w", err)
	}
	return nil
}

// Close stops caracal and cleans up.
func (p *CaracalProber) Close() error {
	// Cancel context (stops all goroutines).
	p.cancel()

	// Wait for all goroutines to finish.
	err := p.g.Wait()

	// Kill caracal process.
	if killErr := p.cmd.Process.Kill(); killErr != nil {
		log.Printf("failed to kill caracal: %v", killErr)
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
