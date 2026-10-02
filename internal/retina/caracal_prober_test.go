// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"bufio"
	"context"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockCaracal is the script that stands in for caracal in the tests.
const mockCaracal = "../../scripts/mock-caracal.sh"

// requireMockCaracal skips the test where the mock caracal script cannot
// run: it needs bash 5.
func requireMockCaracal(t *testing.T) {
	t.Helper()
	if err := exec.Command(mockCaracal, "--help").Run(); err != nil {
		t.Skipf("cannot run %s: %v", mockCaracal, err)
	}
}

func testCaracalConfig(path string, args ...string) *CaracalProberConfig {
	return &CaracalProberConfig{Path: path, Args: args, WriteBufferSize: 4096, StopTimeout: time.Second}
}

func TestAppendCaracalProbe(t *testing.T) {
	pd := PD{Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434}
	line := appendCaracalProbe(nil, &pd, 4, "udp")
	line = appendCaracalProbe(line, &pd, 5, "udp")
	want := "198.51.100.9,24000,33434,4,udp\n198.51.100.9,24000,33434,5,udp\n"
	if string(line) != want {
		t.Fatalf("got %q, want %q", line, want)
	}
}

func TestParseCaracalReply(t *testing.T) {
	// A time exceeded for a UDP probe, with two MPLS labels.
	line := `1790952971310934,17,::ffff:192.0.2.100,::ffff:198.51.100.9,24000,33434,4,1,::ffff:203.0.113.7,1,11,0,250,56,"[(16001,0,1,1),(24,0,0,1)]",123,1` + "\n"
	reply, err := parseCaracalReply([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	want := caracalReply{
		CaptureMicros: 1790952971310934,
		Protocol:      17,
		Destination:   netip.MustParseAddr("198.51.100.9"),
		SrcPort:       24000,
		DstPort:       33434,
		TTL:           4,
		Address:       netip.MustParseAddr("203.0.113.7"),
		ICMPType:      11,
		RTT:           123,
	}
	if reply != want {
		t.Fatalf("got %+v, want %+v", reply, want)
	}

	for _, line := range []string{
		"",
		"1790952971310934,17,::ffff:192.0.2.100",
		`x,17,::ffff:192.0.2.100,::ffff:198.51.100.9,24000,33434,4,1,::ffff:203.0.113.7,1,11,0,250,56,"[]",123,1`,
		`1,17,::ffff:192.0.2.100,nowhere,24000,33434,4,1,::ffff:203.0.113.7,1,11,0,250,56,"[]",123,1`,
		`1,17,::ffff:192.0.2.100,::ffff:198.51.100.9,24000,33434,400,1,::ffff:203.0.113.7,1,11,0,250,56,"[]",123,1`,
		`1,17,::ffff:192.0.2.100,::ffff:198.51.100.9,24000,33434,4,1,::ffff:203.0.113.7,1,11,0,250,56,"[]",,1`,
	} {
		if _, err := parseCaracalReply([]byte(line)); err == nil {
			t.Errorf("expected an error for reply line %q", line)
		}
	}
}

// TestMockCaracal checks that the mock caracal script takes the probes the
// prober writes and answers with replies the prober can decode.
func TestMockCaracal(t *testing.T) {
	requireMockCaracal(t)

	udp := PD{Destination: netip.MustParseAddr("198.51.100.9"), Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434}
	icmp6 := PD{Destination: netip.MustParseAddr("2001:db8::9"), Protocol: 58, FirstHalfWord: 24000, SecondHalfWord: 33434}
	probes := appendCaracalProbe(nil, &udp, 4, "udp")
	probes = appendCaracalProbe(probes, &icmp6, 5, "icmp6")

	cmd := exec.Command(mockCaracal, "--probing-rate", "1000", "--sniffer-wait-time", "0")
	cmd.Env = append(cmd.Environ(), "MOCK_CARACAL_RTT_MS=30")
	cmd.Stdin = strings.NewReader(string(probes) + "not a probe\n")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	if !scanner.Scan() || scanner.Text() != caracalHeader {
		t.Fatalf("unexpected header %q", scanner.Text())
	}
	wants := []caracalReply{
		{Protocol: 17, Destination: udp.Destination, SrcPort: 24000, DstPort: 33434, TTL: 4, Address: netip.MustParseAddr("10.0.0.4"), ICMPType: 11, RTT: 300},
		// The destination port of an ICMP probe does not come back.
		{Protocol: 58, Destination: icmp6.Destination, SrcPort: 24000, DstPort: 0, TTL: 5, Address: netip.MustParseAddr("2001:db8::5"), ICMPType: 3, RTT: 300},
	}
	for _, want := range wants {
		if !scanner.Scan() {
			t.Fatal("missing reply")
		}
		reply, err := parseCaracalReply(scanner.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if reply.CaptureMicros == 0 {
			t.Errorf("reply %+v has no capture timestamp", reply)
		}
		reply.CaptureMicros = 0
		if reply != want {
			t.Errorf("got %+v, want %+v", reply, want)
		}
	}
	if scanner.Scan() {
		t.Fatalf("unexpected line %q", scanner.Text())
	}
}

func TestCaracalProber_RunsUntilCanceled(t *testing.T) {
	requireMockCaracal(t)

	prober := NewCaracalProber(testCaracalConfig(mockCaracal, "--probing-rate", "1000"), slog.New(slog.DiscardHandler))
	pds := make(chan PD)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- prober.Run(ctx, pds, make(chan FIE)) }()

	// The PDs are taken: their probes are written to the process.
	for id := uint32(1); id <= 3; id++ {
		select {
		case pds <- PD{ID: id, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 17}:
		case err := <-done:
			t.Fatalf("Run returned early: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("PD was not taken")
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

func TestCaracalProber_FailsWhenCaracalStops(t *testing.T) {
	for name, config := range map[string]*CaracalProberConfig{
		"not found":      testCaracalConfig("/nonexistent/caracal"),
		"exits":          testCaracalConfig("sh", "-c", "exit 3"),
		"unknown header": testCaracalConfig("sh", "-c", "echo a,b,c; sleep 5"),
	} {
		prober := NewCaracalProber(config, slog.New(slog.DiscardHandler))
		done := make(chan error, 1)
		go func() { done <- prober.Run(t.Context(), make(chan PD), make(chan FIE)) }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: Run returned nil, want an error", name)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: Run did not return", name)
		}
	}
}

// TestCaracalProber_WritesProbes runs a stand-in for caracal that keeps what
// it is given, and checks the probes of the PDs that can be probed.
func TestCaracalProber_WritesProbes(t *testing.T) {
	received := filepath.Join(t.TempDir(), "probes")
	// $0 is the header and $1 the file. The shell stays, to keep the output
	// open: caracal's output only ends when caracal stops.
	config := testCaracalConfig("sh", "-c", `echo "$0"; cat > "$1"`, caracalHeader, received)
	prober := NewCaracalProber(config, slog.New(slog.DiscardHandler))

	pds := make(chan PD)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- prober.Run(ctx, pds, make(chan FIE)) }()

	for _, pd := range []PD{
		{ID: 1, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434},
		// Not probed: the far TTL does not fit, and TCP is not supported.
		{ID: 2, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 255, Protocol: 17},
		{ID: 3, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 6},
		{ID: 4, Destination: netip.MustParseAddr("2001:db8::9"), NearTTL: 254, Protocol: 58, FirstHalfWord: 7, SecondHalfWord: 8},
		{ID: 5, Destination: netip.MustParseAddr("203.0.113.1"), NearTTL: 1, Protocol: 1, FirstHalfWord: 9},
	} {
		select {
		case pds <- pd:
		case err := <-done:
			t.Fatalf("Run returned early: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("PD was not taken")
		}
	}

	want := "198.51.100.9,24000,33434,4,udp\n198.51.100.9,24000,33434,5,udp\n" +
		"2001:db8::9,7,8,254,icmp6\n2001:db8::9,7,8,255,icmp6\n" +
		"203.0.113.1,9,0,1,icmp\n203.0.113.1,9,0,2,icmp\n"
	var got []byte
	deadline := time.Now().Add(5 * time.Second)
	for string(got) != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		got, _ = os.ReadFile(received)
	}
	if string(got) != want {
		t.Errorf("caracal received %q, want %q", got, want)
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

// TestCaracalProber_SurvivesBadReplies checks that lines that are not replies
// do not stop the prober.
func TestCaracalProber_SurvivesBadReplies(t *testing.T) {
	script := `echo "$0"; echo garbage; echo "1,2,3"; echo; echo "log line" >&2; exec sleep 30`
	prober := NewCaracalProber(testCaracalConfig("sh", "-c", script, caracalHeader), slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- prober.Run(ctx, make(chan PD), make(chan FIE)) }()

	select {
	case err := <-done:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(300 * time.Millisecond):
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
