// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// cmd/mock-orchestrator/main.go
package main

import (
	"encoding/json"
	"flag"
	"log"
	"math/rand"
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
	defer listener.Close()

	log.Printf("Mock orchestrator listening on %s", *address)
	log.Printf("Sending %d directives/sec", *rate)

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

func handleAgent(conn net.Conn, rate int) {
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	// Goroutine to receive FIEs from agent
	go func() {
		for {
			var fie api.ForwardingInfoElement
			if err := decoder.Decode(&fie); err != nil {
				log.Printf("Agent disconnected: %v", err)
				return
			}
			log.Printf("✓ Received FIE: %s → %s (near: %s, far: %s)",
				fie.Agent.AgentID,
				fie.DestinationAddress,
				fie.NearInfo.ReplyAddress,
				fie.FarInfo.ReplyAddress,
			)
		}
	}()

	// Send directives at specified rate
	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()

	directiveCounter := 0
	for range ticker.C {
		pd := generateDirective(directiveCounter)
		directiveCounter++

		if err := encoder.Encode(pd); err != nil {
			log.Printf("Send error: %v", err)
			return
		}

		log.Printf("→ Sent directive #%d: %s TTL %d",
			directiveCounter,
			pd.DestinationAddress,
			pd.NearTTL,
		)
	}
}

func generateDirective(counter int) *api.ProbingDirective {
	// List of common destinations to probe
	destinations := []string{
		"8.8.8.8",        // Google DNS
		"1.1.1.1",        // Cloudflare DNS
		"9.9.9.9",        // Quad9 DNS
		"208.67.222.222", // OpenDNS
		"1.0.0.1",        // Cloudflare
		"8.8.4.4",        // Google DNS
	}

	dstIP := net.ParseIP(destinations[counter%len(destinations)])

	// Random TTL between 5-20
	ttl := uint8(5 + rand.Intn(16))

	return &api.ProbingDirective{
		AgentID:            "agent-1",
		IPVersion:          4, // Use numeric value instead of api.IPv4
		Protocol:           api.ICMP,
		DestinationAddress: dstIP,
		NearTTL:            ttl,
		NextHeader: api.NextHeader{
			ICMPNextHeader: &api.ICMPNextHeader{},
		},
	}
}
