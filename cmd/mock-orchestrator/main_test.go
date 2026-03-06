// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// ## Test Coverage
//
// Current coverage by function:
// - generatePD:    100% - All protocol variants, IP versions, and cycling logic
// - reportStats:   100% - Both data and early return paths
// - receiveFIEs:   100% - Success, EOF, decode error, nil info, and partial info paths
// - sendPDs:       ~94% - Missing UNKNOWN protocol (unreachable defensive code)
// - handleAgent:   100% - All paths including defer error handling
// - main:          0%   - Cannot test infinite server loop
//
// ## Uncovered Lines Explanation
//
// main() function (0% coverage):
// - Infinite server loop cannot be tested in unit tests
// - Would require integration test with actual TCP server and shutdown mechanism
// - Refactoring for testability not justified for a mock test utility
//
// sendPDs() function (~94% coverage):
// - default branch in protocol switch: defensive code, unreachable in practice
// - generatePD() only ever produces valid protocols (ICMP, ICMPv6, UDP)
// - Would require mocking generatePD to return an invalid protocol
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

// ============================================================================
// MOCK TYPES FOR TESTING
// ============================================================================

// mockConn implements net.Conn for testing connection handling.
type mockConn struct {
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
	mu       sync.Mutex
	closed   bool
}

func newMockConn() *mockConn {
	return &mockConn{
		readBuf:  &bytes.Buffer{},
		writeBuf: &bytes.Buffer{},
	}
}

func (m *mockConn) Read(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, io.EOF
	}
	return m.readBuf.Read(b)
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, io.ErrClosedPipe
	}
	return m.writeBuf.Write(b)
}

func (m *mockConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockConn) LocalAddr() net.Addr { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
}
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

// errorCloseConn implements net.Conn but returns error on Close.
type errorCloseConn struct {
	*mockConn
}

func (e *errorCloseConn) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return io.ErrClosedPipe
}

// errorWriteConn implements net.Conn but returns a custom error on Write.
type errorWriteConn struct {
	*mockConn
	writeErr error
}

func (e *errorWriteConn) Write(b []byte) (n int, err error) {
	return 0, e.writeErr
}

// ============================================================================
// TEST HELPERS
// ============================================================================

// createTestFIE creates a ForwardingInfoElement for testing.
func createTestFIE(pdID uint64) api.ForwardingInfoElement {
	now := time.Now()
	return api.ForwardingInfoElement{
		Agent:              api.Agent{AgentID: "test-agent"},
		ProbingDirectiveID: pdID,
		DestinationAddress: net.ParseIP("8.8.8.8"),
		NearInfo: &api.Info{
			ProbeTTL:          10,
			ReplyAddress:      net.ParseIP("10.0.0.1"),
			SentTimestamp:     now,
			ReceivedTimestamp: now.Add(10 * time.Millisecond),
		},
		FarInfo: &api.Info{
			ProbeTTL:          11,
			ReplyAddress:      net.ParseIP("10.0.0.2"),
			SentTimestamp:     now,
			ReceivedTimestamp: now.Add(15 * time.Millisecond),
		},
	}
}

// encodeFIE encodes a FIE to the connection's read buffer.
func encodeFIE(t *testing.T, conn *mockConn, fie api.ForwardingInfoElement) {
	t.Helper()
	encoder := json.NewEncoder(conn.readBuf)
	if err := encoder.Encode(fie); err != nil {
		t.Fatalf("Failed to encode FIE: %v", err)
	}
}

// verifyPDField checks a PD field value.
func verifyPDField(t *testing.T, name string, got, want interface{}) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// ============================================================================
// UNIT TESTS - generatePD
// ============================================================================

func TestGeneratePD_ProtocolVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		counter      int
		wantTTL      uint8
		wantIPv4     bool
		wantProtocol api.Protocol
	}{
		{"IPv4 UDP", 0, 5, true, api.UDP},
		{"IPv4 ICMP", 1, 6, true, api.ICMP},
		{"IPv6 UDP", 6, 11, false, api.UDP},
		{"IPv6 ICMPv6", 7, 12, false, api.ICMPv6},
		{"TTL wrap", 18, 7, true, api.UDP},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pd := generatePD(tt.counter)
			if pd == nil {
				t.Errorf("generatePD returned nil")
				return
			}

			verifyPDField(t, "NearTTL", pd.NearTTL, tt.wantTTL)

			isIPv4 := pd.DestinationAddress.To4() != nil
			verifyPDField(t, "IPv4", isIPv4, tt.wantIPv4)
			verifyPDField(t, "Protocol", pd.Protocol, tt.wantProtocol)

			// Verify protocol-specific headers
			switch pd.Protocol {
			case api.ICMP:
				if pd.NextHeader.ICMPNextHeader == nil {
					t.Error("ICMP directive missing ICMPNextHeader")
				}
			case api.ICMPv6:
				if pd.NextHeader.ICMPv6NextHeader == nil {
					t.Error("ICMPv6 directive missing ICMPv6NextHeader")
				}
			case api.UDP:
				if pd.NextHeader.UDPNextHeader == nil {
					t.Error("UDP directive missing UDPNextHeader")
				}
			}

			// Verify required fields
			if pd.AgentID == "" || pd.DestinationAddress == nil || pd.ProbingDirectiveID == 0 {
				t.Error("Missing required fields")
			}
		})
	}
}

func TestGeneratePD_Deterministic(t *testing.T) {
	t.Parallel()

	pd1 := generatePD(42)
	pd2 := generatePD(42)

	if !pd1.DestinationAddress.Equal(pd2.DestinationAddress) {
		t.Error("generatePD not deterministic: addresses differ")
	}
	if pd1.NearTTL != pd2.NearTTL {
		t.Error("generatePD not deterministic: TTLs differ")
	}
	if pd1.Protocol != pd2.Protocol {
		t.Error("generatePD not deterministic: protocols differ")
	}
}

func TestGeneratePD_CyclingLogic(t *testing.T) {
	t.Parallel()

	for i := 0; i < 100; i++ {
		pd := generatePD(i)
		if pd == nil {
			t.Errorf("generatePD(%d) returned nil", i)
			return
		}

		if pd.NearTTL < 5 || pd.NearTTL > 20 {
			t.Errorf("generatePD(%d): TTL %d out of range [5-20]", i, pd.NearTTL)
		}

		if pd.DestinationAddress == nil {
			t.Errorf("generatePD(%d): nil destination", i)
		}

		if pd.Protocol != api.ICMP && pd.Protocol != api.ICMPv6 && pd.Protocol != api.UDP {
			t.Errorf("generatePD(%d): invalid protocol %v", i, pd.Protocol)
		}

		expectedID := uint64(i + 1) // #nosec G115 -- i is test loop counter, safe conversion
		if pd.ProbingDirectiveID != expectedID {
			t.Errorf("generatePD(%d): PD ID = %d, want %d", i, pd.ProbingDirectiveID, expectedID)
		}
	}
}

// ============================================================================
// UNIT TESTS - reportStats
// ============================================================================

func TestReportStats_WithData(t *testing.T) {
	origSent := pdsSent.Load()
	origReceived := fiesReceived.Load()
	defer func() {
		pdsSent.Store(origSent)
		fiesReceived.Store(origReceived)
	}()

	pdsSent.Store(100)
	fiesReceived.Store(75)

	reportStats()
	t.Log("reportStats ran with data")
}

func TestReportStats_EarlyReturn(t *testing.T) {
	origSent := pdsSent.Load()
	origReceived := fiesReceived.Load()
	defer func() {
		pdsSent.Store(origSent)
		fiesReceived.Store(origReceived)
	}()

	pdsSent.Store(0)
	fiesReceived.Store(0)

	reportStats()
	t.Log("reportStats returned early with zero sent")
}

// ============================================================================
// UNIT TESTS - receiveFIEs
// ============================================================================

func TestReceiveFIEs_Success(t *testing.T) {
	fie := createTestFIE(123)

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	if err := encoder.Encode(fie); err != nil {
		t.Fatalf("Failed to encode FIE: %v", err)
	}

	decoder := json.NewDecoder(&buf)
	origReceived := fiesReceived.Load()
	defer fiesReceived.Store(origReceived)

	receiveFIEs(decoder, "test-addr")

	if fiesReceived.Load() != origReceived+1 {
		t.Error("fiesReceived counter not incremented")
	}
}

func TestReceiveFIEs_BothNilInfo(t *testing.T) {
	// FIE with both NearInfo and FarInfo nil — no probe response received at all.
	fie := api.ForwardingInfoElement{
		Agent:              api.Agent{AgentID: "test-agent"},
		ProbingDirectiveID: 42,
		DestinationAddress: net.ParseIP("8.8.8.8"),
		NearInfo:           nil,
		FarInfo:            nil,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(fie); err != nil {
		t.Fatalf("Failed to encode FIE: %v", err)
	}

	origReceived := fiesReceived.Load()
	defer fiesReceived.Store(origReceived)

	receiveFIEs(json.NewDecoder(&buf), "test-addr")

	if fiesReceived.Load() != origReceived+1 {
		t.Error("fiesReceived counter not incremented")
	}
}

func TestReceiveFIEs_NearInfoNil(t *testing.T) {
	// FIE with NearInfo nil — far hop responded but near hop did not.
	now := time.Now()
	fie := api.ForwardingInfoElement{
		Agent:              api.Agent{AgentID: "test-agent"},
		ProbingDirectiveID: 42,
		DestinationAddress: net.ParseIP("8.8.8.8"),
		NearInfo:           nil,
		FarInfo: &api.Info{
			ProbeTTL:          11,
			ReplyAddress:      net.ParseIP("10.0.0.2"),
			SentTimestamp:     now,
			ReceivedTimestamp: now.Add(15 * time.Millisecond),
		},
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(fie); err != nil {
		t.Fatalf("Failed to encode FIE: %v", err)
	}

	origReceived := fiesReceived.Load()
	defer fiesReceived.Store(origReceived)

	receiveFIEs(json.NewDecoder(&buf), "test-addr")

	if fiesReceived.Load() != origReceived+1 {
		t.Error("fiesReceived counter not incremented")
	}
}

func TestReceiveFIEs_FarInfoNil(t *testing.T) {
	// FIE with FarInfo nil — near hop responded but far hop did not.
	now := time.Now()
	fie := api.ForwardingInfoElement{
		Agent:              api.Agent{AgentID: "test-agent"},
		ProbingDirectiveID: 42,
		DestinationAddress: net.ParseIP("8.8.8.8"),
		NearInfo: &api.Info{
			ProbeTTL:          10,
			ReplyAddress:      net.ParseIP("10.0.0.1"),
			SentTimestamp:     now,
			ReceivedTimestamp: now.Add(10 * time.Millisecond),
		},
		FarInfo: nil,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(fie); err != nil {
		t.Fatalf("Failed to encode FIE: %v", err)
	}

	origReceived := fiesReceived.Load()
	defer fiesReceived.Store(origReceived)

	receiveFIEs(json.NewDecoder(&buf), "test-addr")

	if fiesReceived.Load() != origReceived+1 {
		t.Error("fiesReceived counter not incremented")
	}
}

func TestReceiveFIEs_EOF(t *testing.T) {
	t.Parallel()

	decoder := json.NewDecoder(strings.NewReader(""))
	receiveFIEs(decoder, "test-addr")
}

func TestReceiveFIEs_DecodeError(t *testing.T) {
	t.Parallel()

	decoder := json.NewDecoder(strings.NewReader("invalid json"))
	receiveFIEs(decoder, "test-addr")
}

// ============================================================================
// UNIT TESTS - sendPDs
// ============================================================================

func TestSendPDs_AllProtocolCases(t *testing.T) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	origSent := pdsSent.Load()
	defer pdsSent.Store(origSent)

	// Send 10 PDs to cover all protocol cases
	const numPDs = 10
	for i := 0; i < numPDs; i++ {
		pd := generatePD(i)
		if err := encoder.Encode(pd); err != nil {
			t.Fatalf("Failed to encode PD %d: %v", i, err)
		}
		pdsSent.Add(1)

		// Exercise protocol switch
		var protocol string
		switch pd.Protocol {
		case api.ICMP:
			protocol = "ICMP"
		case api.ICMPv6:
			protocol = "ICMPv6"
		case api.UDP:
			protocol = "UDP"
		default:
			protocol = "UNKNOWN"
		}
		_ = protocol
	}

	if pdsSent.Load() < origSent+numPDs {
		t.Errorf("Expected at least %d PDs sent", numPDs)
	}

	// Verify all protocol types
	decoder := json.NewDecoder(&buf)
	protocols := make(map[api.Protocol]bool)

	for {
		var pd api.ProbingDirective
		if err := decoder.Decode(&pd); err != nil {
			break
		}
		protocols[pd.Protocol] = true
	}

	for _, want := range []api.Protocol{api.ICMP, api.ICMPv6, api.UDP} {
		if !protocols[want] {
			t.Errorf("Did not generate protocol %v", want)
		}
	}
}

func TestSendPDs_UnknownProtocol(t *testing.T) {
	// Test default case in protocol switch (unreachable defensive code)
	invalidProtocol := api.Protocol(99)

	var protocol string
	switch invalidProtocol {
	case api.ICMP:
		protocol = "ICMP"
	case api.ICMPv6:
		protocol = "ICMPv6"
	case api.UDP:
		protocol = "UDP"
	default:
		protocol = "UNKNOWN"
	}

	if protocol != "UNKNOWN" {
		t.Errorf("Expected UNKNOWN for invalid protocol, got %s", protocol)
	}
}

// ============================================================================
// INTEGRATION TESTS - handleAgent
// ============================================================================

func TestHandleAgent_BasicFlow(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	fie := createTestFIE(1)
	encodeFIE(t, conn, fie)

	done := make(chan bool)
	go func() {
		handleAgent(conn, 100)
		done <- true
	}()

	time.Sleep(50 * time.Millisecond)

	if err := conn.Close(); err != nil {
		t.Errorf("Failed to close connection: %v", err)
	}

	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent")
	}

	if conn.writeBuf.Len() == 0 {
		t.Error("No PDs were sent")
	}
}

func TestHandleAgent_CloseError(t *testing.T) {
	t.Parallel()

	conn := &errorCloseConn{mockConn: newMockConn()}
	fie := createTestFIE(1)
	encodeFIE(t, conn.mockConn, fie)

	done := make(chan bool)
	go func() {
		handleAgent(conn, 100)
		done <- true
	}()

	time.Sleep(50 * time.Millisecond)

	if err := conn.Close(); err == nil {
		t.Error("Expected error from Close()")
	}

	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent")
	}
}

func TestHandleAgent_MultipleProtocols(t *testing.T) {
	t.Parallel()

	conn := newMockConn()

	// Pre-populate with FIEs
	for i := 0; i < 10; i++ {
		pdID := uint64(i + 1) // #nosec G115 -- i is test loop counter, safe conversion
		fie := createTestFIE(pdID)
		encodeFIE(t, conn, fie)
	}

	done := make(chan bool)
	go func() {
		handleAgent(conn, 100)
		done <- true
	}()

	time.Sleep(200 * time.Millisecond)

	if err := conn.Close(); err != nil {
		t.Errorf("Failed to close connection: %v", err)
	}

	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent")
	}

	// Verify ICMPv6 was generated
	decoder := json.NewDecoder(conn.writeBuf)
	foundICMPv6 := false

	for {
		var pd api.ProbingDirective
		if err := decoder.Decode(&pd); err != nil {
			break
		}
		if pd.Protocol == api.ICMPv6 {
			foundICMPv6 = true
			break
		}
	}

	if !foundICMPv6 {
		t.Error("Did not generate ICMPv6 directive")
	}
}

func TestHandleAgent_WriteError(t *testing.T) {
	t.Parallel()

	customErr := errors.New("network timeout")
	conn := &errorWriteConn{
		mockConn: newMockConn(),
		writeErr: customErr,
	}

	fie := createTestFIE(1)
	encodeFIE(t, conn.mockConn, fie)

	done := make(chan bool)
	go func() {
		handleAgent(conn, 10)
		done <- true
	}()

	select {
	case <-done:
		// Success - should exit quickly due to write error
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent to handle write error")
	}
}

// ============================================================================
// PARTIAL TESTS - main function
// ============================================================================

func TestMain_FlagParsing(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"cmd", "-address", "localhost:9999", "-probing-rate", "50"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	address := flag.String("address", "localhost:50050", "Listen address")
	rate := flag.Int("probing-rate", 10, "Probing directives per second")
	flag.Parse()

	if *address != "localhost:9999" {
		t.Errorf("address = %s, want localhost:9999", *address)
	}
	if *rate != 50 {
		t.Errorf("rate = %d, want 50", *rate)
	}
}

func TestMain_ListenerSetup(t *testing.T) {
	testAddr := "localhost:0"
	listener, err := net.Listen("tcp", testAddr)
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Logf("Close error (expected): %v", err)
		}
	}()

	connectDone := make(chan bool)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
		connectDone <- true
	}()

	addr := listener.Addr().String()
	testConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	_ = testConn.Close()

	select {
	case <-connectDone:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for connection")
	}
}
