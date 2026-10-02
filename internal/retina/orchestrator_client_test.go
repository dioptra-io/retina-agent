// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// dialOrchestrator connects to a local listener and returns both ends.
func dialOrchestrator(t *testing.T) (orchestrator net.Conn, conn *OrchestratorConn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	conn, err = DialOrchestrator(t.Context(), &OrchestratorConfig{
		Address:          listener.Addr().String(),
		Secret:           "s3cret",
		HandshakeTimeout: time.Second,
		ConnectTimeout:   time.Second,
		WriteBufferSize:  4096,
		FlushPeriod:      time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	orchestrator, err = listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orchestrator.Close() })
	return orchestrator, conn
}

func TestOrchestratorConn_HandshakeReceiveSend(t *testing.T) {
	orchestrator, conn := dialOrchestrator(t)
	orchestratorReader := bufio.NewReader(orchestrator)

	// The first PD is written right behind the auth response, to check that
	// the handshake does not swallow it.
	fmt.Fprint(orchestrator, `{"authenticated":true,"message":"authenticated"}`+"\n"+`7,"198.51.100.9",4,17,24000,33434`+"\n") //nolint

	if err := conn.Handshake("a1"); err != nil {
		t.Fatal(err)
	}
	if line, _ := orchestratorReader.ReadString('\n'); line != `{"agent_id":"a1","secret":"s3cret"}`+"\n" {
		t.Fatalf("unexpected auth request %q", line)
	}

	pd, err := conn.ReceivePD()
	if err != nil {
		t.Fatal(err)
	}
	wantPD := PD{
		ID:             7,
		Destination:    netip.MustParseAddr("198.51.100.9"),
		NearTTL:        4,
		Protocol:       17,
		FirstHalfWord:  24000,
		SecondHalfWord: 33434,
	}
	if pd != wantPD {
		t.Fatalf("PD: got %+v, want %+v", pd, wantPD)
	}

	fie := &FIE{PDID: 7, CaptureUnix: 1000, Near: netip.MustParseAddr("192.0.2.1"), NearDelta: 2}
	if err := conn.SendFIE(fie); err != nil {
		t.Fatal(err)
	}
	// The FIE is only buffered until Flush.
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	want := `7,1000,"192.0.2.1",2,"",0` + "\n"
	if line, _ := orchestratorReader.ReadString('\n'); line != want {
		t.Fatalf("FIE record: got %q, want %q", line, want)
	}
}

func TestOrchestratorConn_HandshakeRejected(t *testing.T) {
	orchestrator, conn := dialOrchestrator(t)

	fmt.Fprintln(orchestrator, `{"authenticated":false,"message":"secret is not correct"}`) //nolint
	if err := conn.Handshake("a1"); err == nil {
		t.Fatal("expected an error for a rejected agent")
	}
}

func TestOrchestratorConn_ReceivePDRejectsBadLines(t *testing.T) {
	for _, line := range []string{
		`7,"198.51.100.9",4,17,24000`,
		`7,"198.51.100.9",4,17,24000,33434,1`,
		`x,"198.51.100.9",4,17,24000,33434`,
		`7,"not-an-address",4,17,24000,33434`,
		`7,"198.51.100.9",256,17,24000,33434`,
	} {
		orchestrator, conn := dialOrchestrator(t)
		fmt.Fprintln(orchestrator, line) //nolint
		// The good line behind it is still read.
		fmt.Fprintln(orchestrator, `8,"198.51.100.9",4,17,24000,33434`) //nolint
		if _, err := conn.ReceivePD(); !errors.Is(err, ErrMalformedPD) {
			t.Errorf("PD line %q: got error %v, want ErrMalformedPD", line, err)
		}
		if pd, err := conn.ReceivePD(); err != nil || pd.ID != 8 {
			t.Errorf("after PD line %q: got PD %+v, error %v", line, pd, err)
		}
	}
}

func TestOrchestratorConn_ReceivePDAcceptsVariants(t *testing.T) {
	orchestrator, conn := dialOrchestrator(t)

	// Blank lines are skipped, and a carriage return, an IPv6 destination
	// and a destination without quotes are all accepted.
	fmt.Fprint(orchestrator, "\n\n"+ //nolint
		`1,"198.51.100.9",4,17,24000,33434`+"\r\n"+
		"\n"+
		`2,"2001:db8::9",30,58,1,65535`+"\n"+
		`4294967295,198.51.100.9,1,1,0,0`+"\n")

	for _, want := range []PD{
		{ID: 1, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434},
		{ID: 2, Destination: netip.MustParseAddr("2001:db8::9"), NearTTL: 30, Protocol: 58, FirstHalfWord: 1, SecondHalfWord: 65535},
		{ID: 4294967295, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 1, Protocol: 1},
	} {
		pd, err := conn.ReceivePD()
		if err != nil {
			t.Fatal(err)
		}
		if pd != want {
			t.Errorf("got %+v, want %+v", pd, want)
		}
	}
}

func TestOrchestratorConn_ReceivePDRejectsOverlongLine(t *testing.T) {
	orchestrator, conn := dialOrchestrator(t)

	// Longer than the read buffer: an error, not an endless read.
	fmt.Fprintln(orchestrator, strings.Repeat("9", 100_000)) //nolint
	if _, err := conn.ReceivePD(); err == nil || errors.Is(err, ErrMalformedPD) {
		t.Fatalf("got error %v, want a connection error for an overlong line", err)
	}
}

func TestOrchestratorConn_CloseUnblocksReceivePD(t *testing.T) {
	_, conn := dialOrchestrator(t)

	done := make(chan error, 1)
	go func() {
		_, err := conn.ReceivePD()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = conn.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ReceivePD returned no error on a closed connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReceivePD did not return after Close")
	}
}

func TestOrchestratorConn_HandshakeFailures(t *testing.T) {
	// An orchestrator that does not answer: the handshake times out.
	_, conn := dialOrchestrator(t)
	start := time.Now()
	if err := conn.Handshake("a1"); err == nil {
		t.Error("expected an error from a silent orchestrator")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("handshake took %v, with a timeout of 1s", elapsed)
	}

	// An answer that is not JSON.
	orchestrator, conn := dialOrchestrator(t)
	fmt.Fprintln(orchestrator, "welcome") //nolint
	if err := conn.Handshake("a1"); err == nil {
		t.Error("expected an error for an answer that is not JSON")
	}

	// A connection closed during the handshake.
	orchestrator, conn = dialOrchestrator(t)
	_ = orchestrator.Close()
	if err := conn.Handshake("a1"); err == nil {
		t.Error("expected an error for a closed connection")
	}
}

func TestOrchestratorConn_SendFIEFormats(t *testing.T) {
	orchestrator, conn := dialOrchestrator(t)
	reader := bufio.NewReader(orchestrator)

	for _, test := range []struct {
		fie  FIE
		want string
	}{
		{FIE{PDID: 1, CaptureUnix: 1000}, `1,1000,"",0,"",0`},
		{FIE{PDID: 2, CaptureUnix: 1000, Far: netip.MustParseAddr("192.0.2.2"), FarDelta: 1}, `2,1000,"",0,"192.0.2.2",1`},
		{
			FIE{PDID: 4294967295, CaptureUnix: 1790953678, Near: netip.MustParseAddr("2001:db8::1"), NearDelta: 2, Far: netip.MustParseAddr("2001:db8::2"), FarDelta: 3},
			`4294967295,1790953678,"2001:db8::1",2,"2001:db8::2",3`,
		},
	} {
		if err := conn.SendFIE(&test.fie); err != nil {
			t.Fatal(err)
		}
		if err := conn.Flush(); err != nil {
			t.Fatal(err)
		}
		if line, _ := reader.ReadString('\n'); line != test.want+"\n" {
			t.Errorf("got %q, want %q", line, test.want)
		}
	}
}

// TestOrchestratorConn_SendFIEWhileFlushing sends FIEs from one goroutine
// while another flushes, as the agent does: every FIE must arrive whole and
// in order.
func TestOrchestratorConn_SendFIEWhileFlushing(t *testing.T) {
	orchestrator, conn := dialOrchestrator(t)

	const count = 20_000
	stop := make(chan struct{})
	var group sync.WaitGroup
	group.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = conn.Flush()
			}
		}
	})
	group.Go(func() {
		for id := uint32(1); id <= count; id++ {
			if err := conn.SendFIE(&FIE{PDID: id, CaptureUnix: 1000, Near: netip.MustParseAddr("192.0.2.1")}); err != nil {
				t.Errorf("SendFIE %d: %v", id, err)
				return
			}
		}
	})

	_ = orchestrator.SetReadDeadline(time.Now().Add(20 * time.Second))
	reader := bufio.NewReader(orchestrator)
	for id := 1; id <= count; id++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("FIE %d: %v", id, err)
		}
		if want := fmt.Sprintf(`%d,1000,"192.0.2.1",0,"",0`, id) + "\n"; line != want {
			t.Fatalf("got %q, want %q", line, want)
		}
	}
	close(stop)
	group.Wait()
}
