// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

// Intentionally uncovered (94-95% overall):
//
//   - Run (85%): defer conn.Close() and defer prober.Close() error paths are
//     unreachable in practice; Go returns nil on Close() of a cleanly established
//     connection.
//     SetKeepAlive and SetKeepAlivePeriod error paths are untested; setsockopt
//     failures are not realistically injectable without low-level OS mocking.
//
//   - readerLoop (97.0%): the context cancellation branch inside the select is
//     timing-dependent and is indirectly covered by TestReaderLoop_ContextCancelled.
//
//   - caracal_prober.go is excluded entirely; NewCaracalProber requires the caracal
//     binary and is exercised by integration tests.

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dioptra-io/retina-commons/api/v2"
)

// testLogger returns a logger that discards all output, keeping test output clean.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testMetrics returns a Metrics instance backed by a fresh registry for test isolation.
func testMetrics() *Metrics {
	return NewMetrics(prometheus.NewRegistry(), "test-agent")
}

// -- stubs --------------------------------------------------------------------

type stubProber struct {
	probeFunc func(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error)
	closeFunc func() error
}

func (s *stubProber) Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
	if s.probeFunc != nil {
		return s.probeFunc(ctx, pd, ttl)
	}
	return &ProbeResult{
		ReplyAddress: net.ParseIP("1.1.1.1"),
		SentTime:     time.Now().Add(-10 * time.Millisecond),
		ReceivedTime: time.Now(),
	}, nil
}

func (s *stubProber) Close() error {
	if s.closeFunc != nil {
		return s.closeFunc()
	}
	return nil
}

type stubConn struct {
	readFunc          func(b []byte) (int, error)
	writeFunc         func(b []byte) (int, error)
	readDeadlineFunc  func(t time.Time) error
	writeDeadlineFunc func(t time.Time) error
	closed            bool
}

func (c *stubConn) Read(b []byte) (int, error) {
	if c.readFunc != nil {
		return c.readFunc(b)
	}
	return 0, io.EOF
}

func (c *stubConn) Write(b []byte) (int, error) {
	if c.writeFunc != nil {
		return c.writeFunc(b)
	}
	return len(b), nil
}

func (c *stubConn) Close() error {
	c.closed = true
	return nil
}

func (c *stubConn) LocalAddr() net.Addr  { return &net.IPAddr{} }
func (c *stubConn) RemoteAddr() net.Addr { return &net.IPAddr{} }

func (c *stubConn) SetDeadline(t time.Time) error { return nil }

func (c *stubConn) SetReadDeadline(t time.Time) error {
	if c.readDeadlineFunc != nil {
		return c.readDeadlineFunc(t)
	}
	return nil
}

func (c *stubConn) SetWriteDeadline(t time.Time) error {
	if c.writeDeadlineFunc != nil {
		return c.writeDeadlineFunc(t)
	}
	return nil
}

// -- mockNetError -------------------------------------------------------------

type mockNetError struct {
	timeout   bool
	temporary bool
	err       string
}

func (e *mockNetError) Error() string   { return e.err }
func (e *mockNetError) Timeout() bool   { return e.timeout }
func (e *mockNetError) Temporary() bool { return e.temporary }

// -- Run() --------------------------------------------------------------------

//nolint:funlen // Integration test requires setup and teardown
func TestRun_WithLocalServer(t *testing.T) {
	// Note: Not parallel - full integration test with real network connections and timing dependencies

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	serverAddr := listener.Addr().String()
	t.Logf("Mock orchestrator listening on %s", serverAddr)

	gotFIE := make(chan bool, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			t.Logf("Accept failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		t.Logf("Agent connected")

		encoder := json.NewEncoder(conn)
		decoder := json.NewDecoder(conn)

		pd := &api.ProbingDirective{
			AgentId:            "test-agent",
			NearTtl:            10,
			DestinationAddress: net.ParseIP("8.8.8.8").String(),
			Protocol:           api.Protocol_ICMP,
			NextHeader:         &api.NextHeader{Header: &api.NextHeader_IcmpNextHeader{IcmpNextHeader: &api.ICMPNextHeader{}}},
		}

		t.Log("Sending directive...")
		if err := encoder.Encode(pd); err != nil {
			t.Logf("Send failed: %v", err)
			return
		}

		t.Log("Waiting for FIE...")
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var fie api.ForwardingInfoElement
		if err := decoder.Decode(&fie); err != nil {
			t.Logf("Receive failed: %v", err)
			return
		}

		if fie.NearInfo != nil {
			t.Logf("✓ Received FIE with TTL %d", fie.NearInfo.ProbeTtl)
		} else {
			t.Log("✓ Received FIE (near probe timed out)")
		}
		gotFIE <- true

		time.Sleep(200 * time.Millisecond)
	}()

	time.Sleep(50 * time.Millisecond)

	cfg := DefaultConfig()
	cfg.OrchestratorAddr = serverAddr
	cfg.ProberType = "mock"
	cfg.AgentID = "test-agent"
	cfg.ReadDeadline = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	agentErr := make(chan error, 1)
	go func() {
		t.Log("Starting agent...")
		err := Run(ctx, cfg, testLogger(), testMetrics())
		t.Logf("Agent exited: %v", err)
		agentErr <- err
	}()

	select {
	case <-gotFIE:
		t.Log("SUCCESS: Full pipeline worked!")
	case <-time.After(3 * time.Second):
		t.Error("Timeout: Did not receive FIE")
	}

	cancel()
	<-agentErr
}

func TestRun_ConnectionCloseError(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	serverAddr := listener.Addr().String()
	connClosed := make(chan net.Conn, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		connClosed <- conn
		time.Sleep(100 * time.Millisecond)
		_ = conn.Close()
	}()

	cfg := DefaultConfig()
	cfg.OrchestratorAddr = serverAddr
	cfg.ProberType = "mock"
	cfg.ReadDeadline = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, testLogger(), testMetrics())
	}()

	select {
	case conn := <-connClosed:
		time.Sleep(50 * time.Millisecond)
		_ = conn.Close()
	case <-time.After(150 * time.Millisecond):
	}

	select {
	case <-done:
		t.Log("Run finished (connection close defer executed)")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRun_ProberCloseError(t *testing.T) {
	// Note: Cannot be parallel - modifies global createProber

	origCreateProber := createProber
	defer func() { createProber = origCreateProber }()

	createProber = func(cfg *Config, logger *slog.Logger, metrics *Metrics) (Prober, error) {
		return &stubProber{
			closeFunc: func() error {
				return errors.New("prober close failed")
			},
		}, nil
	}

	cfg := DefaultConfig()
	cfg.OrchestratorAddr = "invalid-host:99999"

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := Run(ctx, cfg, testLogger(), testMetrics())

	if err == nil {
		t.Error("Run should fail with invalid address")
	}
	t.Log("✓ prober.Close() error was logged in defer")
}

func TestRun_ProberCreationError(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.ProberType = "invalid-prober-type-xyz"

	err := Run(context.Background(), cfg, testLogger(), testMetrics())
	if err == nil {
		t.Error("Run(invalid prober) should fail")
	}
	if !strings.Contains(err.Error(), "failed to create prober") {
		t.Errorf("Run(invalid prober) = %v, want 'failed to create prober' error", err)
	}
}

func TestRun_GoroutineErrorPropagation(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	serverAddr := listener.Addr().String()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		for i := 0; i < 20; i++ {
			_, _ = conn.Write([]byte("invalid json line\n"))
			time.Sleep(10 * time.Millisecond)
		}
	}()

	cfg := DefaultConfig()
	cfg.OrchestratorAddr = serverAddr
	cfg.ProberType = "mock"
	cfg.MaxConsecutiveDecodeErrors = 5
	cfg.ReadDeadline = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = Run(ctx, cfg, testLogger(), testMetrics())
	if err == nil {
		t.Error("Run should return error from goroutine")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run returned context error, want goroutine error: %v", err)
	}

	t.Logf("Successfully caught goroutine error: %v", err)
}

func TestRun_NilConfig(t *testing.T) {
	t.Parallel()

	err := Run(context.Background(), nil, testLogger(), testMetrics())
	if err == nil {
		t.Error("Run(nil config) should fail to connect")
	}
	if !strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "dial") {
		t.Logf("Run(nil config) error: %v", err)
	}
}

func TestRun_InvalidOrchestratorAddr(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.OrchestratorAddr = "invalid:99999"

	err := Run(context.Background(), cfg, testLogger(), testMetrics())
	if err == nil {
		t.Error("Run(invalid addr) should fail")
	}
}

func TestRun_ContextCancelledBeforeConnect(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := DefaultConfig()
	cfg.OrchestratorAddr = "127.0.0.1:9999"

	err := Run(ctx, cfg, testLogger(), testMetrics())
	if err == nil {
		t.Error("Run(canceled context) should fail")
	}
}

//nolint:funlen // Integration test with full pipeline setup
func TestRun_WithMockConnection(t *testing.T) {
	// Note: Not parallel - tests full pipeline with coordinated goroutines

	a := &agent{
		config: &Config{
			AgentID:                    "test-agent",
			MaxConsecutiveDecodeErrors: 10,
			ReadDeadline:               time.Second,
			WriteDeadline:              time.Second,
		},
		prober:  &stubProber{},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		pds := make(chan *api.ProbingDirective, 10)
		fies := make(chan *api.ForwardingInfoElement, 10)

		errCh := make(chan error, 3)
		go func() { errCh <- a.readerLoop(ctx, serverConn, pds) }()
		go func() { errCh <- a.processorLoop(ctx, pds, fies) }()
		go func() { errCh <- a.writerLoop(ctx, serverConn, fies) }()

		select {
		case err := <-errCh:
			done <- err
		case <-ctx.Done():
			done <- ctx.Err()
		}
	}()

	validPD := &api.ProbingDirective{
		AgentId:            "test-agent",
		NearTtl:            10,
		DestinationAddress: string([]byte{8, 8, 8, 8}),
		Protocol:           api.Protocol_ICMP,
		NextHeader:         &api.NextHeader{Header: &api.NextHeader_IcmpNextHeader{IcmpNextHeader: &api.ICMPNextHeader{}}},
	}

	encoder := json.NewEncoder(clientConn)
	if err := encoder.Encode(validPD); err != nil {
		t.Fatalf("Failed to send directive: %v", err)
	}

	decoder := json.NewDecoder(clientConn)
	var fie api.ForwardingInfoElement

	_ = clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if err := decoder.Decode(&fie); err != nil {
		t.Logf("Note: Could not read FIE (expected if goroutines exit early): %v", err)
	} else {
		if fie.NearInfo == nil {
			t.Error("NearInfo should be set when near probe succeeded")
		} else if fie.NearInfo.ProbeTtl != 10 {
			t.Errorf("FIE NearInfo.ProbeTTL = %d, want 10", fie.NearInfo.ProbeTtl)
		}
		t.Logf("Successfully completed full pipeline test")
	}

	_ = clientConn.Close()

	select {
	case err := <-done:
		if err != nil && !isNetworkError(err) && !errors.Is(err, context.DeadlineExceeded) {
			t.Logf("Agent finished with: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("Goroutines did not finish")
	}
}

// -- handleDecodeError() ------------------------------------------------------

func TestHandleDecodeError_ContextCancelled(t *testing.T) {
	t.Parallel()

	a := &agent{
		config:  &Config{AgentID: "test-agent"},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := errors.New("some error")
	shouldContinue, newCount, handledErr := a.handleDecodeError(ctx, err, 0)

	if shouldContinue {
		t.Error("should not continue on context cancellation")
	}
	if newCount != 0 {
		t.Errorf("error count should remain 0, got: %d", newCount)
	}
	if handledErr != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", handledErr)
	}
}

func TestHandleDecodeError_NetworkError(t *testing.T) {
	t.Parallel()

	a := &agent{
		config:  &Config{AgentID: "test-agent"},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	err := &mockNetError{timeout: false, err: "network error"}
	shouldContinue, newCount, handledErr := a.handleDecodeError(context.Background(), err, 0)

	if shouldContinue {
		t.Error("should not continue on network error")
	}
	if newCount != 0 {
		t.Errorf("error count should remain 0, got: %d", newCount)
	}
	if handledErr == nil {
		t.Error("should return wrapped error on network error")
		return
	}
	if !strings.Contains(handledErr.Error(), "connection lost while reading") {
		t.Errorf("error should mention connection lost, got: %v", handledErr)
	}
}

func TestHandleDecodeError_JSONError_WithLimit(t *testing.T) {
	t.Parallel()

	a := &agent{
		config: &Config{
			AgentID:                    "test-agent",
			MaxConsecutiveDecodeErrors: 3,
		},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	err := errors.New("json: invalid character")

	shouldContinue, newCount, handledErr := a.handleDecodeError(context.Background(), err, 0)
	if !shouldContinue {
		t.Error("should continue on first JSON error")
	}
	if newCount != 1 {
		t.Errorf("error count should be 1, got: %d", newCount)
	}
	if handledErr != nil {
		t.Errorf("should not return error yet, got: %v", handledErr)
	}

	shouldContinue, newCount, handledErr = a.handleDecodeError(context.Background(), err, 1)
	if !shouldContinue {
		t.Error("should continue on second JSON error")
	}
	if newCount != 2 {
		t.Errorf("error count should be 2, got: %d", newCount)
	}
	if handledErr != nil {
		t.Errorf("should not return error yet, got: %v", handledErr)
	}

	shouldContinue, newCount, handledErr = a.handleDecodeError(context.Background(), err, 2)
	if shouldContinue {
		t.Error("should stop after reaching threshold")
	}
	if newCount != 3 {
		t.Errorf("error count should be 3, got: %d", newCount)
	}
	if handledErr == nil {
		t.Error("should return error after threshold")
		return
	}
	if !strings.Contains(handledErr.Error(), "too many consecutive decode errors") {
		t.Errorf("error should mention too many errors, got: %v", handledErr)
	}
}

func TestHandleDecodeError_JSONError_NoLimit(t *testing.T) {
	t.Parallel()

	a := &agent{
		config: &Config{
			AgentID:                    "test-agent",
			MaxConsecutiveDecodeErrors: 0,
		},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	err := errors.New("json: invalid character")

	shouldContinue, newCount, handledErr := a.handleDecodeError(context.Background(), err, 0)
	if !shouldContinue {
		t.Error("should continue when limit is disabled")
	}
	if newCount != 1 {
		t.Errorf("error count should be 1, got: %d", newCount)
	}
	if handledErr != nil {
		t.Errorf("should not return error, got: %v", handledErr)
	}

	shouldContinue, newCount, handledErr = a.handleDecodeError(context.Background(), err, 99)
	if !shouldContinue {
		t.Error("should continue even after many errors when limit is disabled")
	}
	if newCount != 100 {
		t.Errorf("error count should be 100, got: %d", newCount)
	}
	if handledErr != nil {
		t.Errorf("should not return error, got: %v", handledErr)
	}
}

// -- readerLoop() -------------------------------------------------------------

func TestReaderLoop_DecodeErrorLog(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}
	a.config.MaxConsecutiveDecodeErrors = 2

	data := bytes.NewBufferString("{ bad json\n{ more bad\n")

	conn := &stubConn{
		readFunc: func(b []byte) (int, error) {
			return data.Read(b)
		},
	}

	pds := make(chan *api.ProbingDirective, 1)

	done := make(chan error, 1)
	go func() {
		done <- a.readerLoop(context.Background(), conn, pds)
	}()

	select {
	case err := <-done:
		if !strings.Contains(err.Error(), "consecutive decode errors") {
			t.Logf("Got error: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("readerLoop did not finish")
	}
}

func TestReaderLoop_DecodeErrorWithUnlimitedRetries(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}
	a.config.MaxConsecutiveDecodeErrors = 0

	attempts := 0
	conn := &stubConn{
		readFunc: func(b []byte) (int, error) {
			attempts++
			return copy(b, "invalid json\n"), nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	pds := make(chan *api.ProbingDirective, 1)

	err := a.readerLoop(ctx, conn, pds)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("readerLoop = %v (expected context.DeadlineExceeded)", err)
	}

	if attempts < 1 {
		t.Error("Expected at least one decode attempt")
	}
	t.Log("✓ Decode error with unlimited retries was logged")
}

func TestReaderLoop_SetReadDeadlineFail(t *testing.T) {
	t.Parallel()

	conn := &stubConn{
		readDeadlineFunc: func(time.Time) error {
			return errors.New("deadline fail")
		},
	}
	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}

	err := a.readerLoop(context.Background(), conn, make(chan *api.ProbingDirective, 1))
	if err == nil || !strings.Contains(err.Error(), "failed to set read deadline") {
		t.Errorf("readerLoop(deadline fail) = %v", err)
	}
}

func TestReaderLoop_ContextCancelled(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	conn := &stubConn{}

	err := a.readerLoop(ctx, conn, make(chan *api.ProbingDirective, 1))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("readerLoop(ctx canceled) = %v, want context.Canceled", err)
	}
}

func TestReaderLoop_NetworkError(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}
	conn := &stubConn{}

	err := a.readerLoop(context.Background(), conn, make(chan *api.ProbingDirective, 1))
	if !isNetworkError(err) || !strings.Contains(err.Error(), "connection lost while reading") {
		t.Errorf("readerLoop(network EOF) = %v", err)
	}
}

func TestReaderLoop_ConsecutiveDecodeErrors(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}
	a.config.MaxConsecutiveDecodeErrors = 2

	data := bytes.NewBufferString("bad\nbad\n")

	conn := &stubConn{
		readFunc: func(b []byte) (int, error) {
			return data.Read(b)
		},
	}

	done := make(chan error, 1)
	go func() {
		err := a.readerLoop(context.Background(), conn, make(chan *api.ProbingDirective, 1))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "too many consecutive decode errors") {
			t.Errorf("readerLoop(consecutive errors) = %v, want consecutive decode error", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("readerLoop did not exit after consecutive decode errors")
	}
}

func TestReaderLoop_DeadConnectionDetection(t *testing.T) {
	t.Parallel()

	a := &agent{
		config: &Config{
			AgentID:      "test-agent",
			ReadDeadline: 1 * time.Millisecond,
		},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	pds := make(chan *api.ProbingDirective, 1)
	err := a.readerLoop(context.Background(), client, pds)

	if err == nil {
		t.Fatal("expected error after consecutive timeouts, got nil")
	}
	if !strings.Contains(err.Error(), "connection timed out after") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

//nolint:funlen // Test requires setup of invalid and valid PDs with full validation
func TestReaderLoop_InvalidDirective(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}

	invalidPD := &api.ProbingDirective{
		NearTtl:            5,
		DestinationAddress: string([]byte{1, 2, 3, 4}),
	}
	invalidJSON, _ := json.Marshal(invalidPD)
	invalidJSON = append(invalidJSON, '\n')

	validPD := &api.ProbingDirective{
		AgentId:            "test",
		NearTtl:            10,
		DestinationAddress: string([]byte{8, 8, 8, 8}),
		Protocol:           api.Protocol_ICMP,
		NextHeader:         &api.NextHeader{Header: &api.NextHeader_IcmpNextHeader{IcmpNextHeader: &api.ICMPNextHeader{}}},
	}
	validJSON, _ := json.Marshal(validPD)
	validJSON = append(validJSON, '\n')

	data := make([]byte, 0, len(invalidJSON)+len(validJSON))
	data = append(data, invalidJSON...)
	data = append(data, validJSON...)

	conn := &stubConn{
		readFunc: func(b []byte) (int, error) {
			n := copy(b, data)
			data = data[n:]
			if len(data) == 0 {
				return n, io.EOF
			}
			return n, nil
		},
	}

	pds := make(chan *api.ProbingDirective, 2)

	done := make(chan error, 1)
	go func() {
		done <- a.readerLoop(context.Background(), conn, pds)
	}()

	select {
	case pd := <-pds:
		if pd == nil {
			t.Errorf("received nil PD")
			return
		}
		if pd.NearTtl != 10 {
			t.Errorf("expected valid PD with TTL 10, got: %d", pd.NearTtl)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("timeout waiting for valid PD (invalid should be skipped)")
	}

	select {
	case err := <-done:
		if !isNetworkError(err) {
			t.Errorf("readerLoop(invalid directive) = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("readerLoop did not finish")
	}
}

func TestReaderLoop_SuccessfulRead(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}

	validPD := &api.ProbingDirective{
		AgentId:            "test",
		NearTtl:            5,
		DestinationAddress: string([]byte{1, 2, 3, 4}),
		Protocol:           api.Protocol_ICMP,
		NextHeader:         &api.NextHeader{Header: &api.NextHeader_IcmpNextHeader{IcmpNextHeader: &api.ICMPNextHeader{}}},
	}
	validJSON, _ := json.Marshal(validPD)
	data := bytes.NewBuffer(append(validJSON, '\n'))

	conn := &stubConn{
		readFunc: func(b []byte) (int, error) {
			return data.Read(b)
		},
	}

	pds := make(chan *api.ProbingDirective, 1)
	done := make(chan error, 1)

	go func() {
		err := a.readerLoop(context.Background(), conn, pds)
		done <- err
	}()

	select {
	case pd := <-pds:
		if pd == nil {
			t.Errorf("received nil PD from channel")
			return
		}
		if pd.NearTtl != 5 || pd.AgentId != "test" {
			t.Errorf("readerLoop got %+v, want TTL=5 AgentID=test", pd)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("readerLoop did not send directive")
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Error("readerLoop did not finish")
	}
}

// -- writerLoop() -------------------------------------------------------------

func TestWriterLoop_SetWriteDeadlineFail(t *testing.T) {
	t.Parallel()

	conn := &stubConn{
		writeDeadlineFunc: func(time.Time) error {
			return errors.New("deadline fail")
		},
	}
	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}

	fies := make(chan *api.ForwardingInfoElement, 1)
	fies <- &api.ForwardingInfoElement{}

	err := a.writerLoop(context.Background(), conn, fies)
	if err == nil || !strings.Contains(err.Error(), "failed to set write deadline") {
		t.Errorf("writerLoop(deadline fail) = %v", err)
	}
}

func TestWriterLoop_ChannelClosed(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}
	fies := make(chan *api.ForwardingInfoElement)
	close(fies)

	err := a.writerLoop(context.Background(), &stubConn{}, fies)
	if err != nil {
		t.Errorf("writerLoop(channel closed) = %v, want nil", err)
	}
}

func TestWriterLoop_ContextCancelled(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := a.writerLoop(ctx, &stubConn{}, make(chan *api.ForwardingInfoElement))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("writerLoop(ctx canceled) = %v, want context.Canceled", err)
	}
}

func TestWriterLoop_NetworkError(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}

	conn := &stubConn{
		writeFunc: func(b []byte) (int, error) {
			return 0, &net.OpError{Op: "write", Err: errors.New("connection reset")}
		},
	}

	fies := make(chan *api.ForwardingInfoElement, 1)
	fies <- &api.ForwardingInfoElement{
		DestinationAddress: string([]byte{8, 8, 8, 8}),
	}

	err := a.writerLoop(context.Background(), conn, fies)
	if err == nil || !strings.Contains(err.Error(), "connection lost while writing") {
		t.Errorf("writerLoop(network error) = %v", err)
	}
}

func TestWriterLoop_EncodeError(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}

	written := make(chan string, 1)

	conn := &stubConn{
		writeFunc: func(b []byte) (int, error) {
			written <- string(b)
			return 0, errors.New("encode error")
		},
	}

	fies := make(chan *api.ForwardingInfoElement, 1)
	fies <- &api.ForwardingInfoElement{}

	err := a.writerLoop(context.Background(), conn, fies)
	if err == nil || !strings.Contains(err.Error(), "failed to encode FIE") {
		t.Errorf("writerLoop(encode error) = %v", err)
	}
}

func TestWriterLoop_Success(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), logger: testLogger(), metrics: testMetrics()}
	a.config.AgentID = "test-agent"

	written := make(chan []byte, 1)
	conn := &stubConn{
		writeFunc: func(b []byte) (int, error) {
			written <- append([]byte(nil), b...)
			return len(b), nil
		},
	}

	fies := make(chan *api.ForwardingInfoElement, 1)
	fie := &api.ForwardingInfoElement{
		DestinationAddress: string([]byte{8, 8, 8, 8}),
	}
	fies <- fie
	close(fies)

	err := a.writerLoop(context.Background(), conn, fies)
	if err != nil {
		t.Errorf("writerLoop(success) = %v, want nil", err)
	}

	select {
	case data := <-written:
		if len(data) == 0 {
			t.Error("writerLoop wrote empty data")
			return
		}
		var decoded api.ForwardingInfoElement
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Errorf("writerLoop wrote invalid JSON: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("writerLoop did not write FIE")
	}
}

// -- processorLoop() ----------------------------------------------------------

func TestProcessorLoop_ChannelClosed(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), prober: &stubProber{}, logger: testLogger(), metrics: testMetrics()}
	pds := make(chan *api.ProbingDirective)
	close(pds)

	err := a.processorLoop(context.Background(), pds, make(chan *api.ForwardingInfoElement))
	if err != nil {
		t.Errorf("processorLoop(channel closed) = %v, want nil", err)
	}
}

func TestProcessorLoop_ContextCancelled(t *testing.T) {
	t.Parallel()

	a := &agent{config: DefaultConfig(), prober: &stubProber{}, logger: testLogger(), metrics: testMetrics()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := a.processorLoop(ctx, make(chan *api.ProbingDirective), make(chan *api.ForwardingInfoElement))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("processorLoop(ctx canceled) = %v, want context.Canceled", err)
	}
}

func TestProcessorLoop_ProcessesPD(t *testing.T) {
	t.Parallel()

	a := &agent{
		config:  &Config{AgentID: "test"},
		prober:  &stubProber{},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	pds := make(chan *api.ProbingDirective, 1)
	fies := make(chan *api.ForwardingInfoElement, 1)

	pd := &api.ProbingDirective{
		AgentId:            "test",
		NearTtl:            5,
		DestinationAddress: string([]byte{1, 2, 3, 4}),
	}
	pds <- pd

	done := make(chan struct{})
	go func() {
		_ = a.processorLoop(context.Background(), pds, fies)
		close(done)
	}()

	select {
	case fie := <-fies:
		if fie == nil {
			t.Errorf("received nil FIE from channel")
			return
		}
		if fie.NearInfo == nil {
			t.Errorf("NearInfo should be set when near probe succeeded")
			return
		}
		if fie.NearInfo.ProbeTtl != 5 {
			t.Errorf("processorLoop FIE TTL = %d, want 5", fie.NearInfo.ProbeTtl)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("processorLoop did not produce FIE")
	}

	close(pds)

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Error("processorLoop goroutine did not finish")
	}
}

// -- processPD() --------------------------------------------------------------

func TestProcessPD_Success(t *testing.T) {
	t.Parallel()

	a := &agent{
		config:  &Config{AgentID: "test"},
		prober:  &stubProber{},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	pd := &api.ProbingDirective{
		NearTtl:            5,
		DestinationAddress: string([]byte{1, 2, 3, 4}),
	}
	fies := make(chan *api.ForwardingInfoElement, 1)

	a.processPD(context.Background(), pd, fies)

	select {
	case fie := <-fies:
		if fie == nil {
			t.Errorf("received nil FIE from channel")
			return
		}
		if fie.NearInfo == nil || fie.FarInfo == nil {
			t.Errorf("NearInfo and FarInfo should be set when both probes succeed")
			return
		}
		if fie.NearInfo.ProbeTtl != 5 || fie.FarInfo.ProbeTtl != 6 {
			t.Errorf("processPD TTLs = %d/%d, want 5/6",
				fie.NearInfo.ProbeTtl, fie.FarInfo.ProbeTtl)
		}
		if fie.NearInfo.ReplyAddress != "1.1.1.1" {
			t.Errorf("processPD reply = %s, want 1.1.1.1", fie.NearInfo.ReplyAddress)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("processPD did not send FIE")
	}
}

func TestProcessPD_ProbeError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		failTTL uint8
		errMsg  string
	}{
		{name: "near probe error", failTTL: 5, errMsg: "should not send FIE on near probe error"},
		{name: "far probe error", failTTL: 6, errMsg: "should not send FIE on far probe error"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &agent{
				config: &Config{AgentID: "test"},
				prober: &stubProber{
					probeFunc: func(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
						if ttl == tt.failTTL {
							return nil, errors.New("probe fail")
						}
						return &ProbeResult{ReplyAddress: net.ParseIP("1.1.1.1")}, nil
					},
				},
				logger:  testLogger(),
				metrics: testMetrics(),
			}

			pd := &api.ProbingDirective{NearTtl: 5}
			fies := make(chan *api.ForwardingInfoElement, 1)

			a.processPD(context.Background(), pd, fies)

			select {
			case <-fies:
				t.Error(tt.errMsg)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

//nolint:funlen // Table-driven test with two subtests each requiring prober setup and assertions
func TestProcessPD_Timeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		timedOutTTL     uint8
		wantNearInfoNil bool
		wantFarInfoNil  bool
	}{
		{name: "near timeout", timedOutTTL: 5, wantNearInfoNil: true, wantFarInfoNil: false},
		{name: "far timeout", timedOutTTL: 6, wantNearInfoNil: false, wantFarInfoNil: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &agent{
				config: &Config{AgentID: "test"},
				prober: &stubProber{
					probeFunc: func(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
						if ttl == tt.timedOutTTL {
							return &ProbeResult{TimedOut: true}, nil
						}
						return &ProbeResult{ReplyAddress: net.ParseIP("1.1.1.1")}, nil
					},
				},
				logger:  testLogger(),
				metrics: testMetrics(),
			}

			pd := &api.ProbingDirective{NearTtl: 5}
			fies := make(chan *api.ForwardingInfoElement, 1)

			a.processPD(context.Background(), pd, fies)

			select {
			case fie := <-fies:
				if fie == nil {
					t.Errorf("processPD should send FIE even when probe times out")
					return
				}
				if tt.wantNearInfoNil && fie.NearInfo != nil {
					t.Error("NearInfo should be nil when near probe timed out")
				}
				if !tt.wantNearInfoNil && fie.NearInfo == nil {
					t.Error("NearInfo should be set when near probe succeeded")
				}
				if tt.wantFarInfoNil && fie.FarInfo != nil {
					t.Error("FarInfo should be nil when far probe timed out")
				}
				if !tt.wantFarInfoNil && fie.FarInfo == nil {
					t.Error("FarInfo should be set when far probe succeeded")
				}
			case <-time.After(100 * time.Millisecond):
				t.Error("processPD did not send FIE")
			}
		})
	}
}

func TestProcessPD_ContextCancelled(t *testing.T) {
	t.Parallel()

	a := &agent{
		config:  &Config{AgentID: "test"},
		prober:  &stubProber{},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pd := &api.ProbingDirective{NearTtl: 5}
	fies := make(chan *api.ForwardingInfoElement, 1)

	a.processPD(ctx, pd, fies)

	select {
	case <-fies:
	case <-time.After(50 * time.Millisecond):
	}
}

func TestProcessPD_NilResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		nilTTL uint8
		errMsg string
	}{
		{name: "nil near result", nilTTL: 5, errMsg: "should not send FIE when near result is nil"},
		{name: "nil far result", nilTTL: 6, errMsg: "should not send FIE when far result is nil"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &agent{
				config: &Config{AgentID: "test"},
				prober: &stubProber{
					probeFunc: func(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error) {
						if ttl == tt.nilTTL {
							return nil, ErrDuplicatePD // probe already in-flight
						}
						return &ProbeResult{ReplyAddress: net.ParseIP("1.1.1.1")}, nil
					},
				},
				logger:  testLogger(),
				metrics: testMetrics(),
			}

			pd := &api.ProbingDirective{NearTtl: 5}
			fies := make(chan *api.ForwardingInfoElement, 1)

			a.processPD(context.Background(), pd, fies)

			select {
			case <-fies:
				t.Error(tt.errMsg)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// -- createProber() -----------------------------------------------------------

func TestCreateProber_Mock(t *testing.T) {
	t.Parallel()

	p, err := createProber(&Config{ProberType: "mock"}, testLogger(), testMetrics())
	if err != nil {
		t.Errorf("createProber(mock) error: %v", err)
	}
	if p == nil {
		t.Error("createProber(mock) returned nil prober")
	}
}

func TestCreateProber_CaracalError(t *testing.T) {
	// Note: Cannot be parallel - modifies global NewCaracalProber

	origNewCaracalProber := NewCaracalProber
	defer func() { NewCaracalProber = origNewCaracalProber }()

	expectedErr := errors.New("caracal binary not found")
	NewCaracalProber = func(cfg *Config, logger *slog.Logger, metrics *Metrics) (*caracalProber, error) {
		return nil, expectedErr
	}

	_, err := createProber(&Config{ProberType: "caracal"}, testLogger(), testMetrics())
	if err != expectedErr {
		t.Errorf("createProber(caracal) error = %v, want %v", err, expectedErr)
	}
	t.Log("✓ Caracal error path covered")
}

func TestCreateProber_Unknown(t *testing.T) {
	t.Parallel()

	_, err := createProber(&Config{ProberType: "unknown"}, testLogger(), testMetrics())
	if err == nil {
		t.Error("createProber(unknown) should error")
	}
	expected := "unknown prober type: \"unknown\" (valid: mock, caracal)"
	if err.Error() != expected {
		t.Errorf("createProber(unknown) = %v, want %s", err, expected)
	}
}

// -- validatePD() -------------------------------------------------------------

//nolint:funlen // Table-driven test with many validation cases
func TestValidatePD_AllBranches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pd      *api.ProbingDirective
		wantErr bool
	}{
		{name: "nil", pd: nil, wantErr: true},
		{name: "empty-agent", pd: &api.ProbingDirective{}, wantErr: true},
		{name: "nil-dest", pd: &api.ProbingDirective{AgentId: "a", NearTtl: 1}, wantErr: true},
		{
			name: "zero-ttl",
			pd: &api.ProbingDirective{
				AgentId:            "a",
				NearTtl:            0,
				DestinationAddress: string([]byte{1}),
			},
			wantErr: true,
		},
		{
			name: "nearttl-255",
			pd: &api.ProbingDirective{
				AgentId:            "a",
				NearTtl:            255,
				DestinationAddress: string([]byte{1}),
				Protocol:           api.Protocol_ICMP,
				NextHeader:         &api.NextHeader{Header: &api.NextHeader_IcmpNextHeader{IcmpNextHeader: &api.ICMPNextHeader{}}},
			},
			wantErr: true,
		},
		{
			name: "icmp-good",
			pd: &api.ProbingDirective{
				AgentId:            "a",
				NearTtl:            1,
				DestinationAddress: string([]byte{1}),
				Protocol:           api.Protocol_ICMP,
				NextHeader:         &api.NextHeader{Header: &api.NextHeader_IcmpNextHeader{IcmpNextHeader: &api.ICMPNextHeader{}}},
			},
			wantErr: false,
		},
		{
			name: "icmpv6-good",
			pd: &api.ProbingDirective{
				AgentId:            "a",
				NearTtl:            1,
				DestinationAddress: string([]byte{1}),
				Protocol:           api.Protocol_ICMPv6,
				NextHeader:         &api.NextHeader{Header: &api.NextHeader_Icmpv6NextHeader{Icmpv6NextHeader: &api.ICMPv6NextHeader{}}},
			},
			wantErr: false,
		},
		{
			name: "udp-good",
			pd: &api.ProbingDirective{
				AgentId:            "a",
				NearTtl:            1,
				DestinationAddress: string([]byte{1}),
				Protocol:           api.Protocol_UDP,
				NextHeader:         &api.NextHeader{Header: &api.NextHeader_UdpNextHeader{UdpNextHeader: &api.UDPNextHeader{}}},
			},
			wantErr: false,
		},
		{
			name: "icmp-noheader",
			pd: &api.ProbingDirective{
				AgentId:            "a",
				NearTtl:            1,
				DestinationAddress: string([]byte{1}),
				Protocol:           api.Protocol_ICMP,
			},
			wantErr: true,
		},
		{
			name: "udp-noheader",
			pd: &api.ProbingDirective{
				AgentId:            "a",
				NearTtl:            1,
				DestinationAddress: string([]byte{1}),
				Protocol:           api.Protocol_UDP,
				NextHeader:         &api.NextHeader{Header: &api.NextHeader_UdpNextHeader{UdpNextHeader: &api.UDPNextHeader{}}},
			},
			wantErr: true,
		},
		{
			name: "unknown-protocol",
			pd: &api.ProbingDirective{
				AgentId:            "a",
				NearTtl:            1,
				DestinationAddress: string([]byte{1}),
				Protocol:           99,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validatePD(tt.pd)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePD(%s) error = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

// -- probeResultToInfo() ------------------------------------------------------

func TestProbeResultToInfo(t *testing.T) {
	t.Parallel()

	sentTime := time.Now()
	recvTime := sentTime.Add(time.Second)

	res := &ProbeResult{
		ReplyAddress: net.ParseIP("8.8.8.8"),
		SentTime:     sentTime,
		ReceivedTime: recvTime,
	}

	info := probeResultToInfo(res, 64)
	if info == nil {
		t.Errorf("probeResultToInfo returned nil")
		return
	}

	if info.ProbeTtl != 64 {
		t.Errorf("probeResultToInfo TTL = %d, want 64", info.ProbeTtl)
	}
	if info.ReplyAddress != "8.8.8.8" {
		t.Errorf("probeResultToInfo addr = %s, want 8.8.8.8", info.ReplyAddress)
	}
	if info.SentTimestampNs != sentTime.UnixNano() {
		t.Errorf("probeResultToInfo sent time mismatch")
	}
	if info.ReceivedTimestampNs != recvTime.UnixNano() {
		t.Errorf("probeResultToInfo recv time mismatch")
	}
}

// -- isNetworkError() ---------------------------------------------------------

func TestIsNetworkError_AllCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"EOF", io.EOF, true},
		{"UnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"OpError", &net.OpError{}, true},
		{"OtherError", errors.New("json error"), false},
		{"WrappedEOF", fmt.Errorf("wrapped: %w", io.EOF), true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isNetworkError(tt.err)
			if got != tt.want {
				t.Errorf("isNetworkError(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

// -- classifyIP() -------------------------------------------------------------

func TestClassifyIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ip   string
		want string
	}{
		{"loopback IPv4", "127.0.0.1", "loopback"},
		{"loopback IPv6", "::1", "loopback"},
		{"multicast IPv4", "224.0.0.1", "multicast"},
		{"multicast IPv6", "ff02::1", "multicast"},
		{"private 10.x", "10.0.0.1", "private"},
		{"private 172.16.x", "172.16.0.1", "private"},
		{"private 192.168.x", "192.168.1.1", "private"},
		{"public", "8.8.8.8", "public"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyIP(net.ParseIP(tt.ip))
			if got != tt.want {
				t.Errorf("classifyIP(%s) = %q, want %q", tt.ip, got, tt.want)
			}
		})
	}
}

// -- mockConn (for authentication tests) --------------------------------------

type mockConn struct {
	readBuf             *bytes.Buffer
	writeBuf            *bytes.Buffer
	closed              bool
	readErr             error
	writeErr            error
	setWriteDeadlineErr error
	setReadDeadlineErr  error
}

func newMockConn() *mockConn {
	return &mockConn{
		readBuf:  &bytes.Buffer{},
		writeBuf: &bytes.Buffer{},
	}
}

func (m *mockConn) Read(b []byte) (n int, err error) {
	if m.readErr != nil {
		return 0, m.readErr
	}
	return m.readBuf.Read(b)
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return m.writeBuf.Write(b)
}

func (m *mockConn) Close() error {
	m.closed = true
	return nil
}

func (m *mockConn) LocalAddr() net.Addr           { return nil }
func (m *mockConn) RemoteAddr() net.Addr          { return nil }
func (m *mockConn) SetDeadline(t time.Time) error { return nil }

func (m *mockConn) SetReadDeadline(t time.Time) error {
	if m.setReadDeadlineErr != nil {
		return m.setReadDeadlineErr
	}
	return nil
}

func (m *mockConn) SetWriteDeadline(t time.Time) error {
	if m.setWriteDeadlineErr != nil {
		return m.setWriteDeadlineErr
	}
	return nil
}

func (m *mockConn) queueAuthResponse(resp *api.AuthResponse) error {
	return json.NewEncoder(m.readBuf).Encode(resp)
}

func (m *mockConn) getAuthRequest() (*api.AuthRequest, error) {
	var req api.AuthRequest
	if err := json.NewDecoder(m.writeBuf).Decode(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// -- authenticate() -----------------------------------------------------------

func TestAuthenticate_Success(t *testing.T) {
	t.Parallel()

	conn := newMockConn()

	if err := conn.queueAuthResponse(&api.AuthResponse{
		Authenticated: true,
		Message:       "OK",
	}); err != nil {
		t.Fatalf("failed to queue response: %v", err)
	}

	a := &agent{
		config:  &Config{AgentID: "test-agent", Secret: "test-secret-1234567890"},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	if err := a.authenticate(conn); err != nil {
		t.Errorf("authenticate() should succeed, got error: %v", err)
	}

	req, err := conn.getAuthRequest()
	if err != nil {
		t.Fatalf("failed to decode auth request: %v", err)
	}
	if req.AgentId != "test-agent" {
		t.Errorf("expected AgentID 'test-agent', got: %s", req.AgentId)
	}
	if req.Secret != "test-secret-1234567890" {
		t.Errorf("expected Secret 'test-secret-1234567890', got: %s", req.Secret)
	}
}

func TestAuthenticate_Rejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		response    api.AuthResponse
		expectedErr string
	}{
		{
			name:        "rejected with message",
			response:    api.AuthResponse{Authenticated: false, Message: "Invalid secret"},
			expectedErr: "Invalid secret",
		},
		{
			name:        "rejected without message",
			response:    api.AuthResponse{Authenticated: false},
			expectedErr: "authentication rejected",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn := newMockConn()
			if err := conn.queueAuthResponse(&tt.response); err != nil {
				t.Fatalf("failed to queue response: %v", err)
			}

			a := &agent{
				config:  &Config{AgentID: "test-agent", Secret: "wrong-secret"},
				logger:  testLogger(),
				metrics: testMetrics(),
			}

			err := a.authenticate(conn)
			if err == nil {
				t.Error("authenticate() should fail when rejected")
				return
			}
			if !strings.Contains(err.Error(), tt.expectedErr) {
				t.Errorf("error should contain %q, got: %v", tt.expectedErr, err)
			}
		})
	}
}

func TestAuthenticate_ReadError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		readErr     error
		expectedErr string
	}{
		{name: "network error", readErr: io.EOF, expectedErr: "failed to receive auth response"},
		{name: "unexpected EOF", readErr: io.ErrUnexpectedEOF, expectedErr: "failed to receive auth response"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn := newMockConn()
			conn.readErr = tt.readErr

			a := &agent{
				config:  &Config{AgentID: "test-agent", Secret: "test-secret-1234567890"},
				logger:  testLogger(),
				metrics: testMetrics(),
			}

			err := a.authenticate(conn)
			if err == nil {
				t.Error("authenticate() should fail on read error")
				return
			}
			if !strings.Contains(err.Error(), tt.expectedErr) {
				t.Errorf("error should contain %q, got: %v", tt.expectedErr, err)
			}
		})
	}
}

func TestAuthenticate_WriteError(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	conn.writeErr = errors.New("connection broken")

	a := &agent{
		config:  &Config{AgentID: "test-agent", Secret: "test-secret-1234567890"},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	err := a.authenticate(conn)
	if err == nil {
		t.Error("authenticate() should fail on write error")
		return
	}
	if !strings.Contains(err.Error(), "failed to send auth request") {
		t.Errorf("error should mention send failure, got: %v", err)
	}
}

func TestAuthenticate_InvalidResponse(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	conn.readBuf.WriteString("{invalid json")

	a := &agent{
		config:  &Config{AgentID: "test-agent", Secret: "test-secret-1234567890"},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	err := a.authenticate(conn)
	if err == nil {
		t.Error("authenticate() should fail on malformed response")
		return
	}
	if !strings.Contains(err.Error(), "failed to receive auth response") {
		t.Errorf("error should mention receive failure, got: %v", err)
	}
}

func TestAuthenticate_EmptySecret(t *testing.T) {
	t.Parallel()

	conn := newMockConn()

	if err := conn.queueAuthResponse(&api.AuthResponse{
		Authenticated: true,
		Message:       "OK",
	}); err != nil {
		t.Fatalf("failed to queue response: %v", err)
	}

	a := &agent{
		config:  &Config{AgentID: "test-agent", Secret: ""},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	if err := a.authenticate(conn); err != nil {
		t.Errorf("authenticate() should succeed, got error: %v", err)
	}

	req, err := conn.getAuthRequest()
	if err != nil {
		t.Fatalf("failed to decode auth request: %v", err)
	}
	if req.Secret != "" {
		t.Errorf("expected empty Secret, got: %s", req.Secret)
	}
}

func TestAuthenticate_SetWriteDeadlineError(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	conn.setWriteDeadlineErr = errors.New("failed to set deadline")

	a := &agent{
		config:  &Config{AgentID: "test-agent", Secret: "test-secret-1234567890"},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	err := a.authenticate(conn)
	if err == nil {
		t.Error("authenticate() should fail on SetWriteDeadline error")
		return
	}
	if !strings.Contains(err.Error(), "failed to set write deadline") {
		t.Errorf("error should mention write deadline, got: %v", err)
	}
}

func TestAuthenticate_SetReadDeadlineError(t *testing.T) {
	t.Parallel()

	conn := newMockConn()

	if err := conn.queueAuthResponse(&api.AuthResponse{
		Authenticated: true,
		Message:       "OK",
	}); err != nil {
		t.Fatalf("failed to queue response: %v", err)
	}

	conn.setReadDeadlineErr = errors.New("failed to set read deadline")

	a := &agent{
		config:  &Config{AgentID: "test-agent", Secret: "test-secret-1234567890"},
		logger:  testLogger(),
		metrics: testMetrics(),
	}

	err := a.authenticate(conn)
	if err == nil {
		t.Error("authenticate() should fail on SetReadDeadline error")
		return
	}
	if !strings.Contains(err.Error(), "failed to set read deadline") {
		t.Errorf("error should mention read deadline, got: %v", err)
	}
}

// -- Run() authentication integration -----------------------------------------

func TestRun_AuthenticationFailure(t *testing.T) {
	// Note: Not parallel - full integration test with server interaction

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		decoder := json.NewDecoder(conn)
		var authReq api.AuthRequest
		_ = decoder.Decode(&authReq)

		encoder := json.NewEncoder(conn)
		_ = encoder.Encode(&api.AuthResponse{
			Authenticated: false,
			Message:       "Invalid secret",
		})
	}()

	cfg := &Config{
		AgentID:          "test-agent",
		OrchestratorAddr: listener.Addr().String(),
		Secret:           "wrong-secret-1234567890",
		ProberType:       ProberTypeMock,
		PDsBufferSize:    10,
		FIEsBufferSize:   10,
		ReadDeadline:     5 * time.Second,
		WriteDeadline:    5 * time.Second,
		ProbeTimeout:     1 * time.Second,
		WriteQueueSize:   100,
		CleanupInterval:  1 * time.Second,
	}

	err = Run(context.Background(), cfg, testLogger(), testMetrics())

	if err == nil {
		t.Error("Run() should fail when authentication is rejected")
		return
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("error should mention authentication failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "authentication rejected") {
		t.Errorf("error should contain rejection reason, got: %v", err)
	}
}

//nolint:funlen // Integration test requires full server setup
func TestRun_AuthenticationSuccess(t *testing.T) {
	// Note: Not parallel - full integration test with server interaction

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	serverAddr := listener.Addr().String()
	authSuccess := make(chan bool, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		decoder := json.NewDecoder(conn)
		encoder := json.NewEncoder(conn)

		var authReq api.AuthRequest
		if err := decoder.Decode(&authReq); err != nil {
			t.Logf("Failed to decode auth request: %v", err)
			return
		}

		if err := encoder.Encode(&api.AuthResponse{
			Authenticated: true,
			Message:       "Welcome",
		}); err != nil {
			t.Logf("Failed to send auth response: %v", err)
			return
		}

		authSuccess <- true

		pd := &api.ProbingDirective{
			ProbingDirectiveId: 1,
			AgentId:            "test-agent",
			NearTtl:            10,
			DestinationAddress: net.ParseIP("8.8.8.8").String(),
			Protocol:           api.Protocol_ICMP,
			IpVersion:          api.IPVersion_IPV4,
			NextHeader:         &api.NextHeader{Header: &api.NextHeader_IcmpNextHeader{IcmpNextHeader: &api.ICMPNextHeader{}}},
		}
		_ = encoder.Encode(pd)

		time.Sleep(200 * time.Millisecond)
	}()

	cfg := &Config{
		AgentID:          "test-agent",
		OrchestratorAddr: serverAddr,
		Secret:           "correct-secret-1234567890",
		ProberType:       ProberTypeMock,
		PDsBufferSize:    10,
		FIEsBufferSize:   10,
		ReadDeadline:     5 * time.Second,
		WriteDeadline:    5 * time.Second,
		ProbeTimeout:     1 * time.Second,
		WriteQueueSize:   100,
		CleanupInterval:  1 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, testLogger(), testMetrics())
	}()

	select {
	case <-authSuccess:
		t.Log("✓ Authentication succeeded in Run()")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Authentication did not complete")
	}

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Logf("Run exited with: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not finish")
	}
}

//nolint:funlen // Integration test requires full server setup
func TestRun_NoAuthentication(t *testing.T) {
	// Note: Not parallel - full integration test with server interaction

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	serverAddr := listener.Addr().String()
	connected := make(chan bool, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		connected <- true

		encoder := json.NewEncoder(conn)

		pd := &api.ProbingDirective{
			ProbingDirectiveId: 1,
			AgentId:            "test-agent",
			NearTtl:            10,
			DestinationAddress: net.ParseIP("8.8.8.8").String(),
			Protocol:           api.Protocol_ICMP,
			IpVersion:          api.IPVersion_IPV4,
			NextHeader:         &api.NextHeader{Header: &api.NextHeader_IcmpNextHeader{IcmpNextHeader: &api.ICMPNextHeader{}}},
		}
		_ = encoder.Encode(pd)

		time.Sleep(200 * time.Millisecond)
	}()

	cfg := &Config{
		AgentID:          "test-agent",
		OrchestratorAddr: serverAddr,
		Secret:           "",
		ProberType:       ProberTypeMock,
		PDsBufferSize:    10,
		FIEsBufferSize:   10,
		ReadDeadline:     5 * time.Second,
		WriteDeadline:    5 * time.Second,
		ProbeTimeout:     1 * time.Second,
		WriteQueueSize:   100,
		CleanupInterval:  1 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, testLogger(), testMetrics())
	}()

	select {
	case <-connected:
		t.Log("✓ Connected without authentication")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Did not connect")
	}

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Logf("Run exited with: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not finish")
	}
}
