// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"net/netip"
	"testing"
	"time"
)

var (
	tableDestination = netip.MustParseAddr("198.51.100.9")
	tableStart       = time.Unix(1_000_000, 0)
)

// tablePD returns a UDP PD of one flow.
func tablePD(id uint32, nearTTL uint8) *PD {
	return &PD{ID: id, Destination: tableDestination, NearTTL: nearTTL, Protocol: 17, FirstHalfWord: 24000, SecondHalfWord: 33434}
}

// tableReply returns a reply to the probe of that flow with the given TTL.
// The address it comes from tells the replies to one probe apart.
func tableReply(ttl, from uint8) *caracalReply {
	return &caracalReply{
		CaptureMicros: tableStart.UnixMicro(),
		Protocol:      17,
		Destination:   tableDestination,
		SrcPort:       24000,
		DstPort:       33434,
		TTL:           ttl,
		Address:       netip.AddrFrom4([4]byte{10, 0, from, ttl}),
	}
}

// issue registers a PD and flushes it at the given time after tableStart.
func issue(table *caracalTable, pd *PD, after time.Duration) {
	table.markFlushed([]*caracalRecord{table.register(pd)}, tableStart.Add(after))
}

// checkEmpty checks that nothing is left in the table.
func checkEmpty(t *testing.T, table *caracalTable) {
	t.Helper()
	if len(table.nodes) != 0 || len(table.flushed) != 0 {
		t.Errorf("the table still has %d probes and %d records", len(table.nodes), len(table.flushed))
	}
}

func TestCaracalTable_CompletesWithBothReplies(t *testing.T) {
	table := newCaracalTable(2 * time.Second)
	issue(table, tablePD(1, 4), 0)
	now := tableStart.Add(time.Second)

	if _, complete := table.match(tableReply(4, 0), now); complete {
		t.Fatal("the PD was complete after one reply")
	}
	// A reply to a probe nobody waits for, and a second reply to the near
	// probe, which is already answered.
	for _, reply := range []*caracalReply{tableReply(9, 0), tableReply(4, 1)} {
		if _, complete := table.match(reply, now); complete {
			t.Fatal("a reply nobody waits for completed a PD")
		}
	}

	fie, complete := table.match(tableReply(5, 0), now)
	want := FIE{
		PDID: 1, CaptureUnix: now.Unix(),
		Near: netip.MustParseAddr("10.0.0.4"), NearDelta: 1,
		Far: netip.MustParseAddr("10.0.0.5"), FarDelta: 1,
	}
	if !complete || fie != want {
		t.Fatalf("got %+v (complete %v), want %+v", fie, complete, want)
	}

	// The record waits for nothing more, and its timeout brings no FIE.
	if len(table.nodes) != 0 {
		t.Errorf("%d probes are still waited for", len(table.nodes))
	}
	if fies := table.expire(tableStart.Add(2*time.Second), nil); len(fies) != 0 {
		t.Errorf("got %+v at the timeout of a complete PD", fies)
	}
	checkEmpty(t, table)
}

func TestCaracalTable_ExpiresIncompletePDs(t *testing.T) {
	table := newCaracalTable(2 * time.Second)
	issue(table, tablePD(1, 4), 0)
	issue(table, tablePD(2, 10), 0)
	issue(table, tablePD(3, 20), time.Second)
	table.match(tableReply(11, 0), tableStart)

	if fies := table.expire(tableStart.Add(1999*time.Millisecond), nil); len(fies) != 0 {
		t.Fatalf("got %+v before the timeout", fies)
	}
	fies := table.expire(tableStart.Add(2*time.Second), nil)
	if len(fies) != 2 {
		t.Fatalf("got %+v, want the FIEs of PDs 1 and 2", fies)
	}
	if fies[0].PDID != 1 || fies[0].Near.IsValid() || fies[0].Far.IsValid() {
		t.Errorf("PD 1: got %+v, want no replies", fies[0])
	}
	if fies[1].PDID != 2 || fies[1].Near.IsValid() || fies[1].Far != netip.MustParseAddr("10.0.0.11") {
		t.Errorf("PD 2: got %+v, want only a far reply", fies[1])
	}

	// The third PD was flushed a second later.
	if fies := table.expire(tableStart.Add(3*time.Second), nil); len(fies) != 1 || fies[0].PDID != 3 {
		t.Fatalf("got %+v, want the FIE of PD 3", fies)
	}
	checkEmpty(t, table)
}

// TestCaracalTable_OneReplyOnePD issues two PDs of one path: the far probe
// of the first is the near probe of the second. Each of the two replies to
// that probe goes to one PD.
func TestCaracalTable_OneReplyOnePD(t *testing.T) {
	table := newCaracalTable(2 * time.Second)
	// Both are flushed together, so the lower near TTL is served first.
	second := table.register(tablePD(2, 6))
	first := table.register(tablePD(1, 5))
	table.markFlushed([]*caracalRecord{second, first}, tableStart)

	table.match(tableReply(5, 0), tableStart)
	fie, complete := table.match(tableReply(6, 1), tableStart)
	if !complete || fie.PDID != 1 || fie.Far != netip.MustParseAddr("10.0.1.6") {
		t.Fatalf("got %+v (complete %v), want PD 1 completed by the first reply", fie, complete)
	}
	if _, complete := table.match(tableReply(6, 2), tableStart); complete {
		t.Fatal("PD 2 was complete without its far reply")
	}
	fie, complete = table.match(tableReply(7, 0), tableStart)
	if !complete || fie.PDID != 2 || fie.Near != netip.MustParseAddr("10.0.2.6") {
		t.Fatalf("got %+v (complete %v), want PD 2 with the second reply as its near one", fie, complete)
	}

	table.expire(tableStart.Add(2*time.Second), nil)
	checkEmpty(t, table)
}

// TestCaracalTable_LostReplyLeavesOnePDIncomplete loses one of the two
// replies to a shared probe: only one PD gets it.
func TestCaracalTable_LostReplyLeavesOnePDIncomplete(t *testing.T) {
	table := newCaracalTable(2 * time.Second)
	issue(table, tablePD(1, 5), 0)
	issue(table, tablePD(2, 6), 0)
	for _, ttl := range []uint8{5, 6, 7} {
		table.match(tableReply(ttl, 0), tableStart)
	}

	fies := table.expire(tableStart.Add(2*time.Second), nil)
	if len(fies) != 1 || fies[0].PDID != 2 || fies[0].Near.IsValid() || !fies[0].Far.IsValid() {
		t.Fatalf("got %+v, want PD 2 with only its far reply", fies)
	}
	checkEmpty(t, table)
}

// TestCaracalTable_SamePDTwice issues a PD again while it is in flight: each
// issuance has its own record and gets its own replies, the earlier first.
func TestCaracalTable_SamePDTwice(t *testing.T) {
	table := newCaracalTable(2 * time.Second)
	issue(table, tablePD(1, 4), 0)
	issue(table, tablePD(1, 4), time.Second)
	now := tableStart.Add(time.Second)

	table.match(tableReply(4, 1), now)
	fie, complete := table.match(tableReply(5, 1), now)
	if !complete || fie.PDID != 1 || fie.Near != netip.MustParseAddr("10.0.1.4") {
		t.Fatalf("got %+v (complete %v), want the first issuance complete", fie, complete)
	}
	table.match(tableReply(4, 2), now)
	fie, complete = table.match(tableReply(5, 2), now)
	if !complete || fie.PDID != 1 || fie.Near != netip.MustParseAddr("10.0.2.4") {
		t.Fatalf("got %+v (complete %v), want the second issuance complete", fie, complete)
	}

	table.expire(tableStart.Add(3*time.Second), nil)
	checkEmpty(t, table)
}

// TestCaracalTable_SkipsExpiredRecords gives a reply that comes after the
// timeout of an earlier issuance to the later one.
func TestCaracalTable_SkipsExpiredRecords(t *testing.T) {
	table := newCaracalTable(2 * time.Second)
	issue(table, tablePD(1, 4), 0)
	issue(table, tablePD(2, 4), 1500*time.Millisecond)

	// The first has expired but is not removed yet.
	now := tableStart.Add(2100 * time.Millisecond)
	table.match(tableReply(4, 0), now)
	if fie, complete := table.match(tableReply(5, 0), now); !complete || fie.PDID != 2 {
		t.Fatalf("got %+v (complete %v), want PD 2 complete", fie, complete)
	}
	if fies := table.expire(now, nil); len(fies) != 1 || fies[0].PDID != 1 || fies[0].Near.IsValid() {
		t.Fatalf("got %+v, want PD 1 without replies", fies)
	}
}

// TestCaracalTable_UnflushedRecord checks that a record whose probes are
// still in the write buffer can be answered, does not expire, and is served
// after the flushed ones.
func TestCaracalTable_UnflushedRecord(t *testing.T) {
	table := newCaracalTable(2 * time.Second)
	unflushed := table.register(tablePD(1, 4))
	issue(table, tablePD(2, 4), 0)

	if fies := table.expire(tableStart.Add(time.Hour), nil); len(fies) != 1 || fies[0].PDID != 2 {
		t.Fatalf("got %+v, want only the flushed PD 2 to expire", fies)
	}

	issue(table, tablePD(3, 4), time.Hour)
	now := tableStart.Add(time.Hour)
	table.match(tableReply(4, 0), now)
	if fie, complete := table.match(tableReply(5, 0), now); !complete || fie.PDID != 3 {
		t.Fatalf("got %+v (complete %v), want the flushed PD 3 served first", fie, complete)
	}
	table.match(tableReply(4, 0), now)
	if fie, complete := table.match(tableReply(5, 0), now); !complete || fie.PDID != 1 {
		t.Fatalf("got %+v (complete %v), want the unflushed PD 1 complete", fie, complete)
	}

	// Its flush comes after its FIE: it then leaves the table at its timeout.
	table.markFlushed([]*caracalRecord{unflushed}, now)
	if fies := table.expire(now.Add(2*time.Second), nil); len(fies) != 0 {
		t.Fatalf("got %+v for records that are complete", fies)
	}
	checkEmpty(t, table)
}

// TestCaracalTable_ICMPDestinationPort checks that the destination port
// plays no part for ICMP and ICMPv6, and tells UDP probes apart.
func TestCaracalTable_ICMPDestinationPort(t *testing.T) {
	for _, protocol := range []uint8{1, 58} {
		for _, replyPort := range []uint16{0, 777} {
			table := newCaracalTable(time.Second)
			pd := tablePD(1, 4)
			pd.Protocol, pd.SecondHalfWord = protocol, 777
			issue(table, pd, 0)

			for ttl := uint8(4); ttl <= 5; ttl++ {
				reply := tableReply(ttl, 0)
				reply.Protocol, reply.DstPort = protocol, replyPort
				if _, complete := table.match(reply, tableStart); complete != (ttl == 5) {
					t.Errorf("protocol %d, reply port %d, TTL %d: complete is %v", protocol, replyPort, ttl, complete)
				}
			}
		}
	}

	table := newCaracalTable(time.Second)
	issue(table, tablePD(1, 4), 0)
	for ttl := uint8(4); ttl <= 5; ttl++ {
		reply := tableReply(ttl, 0)
		reply.DstPort++
		if _, complete := table.match(reply, tableStart); complete {
			t.Error("UDP replies with another destination port completed the PD")
		}
	}
}

// TestCaracalTable_ManyRounds issues and completes or expires PDs many times
// over, and checks that the table does not keep anything.
func TestCaracalTable_ManyRounds(t *testing.T) {
	table := newCaracalTable(time.Second)
	now := tableStart
	fies := 0
	for round := range 50 {
		after := now.Sub(tableStart)
		for id := uint32(1); id <= 20; id++ {
			issue(table, tablePD(id, uint8(id*2)), after)
		}
		// Every other round, the PDs are answered.
		if round%2 == 0 {
			for id := uint8(1); id <= 20; id++ {
				table.match(tableReply(id*2, 0), now)
				if _, complete := table.match(tableReply(id*2+1, 0), now); complete {
					fies++
				}
			}
		}
		now = now.Add(time.Second)
		fies += len(table.expire(now, nil))
		checkEmpty(t, table)
	}
	if fies != 50*20 {
		t.Errorf("got %d FIEs, want %d", fies, 50*20)
	}
}

func TestCaracalTable_Stats(t *testing.T) {
	table := newCaracalTable(2 * time.Second)

	// PD 1 gets both replies, PD 2 only its near one, and one reply is for
	// no PD.
	issue(table, tablePD(1, 4), 0)
	issue(table, tablePD(2, 9), 0)
	at := tableStart.Add(time.Second)
	table.match(tableReply(4, 1), at)
	table.match(tableReply(5, 2), at)
	table.match(tableReply(9, 3), at)
	table.match(tableReply(20, 4), at)

	want := caracalTableStats{registered: 2, matched: 3, unmatched: 1, complete: 1}
	if got := table.snapshot(); got != want || got.inFlight() != 1 {
		t.Fatalf("got %+v with %d in flight, want %+v with 1 in flight", got, got.inFlight(), want)
	}

	table.expire(tableStart.Add(3*time.Second), nil)
	want.incomplete = 1
	if got := table.snapshot(); got != want || got.inFlight() != 0 {
		t.Fatalf("after expiry: got %+v with %d in flight, want %+v with none", got, got.inFlight(), want)
	}
}
