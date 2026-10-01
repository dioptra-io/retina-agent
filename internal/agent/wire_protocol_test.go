// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

package agent

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

func TestDecodePDRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		want api.Protocol
		v6   bool
	}{
		{"icmp4", `1234,"1.1.1.1",3,1,12,34` + "\n", api.ICMP, false},
		{"udp6", `99,"2001:db8::1",254,17,33434,443` + "\n", api.UDP, true},
		{"icmp6", `100,"2001:db8::2",1,58,65535,0` + "\n", api.ICMPv6, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pd, err := decodePDRecord(tt.line, "agent-1")
			if err != nil {
				t.Fatal(err)
			}
			if pd.Protocol != tt.want || pd.AgentID != "agent-1" || (pd.IPVersion == api.IPv6) != tt.v6 {
				t.Fatalf("unexpected PD: %+v", pd)
			}
			first, second := extractHalfWords(pd)
			if first == 0 && second == 0 {
				t.Fatal("correlation half-words were not reconstructed")
			}
		})
	}
}

func TestDecodePDRecordRejectsInvalidRows(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		`1,"bad",3,1,0,0`,
		`1,"1.1.1.1",256,1,0,0`,
		`1,"1.1.1.1",3,99,0,0`,
		`1,"1.1.1.1",3,1,0`,
	} {
		if _, err := decodePDRecord(line+"\n", "agent"); err == nil {
			t.Errorf("decodePDRecord(%q) succeeded", line)
		}
	}
}

func TestEncodeFIERecord(t *testing.T) {
	t.Parallel()
	capture := time.Unix(1_790_868_969, 0).UTC()
	fie := &api.ForwardingInfoElement{
		ProbingDirectiveID:  1234,
		ProductionTimestamp: capture,
		NearInfo:            &api.Info{ReplyAddress: net.ParseIP("1.2.3.4"), ReceivedTimestamp: capture.Add(-time.Second)},
	}
	want := "1234,1790868969,\"1.2.3.4\",1,\"\",0\n"
	if got := encodeFIERecord(fie); got != want {
		t.Fatalf("encodeFIERecord() = %q, want %q", got, want)
	}
}

func TestCSVReaderLoop(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	a := &agent{config: &Config{AgentID: "agent-1", ReadDeadline: time.Second}, logger: testLogger(), metrics: testMetrics()}
	pds := make(chan *api.ProbingDirective, 1)
	done := make(chan error, 1)
	go func() {
		done <- a.readerLoopWithReader(context.Background(), client, bufio.NewReader(client), pds)
	}()
	if _, err := server.Write([]byte("\n7,\"2001:db8::1\",3,58,12,34\n")); err != nil {
		t.Fatal(err)
	}
	pd := <-pds
	if pd.ProbingDirectiveID != 7 || pd.AgentID != "agent-1" || pd.IPVersion != api.IPv6 {
		t.Fatalf("unexpected PD: %+v", pd)
	}
	_ = server.Close()
	if err := <-done; err == nil {
		t.Fatal("reader loop succeeded after peer close")
	}
}

func TestCSVWriterLoop(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()
	a := &agent{config: &Config{WriteDeadline: time.Second}, logger: testLogger(), metrics: testMetrics()}
	fies := make(chan *api.ForwardingInfoElement, 1)
	fies <- &api.ForwardingInfoElement{ProbingDirectiveID: 9, ProductionTimestamp: time.Unix(10, 0)}
	close(fies)
	done := make(chan error, 1)
	go func() {
		done <- a.writerLoopWithWriter(context.Background(), client, bufio.NewWriter(client), fies)
	}()
	line, err := bufio.NewReader(server).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "9,10,\"\",0,\"\",0\n" {
		t.Fatalf("unexpected FIE line %q", line)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
