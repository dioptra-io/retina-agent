// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"bufio"
	"fmt"
	"net"
	"net/netip"
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
		if _, err := conn.ReceivePD(); err == nil {
			t.Errorf("expected an error for PD line %q", line)
		}
	}
}
