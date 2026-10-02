// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"bufio"
	"context"
	"log/slog"
	"maps"
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

// testCaracalConfig returns a valid configuration that starts the caracal at
// path.
func testCaracalConfig(path string) *CaracalProberConfig {
	return &CaracalProberConfig{
		Path:            path,
		ProbingRate:     10_000,
		ProbeTimeout:    300 * time.Millisecond,
		WriteBufferSize: 4096,
		StopTimeout:     time.Second,
	}
}

// fakeCaracal writes a shell script that stands in for caracal, and returns
// its path. The script first writes caracal's header.
func fakeCaracal(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caracal")
	content := "#!/bin/sh\necho '" + caracalHeader + "'\n" + script + "\n"
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil { //nolint:gosec // G306: the script must be executable
		t.Fatal(err)
	}
	return path
}

// runCaracalProber runs a caracal prober until the test ends, checks that it
// then stops cleanly, and returns its channels.
func runCaracalProber(t *testing.T, config *CaracalProberConfig) (chan<- PD, <-chan FIE) {
	t.Helper()
	pds := make(chan PD)
	fies := make(chan FIE, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewCaracalProber(config, slog.New(slog.DiscardHandler)).Run(ctx, pds, fies) }()
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
	return pds, fies
}

// sendPD hands a PD to a prober.
func sendPD(t *testing.T, pds chan<- PD, pd PD) {
	t.Helper()
	select {
	case pds <- pd:
	case <-time.After(5 * time.Second):
		t.Fatalf("PD %d was not taken", pd.ID)
	}
}

// receiveFIEs waits for count FIEs and returns them in the order they came.
func receiveFIEs(t *testing.T, fies <-chan FIE, count int) []FIE {
	t.Helper()
	received := make([]FIE, 0, count)
	for len(received) < count {
		select {
		case fie := <-fies:
			received = append(received, fie)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d FIEs, want %d", len(received), count)
		}
	}
	return received
}

// noMoreFIEs checks that no FIE comes for a while.
func noMoreFIEs(t *testing.T, fies <-chan FIE) {
	t.Helper()
	select {
	case fie := <-fies:
		t.Fatalf("unexpected FIE %+v", fie)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestAppendCaracalProbe(t *testing.T) {
	pd := PD{Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434}
	line := appendCaracalProbe(nil, &pd, 4, "udp")
	line = appendCaracalProbe(line, &pd, 5, "udp")
	want := "198.51.100.9,24000,33434,4,udp\n198.51.100.9,24000,33434,5,udp\n"
	if string(line) != want {
		t.Fatalf("got %q, want %q", line, want)
	}

	// The destination port of an ICMP probe is written as zero.
	pd = PD{Destination: netip.MustParseAddr("2001:db8::9"), NearTTL: 4, Protocol: 58, FirstHalfWord: 24000, SecondHalfWord: 33434}
	if line := appendCaracalProbe(nil, &pd, 4, "icmp6"); string(line) != "2001:db8::9,24000,0,4,icmp6\n" {
		t.Fatalf("got %q", line)
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
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestCaracalProberConfig_Args(t *testing.T) {
	config := testCaracalConfig("caracal")
	if got, want := strings.Join(config.args(), " "), "--probing-rate 10000"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Every option left out: caracal runs with its defaults.
	if args := (&CaracalProberConfig{}).args(); len(args) != 0 {
		t.Errorf("got %q, want no arguments", args)
	}

	config = &CaracalProberConfig{
		ProbingRate: 500, Interface: "eth0", BatchSize: 64, LogLevel: "debug", NPackets: 2, MaxProbes: 1000,
		SourceAddressV4: "192.0.2.1", SourceAddressV6: "2001:db8::1", SnifferWaitTime: 3, RateLimitingMethod: "sleep",
		FilterFromPrefixFileExcl: "excl.txt", FilterFromPrefixFileIncl: "incl.txt", FilterMinTTL: 2, FilterMaxTTL: 32,
		CaracalID: 7, MetaRound: "9", NoIntegrityCheck: true,
	}
	want := "--probing-rate 500 --interface eth0 --batch-size 64 --log-level debug --n-packets 2 --max-probes 1000" +
		" --source-address-v4 192.0.2.1 --source-address-v6 2001:db8::1 --sniffer-wait-time 3 --rate-limiting-method sleep" +
		" --filter-from-prefix-file-excl excl.txt --filter-from-prefix-file-incl incl.txt --filter-min-ttl 2 --filter-max-ttl 32" +
		" --caracal-id 7 --meta-round 9 --no-integrity-check"
	if got := strings.Join(config.args(), " "); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCaracalProber_FailsWhenCaracalStops(t *testing.T) {
	exits := filepath.Join(t.TempDir(), "exits")
	if err := os.WriteFile(exits, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { //nolint:gosec // G306: the script must be executable
		t.Fatal(err)
	}
	header := filepath.Join(t.TempDir(), "header")
	if err := os.WriteFile(header, []byte("#!/bin/sh\necho a,b,c\nexec sleep 5\n"), 0o700); err != nil { //nolint:gosec // G306: the script must be executable
		t.Fatal(err)
	}

	for name, config := range map[string]*CaracalProberConfig{
		"not found":      testCaracalConfig("/nonexistent/caracal"),
		"exits":          testCaracalConfig(exits),
		"unknown header": testCaracalConfig(header),
		// Caracal's output only ends when caracal stops.
		"closes output": testCaracalConfig(fakeCaracal(t, "exec cat > /dev/null")),
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
	// The shell stays, to keep the output open.
	pds, _ := runCaracalProber(t, testCaracalConfig(fakeCaracal(t, "cat > '"+received+"'")))

	for _, pd := range []PD{
		{ID: 1, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434},
		// Not probed: the far TTL does not fit, and TCP is not supported.
		{ID: 2, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 255, Protocol: 17},
		{ID: 3, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 6},
		{ID: 4, Destination: netip.MustParseAddr("2001:db8::9"), NearTTL: 254, Protocol: 58, FirstHalfWord: 7, SecondHalfWord: 8},
		{ID: 5, Destination: netip.MustParseAddr("203.0.113.1"), NearTTL: 1, Protocol: 1, FirstHalfWord: 9},
	} {
		sendPD(t, pds, pd)
	}

	want := "198.51.100.9,24000,33434,4,udp\n198.51.100.9,24000,33434,5,udp\n" +
		"2001:db8::9,7,0,254,icmp6\n2001:db8::9,7,0,255,icmp6\n" +
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
}

// TestCaracalProber_SurvivesBadOutput checks that lines that are not replies,
// and a log line longer than the read buffer, neither stop the prober nor
// leave caracal waiting: a PD sent afterwards gets its FIE.
func TestCaracalProber_SurvivesBadOutput(t *testing.T) {
	script := `echo garbage; echo "1,2,3"; echo
head -c 300000 /dev/zero | tr '\\0' x >&2; echo >&2; echo "after" >&2
cat > /dev/null`
	pds, fies := runCaracalProber(t, testCaracalConfig(fakeCaracal(t, script)))

	time.Sleep(300 * time.Millisecond)
	sendPD(t, pds, testPDs[0])
	if fie := receiveFIEs(t, fies, 1)[0]; fie.PDID != 1 || fie.Near.IsValid() || fie.Far.IsValid() {
		t.Fatalf("got %+v, want the FIE of PD 1 without replies", fie)
	}
}

// testPDs are a UDP, an ICMP and an ICMPv6 PD. The mock caracal answers
// their probes from 10.0.0.<ttl> and 2001:db8::<ttl>.
var testPDs = []PD{
	{ID: 1, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434},
	{ID: 2, Destination: netip.MustParseAddr("203.0.113.1"), NearTTL: 9, Protocol: 1, FirstHalfWord: 24001, SecondHalfWord: 777},
	{ID: 3, Destination: netip.MustParseAddr("2001:db8::9"), NearTTL: 20, Protocol: 58, FirstHalfWord: 24002, SecondHalfWord: 778},
}

// TestCaracalProber_SendsCompleteFIEsAtOnce checks that a PD whose probes
// are both answered gets its FIE without waiting for the timeout.
func TestCaracalProber_SendsCompleteFIEsAtOnce(t *testing.T) {
	requireMockCaracal(t)
	config := testCaracalConfig(mockCaracal)
	config.ProbeTimeout = 4 * time.Second
	pds, fies := runCaracalProber(t, config)

	start := time.Now()
	for _, pd := range testPDs {
		sendPD(t, pds, pd)
	}
	received := map[uint32]FIE{}
	for _, fie := range receiveFIEs(t, fies, len(testPDs)) {
		received[fie.PDID] = fie
	}
	if elapsed := time.Since(start); elapsed >= config.ProbeTimeout {
		t.Errorf("the FIEs came after %v, not before the timeout of %v", elapsed, config.ProbeTimeout)
	}
	for id, want := range map[uint32][2]string{
		1: {"10.0.0.4", "10.0.0.5"},
		2: {"10.0.0.9", "10.0.0.10"},
		3: {"2001:db8::20", "2001:db8::21"},
	} {
		fie := received[id]
		if fie.Near != netip.MustParseAddr(want[0]) || fie.Far != netip.MustParseAddr(want[1]) || fie.CaptureUnix == 0 {
			t.Errorf("PD %d: got %+v, want replies from %s and %s", id, fie, want[0], want[1])
		}
	}
	noMoreFIEs(t, fies)
}

// TestCaracalProber_SendsIncompleteFIEsAtTimeout checks that a PD without
// replies gets its FIE once the timeout has passed, and not before.
func TestCaracalProber_SendsIncompleteFIEsAtTimeout(t *testing.T) {
	requireMockCaracal(t)
	t.Setenv("MOCK_CARACAL_REPLY_PERCENT", "0")
	config := testCaracalConfig(mockCaracal)
	pds, fies := runCaracalProber(t, config)

	start := time.Now()
	for _, pd := range testPDs {
		sendPD(t, pds, pd)
	}
	for _, fie := range receiveFIEs(t, fies, len(testPDs)) {
		if fie.Near.IsValid() || fie.Far.IsValid() {
			t.Errorf("PD %d: got %+v, want no replies", fie.PDID, fie)
		}
	}
	if elapsed := time.Since(start); elapsed < config.ProbeTimeout {
		t.Errorf("the FIEs came after %v, before the timeout of %v", elapsed, config.ProbeTimeout)
	}
	noMoreFIEs(t, fies)
}

// TestCaracalProber_SharedProbeAndRepeatedPD sends two PDs of one path, which
// share a probe, and one of them a second time: all three get a complete FIE.
func TestCaracalProber_SharedProbeAndRepeatedPD(t *testing.T) {
	requireMockCaracal(t)
	pds, fies := runCaracalProber(t, testCaracalConfig(mockCaracal))

	first := PD{ID: 1, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 5, Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434}
	second := first
	second.ID, second.NearTTL = 2, 6
	for _, pd := range []PD{first, second, first} {
		sendPD(t, pds, pd)
	}

	count := map[uint32]int{}
	for _, fie := range receiveFIEs(t, fies, 3) {
		count[fie.PDID]++
		want := [2]string{"10.0.0.5", "10.0.0.6"}
		if fie.PDID == 2 {
			want = [2]string{"10.0.0.6", "10.0.0.7"}
		}
		if fie.Near != netip.MustParseAddr(want[0]) || fie.Far != netip.MustParseAddr(want[1]) {
			t.Errorf("PD %d: got %+v, want replies from %s and %s", fie.PDID, fie, want[0], want[1])
		}
	}
	if count[1] != 2 || count[2] != 1 {
		t.Errorf("got %v FIEs per PD, want two for PD 1 and one for PD 2", count)
	}
	noMoreFIEs(t, fies)
}

// TestCaracalProber_FlushesFullBuffer sends more PDs at once than the write
// buffer holds: every one still gets its FIE.
func TestCaracalProber_FlushesFullBuffer(t *testing.T) {
	requireMockCaracal(t)
	config := testCaracalConfig(mockCaracal)
	config.WriteBufferSize = 256
	config.ProbeTimeout = 4 * time.Second
	pds, fies := runCaracalProber(t, config)

	const count = 200
	go func() {
		for id := uint32(1); id <= count; id++ {
			pds <- PD{ID: id, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 17, FirstHalfWord: uint16(id), SecondHalfWord: 33434}
		}
	}()
	seen := map[uint32]bool{}
	for _, fie := range receiveFIEs(t, fies, count) {
		if seen[fie.PDID] || !fie.Near.IsValid() || !fie.Far.IsValid() {
			t.Fatalf("got %+v, want one complete FIE per PD", fie)
		}
		seen[fie.PDID] = true
	}
}

// TestCaracalProber_Stats checks the counters of the stats log line, with a
// caracal that writes two lines that are not replies and answers nothing.
func TestCaracalProber_Stats(t *testing.T) {
	config := testCaracalConfig(fakeCaracal(t, `echo garbage; echo "1,2,3"; cat > /dev/null`))
	prober := NewCaracalProber(config, slog.New(slog.DiscardHandler))
	pds := make(chan PD)
	fies := make(chan FIE, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- prober.Run(ctx, pds, fies) }()
	defer func() {
		cancel()
		<-done
	}()

	// TCP is not a protocol caracal probes with.
	sendPD(t, pds, PD{ID: 9, Destination: netip.MustParseAddr("198.51.100.9"), NearTTL: 4, Protocol: 6})
	sendPD(t, pds, testPDs[0])
	receiveFIEs(t, fies, 1)

	got := map[string]uint64{}
	for _, attr := range prober.stats() {
		got[attr.Key] = attr.Value.Uint64()
	}
	want := map[string]uint64{
		"pds_probed":          1,
		"pds_unprobeable":     1,
		"pds_in_flight":       0,
		"replies_matched":     0,
		"replies_unmatched":   0,
		"replies_undecodable": 2,
		"fies_complete":       0,
		"fies_incomplete":     1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("got stats %v, want %v", got, want)
	}
}
