// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"net/netip"
	"sync"
	"time"
)

// caracalProbeKey is what tells one probe from another, and what a reply
// tells about the probe it answers.
type caracalProbeKey struct {
	destination [16]byte
	srcPort     uint16
	// dstPort is always zero for ICMP and ICMPv6 probes.
	dstPort  uint16
	ttl      uint8
	protocol uint8
}

// caracalDstPort returns the destination port probes are written and matched
// with: the given one for UDP, zero for ICMP and ICMPv6.
func caracalDstPort(protocol uint8, port uint16) uint16 {
	if protocol == 17 {
		return port
	}
	return 0
}

// caracalPDProbeKey returns the key of the probe a PD makes with the given
// TTL: its near TTL, or its near TTL plus one for the far probe.
func caracalPDProbeKey(pd *PD, ttl uint8) caracalProbeKey {
	return caracalProbeKey{
		destination: pd.Destination.As16(),
		srcPort:     pd.FirstHalfWord,
		dstPort:     caracalDstPort(pd.Protocol, pd.SecondHalfWord),
		ttl:         ttl,
		protocol:    pd.Protocol,
	}
}

// caracalReplyProbeKey returns the key of the probe a reply answers.
func caracalReplyProbeKey(reply *caracalReply) caracalProbeKey {
	return caracalProbeKey{
		destination: reply.Destination.As16(),
		srcPort:     reply.SrcPort,
		dstPort:     caracalDstPort(reply.Protocol, reply.DstPort),
		ttl:         reply.TTL,
		protocol:    reply.Protocol,
	}
}

// caracalRecord is one issuance of a PD: its two probes and the replies they
// got. A PD that is issued again while it is in flight gets a second record.
type caracalRecord struct {
	pdID uint32
	// sequence numbers the records in the order they were registered.
	sequence uint64
	nearKey  caracalProbeKey
	farKey   caracalProbeKey
	// lastFlushTime is when the record's probes were sent to caracal: after
	// the write, so not before caracal has them. Caracal may still hold
	// them for a while before they are on the wire. It is the zero time
	// until then.
	lastFlushTime time.Time
	// near and far are the addresses that answered the probes, the zero Addr
	// while there is no answer. nearCapture and farCapture are when the
	// answers were captured, in Unix microseconds.
	near, far               netip.Addr
	nearCapture, farCapture int64
	// done is set once the record's FIE is made.
	done bool
}

// caracalPDNode is a record that waits for the reply to a probe.
type caracalPDNode struct {
	record *caracalRecord
	// far tells that the probe is the record's far one.
	far bool
}

// answered reports whether the node's probe already has its reply.
func (n caracalPDNode) answered() bool {
	if n.far {
		return n.record.far.IsValid()
	}
	return n.record.near.IsValid()
}

// caracalTable gives, for a probe, the records that wait for its reply.
//
// Two PDs can make the same probe: the far probe of a PD with near TTL h is
// the near probe of a PD of the same flow with near TTL h+1. And a PD can be
// in flight more than once. So a probe has a list of records. A reply is
// given to one of them only: each record sent its own packet, and gets its
// own reply.
//
// The methods are safe to call from several goroutines.
type caracalTable struct {
	mu      sync.Mutex
	timeout time.Duration
	// sequence is the sequence number of the last record.
	sequence uint64
	nodes    map[caracalProbeKey][]caracalPDNode
	// flushed holds the records whose probes were sent to caracal, in the
	// order of their flush time, which is the order they expire in. Records
	// whose FIE is already made stay in it until their turn.
	flushed []*caracalRecord
}

func newCaracalTable(timeout time.Duration) *caracalTable {
	return &caracalTable{
		timeout: timeout,
		nodes:   make(map[caracalProbeKey][]caracalPDNode),
	}
}

// register records that a PD waits for the replies to its near and far
// probes. It is called before the probes are written, so that no reply comes
// before it. The record does not expire until markFlushed is called with it.
func (t *caracalTable) register(pd *PD) *caracalRecord {
	record := &caracalRecord{
		pdID:    pd.ID,
		nearKey: caracalPDProbeKey(pd, pd.NearTTL),
		farKey:  caracalPDProbeKey(pd, pd.NearTTL+1),
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sequence++
	record.sequence = t.sequence
	t.nodes[record.nearKey] = append(t.nodes[record.nearKey], caracalPDNode{record: record})
	t.nodes[record.farKey] = append(t.nodes[record.farKey], caracalPDNode{record: record, far: true})
	return record
}

// markFlushed sets the flush time of the records whose probes were just sent
// to caracal. From then on they expire after the timeout.
func (t *caracalTable) markFlushed(records []*caracalRecord, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, record := range records {
		record.lastFlushTime = now
	}
	t.flushed = append(t.flushed, records...)
}

// match gives a reply to one of the records that wait for it. When that
// completes the record, both of its probes being answered, match takes the
// record out of the table and returns its FIE.
//
// Among the records that still wait for this reply and have not expired, the
// one with the earliest flush time is chosen, then the one with the lowest
// near TTL, then the one registered first.
func (t *caracalTable) match(reply *caracalReply, now time.Time) (FIE, bool) {
	key := caracalReplyProbeKey(reply)

	t.mu.Lock()
	defer t.mu.Unlock()

	var chosen *caracalPDNode
	nodes := t.nodes[key]
	for i := range nodes {
		node := &nodes[i]
		if node.answered() || t.expired(node.record, now) {
			continue
		}
		if chosen == nil || waitsBefore(node, chosen) {
			chosen = node
		}
	}
	if chosen == nil {
		return FIE{}, false
	}

	record := chosen.record
	if chosen.far {
		record.far, record.farCapture = reply.Address, reply.CaptureMicros
	} else {
		record.near, record.nearCapture = reply.Address, reply.CaptureMicros
	}
	if !record.near.IsValid() || !record.far.IsValid() {
		return FIE{}, false
	}
	return t.finish(record, now), true
}

// waitsBefore reports whether node a is served before node b.
func waitsBefore(a, b *caracalPDNode) bool {
	// A record that is not flushed yet was written after every flushed one.
	aFlush, bFlush := a.record.lastFlushTime, b.record.lastFlushTime
	if aFlush.IsZero() != bFlush.IsZero() {
		return bFlush.IsZero()
	}
	if !aFlush.Equal(bFlush) {
		return aFlush.Before(bFlush)
	}
	// The probe is the far one of the record with the lower near TTL.
	if a.far != b.far {
		return a.far
	}
	return a.record.sequence < b.record.sequence
}

// expired reports whether the timeout of a record has passed.
func (t *caracalTable) expired(record *caracalRecord, now time.Time) bool {
	return !record.lastFlushTime.IsZero() && now.Sub(record.lastFlushTime) >= t.timeout
}

// expire takes the records whose timeout has passed out of the table and
// appends their FIEs to fies. These FIEs miss one reply or both: the FIE of
// a record with both replies was already returned by match.
func (t *caracalTable) expire(now time.Time, fies []FIE) []FIE {
	t.mu.Lock()
	defer t.mu.Unlock()

	count := 0
	for _, record := range t.flushed {
		if !t.expired(record, now) {
			break
		}
		if !record.done {
			fies = append(fies, t.finish(record, now))
		}
		count++
	}
	// The slice is reused: what is left moves to its start.
	clear(t.flushed[copy(t.flushed, t.flushed[count:]):])
	t.flushed = t.flushed[:len(t.flushed)-count]
	return fies
}

// finish takes a record out of the probe lists and returns its FIE.
func (t *caracalTable) finish(record *caracalRecord, now time.Time) FIE {
	record.done = true
	t.remove(record.nearKey, record)
	t.remove(record.farKey, record)

	fie := FIE{PDID: record.pdID, CaptureUnix: now.Unix()}
	if record.near.IsValid() {
		fie.Near, fie.NearDelta = record.near, secondsBefore(fie.CaptureUnix, record.nearCapture)
	}
	if record.far.IsValid() {
		fie.Far, fie.FarDelta = record.far, secondsBefore(fie.CaptureUnix, record.farCapture)
	}
	return fie
}

// remove takes a record out of the list of a probe.
func (t *caracalTable) remove(key caracalProbeKey, record *caracalRecord) {
	nodes := t.nodes[key]
	for i := range nodes {
		if nodes[i].record == record {
			nodes = append(nodes[:i], nodes[i+1:]...)
			break
		}
	}
	if len(nodes) == 0 {
		delete(t.nodes, key)
		return
	}
	t.nodes[key] = nodes
}

// secondsBefore returns how many seconds a time in Unix microseconds is
// before a time in Unix seconds, or zero when it is not before.
func secondsBefore(unix, micros int64) uint32 {
	delta := unix - micros/1_000_000
	if delta < 0 || delta > int64(^uint32(0)) {
		return 0
	}
	return uint32(delta)
}
