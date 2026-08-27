// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

// Command mock-orchestrator simulates a network measurement orchestrator for testing retina-agent.
//
// Usage:
//
//	mock-orchestrator [-address localhost:50050] [-probing-rate 10] [-secret ""]
//
// Flags:
//
//	-address       Listen address (default: localhost:50050)
//	-probing-rate  Probing directives per second (default: 10)
//	-secret        Shared secret for authentication (default: "", auth disabled)
package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/dioptra-io/retina-commons/framing"
	wire "github.com/dioptra-io/retina-commons/wire/v2"
)

var (
	pdsSent      atomic.Int64
	fiesReceived atomic.Int64
)

// startTime is set once at program start and never written after that.
var startTime = time.Now()

// authTimeout bounds how long a connecting agent has to complete the auth handshake
// before mock-orchestrator gives up on it and closes the connection.
const authTimeout = 5 * time.Second

func main() {
	addr := flag.String("address", "localhost:50050", "Listen address")
	rate := flag.Int("probing-rate", 10, "Probing directives per second")
	secret := flag.String("secret", "", "Shared secret for authentication (empty = disabled)")
	flag.Parse()

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			log.Printf("Failed to close listener: %v", err)
		}
	}()

	log.Printf("Mock orchestrator listening on %s (sending %d PDs/s)", *addr, *rate)
	if *secret == "" {
		log.Printf("Authentication disabled")
	} else {
		log.Printf("Authentication enabled")
	}

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			reportStats()
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		log.Printf("Agent connected from %s", conn.RemoteAddr())
		go handleAgent(conn, *rate, *secret)
	}
}

// The early return on sent == 0 avoids a division by zero before any PDs have been sent.
func reportStats() {
	sent := pdsSent.Load()
	received := fiesReceived.Load()
	duration := time.Since(startTime).Seconds()

	if sent == 0 {
		return
	}

	pdsPerSec := float64(sent) / duration
	fiesPerSec := float64(received) / duration
	successRate := float64(received) / float64(sent) * 100

	log.Printf("📊 Throughput: %.1f PDs/s, %.1f FIEs/s (%.1f%% success) | Total: %d PDs, %d FIEs",
		pdsPerSec, fiesPerSec, successRate, sent, received)
}

// handleAuth performs the authentication handshake. Unlike the old JSON-based
// version, there's no decoder to thread through to the PD/FIE loop —
// framing.Receive reads directly off conn each call, with no buffering
// state to preserve across calls. framing.Send/Receive apply authTimeout
// as their own read/write deadline internally, so no manual
// SetReadDeadline/clear-deadline calls are needed either.
func handleAuth(conn net.Conn, secret string) bool {
	remoteAddr := conn.RemoteAddr().String()

	var req wire.AuthRequest
	if err := framing.Receive(conn, authTimeout, &req); err != nil {
		log.Printf("[%s] Failed to decode auth request: %v", remoteAddr, err)
		return false
	}

	if secret != "" && req.Secret != secret {
		_ = framing.Send(conn, authTimeout, &wire.AuthResponse{Authenticated: false, Message: "secret is not correct"})
		log.Printf("[%s] Authentication failed", remoteAddr)
		return false
	}

	if err := framing.Send(conn, authTimeout, &wire.AuthResponse{Authenticated: true, Message: "authenticated"}); err != nil {
		log.Printf("[%s] Failed to send auth response: %v", remoteAddr, err)
		return false
	}

	log.Printf("[%s] Authenticated", remoteAddr)
	return true
}

// receiveFIEs runs in a separate goroutine so that sending and receiving can proceed
// concurrently on the same connection. sendPDs blocks until the connection closes.
func handleAgent(conn net.Conn, rate int, secret string) {
	remoteAddr := conn.RemoteAddr().String()

	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("[%s] Failed to close connection: %v", remoteAddr, err)
		}
		log.Printf("[%s] Connection closed", remoteAddr)
	}()

	if !handleAuth(conn, secret) {
		return
	}

	go receiveFIEs(conn, remoteAddr)
	sendPDs(conn, remoteAddr, rate)
}

func receiveFIEs(conn net.Conn, remoteAddr string) {
	for {
		var fie wire.ForwardingInfoElement
		if err := framing.Receive(conn, 0, &fie); err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("[%s] Decode error: %v", remoteAddr, err)
			}
			return
		}

		fiesReceived.Add(1)

		if fie.NearInfo == nil && fie.FarInfo == nil {
			log.Printf("[%s] ✓ FIE for PD %d: %s → %s | (no probe response received)",
				remoteAddr,
				fie.ProbingDirectiveId,
				fie.GetAgent().GetAgentId(),
				fie.DestinationAddress,
			)
			continue
		}

		if fie.NearInfo == nil || fie.FarInfo == nil {
			log.Printf("[%s] ✓ FIE for PD %d: %s → %s | (partial probe response: nearInfo=%v farInfo=%v)",
				remoteAddr,
				fie.ProbingDirectiveId,
				fie.GetAgent().GetAgentId(),
				fie.DestinationAddress,
				fie.NearInfo != nil,
				fie.FarInfo != nil,
			)
			continue
		}

		nearRTT := fie.NearInfo.ReceivedTimestamp.AsTime().Sub(fie.NearInfo.SentTimestamp.AsTime())
		farRTT := fie.FarInfo.ReceivedTimestamp.AsTime().Sub(fie.FarInfo.SentTimestamp.AsTime())

		log.Printf("[%s] ✓ FIE for PD %d: %s → %s | Near(TTL%d, %v): %s | Far(TTL%d, %v): %s",
			remoteAddr,
			fie.ProbingDirectiveId,
			fie.GetAgent().GetAgentId(),
			fie.DestinationAddress,
			fie.NearInfo.ProbeTtl,
			nearRTT,
			fie.NearInfo.ReplyAddress,
			fie.FarInfo.ProbeTtl,
			farRTT,
			fie.FarInfo.ReplyAddress,
		)
	}
}

func sendPDs(conn net.Conn, remoteAddr string, rate int) {
	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()

	pdCounter := 0
	for range ticker.C {
		pd := generatePD(pdCounter)
		pdCounter++

		if err := framing.Send(conn, 0, pd); err != nil {
			// Pipe errors indicate the agent disconnected cleanly; anything else is unexpected.
			if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("[%s] Send error: %v", remoteAddr, err)
			return
		}

		pdsSent.Add(1) // incremented after a successful encode to avoid counting failed sends.

		var protocol string
		switch pd.Protocol {
		case wire.Protocol_PROTOCOL_UNSPECIFIED:
			protocol = "UNSPECIFIED" // unreachable: generatePD only produces ICMP, ICMPv6, or UDP
		case wire.Protocol_PROTOCOL_ICMP:
			protocol = "ICMP"
		case wire.Protocol_PROTOCOL_ICMPV6:
			protocol = "ICMPv6"
		case wire.Protocol_PROTOCOL_UDP:
			protocol = "UDP"
		default:
			protocol = "UNKNOWN" // unreachable: generatePD only produces ICMP, ICMPv6, or UDP
		}

		log.Printf("[%s] → PD #%d (PD ID %d): %s %s TTL %d",
			remoteAddr,
			pdCounter,
			pd.ProbingDirectiveId,
			pd.DestinationAddress,
			protocol,
			pd.NearTtl,
		)
	}
}

// generatePD builds a *wire.ProbingDirective directly rather than going
// through model.ProbingDirective — this tool only ever serializes it via
// framing.Send, so there's no reason to round-trip through the domain
// type's net.IP/uint8 typing just to convert straight back to wire types.
func generatePD(counter int) *wire.ProbingDirective {
	destinations := []string{
		// IPv4
		"8.8.8.8",        // Google DNS
		"1.1.1.1",        // Cloudflare DNS
		"9.9.9.9",        // Quad9 DNS
		"208.67.222.222", // OpenDNS
		"1.0.0.1",        // Cloudflare
		"8.8.4.4",        // Google DNS
		// IPv6
		"2001:4860:4860::8888", // Google DNS
		"2606:4700:4700::1111", // Cloudflare DNS
		"2620:fe::fe",          // Quad9 DNS
	}

	dstAddr := destinations[counter%len(destinations)]
	dstIP := net.ParseIP(dstAddr)

	// TTL cycles from 5 to 20
	ttlOffset := counter % 16
	ttl := uint32(5 + ttlOffset) //nolint:gosec // G115: ttlOffset is 0-15, safe for uint32

	ipVersion := wire.IPVersion_IP_VERSION_IPV4
	if dstIP.To4() == nil {
		ipVersion = wire.IPVersion_IP_VERSION_IPV6
	}

	// ProbingDirectiveId is 1-indexed so that ID 0 is never used.
	pd := &wire.ProbingDirective{
		ProbingDirectiveId: uint64(counter + 1), //nolint:gosec // G115: counter is a local loop variable, no overflow
		AgentId:            "agent-1",
		IpVersion:          ipVersion,
		DestinationAddress: dstAddr,
		NearTtl:            ttl,
	}

	// Even counters use UDP (with port fields); odd counters use ICMP/ICMPv6 (no port fields).
	useUDP := counter%2 == 0

	switch {
	case useUDP:
		portOffset := uint32(counter % 100) //nolint:gosec // G115: counter%100 is 0-99, safe for uint32
		pd.Protocol = wire.Protocol_PROTOCOL_UDP
		pd.NextHeader = &wire.NextHeader{
			Header: &wire.NextHeader_UdpNextHeader{
				UdpNextHeader: &wire.UDPNextHeader{
					SourcePort:      50000 + portOffset,
					DestinationPort: 33434 + portOffset,
				},
			},
		}
	case ipVersion == wire.IPVersion_IP_VERSION_IPV6:
		pd.Protocol = wire.Protocol_PROTOCOL_ICMPV6
		pd.NextHeader = &wire.NextHeader{
			Header: &wire.NextHeader_Icmpv6NextHeader{
				Icmpv6NextHeader: &wire.ICMPv6NextHeader{},
			},
		}
	default:
		pd.Protocol = wire.Protocol_PROTOCOL_ICMP
		pd.NextHeader = &wire.NextHeader{
			Header: &wire.NextHeader_IcmpNextHeader{
				IcmpNextHeader: &wire.ICMPNextHeader{},
			},
		}
	}

	return pd
}
