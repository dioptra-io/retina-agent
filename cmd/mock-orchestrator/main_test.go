// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

package main

import (
	"testing"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
)

func TestGeneratePD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		counter      int
		wantTTL      uint8
		wantIPv4     bool
		wantProtocol api.Protocol
	}{
		{
			name:         "first directive - IPv4 UDP",
			counter:      0,
			wantTTL:      5,
			wantIPv4:     true,
			wantProtocol: api.UDP,
		},
		{
			name:         "second directive - IPv4 ICMP",
			counter:      1,
			wantTTL:      6,
			wantIPv4:     true,
			wantProtocol: api.ICMP,
		},
		{
			name:         "IPv6 directive - UDP",
			counter:      6, // First IPv6 in list
			wantTTL:      11,
			wantIPv4:     false,
			wantProtocol: api.UDP,
		},
		{
			name:         "IPv6 directive - ICMPv6",
			counter:      7,
			wantTTL:      12,
			wantIPv4:     false,
			wantProtocol: api.ICMPv6,
		},
		{
			name:         "TTL wraps around",
			counter:      18, // 18 % 9 = 0 (first IPv4), TTL: 5 + (18 % 16) = 7
			wantTTL:      7,
			wantIPv4:     true,
			wantProtocol: api.UDP,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pd := generatePD(tt.counter)

			if pd == nil {
				t.Fatal("generatePD returned nil")
			}

			// Check TTL
			if pd.NearTTL != tt.wantTTL {
				t.Errorf("NearTTL = %d, want %d", pd.NearTTL, tt.wantTTL)
			}

			// Check IP version
			isIPv4 := pd.DestinationAddress.To4() != nil
			if isIPv4 != tt.wantIPv4 {
				t.Errorf("IPv4 = %v, want %v (addr: %s)", isIPv4, tt.wantIPv4, pd.DestinationAddress)
			}

			// Check protocol
			if pd.Protocol != tt.wantProtocol {
				t.Errorf("Protocol = %v, want %v", pd.Protocol, tt.wantProtocol)
			}

			// Verify protocol-specific headers exist
			switch pd.Protocol {
			case api.ICMP:
				if pd.NextHeader.ICMPNextHeader == nil {
					t.Error("ICMP directive missing ICMPNextHeader")
				}
			case api.ICMPv6:
				if pd.NextHeader.ICMPv6NextHeader == nil {
					t.Error("ICMPv6 directive missing ICMPv6NextHeader")
				}
			case api.UDP:
				if pd.NextHeader.UDPNextHeader == nil {
					t.Error("UDP directive missing UDPNextHeader")
				}
			}

			// Verify required fields
			if pd.AgentID == "" {
				t.Error("AgentID is empty")
			}
			if pd.DestinationAddress == nil {
				t.Error("DestinationAddress is nil")
			}
		})
	}
}

func TestGeneratePD_Deterministic(t *testing.T) {
	t.Parallel()

	// Same counter should produce identical directives
	pd1 := generatePD(42)
	pd2 := generatePD(42)

	if !pd1.DestinationAddress.Equal(pd2.DestinationAddress) {
		t.Error("generatePD not deterministic: addresses differ")
	}
	if pd1.NearTTL != pd2.NearTTL {
		t.Error("generatePD not deterministic: TTLs differ")
	}
	if pd1.Protocol != pd2.Protocol {
		t.Error("generatePD not deterministic: protocols differ")
	}
}

func TestGeneratePD_Coverage(t *testing.T) {
	t.Parallel()

	// Generate 100 directives to cover cycling logic
	for i := 0; i < 100; i++ {
		pd := generatePD(i)

		if pd == nil {
			t.Fatalf("generatePD(%d) returned nil", i)
		}

		// Verify TTL range
		if pd.NearTTL < 5 || pd.NearTTL > 20 {
			t.Errorf("generatePD(%d): TTL %d out of range [5-20]", i, pd.NearTTL)
		}

		// Verify destination is valid
		if pd.DestinationAddress == nil {
			t.Errorf("generatePD(%d): nil destination", i)
		}

		// Verify protocol is valid
		if pd.Protocol != api.ICMP && pd.Protocol != api.ICMPv6 && pd.Protocol != api.UDP {
			t.Errorf("generatePD(%d): invalid protocol %v", i, pd.Protocol)
		}
	}
}

func TestReportStats(t *testing.T) {
	t.Parallel()

	// Reset counters for this test
	directivesSent.Store(100)
	fiesReceived.Store(75)

	// Just verify it doesn't crash
	reportStats()

	// With zero directives
	directivesSent.Store(0)
	fiesReceived.Store(0)
	reportStats() // Should return early
}
