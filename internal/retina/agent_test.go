// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// testConfig returns a valid configuration for an agent of the orchestrator
// at address.
func testConfig(address string) *Config {
	return &Config{
		ID: "a1",
		Orchestrator: OrchestratorConfig{
			Address:             address,
			ConnectTimeout:      time.Second,
			HandshakeTimeout:    time.Second,
			WriteBufferSize:     4096,
			FlushPeriod:         10 * time.Millisecond,
			ReconnectMinBackoff: 10 * time.Millisecond,
			ReconnectMaxBackoff: 20 * time.Millisecond,
		},
		Prober: ProberConfig{
			PDQueueSize:  16,
			FIEQueueSize: 16,
			Mock:         MockProberConfig{Delay: 200 * time.Millisecond, MaxInflight: 16},
		},
	}
}

func TestAgent_ReconnectsAndStops(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	// The orchestrator accepts each agent connection, authenticates it and
	// drops it: once before the handshake, to cover that path too.
	sessions := make(chan struct{}, 16)
	go func() {
		for i := 0; ; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if i > 0 {
				_, _ = bufio.NewReader(conn).ReadString('\n')
				fmt.Fprintln(conn, `{"authenticated":true,"message":"authenticated"}`) //nolint
				sessions <- struct{}{}
			}
			_ = conn.Close()
		}
	}()

	agent, err := NewAgent(testConfig(listener.Addr().String()), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()

	for range 3 {
		select {
		case <-sessions:
		case <-time.After(5 * time.Second):
			t.Fatal("agent did not reconnect")
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: got %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestAgent_StopsWhileConnected(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	connected := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		_, _ = reader.ReadString('\n')
		fmt.Fprintln(conn, `{"authenticated":true,"message":"authenticated"}`) //nolint
		close(connected)
		// Silent until the agent closes the connection.
		_, _ = reader.ReadString('\n')
	}()

	agent, err := NewAgent(testConfig(listener.Addr().String()), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()

	<-connected
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: got %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestConfig_Validate(t *testing.T) {
	if err := testConfig("127.0.0.1:1").Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, change := range map[string]func(*Config){
		"empty id":            func(c *Config) { c.ID = "" },
		"empty address":       func(c *Config) { c.Orchestrator.Address = "" },
		"no write buffer":     func(c *Config) { c.Orchestrator.WriteBufferSize = 0 },
		"no flush period":     func(c *Config) { c.Orchestrator.FlushPeriod = 0 },
		"no min backoff":      func(c *Config) { c.Orchestrator.ReconnectMinBackoff = 0 },
		"max below min":       func(c *Config) { c.Orchestrator.ReconnectMaxBackoff = time.Millisecond },
		"negative PD queue":   func(c *Config) { c.Prober.PDQueueSize = -1 },
		"negative FIE queue":  func(c *Config) { c.Prober.FIEQueueSize = -1 },
		"negative mock delay": func(c *Config) { c.Prober.Mock.Delay = -1 },
		"no mock inflight":    func(c *Config) { c.Prober.Mock.MaxInflight = 0 },
		"no caracal path":     func(c *Config) { c.Prober.Caracal = testCaracalConfig("") },
		"no caracal buffer": func(c *Config) {
			c.Prober.Caracal = testCaracalConfig("caracal")
			c.Prober.Caracal.WriteBufferSize = 0
		},
		"no caracal timeout": func(c *Config) {
			c.Prober.Caracal = testCaracalConfig("caracal")
			c.Prober.Caracal.StopTimeout = 0
		},
	} {
		config := testConfig("127.0.0.1:1")
		change(config)
		if err := config.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	// The mock prober's configuration is not looked at when caracal is used.
	config := testConfig("127.0.0.1:1")
	config.Prober.Caracal = testCaracalConfig("caracal")
	config.Prober.Mock = MockProberConfig{}
	if err := config.Validate(); err != nil {
		t.Errorf("valid caracal config rejected: %v", err)
	}
}

func TestNewAgent(t *testing.T) {
	if _, err := NewAgent(nil, nil); err == nil {
		t.Error("expected an error for a nil config")
	}
	if _, err := NewAgent(&Config{}, nil); err == nil {
		t.Error("expected an error for an empty config")
	}

	// A nil logger is allowed, and the prober follows the configuration.
	agent, err := NewAgent(testConfig("127.0.0.1:1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.prober.(*MockProber); !ok {
		t.Errorf("got prober %T, want the mock prober", agent.prober)
	}

	config := testConfig("127.0.0.1:1")
	config.Prober.Caracal = testCaracalConfig("caracal")
	agent, err = NewAgent(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.prober.(*CaracalProber); !ok {
		t.Errorf("got prober %T, want the caracal prober", agent.prober)
	}
}

func TestJitter(t *testing.T) {
	backoff := time.Second
	for range 1000 {
		if wait := jitter(backoff); wait < 800*time.Millisecond || wait > 1200*time.Millisecond {
			t.Fatalf("jitter(%v) = %v, want within 20%%", backoff, wait)
		}
	}
}

// runAgent runs an agent until the test ends, and checks that it then stops
// cleanly.
func runAgent(t *testing.T, config *Config) {
	t.Helper()
	agent, err := NewAgent(config, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: got %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
}

// accept waits for the agent's next connection.
func accept(t *testing.T, listener net.Listener) net.Conn {
	t.Helper()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("agent did not connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// TestAgent_NoPDLostUnderBackpressure sends many more PDs than the queues
// and the prober hold: the agent must slow the orchestrator down, and every
// PD must come back as a FIE, in order.
func TestAgent_NoPDLostUnderBackpressure(t *testing.T) {
	listener := listen(t)
	config := testConfig(listener.Addr().String())
	config.Prober = ProberConfig{PDQueueSize: 1, FIEQueueSize: 1, Mock: MockProberConfig{MaxInflight: 2}}
	runAgent(t, config)

	const count = 300
	conn := accept(t, listener)
	reader := authenticate(t, conn)
	go func() {
		for id := 1; id <= count; id++ {
			fmt.Fprintf(conn, "%d,%q,4,17,24000,33434\n", id, "198.51.100.9") //nolint
		}
	}()

	for id := 1; id <= count; id++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("FIE %d: %v", id, err)
		}
		if !strings.HasPrefix(line, fmt.Sprintf("%d,", id)) {
			t.Fatalf("FIE %d: got line %q", id, line)
		}
	}
}

func TestAgent_RetriesAfterRejectedHandshake(t *testing.T) {
	listener := listen(t)
	runAgent(t, testConfig(listener.Addr().String()))

	rejected := accept(t, listener)
	if _, err := bufio.NewReader(rejected).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(rejected, `{"authenticated":false,"message":"secret is not correct"}`) //nolint

	// The agent comes back, and works once it is accepted.
	conn := accept(t, listener)
	reader := authenticate(t, conn)
	fmt.Fprintf(conn, "1,%q,4,17,24000,33434\n", "198.51.100.9") //nolint
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "1,") {
		t.Fatalf("got FIE line %q, error %v", line, err)
	}
}

func TestAgent_ReconnectsAfterMalformedPD(t *testing.T) {
	listener := listen(t)
	runAgent(t, testConfig(listener.Addr().String()))

	// A line that is not a PD ends the connection.
	first := accept(t, listener)
	reader := authenticate(t, first)
	fmt.Fprintln(first, "this is not a PD") //nolint
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("the agent kept the connection after a malformed PD")
	}

	second := accept(t, listener)
	reader = authenticate(t, second)
	fmt.Fprintf(second, "1,%q,4,17,24000,33434\n", "198.51.100.9") //nolint
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "1,") {
		t.Fatalf("got FIE line %q, error %v", line, err)
	}
}

// TestAgent_StopsWhileProberIsBlocked stops an agent that has no connection
// and whose prober is waiting for room in the FIE queue.
func TestAgent_StopsWhileProberIsBlocked(t *testing.T) {
	// Nothing listens on the address.
	config := testConfig("127.0.0.1:1")
	config.Prober.FIEQueueSize = 1
	config.Prober.Mock.Delay = 0
	agent, err := NewAgent(config, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for id := uint32(1); id <= 5; id++ {
		agent.pds <- PD{ID: id}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()

	// The prober has filled the FIE queue and cannot write the next FIE.
	deadline := time.Now().Add(5 * time.Second)
	for len(agent.fies) < 1 || len(agent.pds) > 3 {
		if time.Now().After(deadline) {
			t.Fatal("the prober did not fill the FIE queue")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: got %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// authenticate answers the agent's auth request on an accepted connection.
func authenticate(t *testing.T, conn net.Conn) *bufio.Reader {
	t.Helper()
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Errorf("cannot read auth request: %v", err)
	}
	fmt.Fprintln(conn, `{"authenticated":true,"message":"authenticated"}`) //nolint
	return reader
}

func TestAgent_ProbesAndKeepsFIEsAcrossReconnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	agent, err := NewAgent(testConfig(listener.Addr().String()), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- agent.Run(t.Context()) }()

	// The first connection is dropped right after its PDs, before the mock
	// prober's delay has passed.
	first, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	authenticate(t, first)
	for id := 1; id <= 3; id++ {
		fmt.Fprintf(first, "%d,%q,4,17,24000,33434\n", id, "198.51.100.9") //nolint
	}
	time.Sleep(50 * time.Millisecond)
	_ = first.Close()

	// Their FIEs arrive on the second connection, followed by the FIE of a
	// PD sent on it.
	second, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	reader := authenticate(t, second)
	fmt.Fprintf(second, "4,%q,4,17,24000,33434\n", "198.51.100.9") //nolint

	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	for id := 1; id <= 4; id++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("FIE %d: %v", id, err)
		}
		var gotID int
		var rest string
		if _, err := fmt.Sscanf(line, "%d,%s", &gotID, &rest); err != nil || gotID != id {
			t.Fatalf("FIE %d: got line %q", id, line)
		}
		if !strings.HasSuffix(rest, `,"192.0.2.1",0,"192.0.2.2",0`) {
			t.Fatalf("FIE %d: got line %q", id, line)
		}
	}
}

func TestAgent_DiscardsQueuesOnDisconnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	config := testConfig(listener.Addr().String())
	config.Prober.DiscardQueuedOnDisconnect = true
	agent, err := NewAgent(config, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- agent.Run(t.Context()) }()

	first, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	authenticate(t, first)
	for id := 1; id <= 3; id++ {
		fmt.Fprintf(first, "%d,%q,4,17,24000,33434\n", id, "198.51.100.9") //nolint
	}
	time.Sleep(50 * time.Millisecond)
	_ = first.Close()

	// The agent is kept waiting for the handshake until the FIEs of the
	// first connection are in the queue. They are discarded, so the first
	// FIE of the second connection is that of its own PD.
	second, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	time.Sleep(400 * time.Millisecond)
	reader := authenticate(t, second)
	fmt.Fprintf(second, "4,%q,4,17,24000,33434\n", "198.51.100.9") //nolint

	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "4,") {
		t.Fatalf("got FIE line %q, want the FIE of PD 4", line)
	}
}

// failingProber is a prober that fails as soon as it runs.
type failingProber struct{}

func (failingProber) Run(context.Context, <-chan PD, chan<- FIE) error {
	return errors.New("boom")
}

func TestAgent_StopsWhenProberFails(t *testing.T) {
	// Nothing listens on the address: the agent is between connections.
	agent, err := NewAgent(testConfig("127.0.0.1:1"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	agent.prober = failingProber{}

	done := make(chan error, 1)
	go func() { done <- agent.Run(t.Context()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run: got nil, want the prober's error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the prober failed")
	}
}
