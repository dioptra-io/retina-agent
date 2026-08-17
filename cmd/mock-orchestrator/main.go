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
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
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

// handleAuth performs the authentication handshake. The decoder is created here and returned
// so that the PD/FIE loop reuses it — avoiding loss of buffered bytes.
// A read deadline bounds the handshake so a connected-but-silent client can't hang the
// goroutine forever; it's cleared once auth succeeds so it doesn't affect the PD/FIE loop.
// Returns the shared decoder and true on success, nil and false on failure.
func handleAuth(conn net.Conn, secret string) (*json.Decoder, bool) {
	remoteAddr := conn.RemoteAddr().String()

	if err := conn.SetReadDeadline(time.Now().Add(authTimeout)); err != nil {
		log.Printf("[%s] Failed to set auth read deadline: %v", remoteAddr, err)
		return nil, false
	}

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var req api.AuthRequest
	if err := decoder.Decode(&req); err != nil {
		log.Printf("[%s] Failed to decode auth request: %v", remoteAddr, err)
		return nil, false
	}

	if secret != "" && req.Secret != secret {
		_ = encoder.Encode(api.AuthResponse{Authenticated: false, Message: "secret is not correct"})
		log.Printf("[%s] Authentication failed", remoteAddr)
		return nil, false
	}

	if err := encoder.Encode(api.AuthResponse{Authenticated: true, Message: "authenticated"}); err != nil {
		log.Printf("[%s] Failed to send auth response: %v", remoteAddr, err)
		return nil, false
	}

	// Clear the deadline now that the handshake is done; the PD/FIE loop manages its own lifetime.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		log.Printf("[%s] Failed to clear read deadline: %v", remoteAddr, err)
		return nil, false
	}

	log.Printf("[%s] Authenticated", remoteAddr)
	return decoder, true
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

	decoder, ok := handleAuth(conn, secret)
	if !ok {
		return
	}

	encoder := json.NewEncoder(conn)

	go receiveFIEs(decoder, remoteAddr)
	sendPDs(encoder, remoteAddr, rate)
}

func receiveFIEs(decoder *json.Decoder, remoteAddr string) {
	for {
		var fie api.ForwardingInfoElement
		if err := decoder.Decode(&fie); err != nil {
			if err != io.EOF {
				log.Printf("[%s] Decode error: %v", remoteAddr, err)
			}
			return
		}

		fiesReceived.Add(1)

		if fie.NearInfo == nil && fie.FarInfo == nil {
			log.Printf("[%s] ✓ FIE for PD %d: %s → %s | (no probe response received)",
				remoteAddr,
				fie.ProbingDirectiveID,
				fie.Agent.AgentID,
				fie.DestinationAddress,
			)
			continue
		}

		if fie.NearInfo == nil || fie.FarInfo == nil {
			log.Printf("[%s] ✓ FIE for PD %d: %s → %s | (partial probe response: nearInfo=%v farInfo=%v)",
				remoteAddr,
				fie.ProbingDirectiveID,
				fie.Agent.AgentID,
				fie.DestinationAddress,
				fie.NearInfo != nil,
				fie.FarInfo != nil,
			)
			continue
		}

		nearRTT := fie.NearInfo.ReceivedTimestamp.Sub(fie.NearInfo.SentTimestamp)
		farRTT := fie.FarInfo.ReceivedTimestamp.Sub(fie.FarInfo.SentTimestamp)

		log.Printf("[%s] ✓ FIE for PD %d: %s → %s | Near(TTL%d, %v): %s | Far(TTL%d, %v): %s",
			remoteAddr,
			fie.ProbingDirectiveID,
			fie.Agent.AgentID,
			fie.DestinationAddress,
			fie.NearInfo.ProbeTTL,
			nearRTT,
			fie.NearInfo.ReplyAddress,
			fie.FarInfo.ProbeTTL,
			farRTT,
			fie.FarInfo.ReplyAddress,
		)
	}
}

func sendPDs(encoder *json.Encoder, remoteAddr string, rate int) {
	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()

	pdCounter := 0
	for range ticker.C {
		pd := generatePD(pdCounter)
		pdCounter++

		if err := encoder.Encode(pd); err != nil {
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
		case api.ICMP:
			protocol = "ICMP"
		case api.ICMPv6:
			protocol = "ICMPv6"
		case api.UDP:
			protocol = "UDP"
		default:
			protocol = "UNKNOWN" // unreachable: generatePD only produces ICMP, ICMPv6, or UDP
		}

		log.Printf("[%s] → PD #%d (PD ID %d): %s %s TTL %d",
			remoteAddr,
			pdCounter,
			pd.ProbingDirectiveID,
			pd.DestinationAddress,
			protocol,
			pd.NearTTL,
		)
	}
}

func generatePD(counter int) *api.ProbingDirective {
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

	dstIP := net.ParseIP(destinations[counter%len(destinations)])

	// TTL cycles from 5 to 20
	ttlOffset := counter % 16
	ttl := uint8(5 + ttlOffset) // #nosec G115 -- ttlOffset is 0-15, safe for uint8

	ipVersion := api.IPv4
	if dstIP.To4() == nil {
		ipVersion = api.IPv6
	}

	// ProbingDirectiveID is 1-indexed so that ID 0 is never used.
	pd := &api.ProbingDirective{
		ProbingDirectiveID: uint64(counter + 1), // #nosec G115 -- counter is test value, no overflow
		AgentID:            "agent-1",
		IPVersion:          ipVersion,
		DestinationAddress: dstIP,
		NearTTL:            ttl,
	}

	// Even counters use UDP (with port fields); odd counters use ICMP/ICMPv6 (no port fields).
	useUDP := counter%2 == 0

	if useUDP {
		portOffset := counter % 100
		pd.Protocol = api.UDP
		pd.NextHeader = api.NextHeader{
			UDPNextHeader: &api.UDPNextHeader{
				SourcePort:      uint16(50000 + portOffset), // #nosec G115 -- portOffset is 0-99, safe for uint16
				DestinationPort: uint16(33434 + portOffset), // #nosec G115 -- portOffset is 0-99, safe for uint16
			},
		}
	} else {
		if ipVersion == api.IPv6 {
			pd.Protocol = api.ICMPv6
			pd.NextHeader = api.NextHeader{
				ICMPv6NextHeader: &api.ICMPv6NextHeader{},
			}
		} else {
			pd.Protocol = api.ICMP
			pd.NextHeader = api.NextHeader{
				ICMPNextHeader: &api.ICMPNextHeader{},
			}
		}
	}

	return pd
}
