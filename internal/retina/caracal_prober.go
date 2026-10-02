// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// caracalHeader is the first line caracal writes to its output. The columns
// are those of caracal v0.15.4.
const caracalHeader = "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"

// caracalReadBufferSize is the size in bytes of the buffer caracal's output
// is read through.
const caracalReadBufferSize = 64 * 1024

// caracalIdleTime is how long the reader of caracal's output must have been
// waiting for a reply before it is taken that no reply is waiting to be read.
const caracalIdleTime = 10 * time.Millisecond

// caracalExpiryPeriod is how often the PDs in flight are checked for their
// timeout. A FIE that misses a reply is sent at most this long after the
// timeout.
const caracalExpiryPeriod = 100 * time.Millisecond

// caracalFixedArgs are the options caracal is always started with. They are
// the defaults of caracal v0.15.4, passed all the same so that a later
// caracal with other defaults behaves as this one: one packet per probe,
// which the matching of replies relies on, one second of capture after the
// input ends, the round column at 1, and TTL filters that let every TTL
// through.
//
// The rest of caracal's options cannot be given their default and are left
// out: caracal probes from the default interface and its addresses, with a
// random caracal ID, without a limit on the number of probes or a prefix
// filter, and drops the replies that fail its integrity check.
var caracalFixedArgs = []string{
	"--n-packets", "1",
	"--sniffer-wait-time", "1",
	"--meta-round", "1",
	"--filter-min-ttl", "0",
	"--filter-max-ttl", "255",
}

// CaracalProberConfig configures the caracal prober. The first group of
// fields are caracal's own options, named after them; a zero value leaves
// the option out, so that caracal uses its default. Caracal is also given
// caracalFixedArgs, and its rate, which follows from the prober's max PD
// rate.
type CaracalProberConfig struct {
	// Path is the caracal executable. A name without a slash is looked up in
	// PATH.
	Path string `json:"path"`

	// BatchSize is the number of packets sent between two checks of the rate
	// (--batch-size).
	BatchSize int `json:"batch_size"`
	// LogLevel is caracal's minimum log level: trace, debug, info, warning,
	// error or fatal (--log-level).
	LogLevel string `json:"log_level"`
	// RateLimitingMethod is how caracal limits its rate: auto, active, sleep
	// or none (--rate-limiting-method).
	RateLimitingMethod string `json:"rate_limiting_method"`

	// ProbeTimeout is how long the replies to a PD's probes are waited for,
	// from when the probes are sent to caracal to when caracal captures the
	// replies. It relies on caracal's capture timestamps and the agent's
	// clock being the same clock. A PD whose two probes are
	// answered gets its FIE at once; the others get theirs, with the replies
	// that came, once the timeout has passed.
	ProbeTimeout time.Duration `json:"probe_timeout"`
	// WriteBufferSize is the size in bytes of the buffer probes are written
	// to before they are sent to caracal. The buffer is sent when it is full
	// and whenever no PD is waiting.
	WriteBufferSize int `json:"write_buffer_size"`
	// StopTimeout is how long caracal is given to exit once it has been
	// killed, before its output is abandoned.
	StopTimeout time.Duration `json:"stop_timeout"`
}

func (c *CaracalProberConfig) validate() error {
	if c.Path == "" {
		return fmt.Errorf("path cannot be empty")
	}
	if c.BatchSize < 0 {
		return fmt.Errorf("batch size cannot be negative: got %d", c.BatchSize)
	}
	if c.ProbeTimeout <= 0 {
		return fmt.Errorf("probe timeout must be positive: got %v", c.ProbeTimeout)
	}
	if c.WriteBufferSize < 1 {
		return fmt.Errorf("write buffer size must be at least 1: got %d", c.WriteBufferSize)
	}
	if c.StopTimeout <= 0 {
		return fmt.Errorf("stop timeout must be positive: got %v", c.StopTimeout)
	}
	return nil
}

// args returns the arguments caracal is started with, for an agent that
// hands it at most maxPDRate PDs per second.
//
// A PD makes two packets. Caracal's own rate (--probing-rate) is set a tenth
// above what the agent hands it, so that caracal never holds probes back in
// normal operation, and only spreads out the PDs that reach it in a burst.
func (c *CaracalProberConfig) args(maxPDRate int) []string {
	var args []string
	text := func(option, value string) {
		if value != "" {
			args = append(args, option, value)
		}
	}
	number := func(option string, value int) {
		if value != 0 {
			args = append(args, option, strconv.Itoa(value))
		}
	}
	number("--probing-rate", 2*maxPDRate*11/10)
	number("--batch-size", c.BatchSize)
	text("--log-level", c.LogLevel)
	text("--rate-limiting-method", c.RateLimitingMethod)
	return append(args, caracalFixedArgs...)
}

// CaracalProber is a prober that sends its probes with a caracal process.
//
// Caracal reads one probe per line on its standard input, as
// dst_addr,src_port,dst_port,ttl,protocol, and sends it at its own probing
// rate. It writes one line per reply it captures to its standard output, and
// its logs to its standard error. A reply does not say which line it answers:
// it carries the probe's destination, ports and TTL, read back from the
// packet the reply quotes.
//
// The prober registers the probes it writes in a table, which gives the PDs
// a reply answers. A PD's FIE is sent as soon as both of its probes are
// answered. A separate loop looks for the PDs whose timeout has passed, and
// sends their FIEs with the replies that came.
type CaracalProber struct {
	config    *CaracalProberConfig
	maxPDRate int
	logger    *slog.Logger
	table     *caracalTable
	// pdsUnprobeable counts the PDs that were not probed, and
	// repliesUndecodable the lines of caracal's output that were not replies.
	pdsUnprobeable     atomic.Uint64
	repliesUndecodable atomic.Uint64
	// idleSince is when the reader of caracal's output started to wait for
	// its next line with nothing left to read, in Unix nanoseconds. It is
	// zero while the reader is busy: decoding replies, or waiting for room in
	// the FIE queue.
	idleSince atomic.Int64
}

// NewCaracalProber creates a caracal prober for an agent that hands it at
// most maxPDRate PDs per second. The caracal process is started by Run.
func NewCaracalProber(config *CaracalProberConfig, maxPDRate int, logger *slog.Logger) *CaracalProber {
	return &CaracalProber{
		config:    config,
		maxPDRate: maxPDRate,
		logger:    logger,
		table:     newCaracalTable(config.ProbeTimeout),
	}
}

// Run implements Prober. It starts caracal and returns an error when caracal
// stops on its own.
func (p *CaracalProber) Run(ctx context.Context, pds <-chan PD, fies chan<- FIE) error {
	// Canceling the command's context kills caracal.
	group, groupCtx := errgroup.WithContext(ctx)
	cmd := exec.CommandContext(groupCtx, p.config.Path, p.config.args(p.maxPDRate)...) //nolint:gosec // G204: the path is the operator's configuration
	cmd.WaitDelay = p.config.StopTimeout

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("cannot open caracal input: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cannot open caracal output: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("cannot open caracal logs: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start caracal: %w", err)
	}
	p.logger.Info("Caracal started", slog.Int("pid", cmd.Process.Pid))

	// A process caracal has started may keep the pipes open after caracal
	// is killed: closing them is what unblocks the reads.
	stop := context.AfterFunc(groupCtx, func() {
		_ = stdout.Close()
		_ = stderr.Close()
	})
	defer stop()

	group.Go(func() error { return p.logOutput(stderr) })
	group.Go(func() error { return p.readReplies(groupCtx, stdout, fies) })
	group.Go(func() error { return p.writeProbes(groupCtx, stdin, pds) })
	group.Go(func() error { return p.expirePDs(groupCtx, fies) })

	err = group.Wait()
	// Wait only once the pipes are no longer read.
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("caracal stopped: %w", errors.Join(err, waitErr))
}

// stats implements statsProber.
func (p *CaracalProber) stats() []slog.Attr {
	table := p.table.snapshot()
	return []slog.Attr{
		slog.Uint64("pds_probed", table.registered),
		slog.Uint64("pds_unprobeable", p.pdsUnprobeable.Load()),
		slog.Uint64("pds_in_flight", table.inFlight()),
		slog.Uint64("replies_matched", table.matched),
		slog.Uint64("replies_unmatched", table.unmatched),
		slog.Uint64("replies_undecodable", p.repliesUndecodable.Load()),
		slog.Uint64("fies_complete", table.complete),
		slog.Uint64("fies_incomplete", table.incomplete),
	}
}

// writeProbes registers every PD and writes its near and far probe to
// caracal. It returns when ctx is done or caracal no longer takes probes.
func (p *CaracalProber) writeProbes(ctx context.Context, stdin io.WriteCloser, pds <-chan PD) error {
	defer func() { _ = stdin.Close() }()
	writer := bufio.NewWriterSize(stdin, p.config.WriteBufferSize)
	var line []byte
	// unflushed are the records whose probes are still in the buffer.
	var unflushed []*caracalRecord

	// flush sends the buffer to caracal. The flush time of the records is
	// taken after the write: caracal has their probes by then.
	flush := func() error {
		if err := writer.Flush(); err != nil {
			return fmt.Errorf("cannot write probes: %w", err)
		}
		p.table.markFlushed(unflushed, time.Now())
		unflushed = unflushed[:0]
		return nil
	}

	for {
		select {
		case pd := <-pds:
			protocol, ok := caracalProtocol(pd.Protocol)
			if !ok || pd.NearTTL == 255 {
				p.pdsUnprobeable.Add(1)
				p.logger.Warn("Cannot probe PD", slog.Uint64("pd_id", uint64(pd.ID)), slog.Int("protocol", int(pd.Protocol)), slog.Int("near_ttl", int(pd.NearTTL)))
				continue
			}
			line = appendCaracalProbe(line[:0], &pd, pd.NearTTL, protocol)
			line = appendCaracalProbe(line, &pd, pd.NearTTL+1, protocol)
			// The buffer is only ever sent by flush, so that every record
			// gets its flush time.
			if writer.Available() < len(line) {
				if err := flush(); err != nil {
					return err
				}
			}
			// The probes are registered before they are written, so that
			// no reply comes before them.
			unflushed = append(unflushed, p.table.register(&pd))
			if _, err := writer.Write(line); err != nil {
				return fmt.Errorf("cannot write probes: %w", err)
			}
			// Probes are sent in groups while PDs keep coming, and at once
			// when none is waiting.
			if len(pds) == 0 {
				if err := flush(); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// readLine reads the next line of caracal's output. While it waits with
// nothing left in the buffer, the reader counts as idle.
func (p *CaracalProber) readLine(reader *bufio.Reader) ([]byte, error) {
	if reader.Buffered() == 0 {
		p.idleSince.Store(time.Now().UnixNano())
		defer p.idleSince.Store(0)
	}
	return reader.ReadSlice('\n')
}

// readerIdle reports whether no reply is waiting to be read: the reader has
// been waiting for caracal's next line for the idle time. A reader that only
// just started to wait may be about to find replies in the pipe.
func (p *CaracalProber) readerIdle(now time.Time) bool {
	since := p.idleSince.Load()
	return since != 0 && now.UnixNano()-since >= int64(caracalIdleTime)
}

// readReplies reads the replies caracal captures and gives each to a PD that
// waits for it. It sends the FIE of a PD once both of its probes are
// answered. It returns an error when caracal's output ends, which it only
// does when caracal stops.
func (p *CaracalProber) readReplies(ctx context.Context, stdout io.Reader, fies chan<- FIE) error {
	reader := bufio.NewReaderSize(stdout, caracalReadBufferSize)

	header, err := p.readLine(reader)
	if err != nil {
		return fmt.Errorf("cannot read caracal header: %w", err)
	}
	if string(bytes.TrimSpace(header)) != caracalHeader {
		return fmt.Errorf("unexpected caracal header %q: this agent expects the output of caracal v0.15.4", header)
	}

	for {
		line, err := p.readLine(reader)
		if err != nil {
			return fmt.Errorf("cannot read caracal replies: %w", err)
		}
		reply, err := parseCaracalReply(line)
		if err != nil {
			p.repliesUndecodable.Add(1)
			p.logger.Warn("Cannot decode caracal reply", slog.Any("err", err))
			continue
		}
		if fie, complete := p.table.match(&reply, time.Now()); complete {
			select {
			case fies <- fie:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// expirePDs looks every expiry period for the PDs whose timeout has passed,
// and sends their FIEs, which miss one reply or both. PDs whose replies may
// still be waiting to be read are left for a later round: see
// caracalTable.expire. It returns when ctx is done.
func (p *CaracalProber) expirePDs(ctx context.Context, fies chan<- FIE) error {
	ticker := time.NewTicker(caracalExpiryPeriod)
	defer ticker.Stop()
	var expired []FIE

	for {
		select {
		case now := <-ticker.C:
			expired = p.table.expire(now, p.readerIdle(now), expired[:0])
			for i := range expired {
				select {
				case fies <- expired[i]:
				case <-ctx.Done():
					return nil
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// logOutput logs what caracal writes to its standard error, until it ends.
// It reports no error: caracal stopping is noticed where its replies are
// read. A line longer than the read buffer is logged in pieces, so that
// caracal is never left waiting for its logs to be read.
func (p *CaracalProber) logOutput(stderr io.Reader) error {
	reader := bufio.NewReader(stderr)
	for {
		line, err := reader.ReadSlice('\n')
		if text := bytes.TrimRight(line, "\r\n"); len(text) > 0 {
			p.logger.Info(string(text), slog.String("source", "caracal"))
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil
		}
	}
}

// caracalProtocol returns caracal's name for an IP protocol number.
func caracalProtocol(protocol uint8) (string, bool) {
	switch protocol {
	case 1:
		return "icmp", true
	case 17:
		return "udp", true
	case 58:
		return "icmp6", true
	default:
		return "", false
	}
}

// appendCaracalProbe appends the line that makes caracal send one probe of a
// PD with the given TTL: dst_addr,src_port,dst_port,ttl,protocol.
//
// For ICMP and ICMPv6, dst_port is written as zero: it is not what tells
// these probes apart.
func appendCaracalProbe(line []byte, pd *PD, ttl uint8, protocol string) []byte {
	line = pd.Destination.AppendTo(line)
	line = append(line, ',')
	line = strconv.AppendUint(line, uint64(pd.FirstHalfWord), 10)
	line = append(line, ',')
	line = strconv.AppendUint(line, uint64(caracalDstPort(pd.Protocol, pd.SecondHalfWord)), 10)
	line = append(line, ',')
	line = strconv.AppendUint(line, uint64(ttl), 10)
	line = append(line, ',')
	line = append(line, protocol...)
	return append(line, '\n')
}

// caracalReply is one reply captured by caracal, reduced to what identifies
// the probe it answers and what a FIE reports.
type caracalReply struct {
	// CaptureMicros is when the reply was captured, in Unix microseconds.
	CaptureMicros int64
	// Protocol, Destination, SrcPort, DstPort and TTL are those of the probe.
	// DstPort is always zero for ICMP and ICMPv6 probes. For an echo reply,
	// Destination is the address that replied.
	Protocol    uint8
	Destination netip.Addr
	SrcPort     uint16
	DstPort     uint16
	TTL         uint8
	// Address is the address the reply came from.
	Address netip.Addr
	// ICMPType and ICMPCode are those of the reply.
	ICMPType uint8
	ICMPCode uint8
	// RTT is the round-trip time in tenths of a millisecond. Caracal
	// computes it from 16 bits of the send time, so it wraps around after
	// 6.5 seconds.
	RTT uint16
}

// parseCaracalReply decodes one line of caracal's output. Addresses are
// written as IPv6, IPv4 ones as ::ffff:a.b.c.d, and are returned unmapped.
func parseCaracalReply(line []byte) (caracalReply, error) {
	line = bytes.TrimSpace(line)

	// The first 12 columns have no quotes. The MPLS labels column further on
	// is quoted and may hold commas, so rtt is taken from the end.
	var fields [12][]byte
	rest := line
	for i := range fields {
		field, after, found := bytes.Cut(rest, []byte{','})
		if !found {
			return caracalReply{}, fmt.Errorf("reply %q: too few columns", line)
		}
		fields[i], rest = field, after
	}
	round := bytes.LastIndexByte(rest, ',')
	if round < 0 {
		return caracalReply{}, fmt.Errorf("reply %q: too few columns", line)
	}
	rtt := bytes.LastIndexByte(rest[:round], ',')
	if rtt < 0 {
		return caracalReply{}, fmt.Errorf("reply %q: too few columns", line)
	}

	invalid := func(name string) (caracalReply, error) {
		return caracalReply{}, fmt.Errorf("reply %q: invalid %s", line, name)
	}

	capture, err := strconv.ParseInt(string(fields[0]), 10, 64)
	if err != nil {
		return invalid("capture timestamp")
	}
	protocol, err := strconv.ParseUint(string(fields[1]), 10, 8)
	if err != nil {
		return invalid("probe protocol")
	}
	destination, err := netip.ParseAddr(string(fields[3]))
	if err != nil {
		return invalid("probe destination")
	}
	srcPort, err := strconv.ParseUint(string(fields[4]), 10, 16)
	if err != nil {
		return invalid("probe source port")
	}
	dstPort, err := strconv.ParseUint(string(fields[5]), 10, 16)
	if err != nil {
		return invalid("probe destination port")
	}
	ttl, err := strconv.ParseUint(string(fields[6]), 10, 8)
	if err != nil {
		return invalid("probe TTL")
	}
	address, err := netip.ParseAddr(string(fields[8]))
	if err != nil {
		return invalid("reply address")
	}
	icmpType, err := strconv.ParseUint(string(fields[10]), 10, 8)
	if err != nil {
		return invalid("ICMP type")
	}
	icmpCode, err := strconv.ParseUint(string(fields[11]), 10, 8)
	if err != nil {
		return invalid("ICMP code")
	}
	roundTrip, err := strconv.ParseUint(string(rest[rtt+1:round]), 10, 16)
	if err != nil {
		return invalid("rtt")
	}

	return caracalReply{
		CaptureMicros: capture,
		Protocol:      uint8(protocol),
		Destination:   destination.Unmap(),
		SrcPort:       uint16(srcPort),
		DstPort:       uint16(dstPort),
		TTL:           uint8(ttl),
		Address:       address.Unmap(),
		ICMPType:      uint8(icmpType),
		ICMPCode:      uint8(icmpCode),
		RTT:           uint16(roundTrip),
	}, nil
}
