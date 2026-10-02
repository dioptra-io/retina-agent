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
	"time"

	"golang.org/x/sync/errgroup"
)

// caracalHeader is the first line caracal writes to its output. The columns
// are those of caracal v0.15.4.
const caracalHeader = "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"

// caracalReadBufferSize is the size in bytes of the buffer caracal's output
// is read through.
const caracalReadBufferSize = 64 * 1024

// CaracalProberConfig configures the caracal prober.
type CaracalProberConfig struct {
	// Path is the caracal executable. A name without a slash is looked up in
	// PATH.
	Path string `json:"path"`
	// Args are the arguments caracal is started with, such as
	// "--probing-rate", "20000". Caracal's own default rate is 100 packets
	// per second.
	Args []string `json:"args"`
	// WriteBufferSize is the size in bytes of the buffer probes are written
	// to before they are sent to caracal. The buffer is also sent whenever
	// no PD is waiting.
	WriteBufferSize int `json:"write_buffer_size"`
	// StopTimeout is how long caracal is given to exit once it has been
	// killed, before its output is abandoned.
	StopTimeout time.Duration `json:"stop_timeout"`
}

func (c *CaracalProberConfig) validate() error {
	if c.Path == "" {
		return fmt.Errorf("path cannot be empty")
	}
	if c.WriteBufferSize < 1 {
		return fmt.Errorf("write buffer size must be at least 1: got %d", c.WriteBufferSize)
	}
	if c.StopTimeout <= 0 {
		return fmt.Errorf("stop timeout must be positive: got %v", c.StopTimeout)
	}
	return nil
}

// CaracalProber is a prober that sends its probes with a caracal process.
//
// Caracal reads one probe per line on its standard input, as
// dst_addr,src_port,dst_port,ttl,protocol, and sends it at its own probing
// rate. It writes one line per reply it captures to its standard output, and
// its logs to its standard error. A reply does not say which line it answers:
// it carries the probe's destination, ports and TTL, read back from the
// packet the reply quotes.
type CaracalProber struct {
	config *CaracalProberConfig
	logger *slog.Logger
}

// NewCaracalProber creates a caracal prober. The caracal process is started
// by Run.
func NewCaracalProber(config *CaracalProberConfig, logger *slog.Logger) *CaracalProber {
	return &CaracalProber{config: config, logger: logger}
}

// Run implements Prober. It starts caracal and returns an error when caracal
// stops on its own.
func (p *CaracalProber) Run(ctx context.Context, pds <-chan PD, fies chan<- FIE) error {
	// Canceling the command's context kills caracal.
	group, groupCtx := errgroup.WithContext(ctx)
	cmd := exec.CommandContext(groupCtx, p.config.Path, p.config.Args...) //nolint:gosec // G204: the path is the operator's configuration
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
	group.Go(func() error { return p.readReplies(stdout, fies) })
	group.Go(func() error { return p.writeProbes(groupCtx, stdin, pds) })

	err = group.Wait()
	// Wait only once the pipes are no longer read.
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("caracal stopped: %w", errors.Join(err, waitErr))
}

// writeProbes writes the near and the far probe of every PD to caracal. It
// returns when ctx is done or caracal no longer takes probes.
func (p *CaracalProber) writeProbes(ctx context.Context, stdin io.WriteCloser, pds <-chan PD) error {
	defer func() { _ = stdin.Close() }()
	writer := bufio.NewWriterSize(stdin, p.config.WriteBufferSize)
	var line []byte

	for {
		select {
		case pd := <-pds:
			// TODO: register the PD as in flight before its probes are
			// written, bound the PDs in flight, and report the PDs whose
			// probes are not answered in time.
			protocol, ok := caracalProtocol(pd.Protocol)
			if !ok || pd.NearTTL == 255 {
				p.logger.Warn("Cannot probe PD", slog.Uint64("pd_id", uint64(pd.ID)), slog.Int("protocol", int(pd.Protocol)), slog.Int("near_ttl", int(pd.NearTTL)))
				continue
			}
			line = appendCaracalProbe(line[:0], &pd, pd.NearTTL, protocol)
			line = appendCaracalProbe(line, &pd, pd.NearTTL+1, protocol)
			if _, err := writer.Write(line); err != nil {
				return fmt.Errorf("cannot write probes: %w", err)
			}
			// Probes are sent in groups while PDs keep coming, and at once
			// when none is waiting.
			if len(pds) == 0 {
				if err := writer.Flush(); err != nil {
					return fmt.Errorf("cannot write probes: %w", err)
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// readReplies reads the replies caracal captures. It returns an error when
// caracal's output ends, which it only does when caracal stops.
func (p *CaracalProber) readReplies(stdout io.Reader, _ chan<- FIE) error {
	reader := bufio.NewReaderSize(stdout, caracalReadBufferSize)

	header, err := reader.ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("cannot read caracal header: %w", err)
	}
	if string(bytes.TrimSpace(header)) != caracalHeader {
		return fmt.Errorf("unexpected caracal header %q: this agent expects the output of caracal v0.15.4", header)
	}

	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return fmt.Errorf("cannot read caracal replies: %w", err)
		}
		reply, err := parseCaracalReply(line)
		if err != nil {
			p.logger.Warn("Cannot decode caracal reply", slog.Any("err", err))
			continue
		}
		// TODO: match the reply to the PD in flight it answers, and write
		// the PD's FIE to fies once both probes are answered or timed out.
		_ = reply
	}
}

// logOutput logs what caracal writes to its standard error.
func (p *CaracalProber) logOutput(stderr io.Reader) error {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		p.logger.Info(scanner.Text(), slog.String("source", "caracal"))
	}
	return nil
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
// For ICMP and ICMPv6, caracal puts src_port in the ICMP checksum and
// identifier, and ignores dst_port.
func appendCaracalProbe(line []byte, pd *PD, ttl uint8, protocol string) []byte {
	line = pd.Destination.AppendTo(line)
	line = append(line, ',')
	line = strconv.AppendUint(line, uint64(pd.FirstHalfWord), 10)
	line = append(line, ',')
	line = strconv.AppendUint(line, uint64(pd.SecondHalfWord), 10)
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
