// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// caracal_prober.go
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
	"sync/atomic"
	"time"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
)

// CaracalProber implements high-throughput probing using caracal.
// It uses a pipelined architecture for maximum throughput.
type CaracalProber struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *csv.Reader
	stderr io.ReadCloser

	// UDP probe ID generation (for source port encoding)
	nextUDPID uint32

	// Pending probes
	// UDP: keyed by probe ID (from source port)
	// ICMP: keyed by tuple (dst_addr + ttl + time_second)
	pendingUDP  map[uint32]chan *ProbeResult
	pendingICMP map[icmpKey]*icmpProbe
	pendingMu   sync.RWMutex

	// Writer queue
	writeQueue chan *probeRequest

	// Error tracking
	writerErr atomic.Value
	readerErr atomic.Value

	config *Config
	done   chan struct{}
}

// icmpKey uniquely identifies an ICMP probe within a time window.
type icmpKey struct {
	dstAddr    string
	ttl        uint8
	timeSecond int64 // Unix timestamp in seconds
}

// icmpProbe tracks an ICMP probe waiting for a result.
type icmpProbe struct {
	resultCh chan *ProbeResult
	sentTime time.Time
}

// probeRequest represents a single probe to be sent.
type probeRequest struct {
	id        uint32 // Only used for UDP
	directive *api.ProbingDirective
	ttl       uint8
	resultCh  chan *ProbeResult
}

// NewCaracalProber creates and starts a caracal prober.
func NewCaracalProber(cfg *Config) (*CaracalProber, error) {
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
		return nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to start caracal: %w", err)
	}

	p := &CaracalProber{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      csv.NewReader(stdoutPipe),
		stderr:      stderr,
		nextUDPID:   1,
		pendingUDP:  make(map[uint32]chan *ProbeResult),
		pendingICMP: make(map[icmpKey]*icmpProbe),
		writeQueue:  make(chan *probeRequest, 1000),
		config:      cfg,
		done:        make(chan struct{}),
	}

	// Skip CSV header
	if _, err := p.stdout.Read(); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("failed to read CSV header: %w", err)
	}

	// Start background workers
	go p.writerLoop()
	go p.readerLoop()
	go p.logStderr()
	go p.cleanupLoop()

	return p, nil
}

// Probe sends a probe and waits for the result.
func (p *CaracalProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	if err := p.checkErrors(); err != nil {
		return nil, err
	}

	resultCh := make(chan *ProbeResult, 1)

	var probeID uint32
	var cleanupFunc func()

	if pd.Protocol == api.UDP {
		// UDP: Use source port for perfect correlation
		probeID = atomic.AddUint32(&p.nextUDPID, 1)

		p.pendingMu.Lock()
		p.pendingUDP[probeID] = resultCh
		p.pendingMu.Unlock()

		cleanupFunc = func() {
			p.pendingMu.Lock()
			delete(p.pendingUDP, probeID)
			p.pendingMu.Unlock()
		}

	} else {
		// ICMP: Use tuple matching
		now := time.Now()
		key := icmpKey{
			dstAddr:    pd.DestinationAddress.String(),
			ttl:        ttl,
			timeSecond: now.Unix(),
		}

		p.pendingMu.Lock()
		// Check if duplicate probe already pending in same second
		if existing, exists := p.pendingICMP[key]; exists {
			p.pendingMu.Unlock()
			// Wait for existing probe to complete (share result)
			return <-existing.resultCh, nil
		}

		p.pendingICMP[key] = &icmpProbe{
			resultCh: resultCh,
			sentTime: now,
		}
		p.pendingMu.Unlock()

		cleanupFunc = func() {
			p.pendingMu.Lock()
			delete(p.pendingICMP, key)
			p.pendingMu.Unlock()
		}
	}

	defer cleanupFunc()

	// Queue probe for writing
	req := &probeRequest{
		id:        probeID,
		directive: pd,
		ttl:       ttl,
		resultCh:  resultCh,
	}

	select {
	case p.writeQueue <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, fmt.Errorf("prober is closed")
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
	case <-p.done:
		return nil, fmt.Errorf("prober is closed")
	}
}

// writerLoop continuously writes probe specs to caracal's stdin.
func (p *CaracalProber) writerLoop() {
	defer p.stdin.Close()

	for {
		select {
		case req := <-p.writeQueue:
			if err := p.writeProbe(req); err != nil {
				p.writerErr.Store(err)
				log.Printf("writer error: %v", err)
				return
			}
		case <-p.done:
			return
		}
	}
}

// writeProbe writes a single probe spec to caracal's stdin.
func (p *CaracalProber) writeProbe(req *probeRequest) error {
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
		// Encode probe ID in source port
		basePort := 50000
		maxConcurrent := 10000
		srcPort := basePort + int(req.id%uint32(maxConcurrent))

		var dstPort uint16
		if pd.NextHeader.UDPNextHeader != nil {
			dstPort = pd.NextHeader.UDPNextHeader.DestinationPort
		} else {
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
func (p *CaracalProber) readerLoop() {
	for {
		select {
		case <-p.done:
			return
		default:
		}

		record, err := p.stdout.Read()
		if err != nil {
			if err == io.EOF {
				p.readerErr.Store(fmt.Errorf("caracal stdout closed"))
			} else {
				p.readerErr.Store(fmt.Errorf("read error: %w", err))
			}
			return
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

	// Parse result
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

	// Route result based on protocol
	if protocol == 17 { // UDP
		return p.handleUDPResult(record, result)
	} else if protocol == 1 || protocol == 58 { // ICMP or ICMPv6
		return p.handleICMPResult(record, result)
	}

	return fmt.Errorf("unsupported protocol: %d", protocol)
}

// handleUDPResult routes UDP result using source port.
func (p *CaracalProber) handleUDPResult(record []string, result *ProbeResult) error {
	srcPort, err := strconv.ParseUint(record[4], 10, 16)
	if err != nil {
		return fmt.Errorf("invalid UDP source port: %s", record[4])
	}

	basePort := uint32(50000)
	maxConcurrent := uint32(10000)
	probeID := ((uint32(srcPort) - basePort) % maxConcurrent)
	if probeID == 0 {
		probeID = maxConcurrent
	}

	p.pendingMu.RLock()
	resultCh, exists := p.pendingUDP[probeID]
	p.pendingMu.RUnlock()

	if exists {
		select {
		case resultCh <- result:
		default:
		}
	}

	return nil
}

// handleICMPResult routes ICMP result using tuple matching.
func (p *CaracalProber) handleICMPResult(record []string, result *ProbeResult) error {
	dstAddr := record[3]
	ttl, err := strconv.ParseUint(record[6], 10, 8)
	if err != nil {
		return fmt.Errorf("invalid TTL: %s", record[6])
	}

	timeSecond := result.SentTime.Unix()

	for _, ts := range []int64{timeSecond, timeSecond - 1} {
		key := icmpKey{
			dstAddr:    dstAddr,
			ttl:        uint8(ttl),
			timeSecond: ts,
		}

		p.pendingMu.RLock()
		probe, exists := p.pendingICMP[key]
		p.pendingMu.RUnlock()

		if exists {
			select {
			case probe.resultCh <- result:
			default:
			}
			return nil
		}
	}

	return nil
}

// cleanupLoop periodically removes stale ICMP probes.
func (p *CaracalProber) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.cleanupStaleICMP()
		case <-p.done:
			return
		}
	}
}

// cleanupStaleICMP removes ICMP probes older than timeout.
func (p *CaracalProber) cleanupStaleICMP() {
	now := time.Now()
	cutoff := now.Add(-p.config.ProbeTimeout - 5*time.Second)

	p.pendingMu.Lock()
	for key, probe := range p.pendingICMP {
		if probe.sentTime.Before(cutoff) {
			delete(p.pendingICMP, key)
		}
	}
	p.pendingMu.Unlock()
}

// checkErrors returns any errors from background goroutines.
func (p *CaracalProber) checkErrors() error {
	if err := p.writerErr.Load(); err != nil {
		return fmt.Errorf("writer error: %w", err.(error))
	}
	if err := p.readerErr.Load(); err != nil {
		return fmt.Errorf("reader error: %w", err.(error))
	}
	return nil
}

// logStderr logs caracal's stderr.
func (p *CaracalProber) logStderr() {
	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		log.Printf("caracal: %s", scanner.Text())
	}
}

// Close stops caracal and cleans up.
func (p *CaracalProber) Close() error {
	close(p.done)
	p.stdin.Close()

	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		if err := p.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill caracal: %w", err)
		}
		return fmt.Errorf("caracal did not exit gracefully, killed")
	}
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
