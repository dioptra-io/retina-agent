// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import "net/netip"

// PD is a probing directive, as the orchestrator sends it to the agent.
type PD struct {
	ID          uint32
	Destination netip.Addr
	NearTTL     uint8
	// Protocol is the IANA IP protocol number: 1 ICMP, 17 UDP, 58 ICMPv6.
	Protocol uint8
	// FirstHalfWord and SecondHalfWord are the protocol-specific header
	// words: the source and destination ports for UDP.
	FirstHalfWord  uint16
	SecondHalfWord uint16
}

// probeable reports whether the PD can be probed: its protocol is one the
// agent probes with, and its far TTL, the near TTL plus one, fits in a TTL.
func (pd *PD) probeable() bool {
	switch pd.Protocol {
	case 1, 17, 58:
		return pd.NearTTL < 255
	default:
		return false
	}
}

// FIE is a forwarding info element, as the agent reports it to the
// orchestrator.
type FIE struct {
	PDID uint32
	// CaptureUnix is when the agent produced the FIE, in Unix seconds.
	CaptureUnix int64
	// Near and Far are the reply addresses. The zero Addr means no reply.
	Near netip.Addr
	Far  netip.Addr
	// NearDelta and FarDelta are the seconds between each reply and the
	// capture. They are zero when there was no reply.
	NearDelta uint32
	FarDelta  uint32
}
