// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

// All functions reach 100% coverage except:
//   - main(): infinite server loop cannot be tested in unit tests.
//   - sendPDs(): default protocol branch is unreachable; generatePD only produces ICMP, ICMPv6, or UDP.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

// -- mock types ---------------------------------------------------------------

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

// limitedWriteConn allows up to limit writes before returning io.ErrClosedPipe.
type limitedWriteConn struct {
	*mockConn
	limit int
	count int
}

func (l *limitedWriteConn) Write(b []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count >= l.limit {
		return 0, io.ErrClosedPipe
	}
	l.count++
	return l.writeBuf.Write(b)
}

// errorDeadlineConn returns an error from SetReadDeadline. failOnCall controls which
// invocation fails: 1 for the initial deadline set (before decode), 2 for the clearing
// deadline (after successful auth) — handleAuth calls SetReadDeadline twice on the
// success path, so a single failing mock can't exercise both branches at once.
type errorDeadlineConn struct {
	*mockConn
	failOnCall int
	callCount  int
}

func (e *errorDeadlineConn) SetReadDeadline(t time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.callCount++
	if e.callCount == e.failOnCall {
		return errors.New("deadline error")
	}
	return nil
}

// -- test helpers -------------------------------------------------------------

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
func encodeFIE(t *testing.T, conn *mockConn, fie *api.ForwardingInfoElement) {
	t.Helper()
	encoder := json.NewEncoder(conn.readBuf)
	if err := encoder.Encode(fie); err != nil {
		t.Fatalf("Failed to encode FIE: %v", err)
	}
}

// writeAuthRequest encodes an AuthRequest to the connection's read buffer. handleAgent's
// first read is now the auth handshake, so any test driving handleAgent (or handleAuth
// directly) needs one of these ahead of whatever payload it expects sendPDs/receiveFIEs
// to see afterward.
func writeAuthRequest(t *testing.T, conn *mockConn, secret string) {
	t.Helper()
	encoder := json.NewEncoder(conn.readBuf)
	if err := encoder.Encode(api.AuthRequest{Secret: secret}); err != nil { //nolint:gosec // G117: secret is the point of this struct, never logged
		t.Fatalf("Failed to encode auth request: %v", err)
	}
}

// -- generatePD ---------------------------------------------------------------

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

			if pd.NearTTL != tt.wantTTL {
				t.Errorf("NearTTL = %v, want %v", pd.NearTTL, tt.wantTTL)
			}

			isIPv4 := pd.DestinationAddress.To4() != nil
			if isIPv4 != tt.wantIPv4 {
				t.Errorf("IPv4 = %v, want %v", isIPv4, tt.wantIPv4)
			}

			if pd.Protocol != tt.wantProtocol {
				t.Errorf("Protocol = %v, want %v", pd.Protocol, tt.wantProtocol)
			}

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

// -- reportStats --------------------------------------------------------------

func TestReportStats_WithData(t *testing.T) {
	t.Parallel()

	origSent := pdsSent.Load()
	origReceived := fiesReceived.Load()
	defer func() {
		pdsSent.Store(origSent)
		fiesReceived.Store(origReceived)
	}()

	pdsSent.Store(100)
	fiesReceived.Store(75)

	reportStats()
}

func TestReportStats_EarlyReturn(t *testing.T) {
	t.Parallel()

	origSent := pdsSent.Load()
	origReceived := fiesReceived.Load()
	defer func() {
		pdsSent.Store(origSent)
		fiesReceived.Store(origReceived)
	}()

	pdsSent.Store(0)
	fiesReceived.Store(0)

	reportStats()
}

// -- handleAuth -----------------------------------------------------------------

func TestHandleAuth_SuccessNoSecret(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	writeAuthRequest(t, conn, "")

	decoder, ok := handleAuth(conn, "")
	if !ok {
		t.Fatal("handleAuth failed, want success")
	}
	if decoder == nil {
		t.Fatal("handleAuth returned nil decoder on success")
	}

	var resp api.AuthResponse
	if err := json.NewDecoder(conn.writeBuf).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode auth response: %v", err)
	}
	if !resp.Authenticated {
		t.Error("AuthResponse.Authenticated = false, want true")
	}
}

func TestHandleAuth_SuccessMatchingSecret(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	writeAuthRequest(t, conn, "correct-secret")

	decoder, ok := handleAuth(conn, "correct-secret")
	if !ok {
		t.Fatal("handleAuth failed, want success")
	}
	if decoder == nil {
		t.Fatal("handleAuth returned nil decoder on success")
	}
}

func TestHandleAuth_WrongSecret(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	writeAuthRequest(t, conn, "wrong-secret")

	decoder, ok := handleAuth(conn, "correct-secret")
	if ok {
		t.Fatal("handleAuth succeeded, want failure")
	}
	if decoder != nil {
		t.Error("handleAuth returned non-nil decoder on failure")
	}

	var resp api.AuthResponse
	if err := json.NewDecoder(conn.writeBuf).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode auth response: %v", err)
	}
	if resp.Authenticated {
		t.Error("AuthResponse.Authenticated = true, want false")
	}
}

func TestHandleAuth_DecodeError(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	conn.readBuf.WriteString("not valid json")

	decoder, ok := handleAuth(conn, "some-secret")
	if ok {
		t.Fatal("handleAuth succeeded, want failure")
	}
	if decoder != nil {
		t.Error("handleAuth returned non-nil decoder on failure")
	}
}

func TestHandleAuth_ResponseWriteError(t *testing.T) {
	t.Parallel()

	conn := &errorWriteConn{mockConn: newMockConn(), writeErr: errors.New("network error")}
	writeAuthRequest(t, conn.mockConn, "")

	decoder, ok := handleAuth(conn, "")
	if ok {
		t.Fatal("handleAuth succeeded, want failure")
	}
	if decoder != nil {
		t.Error("handleAuth returned non-nil decoder on failure")
	}
}

func TestHandleAuth_SetDeadlineError(t *testing.T) {
	t.Parallel()

	// Fails on the first SetReadDeadline call, made before the auth request is decoded.
	conn := &errorDeadlineConn{mockConn: newMockConn(), failOnCall: 1}
	writeAuthRequest(t, conn.mockConn, "")

	decoder, ok := handleAuth(conn, "")
	if ok {
		t.Fatal("handleAuth succeeded, want failure")
	}
	if decoder != nil {
		t.Error("handleAuth returned non-nil decoder on failure")
	}
}

func TestHandleAuth_ClearDeadlineError(t *testing.T) {
	t.Parallel()

	// Fails on the second SetReadDeadline call, made after a successful handshake to
	// clear the deadline before handing the decoder off to the PD/FIE loop.
	conn := &errorDeadlineConn{mockConn: newMockConn(), failOnCall: 2}
	writeAuthRequest(t, conn.mockConn, "")

	decoder, ok := handleAuth(conn, "")
	if ok {
		t.Fatal("handleAuth succeeded, want failure")
	}
	if decoder != nil {
		t.Error("handleAuth returned non-nil decoder on failure")
	}
}

// -- receiveFIEs --------------------------------------------------------------

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

// -- sendPDs ------------------------------------------------------------------

func TestSendPDs_SendsPDsUntilWriteError(t *testing.T) {
	t.Parallel()

	conn := &limitedWriteConn{mockConn: newMockConn(), limit: 5}
	encoder := json.NewEncoder(conn)

	sendPDs(encoder, "test-addr", 1000)

	decoder := json.NewDecoder(conn.writeBuf)
	count := 0
	for {
		var pd api.ProbingDirective
		if err := decoder.Decode(&pd); err != nil {
			break
		}
		count++
	}
	if count != 5 {
		t.Errorf("PDs written = %d, want 5", count)
	}
}

func TestSendPDs_ExitsOnNetworkError(t *testing.T) {
	t.Parallel()

	conn := &errorWriteConn{mockConn: newMockConn(), writeErr: errors.New("network error")}
	encoder := json.NewEncoder(conn)

	sendPDs(encoder, "test-addr", 1000)

	if conn.writeBuf.Len() != 0 {
		t.Error("data written to buffer despite write error")
	}
}

// -- handleAgent --------------------------------------------------------------

func TestHandleAgent_BasicFlow(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	writeAuthRequest(t, conn, "")
	fie := createTestFIE(1)
	encodeFIE(t, conn, &fie)

	done := make(chan bool)
	go func() {
		handleAgent(conn, 100, "")
		done <- true
	}()

	time.Sleep(50 * time.Millisecond)

	if err := conn.Close(); err != nil {
		t.Errorf("Failed to close connection: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent")
	}

	// writeBuf now contains the auth response ahead of the PDs; just confirm something was written.
	if conn.writeBuf.Len() == 0 {
		t.Error("No data was sent")
	}
}

func TestHandleAgent_AuthFailureClosesConnection(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	writeAuthRequest(t, conn, "wrong-secret")

	done := make(chan bool)
	go func() {
		handleAgent(conn, 100, "correct-secret")
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent to reject bad auth")
	}

	var resp api.AuthResponse
	if err := json.NewDecoder(conn.writeBuf).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode auth response: %v", err)
	}
	if resp.Authenticated {
		t.Error("AuthResponse.Authenticated = true, want false")
	}
	// No PDs should follow a failed handshake.
	var pd api.ProbingDirective
	if err := json.NewDecoder(conn.writeBuf).Decode(&pd); err == nil {
		t.Error("PD written to buffer after failed auth, want none")
	}
}

func TestHandleAgent_CloseError(t *testing.T) {
	t.Parallel()

	conn := &errorCloseConn{mockConn: newMockConn()}
	writeAuthRequest(t, conn.mockConn, "")
	fie := createTestFIE(1)
	encodeFIE(t, conn.mockConn, &fie)

	done := make(chan bool)
	go func() {
		handleAgent(conn, 100, "")
		done <- true
	}()

	time.Sleep(50 * time.Millisecond)
	if err := conn.mockConn.Close(); err != nil { // triggers io.ErrClosedPipe on Write, causing sendPDs to exit
		t.Errorf("Failed to close connection: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent")
	}
}

func TestHandleAgent_MultipleProtocols(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	writeAuthRequest(t, conn, "")

	for i := 0; i < 10; i++ {
		pdID := uint64(i + 1) // #nosec G115 -- i is test loop counter, safe conversion
		fie := createTestFIE(pdID)
		encodeFIE(t, conn, &fie)
	}

	done := make(chan bool)
	go func() {
		handleAgent(conn, 100, "")
		done <- true
	}()

	time.Sleep(200 * time.Millisecond)

	if err := conn.Close(); err != nil {
		t.Errorf("Failed to close connection: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent")
	}

	decoder := json.NewDecoder(conn.writeBuf)

	// First message on the wire is now the auth response, not a PD — consume and check it
	// before looking for the ICMPv6 directive among the PDs that follow.
	var resp api.AuthResponse
	if err := decoder.Decode(&resp); err != nil {
		t.Fatalf("Failed to decode auth response: %v", err)
	}
	if !resp.Authenticated {
		t.Fatal("AuthResponse.Authenticated = false, want true")
	}

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

	conn := &errorWriteConn{
		mockConn: newMockConn(),
		writeErr: errors.New("network timeout"),
	}

	writeAuthRequest(t, conn.mockConn, "")
	fie := createTestFIE(1)
	encodeFIE(t, conn.mockConn, &fie)

	done := make(chan bool)
	go func() {
		handleAgent(conn, 10, "")
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for handleAgent to handle write error")
	}
}

// -- main ---------------------------------------------------------------------

func TestMain_ListenerSetup(t *testing.T) {
	listener, err := net.Listen("tcp", "localhost:0")
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

	testConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	_ = testConn.Close()

	select {
	case <-connectDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for connection")
	}
}
