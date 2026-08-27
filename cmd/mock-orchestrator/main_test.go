// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

// All functions reach 100% coverage except:
//   - main(): infinite server loop cannot be tested in unit tests.
//   - sendPDs(): default protocol branch is unreachable; generatePD only produces ICMP, ICMPv6, or UDP.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	wire "github.com/dioptra-io/retina-commons/wire/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// limitedWriteConn allows up to limit Write calls before returning
// io.ErrClosedPipe. framing.Send issues two separate Write calls per
// frame (header, then payload) — unlike the old one-JSON-object-per-Write
// behavior, one complete frame here costs two calls, so limit must be set
// to 2x the desired frame count, not 1x.
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

// errorDeadlineConn returns an error from SetReadDeadline. handleAuth now
// makes exactly one framing.Receive call (which sets the read deadline
// internally, once) — unlike the old JSON version, there's no separate
// clear-deadline step afterward, so failOnCall only ever has one
// meaningful value.
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

// encodeFrame builds a length-prefixed protobuf frame matching what
// framing.Send would produce.
func encodeFrame(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("cannot marshal message: %v", err)
	}
	header := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(header, uint32(len(payload))) //nolint:gosec // G115: a test payload never approaches uint32 range
	return append(header, payload...)
}

// decodeFrame reads one length-prefixed protobuf frame from r and
// unmarshals it into msg. Returns an error (often just "buffer exhausted")
// rather than failing the test itself — callers looping until "no more
// frames" should treat any returned error as done, matching how the old
// decoder.Decode() error was used to end a loop, not just to fail a test.
func decodeFrame(r *bytes.Buffer, msg proto.Message) error {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(header)
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return proto.Unmarshal(payload, msg)
}

// createTestFIE creates a ForwardingInfoElement for testing.
func createTestFIE(pdID uint64) *wire.ForwardingInfoElement {
	now := timestamppb.Now()
	return &wire.ForwardingInfoElement{
		Agent:              &wire.Agent{AgentId: "test-agent"},
		ProbingDirectiveId: pdID,
		DestinationAddress: "8.8.8.8",
		NearInfo: &wire.Info{
			ProbeTtl:          10,
			ReplyAddress:      "10.0.0.1",
			SentTimestamp:     now,
			ReceivedTimestamp: timestamppb.New(now.AsTime().Add(10 * time.Millisecond)),
		},
		FarInfo: &wire.Info{
			ProbeTtl:          11,
			ReplyAddress:      "10.0.0.2",
			SentTimestamp:     now,
			ReceivedTimestamp: timestamppb.New(now.AsTime().Add(15 * time.Millisecond)),
		},
	}
}

// encodeFIE writes a length-prefixed FIE frame to the connection's read buffer.
func encodeFIE(t *testing.T, conn *mockConn, fie *wire.ForwardingInfoElement) {
	t.Helper()
	if _, err := conn.readBuf.Write(encodeFrame(t, fie)); err != nil {
		t.Fatalf("Failed to write FIE frame: %v", err)
	}
}

// writeAuthRequest writes a length-prefixed AuthRequest frame to the
// connection's read buffer. handleAgent's first read is the auth handshake,
// so any test driving handleAgent (or handleAuth directly) needs one of
// these ahead of whatever payload it expects sendPDs/receiveFIEs to see
// afterward.
func writeAuthRequest(t *testing.T, conn *mockConn, secret string) {
	t.Helper()
	req := &wire.AuthRequest{Secret: secret} //nolint:gosec // G117: secret is the point of this struct, never logged
	if _, err := conn.readBuf.Write(encodeFrame(t, req)); err != nil {
		t.Fatalf("Failed to write auth request frame: %v", err)
	}
}

// -- generatePD ---------------------------------------------------------------

// assertNextHeaderPresent checks that pd's NextHeader matches its Protocol.
// Split out of TestGeneratePD_ProtocolVariants to keep that test's own
// cyclomatic complexity down.
func assertNextHeaderPresent(t *testing.T, pd *wire.ProbingDirective) {
	t.Helper()
	switch pd.Protocol {
	case wire.Protocol_PROTOCOL_UNSPECIFIED:
		// not used by any test case here
	case wire.Protocol_PROTOCOL_ICMP:
		if pd.NextHeader.GetIcmpNextHeader() == nil {
			t.Error("ICMP directive missing ICMPNextHeader")
		}
	case wire.Protocol_PROTOCOL_ICMPV6:
		if pd.NextHeader.GetIcmpv6NextHeader() == nil {
			t.Error("ICMPv6 directive missing ICMPv6NextHeader")
		}
	case wire.Protocol_PROTOCOL_UDP:
		if pd.NextHeader.GetUdpNextHeader() == nil {
			t.Error("UDP directive missing UDPNextHeader")
		}
	}
}

func TestGeneratePD_ProtocolVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		counter      int
		wantTTL      uint32
		wantIPv4     bool
		wantProtocol wire.Protocol
	}{
		{"IPv4 UDP", 0, 5, true, wire.Protocol_PROTOCOL_UDP},
		{"IPv4 ICMP", 1, 6, true, wire.Protocol_PROTOCOL_ICMP},
		{"IPv6 UDP", 6, 11, false, wire.Protocol_PROTOCOL_UDP},
		{"IPv6 ICMPv6", 7, 12, false, wire.Protocol_PROTOCOL_ICMPV6},
		{"TTL wrap", 18, 7, true, wire.Protocol_PROTOCOL_UDP},
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

			if pd.NearTtl != tt.wantTTL {
				t.Errorf("NearTtl = %v, want %v", pd.NearTtl, tt.wantTTL)
			}

			isIPv4 := net.ParseIP(pd.DestinationAddress).To4() != nil
			if isIPv4 != tt.wantIPv4 {
				t.Errorf("IPv4 = %v, want %v", isIPv4, tt.wantIPv4)
			}

			if pd.Protocol != tt.wantProtocol {
				t.Errorf("Protocol = %v, want %v", pd.Protocol, tt.wantProtocol)
			}

			assertNextHeaderPresent(t, pd)

			if pd.AgentId == "" || pd.DestinationAddress == "" || pd.ProbingDirectiveId == 0 {
				t.Error("Missing required fields")
			}
		})
	}
}

func TestGeneratePD_Deterministic(t *testing.T) {
	t.Parallel()

	pd1 := generatePD(42)
	pd2 := generatePD(42)

	// DestinationAddress is a plain string on the wire type (unlike the old
	// api.ProbingDirective's net.IP), so a direct comparison replaces
	// net.IP.Equal — no normalization concerns here since both come from
	// the same generatePD logic.
	if pd1.DestinationAddress != pd2.DestinationAddress {
		t.Error("generatePD not deterministic: addresses differ")
	}
	if pd1.NearTtl != pd2.NearTtl {
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

		if pd.NearTtl < 5 || pd.NearTtl > 20 {
			t.Errorf("generatePD(%d): TTL %d out of range [5-20]", i, pd.NearTtl)
		}

		// DestinationAddress is a string now — empty string, not nil,
		// signals "unset".
		if pd.DestinationAddress == "" {
			t.Errorf("generatePD(%d): empty destination", i)
		}

		if pd.Protocol != wire.Protocol_PROTOCOL_ICMP &&
			pd.Protocol != wire.Protocol_PROTOCOL_ICMPV6 &&
			pd.Protocol != wire.Protocol_PROTOCOL_UDP {
			t.Errorf("generatePD(%d): invalid protocol %v", i, pd.Protocol)
		}

		expectedID := uint64(i + 1)
		if pd.ProbingDirectiveId != expectedID {
			t.Errorf("generatePD(%d): PD ID = %d, want %d", i, pd.ProbingDirectiveId, expectedID)
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

	if !handleAuth(conn, "") {
		t.Fatal("handleAuth failed, want success")
	}

	var resp wire.AuthResponse
	if err := decodeFrame(conn.writeBuf, &resp); err != nil {
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

	if !handleAuth(conn, "correct-secret") {
		t.Fatal("handleAuth failed, want success")
	}
}

func TestHandleAuth_WrongSecret(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	writeAuthRequest(t, conn, "wrong-secret")

	if handleAuth(conn, "correct-secret") {
		t.Fatal("handleAuth succeeded, want failure")
	}

	var resp wire.AuthResponse
	if err := decodeFrame(conn.writeBuf, &resp); err != nil {
		t.Fatalf("Failed to decode auth response: %v", err)
	}
	if resp.Authenticated {
		t.Error("AuthResponse.Authenticated = true, want false")
	}
}

func TestHandleAuth_DecodeError(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	// Garbage bytes: interpreted as a length-prefixed frame, this either
	// declares a bogus (huge) length or fails to unmarshal as AuthRequest —
	// either way, framing.Receive fails, same as malformed JSON did before.
	conn.readBuf.WriteString("not a valid frame")

	if handleAuth(conn, "some-secret") {
		t.Fatal("handleAuth succeeded, want failure")
	}
}

func TestHandleAuth_ResponseWriteError(t *testing.T) {
	t.Parallel()

	conn := &errorWriteConn{mockConn: newMockConn(), writeErr: errors.New("network error")}
	writeAuthRequest(t, conn.mockConn, "")

	if handleAuth(conn, "") {
		t.Fatal("handleAuth succeeded, want failure")
	}
}

func TestHandleAuth_SetDeadlineError(t *testing.T) {
	t.Parallel()

	// Fails on the only SetReadDeadline call handleAuth's single
	// framing.Receive makes internally.
	conn := &errorDeadlineConn{mockConn: newMockConn(), failOnCall: 1}
	writeAuthRequest(t, conn.mockConn, "")

	if handleAuth(conn, "") {
		t.Fatal("handleAuth succeeded, want failure")
	}
}

// -- receiveFIEs --------------------------------------------------------------

// TestReceiveFIEs_Success and its siblings below deliberately do not call
// t.Parallel() — they read/reset the shared global fiesReceived counter
// (via defer fiesReceived.Store(origReceived)), and running concurrently
// with each other, or with TestReportStats_*, would race on it. Go runs
// all non-parallel tests to completion before starting the parallel batch,
// which is what keeps this safe.
func TestReceiveFIEs_Success(t *testing.T) {
	conn := newMockConn()
	encodeFIE(t, conn, createTestFIE(123))

	origReceived := fiesReceived.Load()
	defer fiesReceived.Store(origReceived)

	receiveFIEs(conn, "test-addr")

	if fiesReceived.Load() != origReceived+1 {
		t.Error("fiesReceived counter not incremented")
	}
}

func TestReceiveFIEs_BothNilInfo(t *testing.T) {
	// FIE with both NearInfo and FarInfo nil — no probe response received at all.
	fie := &wire.ForwardingInfoElement{
		Agent:              &wire.Agent{AgentId: "test-agent"},
		ProbingDirectiveId: 42,
		DestinationAddress: "8.8.8.8",
		NearInfo:           nil,
		FarInfo:            nil,
	}

	conn := newMockConn()
	encodeFIE(t, conn, fie)

	origReceived := fiesReceived.Load()
	defer fiesReceived.Store(origReceived)

	receiveFIEs(conn, "test-addr")

	if fiesReceived.Load() != origReceived+1 {
		t.Error("fiesReceived counter not incremented")
	}
}

func TestReceiveFIEs_NearInfoNil(t *testing.T) {
	// FIE with NearInfo nil — far hop responded but near hop did not.
	now := timestamppb.Now()
	fie := &wire.ForwardingInfoElement{
		Agent:              &wire.Agent{AgentId: "test-agent"},
		ProbingDirectiveId: 42,
		DestinationAddress: "8.8.8.8",
		NearInfo:           nil,
		FarInfo: &wire.Info{
			ProbeTtl:          11,
			ReplyAddress:      "10.0.0.2",
			SentTimestamp:     now,
			ReceivedTimestamp: timestamppb.New(now.AsTime().Add(15 * time.Millisecond)),
		},
	}

	conn := newMockConn()
	encodeFIE(t, conn, fie)

	origReceived := fiesReceived.Load()
	defer fiesReceived.Store(origReceived)

	receiveFIEs(conn, "test-addr")

	if fiesReceived.Load() != origReceived+1 {
		t.Error("fiesReceived counter not incremented")
	}
}

func TestReceiveFIEs_FarInfoNil(t *testing.T) {
	// FIE with FarInfo nil — near hop responded but far hop did not.
	now := timestamppb.Now()
	fie := &wire.ForwardingInfoElement{
		Agent:              &wire.Agent{AgentId: "test-agent"},
		ProbingDirectiveId: 42,
		DestinationAddress: "8.8.8.8",
		NearInfo: &wire.Info{
			ProbeTtl:          10,
			ReplyAddress:      "10.0.0.1",
			SentTimestamp:     now,
			ReceivedTimestamp: timestamppb.New(now.AsTime().Add(10 * time.Millisecond)),
		},
		FarInfo: nil,
	}

	conn := newMockConn()
	encodeFIE(t, conn, fie)

	origReceived := fiesReceived.Load()
	defer fiesReceived.Store(origReceived)

	receiveFIEs(conn, "test-addr")

	if fiesReceived.Load() != origReceived+1 {
		t.Error("fiesReceived counter not incremented")
	}
}

func TestReceiveFIEs_EOF(t *testing.T) {
	t.Parallel()

	// Default mockConn has an empty readBuf — Read returns io.EOF immediately.
	receiveFIEs(newMockConn(), "test-addr")
}

func TestReceiveFIEs_DecodeError(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	conn.readBuf.WriteString("not a valid frame")
	receiveFIEs(conn, "test-addr")
}

// -- sendPDs ------------------------------------------------------------------

func TestSendPDs_SendsPDsUntilWriteError(t *testing.T) {
	t.Parallel()

	// limit is 2x the desired frame count: framing.Send issues one Write
	// call for the header and one for the payload, per frame.
	conn := &limitedWriteConn{mockConn: newMockConn(), limit: 10}

	sendPDs(conn, "test-addr", 1000)

	count := 0
	for {
		var pd wire.ProbingDirective
		if err := decodeFrame(conn.writeBuf, &pd); err != nil {
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

	sendPDs(conn, "test-addr", 1000)

	if conn.writeBuf.Len() != 0 {
		t.Error("data written to buffer despite write error")
	}
}

// -- handleAgent --------------------------------------------------------------

func TestHandleAgent_BasicFlow(t *testing.T) {
	t.Parallel()

	conn := newMockConn()
	writeAuthRequest(t, conn, "")
	encodeFIE(t, conn, createTestFIE(1))

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

	var resp wire.AuthResponse
	if err := decodeFrame(conn.writeBuf, &resp); err != nil {
		t.Fatalf("Failed to decode auth response: %v", err)
	}
	if resp.Authenticated {
		t.Error("AuthResponse.Authenticated = true, want false")
	}
	// No PDs should follow a failed handshake.
	var pd wire.ProbingDirective
	if err := decodeFrame(conn.writeBuf, &pd); err == nil {
		t.Error("PD written to buffer after failed auth, want none")
	}
}

func TestHandleAgent_CloseError(t *testing.T) {
	t.Parallel()

	conn := &errorCloseConn{mockConn: newMockConn()}
	writeAuthRequest(t, conn.mockConn, "")
	encodeFIE(t, conn.mockConn, createTestFIE(1))

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
		encodeFIE(t, conn, createTestFIE(uint64(i+1)))
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

	// First message on the wire is the auth response, not a PD — consume and check it
	// before looking for the ICMPv6 directive among the PDs that follow.
	var resp wire.AuthResponse
	if err := decodeFrame(conn.writeBuf, &resp); err != nil {
		t.Fatalf("Failed to decode auth response: %v", err)
	}
	if !resp.Authenticated {
		t.Fatal("AuthResponse.Authenticated = false, want true")
	}

	foundICMPv6 := false
	for {
		var pd wire.ProbingDirective
		if err := decodeFrame(conn.writeBuf, &pd); err != nil {
			break
		}
		if pd.Protocol == wire.Protocol_PROTOCOL_ICMPV6 {
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
	encodeFIE(t, conn.mockConn, createTestFIE(1))

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
