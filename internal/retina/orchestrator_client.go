// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

// OrchestratorConfig configures the connection to the orchestrator.
type OrchestratorConfig struct {
	// Address is the TCP address of the orchestrator, in the form "host:port".
	Address string `json:"address"`
	// Secret is the shared secret presented in the handshake.
	Secret string `json:"-"`
	// HandshakeTimeout bounds the whole handshake. Zero means no limit.
	HandshakeTimeout time.Duration `json:"handshake_timeout"`
	// KeepAliveIdle, KeepAliveInterval and KeepAliveCount are the TCP
	// keepalive parameters of the connection: a dead orchestrator is detected
	// after about Idle + Interval * Count. Zero values use the defaults of the
	// net package. They should match the orchestrator's values for agent
	// connections, so that both ends give up on a dead peer at the same time.
	KeepAliveIdle     time.Duration `json:"keep_alive_idle"`
	KeepAliveInterval time.Duration `json:"keep_alive_interval"`
	KeepAliveCount    int           `json:"keep_alive_count"`
	// WriteBufferSize is the size in bytes of the buffer FIEs are written to
	// before they are sent to the orchestrator.
	WriteBufferSize int `json:"write_buffer_size"`
	// FlushPeriod is how often buffered FIEs are sent to the orchestrator. A
	// FIE reaches the orchestrator at most this long after SendFIE, sooner
	// when the buffer fills up.
	FlushPeriod time.Duration `json:"flush_period"`
}

func (c *OrchestratorConfig) validate() error {
	if c.Address == "" {
		return fmt.Errorf("address cannot be empty")
	}
	if c.WriteBufferSize < 1 {
		return fmt.Errorf("write buffer size must be at least 1: got %d", c.WriteBufferSize)
	}
	if c.FlushPeriod <= 0 {
		return fmt.Errorf("flush period must be positive: got %v", c.FlushPeriod)
	}
	return nil
}

// OrchestratorConn is a connection to the orchestrator. The handshake is one
// JSON line each way; after it, PDs and FIEs are exchanged as CSV lines.
//
// ReceivePD may be called from one goroutine and SendFIE from another. Flush
// and Close are safe to call from any goroutine, and Close unblocks the others.
type OrchestratorConn struct {
	config *OrchestratorConfig
	conn   net.Conn
	reader *bufio.Reader
	// writeMu guards writer and line, which SendFIE and Flush share.
	writeMu sync.Mutex
	writer  *bufio.Writer
	// line is the buffer a FIE line is built in before it is written.
	line []byte
}

// DialOrchestrator connects to the orchestrator. The returned connection is
// not authenticated yet: call Handshake on it.
func DialOrchestrator(ctx context.Context, config *OrchestratorConfig) (*OrchestratorConn, error) {
	dialer := net.Dialer{KeepAliveConfig: net.KeepAliveConfig{
		Enable:   true,
		Idle:     config.KeepAliveIdle,
		Interval: config.KeepAliveInterval,
		Count:    config.KeepAliveCount,
	}}
	conn, err := dialer.DialContext(ctx, "tcp", config.Address)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to orchestrator: %w", err)
	}
	return &OrchestratorConn{
		config: config,
		conn:   conn,
		reader: bufio.NewReader(conn),
		writer: bufio.NewWriterSize(conn, config.WriteBufferSize),
	}, nil
}

// Handshake sends the authentication request for agentID and reads the
// orchestrator's answer. It returns an error if the agent is rejected.
func (c *OrchestratorConn) Handshake(agentID string) error {
	if err := c.conn.SetDeadline(deadline(c.config.HandshakeTimeout)); err != nil {
		return fmt.Errorf("cannot set handshake deadline: %w", err)
	}

	request := api.AuthRequest{AgentID: agentID, Secret: c.config.Secret}
	if err := json.NewEncoder(c.writer).Encode(&request); err != nil { //nolint:gosec // G117: the secret is what the request is for
		return fmt.Errorf("cannot send auth request: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("cannot send auth request: %w", err)
	}

	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("cannot read auth response: %w", err)
	}
	var response api.AuthResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("cannot decode auth response: %w", err)
	}
	if !response.Authenticated {
		return fmt.Errorf("not authenticated: %s", response.Message)
	}

	if err := c.conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("cannot clear handshake deadline: %w", err)
	}
	return nil
}

// ReceivePD blocks until the orchestrator sends its next PD, as the CSV line
// id,destination,near_ttl,protocol,first_half_word,second_half_word.
// ReceivePD has no deadline: the orchestrator may stay silent for as long as
// it has nothing to issue, and a dead orchestrator is detected by TCP
// keepalive.
func (c *OrchestratorConn) ReceivePD() (PD, error) {
	var line []byte
	for len(line) == 0 {
		raw, err := c.reader.ReadSlice('\n')
		if err != nil {
			return PD{}, fmt.Errorf("cannot read PD: %w", err)
		}
		line = bytes.TrimSpace(raw)
	}

	var fields [6][]byte
	rest := line
	for i := range fields {
		field, after, found := bytes.Cut(rest, []byte{','})
		if found != (i < len(fields)-1) {
			return PD{}, fmt.Errorf("cannot decode PD %q: want %d fields", line, len(fields))
		}
		fields[i], rest = field, after
	}

	id, err := strconv.ParseUint(string(fields[0]), 10, 32)
	if err != nil {
		return PD{}, fmt.Errorf("cannot decode PD %q: invalid id: %w", line, err)
	}
	destination, err := netip.ParseAddr(string(bytes.Trim(fields[1], `"`)))
	if err != nil {
		return PD{}, fmt.Errorf("cannot decode PD %q: invalid destination: %w", line, err)
	}
	nearTTL, err := strconv.ParseUint(string(fields[2]), 10, 8)
	if err != nil {
		return PD{}, fmt.Errorf("cannot decode PD %q: invalid near TTL: %w", line, err)
	}
	protocol, err := strconv.ParseUint(string(fields[3]), 10, 8)
	if err != nil {
		return PD{}, fmt.Errorf("cannot decode PD %q: invalid protocol: %w", line, err)
	}
	firstHalfWord, err := strconv.ParseUint(string(fields[4]), 10, 16)
	if err != nil {
		return PD{}, fmt.Errorf("cannot decode PD %q: invalid first half word: %w", line, err)
	}
	secondHalfWord, err := strconv.ParseUint(string(fields[5]), 10, 16)
	if err != nil {
		return PD{}, fmt.Errorf("cannot decode PD %q: invalid second half word: %w", line, err)
	}

	return PD{
		ID:             uint32(id),
		Destination:    destination,
		NearTTL:        uint8(nearTTL),
		Protocol:       uint8(protocol),
		FirstHalfWord:  uint16(firstHalfWord),
		SecondHalfWord: uint16(secondHalfWord),
	}, nil
}

// SendFIE writes one FIE for the orchestrator, as the CSV line
// id,capture_unix,near_address,near_delta,far_address,far_delta.
// The FIE is buffered: it is sent when the buffer fills up or on the next
// Flush. SendFIE has no deadline: once the buffer is full it waits for as long
// as the orchestrator applies backpressure, and returns when the connection
// fails or is closed.
func (c *OrchestratorConn) SendFIE(fie *FIE) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	line := c.line[:0]
	line = strconv.AppendUint(line, uint64(fie.PDID), 10)
	line = append(line, ',')
	line = strconv.AppendInt(line, fie.CaptureUnix, 10)
	line = appendReply(line, fie.Near, fie.NearDelta)
	line = appendReply(line, fie.Far, fie.FarDelta)
	line = append(line, '\n')
	c.line = line
	if _, err := c.writer.Write(line); err != nil {
		return fmt.Errorf("cannot send FIE: %w", err)
	}
	return nil
}

// Flush sends the buffered FIEs to the orchestrator. Like SendFIE it has no
// deadline.
func (c *OrchestratorConn) Flush() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("cannot flush FIEs: %w", err)
	}
	return nil
}

// RemoteAddr returns the orchestrator's network address.
func (c *OrchestratorConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Close closes the connection.
func (c *OrchestratorConn) Close() error {
	return c.conn.Close()
}

// appendReply appends one quoted reply address and its delta. The zero Addr
// means there was no reply, and is written as an empty address.
func appendReply(line []byte, reply netip.Addr, delta uint32) []byte {
	line = append(line, ',', '"')
	if !reply.IsValid() {
		return append(line, '"', ',', '0')
	}
	line = reply.AppendTo(line)
	line = append(line, '"', ',')
	return strconv.AppendUint(line, uint64(delta), 10)
}

// deadline returns the time a timeout expires, or no deadline for zero.
func deadline(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}
