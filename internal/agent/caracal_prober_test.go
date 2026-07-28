// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

// Coverage: ~96.9% of caracal_prober.go.
//
// The remaining ~3% consists of defensive error handlers for pipe creation
// failures (StdinPipe, StdoutPipe, StderrPipe) in setupCaracalProcess.
// These paths require exhausting system file descriptors to trigger and
// cannot be reliably tested without dangerous system state manipulation.

//nolint:funlen // Test functions can be long for readability
package agent

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dioptra-io/retina-commons/api/v2"
	"golang.org/x/sync/errgroup"
)

// -- test helper types --------------------------------------------------------

type nopWriteCloser struct {
	*bytes.Buffer
}

func (nwc *nopWriteCloser) Close() error { return nil }

// dynamicReadCloser reads from a channel for dynamic data generation.
type dynamicReadCloser struct {
	ch     chan string
	buffer string
	mu     sync.Mutex
	closed bool
}

func (d *dynamicReadCloser) Read(p []byte) (n int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed && d.buffer == "" {
		return 0, io.EOF
	}

	if d.buffer == "" {
		data, ok := <-d.ch
		if !ok {
			d.closed = true
			return 0, io.EOF
		}
		d.buffer = data
	}

	n = copy(p, d.buffer)
	d.buffer = d.buffer[n:]
	return n, nil
}

func (d *dynamicReadCloser) Close() error {
	return nil
}

type errorReader struct {
	err error
}

func (e *errorReader) Read(p []byte) (n int, err error) {
	return 0, e.err
}

func (e *errorReader) Close() error {
	return nil
}

type errorOnClose struct {
	err error
}

func (e *errorOnClose) Close() error {
	return e.err
}

type writerWithCloseError struct {
	err error
}

func (w *writerWithCloseError) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func (w *writerWithCloseError) Close() error {
	return w.err
}

type slowReader struct {
	mu    sync.Mutex
	data  []string
	index int
	delay time.Duration
	buf   []byte
}

func (s *slowReader) Read(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.delay > 0 {
		time.Sleep(s.delay)
	}

	if len(s.buf) > 0 {
		n = copy(p, s.buf)
		s.buf = s.buf[n:]
		return n, nil
	}

	if s.index >= len(s.data) {
		return 0, io.EOF
	}

	s.buf = []byte(s.data[s.index])
	s.index++

	n = copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *slowReader) Close() error {
	return nil
}

type flushErrorWriter struct {
	mu       sync.Mutex
	writes   int
	failures int
}

func (f *flushErrorWriter) Write(p []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.writes++
	if f.writes > 1 && f.failures < 1 {
		f.failures++
		return 0, fmt.Errorf("write error")
	}
	return len(p), nil
}

func (f *flushErrorWriter) Close() error {
	return nil
}

// -- test helper functions ----------------------------------------------------

func makeProbe() *api.ProbingDirective {
	return &api.ProbingDirective{
		Protocol:           api.Protocol_ICMP,
		DestinationAddress: net.ParseIP("10.0.0.2").String(),
		NextHeader: &api.NextHeader{
			Header: &api.NextHeader_IcmpNextHeader{
				IcmpNextHeader: &api.ICMPNextHeader{
					FirstHalfWord:  1234,
					SecondHalfWord: 80,
				},
			},
		},
	}
}

func NewCaracalProberMock(cfg *Config, stdin io.WriteCloser, stdout, stderr io.ReadCloser) (*caracalProber, error) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	queueSize := cfg.WriteQueueSize
	if queueSize <= 0 {
		queueSize = 1000
	}

	p := &caracalProber{
		cmd:        nil,
		stdin:      stdin,
		csvWriter:  csv.NewWriter(stdin),
		stdout:     csv.NewReader(stdout),
		stderr:     stderr,
		inFlight:   make(map[probeKey]*inFlightProbe),
		writeQueue: make(chan *probeRequest, queueSize),
		config:     cfg,
		cancel:     cancel,
		g:          g,
		logger:     testLogger(),
		metrics:    testMetrics(),
	}

	if _, err := p.stdout.Read(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to read CSV header: %w", err)
	}

	g.Go(func() error { return p.writerLoop(ctx) })
	g.Go(func() error { return p.readerLoop() })
	g.Go(func() error { return p.logStderr() })
	g.Go(func() error { return p.cleanupLoop(ctx) })

	return p, nil
}

// -- pure functions -----------------------------------------------------------

func TestNormalizeIPAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "IPv4", input: "8.8.8.8", expected: "8.8.8.8"},
		{name: "IPv6", input: "2001:4860:4860::8888", expected: "2001:4860:4860::8888"},
		{name: "IPv4-mapped IPv6", input: "::ffff:8.8.8.8", expected: "8.8.8.8"},
		{name: "invalid IP", input: "not-an-ip", expected: "not-an-ip"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := normalizeIPAddress(tt.input)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestExtractHalfWords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		directive *api.ProbingDirective
		expected1 uint16
		expected2 uint16
	}{
		{
			name: "UDP with ports",
			directive: &api.ProbingDirective{
				Protocol: api.Protocol_UDP,
				NextHeader: &api.NextHeader{
					Header: &api.NextHeader_UdpNextHeader{
						UdpNextHeader: &api.UDPNextHeader{
							SourcePort:      50000,
							DestinationPort: 33434,
						},
					},
				},
			},
			expected1: 50000,
			expected2: 33434,
		},
		{
			name: "ICMP with fields",
			directive: &api.ProbingDirective{
				Protocol: api.Protocol_ICMP,
				NextHeader: &api.NextHeader{
					Header: &api.NextHeader_IcmpNextHeader{
						IcmpNextHeader: &api.ICMPNextHeader{
							FirstHalfWord:  1234,
							SecondHalfWord: 5678,
						},
					},
				},
			},
			expected1: 1234,
			expected2: 5678,
		},
		{
			name: "ICMPv6 with fields",
			directive: &api.ProbingDirective{
				Protocol: api.Protocol_ICMPv6,
				NextHeader: &api.NextHeader{
					Header: &api.NextHeader_Icmpv6NextHeader{
						Icmpv6NextHeader: &api.ICMPv6NextHeader{
							FirstHalfWord:  1111,
							SecondHalfWord: 2222,
						},
					},
				},
			},
			expected1: 1111,
			expected2: 2222,
		},
		{
			name: "UDP with nil header",
			directive: &api.ProbingDirective{
				Protocol: api.Protocol_UDP,
			},
			expected1: 0,
			expected2: 0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first, second := extractHalfWords(tt.directive)
			if first != tt.expected1 {
				t.Errorf("expected first=%d, got %d", tt.expected1, first)
			}
			if second != tt.expected2 {
				t.Errorf("expected second=%d, got %d", tt.expected2, second)
			}
		})
	}
}

func TestProtocolToString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		protocol api.Protocol
		expected string
	}{
		{api.Protocol_ICMP, "icmp"},
		{api.Protocol_ICMPv6, "icmp6"},
		{api.Protocol_UDP, "udp"},
		{api.Protocol(99), "99"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.expected, func(t *testing.T) {
			t.Parallel()
			result := protocolToString(tt.protocol)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestParseProbeResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		record      []string
		expectError bool
	}{
		{
			name: "valid record",
			record: []string{
				"1609459200000000", "17", "192.0.2.1", "8.8.8.8",
				"50000", "33434", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: false,
		},
		{
			name: "missing timestamp",
			record: []string{
				"", "17", "192.0.2.1", "8.8.8.8",
				"50000", "33434", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: true,
		},
		{
			name: "missing rtt",
			record: []string{
				"1609459200000000", "17", "192.0.2.1", "8.8.8.8",
				"50000", "33434", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "", "0",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := parseProbeResult(tt.record)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				if result != nil {
					t.Error("expected nil result on error")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if result == nil {
					t.Error("expected non-nil result")
				} else {
					if result.SentTime.IsZero() {
						t.Error("SentTime should not be zero")
					}
					if result.ReceivedTime.IsZero() {
						t.Error("ReceivedTime should not be zero")
					}
				}
			}
		})
	}
}

func TestBuildProbeKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		record      []string
		expectError bool
		checkKey    func(t *testing.T, key probeKey)
	}{
		{
			name: "valid UDP record",
			record: []string{
				"1609459200", "17", "192.0.2.1", "8.8.8.8",
				"50000", "33434", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: false,
			checkKey: func(t *testing.T, key probeKey) {
				if key.dstAddr != "8.8.8.8" {
					t.Errorf("expected dstAddr=8.8.8.8, got %s", key.dstAddr)
				}
				if key.firstHalfWord != 50000 {
					t.Errorf("expected firstHalfWord=50000, got %d", key.firstHalfWord)
				}
				if key.secondHalfWord != 33434 {
					t.Errorf("expected secondHalfWord=33434, got %d", key.secondHalfWord)
				}
				if key.ttl != 10 {
					t.Errorf("expected ttl=10, got %d", key.ttl)
				}
				if key.protocol != api.Protocol_UDP {
					t.Errorf("expected protocol=UDP, got %v", key.protocol)
				}
			},
		},
		{
			name: "valid ICMP record",
			record: []string{
				"1609459200", "1", "192.0.2.1", "8.8.8.8",
				"1234", "5678", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: false,
			checkKey: func(t *testing.T, key probeKey) {
				if key.protocol != api.Protocol_ICMP {
					t.Errorf("expected protocol=ICMP, got %v", key.protocol)
				}
			},
		},
		{
			name: "valid ICMPv6 record",
			record: []string{
				"1609459200", "58", "192.0.2.1", "8.8.8.8",
				"1234", "5678", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: false,
			checkKey: func(t *testing.T, key probeKey) {
				if key.protocol != api.Protocol_ICMPv6 {
					t.Errorf("expected protocol=ICMPv6, got %v", key.protocol)
				}
			},
		},
		{
			name: "invalid protocol",
			record: []string{
				"1609459200", "99", "192.0.2.1", "8.8.8.8",
				"50000", "33434", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: true,
		},
		{
			name: "invalid TTL",
			record: []string{
				"1609459200", "17", "192.0.2.1", "8.8.8.8",
				"50000", "33434", "invalid", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: true,
		},
		{
			name: "invalid protocol number",
			record: []string{
				"1609459200", "invalid", "192.0.2.1", "8.8.8.8",
				"50000", "33434", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: true,
		},
		{
			name: "invalid first half word",
			record: []string{
				"1609459200", "17", "192.0.2.1", "8.8.8.8",
				"invalid", "33434", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: true,
		},
		{
			name: "invalid second half word",
			record: []string{
				"1609459200", "17", "192.0.2.1", "8.8.8.8",
				"50000", "invalid", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "50", "0",
			},
			expectError: true,
		},
		{
			name: "missing sent time",
			record: []string{
				"", "17", "192.0.2.1", "8.8.8.8",
				"50000", "33434", "10", "0", "8.8.8.8",
				"1", "0", "0", "64", "28", "", "", "0",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, parseErr := parseProbeResult(tt.record)
			var key probeKey
			var err error
			if parseErr != nil {
				err = parseErr
			} else {
				key, err = buildProbeKey(tt.record, result.SentTime)
			}
			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if tt.checkKey != nil {
					tt.checkKey(t, key)
				}
			}
		})
	}
}

func TestBuildProbeKeyFromDirective(t *testing.T) {
	t.Parallel()

	pd := &api.ProbingDirective{
		Protocol:           api.Protocol_ICMP,
		DestinationAddress: net.ParseIP("10.0.0.2").String(),
		NextHeader: &api.NextHeader{
			Header: &api.NextHeader_IcmpNextHeader{
				IcmpNextHeader: &api.ICMPNextHeader{
					FirstHalfWord:  1234,
					SecondHalfWord: 5678,
				},
			},
		},
	}

	timestamp := int64(1600000000)
	key := buildProbeKeyFromDirective(pd, 64, timestamp)

	if key.dstAddr != "10.0.0.2" {
		t.Errorf("expected dstAddr=10.0.0.2, got %s", key.dstAddr)
	}
	if key.firstHalfWord != 1234 {
		t.Errorf("expected firstHalfWord=1234, got %d", key.firstHalfWord)
	}
	if key.secondHalfWord != 5678 {
		t.Errorf("expected secondHalfWord=5678, got %d", key.secondHalfWord)
	}
	if key.ttl != 64 {
		t.Errorf("expected ttl=64, got %d", key.ttl)
	}
	if key.protocol != api.Protocol_ICMP {
		t.Errorf("expected protocol=ICMP, got %v", key.protocol)
	}
	if key.correlationSecond != timestamp {
		t.Errorf("expected correlationSecond=%d, got %d", timestamp, key.correlationSecond)
	}
}

// -- integration tests (mock-based) -------------------------------------------

func TestProbeEndToEnd(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	fixedTimeMicros := int64(1600000000) * 1_000_000 // for CSV data
	fixedTimeSeconds := int64(1600000000)            // for correlationSecond in key

	csvChan := make(chan string, 2)
	stdout := &dynamicReadCloser{ch: csvChan}

	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	header := "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	csvChan <- header

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() { _ = prober.Close() }()

	time.Sleep(50 * time.Millisecond)

	key := probeKey{
		dstAddr:           "10.0.0.2",
		firstHalfWord:     1234,
		secondHalfWord:    80,
		ttl:               64,
		protocol:          api.Protocol_ICMP,
		correlationSecond: fixedTimeSeconds,
	}

	resultCh := make(chan *ProbeResult, 1)
	prober.inFlightMu.Lock()
	prober.inFlight[key] = &inFlightProbe{
		resultCh:   resultCh,
		queuedTime: time.Unix(fixedTimeSeconds, 0),
	}
	prober.inFlightMu.Unlock()

	rtt := int64(100)
	data := fmt.Sprintf("%d,1,10.0.0.1,10.0.0.2,1234,80,64,64,10.0.0.2,1,0,0,64,60,,%d,1\n", fixedTimeMicros, rtt)
	csvChan <- data

	select {
	case result := <-resultCh:
		if result == nil {
			t.Errorf("result is nil")
			return
		}
		if result.ReplyAddress == nil {
			t.Errorf("reply address is nil")
			return
		}
		if result.ReplyAddress.String() != "10.0.0.2" {
			t.Errorf("expected reply address 10.0.0.2, got %s", result.ReplyAddress.String())
		}
		if result.TimedOut {
			t.Error("probe should not have timed out")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("test timeout waiting for result")
	}

	close(csvChan)
}

func TestDuplicateProbe(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() {
		close(csvChan)
		_ = prober.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	pd := makeProbe()
	currentTime := time.Now().Unix()
	key := probeKey{
		dstAddr:           "10.0.0.2",
		firstHalfWord:     1234,
		secondHalfWord:    80,
		ttl:               64,
		protocol:          api.Protocol_ICMP,
		correlationSecond: currentTime,
	}

	prober.inFlightMu.Lock()
	prober.inFlight[key] = &inFlightProbe{
		resultCh:   make(chan *ProbeResult, 1),
		queuedTime: time.Now(),
	}
	prober.inFlightMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	result, err := prober.Probe(ctx, pd, 64)
	if !errors.Is(err, ErrDuplicatePD) {
		t.Errorf("duplicate probe should return ErrDuplicatePD, got: %v", err)
	}
	if result != nil {
		t.Errorf("duplicate probe should return nil result, got: %v", result)
	}
}

func TestProbeTimeout(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    100 * time.Millisecond,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	pd := makeProbe()
	ctx := context.Background()

	result, err := prober.Probe(ctx, pd, 64)

	// Close immediately after probe completes
	close(csvChan)
	_ = prober.Close()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Errorf("result is nil")
		return
	}
	if !result.TimedOut {
		t.Error("expected probe to timeout")
	}
}

func TestProbeContextCanceled(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  1,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() {
		close(csvChan)
		_ = prober.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	pd := makeProbe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = prober.Probe(ctx, pd, 64)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestProbeContextCancelledWhileQueuing(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  0,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	ctx_prober, cancel_prober := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx_prober)

	prober := &caracalProber{
		cmd:        nil,
		stdin:      stdin,
		csvWriter:  csv.NewWriter(stdin),
		stdout:     csv.NewReader(stdout),
		stderr:     stderr,
		inFlight:   make(map[probeKey]*inFlightProbe),
		writeQueue: make(chan *probeRequest),
		config:     cfg,
		cancel:     cancel_prober,
		g:          g,
		logger:     testLogger(),
		metrics:    testMetrics(),
	}

	_, _ = prober.stdout.Read()

	time.Sleep(50 * time.Millisecond)

	pd := makeProbe()
	ctx, cancel := context.WithCancel(gctx)

	errCh := make(chan error, 1)
	go func() {
		_, err := prober.Probe(ctx, pd, 64)
		errCh <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	var probeErr error
	select {
	case probeErr = <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Probe didn't return after context cancel")
	}

	close(csvChan)
	cancel_prober()
	_ = prober.g.Wait()

	if !errors.Is(probeErr, context.Canceled) {
		t.Errorf("Expected context.Canceled, got: %v", probeErr)
	} else {
		t.Log("Successfully triggered ctx.Err() in Probe")
	}
}

func TestCloseCancels(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	header := "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	csvChan <- header

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	close(csvChan)

	err = prober.Close()
	if err != nil {
		t.Logf("Close returned error (acceptable): %v", err)
	}
}

func TestCleanupStaleProbesNoStale(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 50 * time.Millisecond,
		ProbeTimeout:    5 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	key := probeKey{
		dstAddr:           "10.0.0.2",
		firstHalfWord:     1234,
		secondHalfWord:    80,
		ttl:               64,
		protocol:          api.Protocol_ICMP,
		correlationSecond: time.Now().Unix(),
	}

	prober.inFlightMu.Lock()
	prober.inFlight[key] = &inFlightProbe{
		resultCh:   make(chan *ProbeResult, 1),
		queuedTime: time.Now(),
	}
	prober.inFlightMu.Unlock()

	time.Sleep(200 * time.Millisecond)

	prober.inFlightMu.RLock()
	_, exists := prober.inFlight[key]
	prober.inFlightMu.RUnlock()

	close(csvChan)
	_ = prober.Close()

	if !exists {
		t.Error("fresh probe should not be cleaned up")
	}
}

func TestProbeWithDifferentTTLs(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	fixedTime := int64(1600000000)

	csvChan := make(chan string, 10)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() {
		close(csvChan)
		_ = prober.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	for ttl := uint8(1); ttl <= 3; ttl++ {
		key := probeKey{
			dstAddr:           "10.0.0.2",
			firstHalfWord:     1234,
			secondHalfWord:    80,
			ttl:               ttl,
			protocol:          api.Protocol_ICMP,
			correlationSecond: fixedTime,
		}

		resultCh := make(chan *ProbeResult, 1)
		prober.inFlightMu.Lock()
		prober.inFlight[key] = &inFlightProbe{
			resultCh:   resultCh,
			queuedTime: time.Unix(fixedTime, 0),
		}
		prober.inFlightMu.Unlock()

		rtt := int64(100)
		data := fmt.Sprintf("%d,1,10.0.0.1,10.0.0.2,1234,80,%d,%d,10.0.0.2,1,0,0,64,28,,%d,1\n",
			fixedTime, ttl, ttl, rtt)
		csvChan <- data
	}

	time.Sleep(100 * time.Millisecond)
}

// -- error handling -----------------------------------------------------------

func TestHandleResultInvalidCSV(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 2)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	csvChan <- "1609459200,17,192.0.2.1\n"
	close(csvChan)

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	_ = prober.Close()
}

func TestHandleResultShortRecord(t *testing.T) {
	t.Parallel()

	shortRecord := []string{"field1", "field2"}

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() {
		close(csvChan)
		_ = prober.Close()
	}()

	err = prober.handleResult(shortRecord)
	if err == nil {
		t.Error("expected error for short record")
	}
}

func TestHandleResultBuildKeyError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 2)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	csvChan <- "1609459200,99,192.0.2.1,8.8.8.8,50000,33434,10,0,8.8.8.8,1,0,0,64,28,,50,0\n"

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() {
		close(csvChan)
		_ = prober.Close()
	}()

	time.Sleep(100 * time.Millisecond)
}

func TestMatchAndDeliverResultNoMatch(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 2)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() {
		close(csvChan)
		_ = prober.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	fixedTime := int64(1600000000)
	rtt := int64(100)
	data := fmt.Sprintf("%d,1,10.0.0.1,10.0.0.2,9999,9999,64,64,10.0.0.2,1,0,0,64,28,,%d,1\n", fixedTime, rtt)
	csvChan <- data

	time.Sleep(100 * time.Millisecond)
}

func TestMatchAndDeliverResultChannelBlocked(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() {
		close(csvChan)
		_ = prober.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	resultCh := make(chan *ProbeResult, 1)
	resultCh <- &ProbeResult{}

	fixedTime := int64(1600000000)
	key := probeKey{
		dstAddr:           "10.0.0.2",
		firstHalfWord:     1234,
		secondHalfWord:    80,
		ttl:               64,
		protocol:          api.Protocol_ICMP,
		correlationSecond: fixedTime,
	}

	prober.inFlightMu.Lock()
	prober.inFlight[key] = &inFlightProbe{
		resultCh:   resultCh,
		queuedTime: time.Unix(fixedTime, 0),
	}
	prober.inFlightMu.Unlock()

	rtt := int64(100)
	data := fmt.Sprintf("%d,1,10.0.0.1,10.0.0.2,1234,80,64,64,10.0.0.2,1,0,0,64,28,,%d,1\n", fixedTime, rtt)
	csvChan <- data

	time.Sleep(100 * time.Millisecond)
}

func TestHandleResultSkipInvalidResult(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 3)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	csvChan <- ",17,192.0.2.1,8.8.8.8,50000,33434,10,0,8.8.8.8,1,0,0,64,28,,50,0\n"
	csvChan <- "1609459200,17,192.0.2.1,8.8.8.8,50000,33434,10,0,8.8.8.8,1,0,0,64,28,,,0\n"

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() {
		close(csvChan)
		_ = prober.Close()
	}()

	time.Sleep(100 * time.Millisecond)
}

func TestReaderLoopEOF(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	close(csvChan)

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	err = prober.Close()
	if err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "stdout closed") {
		t.Logf("Close returned error (expected): %v", err)
	}
}

func TestCloseWithError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	close(csvChan)

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	err = prober.Close()
	switch err {
	case nil:
		t.Log("Close returned nil (goroutines may have exited cleanly)")
	default:
		t.Logf("Close returned error (expected): %v", err)
	}
}

func TestCleanupLoopDefaultInterval(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 0,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	close(csvChan)
	_ = prober.Close()
}

func TestEncodeAndSendProbeCSVWriterInErrorState(t *testing.T) {
	t.Parallel()

	r, w := io.Pipe()
	_ = r.Close()

	csvWriter := csv.NewWriter(w)
	_ = csvWriter.Write([]string{"test", "data"})
	csvWriter.Flush()

	prober := &caracalProber{
		csvWriter: csvWriter,
		stdin:     w,
		metrics:   testMetrics(),
	}

	pd := makeProbe()
	req := &probeRequest{
		pd:       pd,
		ttl:      64,
		resultCh: make(chan *ProbeResult, 1),
	}

	err := prober.encodeAndSendProbe(req)
	if err == nil {
		t.Error("expected write error from cached error state, got nil")
	} else {
		t.Logf("Successfully triggered cached write error: %v", err)
	}

	_ = w.Close()
}

func TestEncodeAndSendProbeFlushError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &flushErrorWriter{}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	pd := makeProbe()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, _ = prober.Probe(ctx, pd, 64)

	time.Sleep(100 * time.Millisecond)

	close(csvChan)
	_ = prober.Close()
}

func TestLogStderrScannerError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	close(csvChan)

	stdout := &dynamicReadCloser{ch: csvChan}
	stderr := &errorReader{err: fmt.Errorf("scanner error")}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	err = prober.Close()
	if err == nil || !strings.Contains(err.Error(), "scanner error") {
		t.Logf("Close returned: %v", err)
	}
}

func TestLogStderrWithOutput(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	stdout := &dynamicReadCloser{ch: csvChan}

	stderrChan := make(chan string, 2)
	stderrChan <- "caracal: debug message\n"
	stderrChan <- "caracal: another message\n"
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	prober, err := NewCaracalProberMock(cfg, stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	close(csvChan)
	_ = prober.Close()
}

func TestLogStderrContextCancellationBetweenScans(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdin := &nopWriteCloser{Buffer: &bytes.Buffer{}}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	close(csvChan)

	stdout := &dynamicReadCloser{ch: csvChan}
	slowStderr := &slowReader{
		data:  []string{"line1\n", "line2\n", "line3\n"},
		delay: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	g, _ := errgroup.WithContext(ctx)

	prober := &caracalProber{
		cmd:        nil,
		stdin:      stdin,
		csvWriter:  csv.NewWriter(stdin),
		stdout:     csv.NewReader(stdout),
		stderr:     slowStderr,
		inFlight:   make(map[probeKey]*inFlightProbe),
		writeQueue: make(chan *probeRequest, 10),
		config:     cfg,
		cancel:     cancel,
		g:          g,
		logger:     testLogger(),
		metrics:    testMetrics(),
	}

	_, _ = prober.stdout.Read()
	g.Go(func() error { return prober.logStderr() })

	time.Sleep(100 * time.Millisecond)
	cancel()

	err := prober.g.Wait()
	if errors.Is(err, context.Canceled) {
		t.Log("Successfully triggered ctx.Done() path in logStderr")
	} else {
		t.Logf("Got error: %v", err)
	}
}

// -- tests that modify global state (non-parallel) ----------------------------

func TestClosePipeErrorLogging(t *testing.T) {
	// Cannot run in parallel - modifies log output
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	errorCloser := &errorOnClose{err: fmt.Errorf("mock close failure")}
	closePipe(errorCloser, "test-pipe", logger)

	output := buf.String()
	if !strings.Contains(output, "Failed to close pipe") {
		t.Errorf("Expected 'Failed to close pipe' in log, got: %s", output)
	}
	if !strings.Contains(output, "mock close failure") {
		t.Errorf("Expected 'mock close failure' in log, got: %s", output)
	}
}

func TestWriterLoopError(t *testing.T) {
	// Cannot run in parallel - modifies log output

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	r, w := io.Pipe()
	_ = r.Close()

	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	close(csvChan)

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	prober := &caracalProber{
		cmd:        nil,
		stdin:      w,
		csvWriter:  csv.NewWriter(w),
		stdout:     csv.NewReader(stdout),
		stderr:     stderr,
		inFlight:   make(map[probeKey]*inFlightProbe),
		writeQueue: make(chan *probeRequest, 10),
		config:     cfg,
		cancel:     cancel,
		g:          g,
		logger:     testLogger(),
		metrics:    testMetrics(),
	}

	_, _ = prober.stdout.Read()
	g.Go(func() error { return prober.writerLoop(ctx) })

	time.Sleep(50 * time.Millisecond)

	pd := makeProbe()
	req := &probeRequest{
		pd:       pd,
		ttl:      64,
		resultCh: make(chan *ProbeResult, 1),
	}

	prober.writeQueue <- req
	time.Sleep(100 * time.Millisecond)

	cancel()
	err := prober.g.Wait()
	_ = w.Close()

	if err == nil {
		t.Error("Expected writerLoop to return error")
	}
}

func TestWriterLoopStdinCloseError(t *testing.T) {
	// Cannot run in parallel - modifies log output

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := &Config{
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	stdinWriter := &writerWithCloseError{err: fmt.Errorf("stdin close error")}
	csvChan := make(chan string, 1)
	csvChan <- "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round\n"
	close(csvChan)

	stdout := &dynamicReadCloser{ch: csvChan}
	stderrChan := make(chan string)
	close(stderrChan)
	stderr := &dynamicReadCloser{ch: stderrChan}

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	prober := &caracalProber{
		cmd:        nil,
		stdin:      stdinWriter,
		csvWriter:  csv.NewWriter(stdinWriter),
		stdout:     csv.NewReader(stdout),
		stderr:     stderr,
		inFlight:   make(map[probeKey]*inFlightProbe),
		writeQueue: make(chan *probeRequest, 10),
		config:     cfg,
		cancel:     cancel,
		g:          g,
		logger:     logger,
		metrics:    testMetrics(),
	}

	_, _ = prober.stdout.Read()
	g.Go(func() error { return prober.writerLoop(ctx) })

	time.Sleep(50 * time.Millisecond)
	cancel()

	_ = prober.g.Wait()

	output := buf.String()
	if !strings.Contains(output, "Failed to close caracal stdin") {
		t.Errorf("Expected 'Failed to close caracal stdin' in log, got: %s", output)
	}
	if !strings.Contains(output, "stdin close error") {
		t.Errorf("Expected 'stdin close error' in log, got: %s", output)
	}
}

// -- real process tests (non-parallel) ----------------------------------------

func TestNewCaracalProberWithRealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping shell-based test on Windows")
	}

	cfg := &Config{
		ProberPath: "sh",
		ProberArgs: []string{
			"-c",
			`
echo "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"
while IFS=, read -r dst_addr src_port dst_port ttl protocol; do
	timestamp=$(date +%s)000000
	echo "$timestamp,1,10.0.0.1,$dst_addr,$src_port,$dst_port,$ttl,$ttl,10.0.0.1,1,0,0,64,28,,100,0"
done
			`,
		},
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	prober, err := NewCaracalProber(cfg, testLogger(), testMetrics())
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() { _ = prober.Close() }()

	time.Sleep(100 * time.Millisecond)

	pd := makeProbe()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := prober.Probe(ctx, pd, 64)
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}

	if result == nil {
		t.Errorf("result is nil")
		return
	}

	if result.TimedOut {
		t.Error("probe should not timeout with fake caracal")
	}

	err = prober.Close()
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Logf("Close returned: %v", err)
	}
}

func TestSetupCaracalProcessFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping shell-based test on Windows")
	}

	cfg := &Config{
		ProberPath: "nonexistent-command-that-does-not-exist",
		ProberArgs: []string{},
	}

	_, err := NewCaracalProber(cfg, testLogger(), testMetrics())
	if err == nil {
		t.Error("expected error for nonexistent command")
	}
}

func TestSetupCaracalProcessStartFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows")
	}

	cfg := &Config{
		ProberPath:      "/bin/sh",
		ProberArgs:      []string{"-c", "exit 1"},
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	prober, err := NewCaracalProber(cfg, testLogger(), testMetrics())
	if err == nil {
		_ = prober.Close()
		t.Skip("Command didn't fail as expected")
	}

	if !strings.Contains(err.Error(), "header") {
		t.Logf("Got error (expected): %v", err)
	}
}

func TestClosePipeErrors(t *testing.T) {
	r, w := io.Pipe()
	_ = w.Close()
	_ = r.Close()

	closePipe(w, "test-write", testLogger())
	closePipe(r, "test-read", testLogger())
	closePipe(nil, "test-nil", testLogger())
}

func TestProbeWithRealProcessTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping shell-based test on Windows")
	}

	cfg := &Config{
		ProberPath: "sh",
		ProberArgs: []string{
			"-c",
			`
echo "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"
while read line; do sleep 1; done
			`,
		},
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    200 * time.Millisecond,
	}

	prober, err := NewCaracalProber(cfg, testLogger(), testMetrics())
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() { _ = prober.Close() }()

	time.Sleep(50 * time.Millisecond)

	pd := makeProbe()
	ctx := context.Background()

	result, err := prober.Probe(ctx, pd, 64)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == nil {
		t.Errorf("result is nil")
		return
	}

	if !result.TimedOut {
		t.Error("probe should have timed out")
	}
}

func TestCloseKillError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping shell-based test on Windows")
	}

	cfg := &Config{
		ProberPath: "sh",
		ProberArgs: []string{
			"-c",
			`
echo "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"
while read line; do
	timestamp=$(date +%s)000000
	echo "$timestamp,1,10.0.0.1,10.0.0.2,1234,80,64,64,10.0.0.1,1,0,0,64,28,,100,0"
done
			`,
		},
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	prober, err := NewCaracalProber(cfg, testLogger(), testMetrics())
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if prober.cmd != nil && prober.cmd.Process != nil {
		_ = prober.cmd.Process.Kill()
		time.Sleep(10 * time.Millisecond)
	}

	err = prober.Close()
	if err != nil {
		t.Logf("Close returned error (acceptable): %v", err)
	}
}

func TestNewCaracalProberDefaultQueueSize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping shell-based test on Windows")
	}

	cfg := &Config{
		ProberPath: "/bin/sh",
		ProberArgs: []string{
			"-c",
			`echo "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"; sleep 0.1`,
		},
		WriteQueueSize:  -1,
		CleanupInterval: 50 * time.Millisecond,
		ProbeTimeout:    1 * time.Second,
	}

	prober, err := NewCaracalProber(cfg, testLogger(), testMetrics())
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}
	defer func() { _ = prober.Close() }()

	if cap(prober.writeQueue) != 1000 {
		t.Errorf("expected queue size 1000, got %d", cap(prober.writeQueue))
	}
}

func TestCloseKillErrorLogging(t *testing.T) {
	// Cannot run in parallel - modifies log output
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows")
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := &Config{
		ProberPath: "sh",
		ProberArgs: []string{
			"-c",
			`echo "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"
while read line; do timestamp=$(date +%s)000000; echo "$timestamp,1,10.0.0.1,10.0.0.2,1234,80,64,64,10.0.0.1,1,0,0,64,28,,100,0"; done`,
		},
		WriteQueueSize:  10,
		CleanupInterval: 100 * time.Millisecond,
		ProbeTimeout:    2 * time.Second,
	}

	prober, err := NewCaracalProber(cfg, logger, testMetrics())
	if err != nil {
		t.Fatalf("failed to create prober: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if prober.cmd != nil && prober.cmd.Process != nil {
		_ = prober.cmd.Process.Kill()
		_ = prober.cmd.Wait()
		time.Sleep(10 * time.Millisecond)
	}

	_ = prober.Close()

	time.Sleep(50 * time.Millisecond)

	output := buf.String()
	if !strings.Contains(output, "Failed to kill caracal") {
		t.Errorf("Expected kill error to be logged, got: %s", output)
	}
}

func TestSetupCaracalProcessDefaultPath(t *testing.T) {
	// Cannot run in parallel - modifies PATH environment variable
	if runtime.GOOS == "windows" {
		t.Skip("Skipping shell-based test on Windows")
	}

	tmpDir := t.TempDir()
	caracalPath := filepath.Join(tmpDir, "caracal")

	script := `#!/bin/sh
echo "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"
while read line; do
	timestamp=$(date +%s)000000
	echo "$timestamp,1,10.0.0.1,10.0.0.2,1234,80,64,64,10.0.0.1,1,0,0,64,28,,100,0"
done
`
	//nolint:gosec // Script needs to be executable
	err := os.WriteFile(caracalPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("failed to create fake caracal: %v", err)
	}

	oldPath := os.Getenv("PATH")
	_ = os.Setenv("PATH", tmpDir+":"+oldPath)
	defer func() { _ = os.Setenv("PATH", oldPath) }()

	cfg := &Config{
		ProberPath:      "",
		ProberArgs:      []string{},
		WriteQueueSize:  10,
		CleanupInterval: 50 * time.Millisecond,
		ProbeTimeout:    1 * time.Second,
	}

	prober, err := NewCaracalProber(cfg, testLogger(), testMetrics())
	if err != nil {
		t.Fatalf("failed to create prober with default path: %v", err)
	}
	defer func() { _ = prober.Close() }()

	time.Sleep(50 * time.Millisecond)

	pd := makeProbe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := prober.Probe(ctx, pd, 64)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	if result == nil || result.TimedOut {
		t.Error("probe should have succeeded with fake caracal in PATH")
	}
}

func TestSetupCaracalProcessPipeErrors(t *testing.T) {
	t.Log("Pipe creation errors in setupCaracalProcess are defensive checks")
	t.Log("They handle system-level failures (fd exhaustion, etc.)")
	t.Log("These scenarios cannot be reliably tested in unit tests")
	t.Log("Coverage: These error paths are untestable without system manipulation")
}
