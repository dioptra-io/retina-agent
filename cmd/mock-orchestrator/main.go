// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Command mock-orchestrator simulates a network measurement orchestrator for testing retina-agent.
//
// It listens for agent connections, sends deterministic probing directives at a configurable rate,
// and logs received forwarding information elements (FIEs).
//
// Usage:
//
//	mock-orchestrator [-address localhost:50050] [-rate 10]
//
// Flags:
//
//	-address  Listen address (default: localhost:50050)
//	-rate     Directives per second (default: 10)
//
// The mock generates diverse test traffic including:
//   - IPv4 and IPv6 destinations
//   - Alternating ICMP and UDP protocols
//   - Cycling TTL values (5-20)
//   - Popular public DNS resolvers as probe targets
//
// Directives are deterministic (based on counter) for reproducible testing.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"time"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
)

func main() {
	address := flag.String("address", "localhost:50050", "Listen address")
	rate := flag.Int("rate", 10, "Directives per second")
	flag.Parse()

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			log.Printf("Failed to close listener: %v", err)
		}
	}()

	log.Printf("Mock orchestrator listening on %s (sending %d directives/sec)", *address, *rate)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}

		log.Printf("Agent connected from %s", conn.RemoteAddr())
		go handleAgent(conn, *rate)
	}
}

// handleAgent manages a connection with a single agent, sending directives and receiving FIEs.
func handleAgent(conn net.Conn, rate int) {
	remoteAddr := conn.RemoteAddr().String()

	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("[%s] Failed to close connection: %v", remoteAddr, err)
		}
		log.Printf("[%s] Connection closed", remoteAddr)
	}()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	// Start goroutine to receive FIEs from agent
	go receiveFIEs(decoder, remoteAddr)

	// Send PDs at specified rate
	sendPDs(encoder, remoteAddr, rate)
}

// receiveFIEs continuously receives and logs FIEs from the agent.
func receiveFIEs(decoder *json.Decoder, remoteAddr string) {
	for {
		var fie api.ForwardingInfoElement
		if err := decoder.Decode(&fie); err != nil {
			if err != io.EOF {
				log.Printf("[%s] Decode error: %v", remoteAddr, err)
			}
			return
		}

		nearRTT := fie.NearInfo.ReceivedTimestamp.Sub(fie.NearInfo.SentTimestamp)
		farRTT := fie.FarInfo.ReceivedTimestamp.Sub(fie.FarInfo.SentTimestamp)

		log.Printf("[%s] ✓ FIE: %s → %s | Near(TTL%d, %v): %s | Far(TTL%d, %v): %s",
			remoteAddr,
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

// sendPDs continuously sends probing directives to the agent at the specified rate.
func sendPDs(encoder *json.Encoder, remoteAddr string, rate int) {
	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()

	pdCounter := 0
	for range ticker.C {
		pd := generatePD(pdCounter)
		pdCounter++

		if err := encoder.Encode(pd); err != nil {
			log.Printf("[%s] Send error: %v", remoteAddr, err)
			return
		}

		var protocol string
		switch pd.Protocol {
		case api.ICMP:
			protocol = "ICMP"
		case api.ICMPv6:
			protocol = "ICMPv6"
		case api.UDP:
			protocol = "UDP"
		default:
			protocol = "UNKNOWN"
		}

		log.Printf("[%s] → Directive #%d: %s %s TTL %d",
			remoteAddr,
			pdCounter,
			pd.DestinationAddress,
			protocol,
			pd.NearTTL,
		)
	}
}

// generatePD creates a deterministic probing directive for testing.
// It cycles through various IPv4/IPv6 destinations and alternates between ICMP and UDP protocols.
func generatePD(counter int) *api.ProbingDirective {
	// List of common destinations to probe (IPv4 and IPv6)
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

	// Determine IP version
	ipVersion := api.IPVersion(4)
	if dstIP.To4() == nil {
		ipVersion = api.IPVersion(6)
	}

	// Build base directive with common fields
	pd := &api.ProbingDirective{
		AgentID:            "agent-1",
		IPVersion:          ipVersion,
		DestinationAddress: dstIP,
		NearTTL:            ttl,
	}

	// Alternate between ICMP and UDP
	useUDP := counter%2 == 0

	if useUDP {
		// UDP probe with deterministic ports
		portOffset := counter % 100
		pd.Protocol = api.UDP
		pd.NextHeader = api.NextHeader{
			UDPNextHeader: &api.UDPNextHeader{
				SourcePort:      uint16(50000 + portOffset), // #nosec G115 -- portOffset is 0-99, safe for uint16
				DestinationPort: uint16(33434 + portOffset), // #nosec G115 -- portOffset is 0-99, safe for uint16
			},
		}
	} else {
		// ICMP probe
		if ipVersion == api.IPVersion(6) {
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
