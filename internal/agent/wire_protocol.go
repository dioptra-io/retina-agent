// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/dioptra-io/retina-commons/api/v1"
)

const (
	pdWireFields  = 6
	fieWireFields = 6
)

// decodePDRecord decodes the compact post-handshake PD representation:
// id,destination,near_ttl,protocol,first_half_word,second_half_word.
func decodePDRecord(line, agentID string) (*api.ProbingDirective, error) {
	record, err := readCSVRecord(line)
	if err != nil {
		return nil, fmt.Errorf("invalid PD CSV: %w", err)
	}
	if len(record) != pdWireFields {
		return nil, fmt.Errorf("invalid PD CSV: got %d fields, want %d", len(record), pdWireFields)
	}

	// PD IDs are 32-bit.
	id, err := strconv.ParseUint(record[0], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid probing directive ID %q: %w", record[0], err)
	}
	destination := net.ParseIP(record[1])
	if destination == nil {
		return nil, fmt.Errorf("invalid destination address %q", record[1])
	}
	nearTTL, err := parseUint8(record[2], "near TTL")
	if err != nil {
		return nil, err
	}
	protocolNumber, err := parseUint8(record[3], "protocol number")
	if err != nil {
		return nil, err
	}
	firstHalf, err := parseUint16(record[4], "first half-word")
	if err != nil {
		return nil, err
	}
	secondHalf, err := parseUint16(record[5], "second half-word")
	if err != nil {
		return nil, err
	}

	pd := &api.ProbingDirective{
		ProbingDirectiveID: uint32(id),
		AgentID:            agentID,
		DestinationAddress: destination,
		NearTTL:            nearTTL,
		Protocol:           api.Protocol(protocolNumber),
	}
	if destination.To4() == nil {
		pd.IPVersion = api.IPv6
	} else {
		pd.IPVersion = api.IPv4
	}

	switch pd.Protocol {
	case api.ICMP:
		pd.NextHeader.ICMPNextHeader = &api.ICMPNextHeader{FirstHalfWord: firstHalf, SecondHalfWord: secondHalf}
	case api.ICMPv6:
		pd.NextHeader.ICMPv6NextHeader = &api.ICMPv6NextHeader{FirstHalfWord: firstHalf, SecondHalfWord: secondHalf}
	case api.UDP:
		pd.NextHeader.UDPNextHeader = &api.UDPNextHeader{SourcePort: firstHalf, DestinationPort: secondHalf}
	default:
		return nil, fmt.Errorf("unsupported protocol number %d", protocolNumber)
	}
	return pd, nil
}

// encodeFIERecord encodes the compact post-handshake FIE representation:
// id,capture_unix,near_address,near_delta,far_address,far_delta.
func encodeFIERecord(fie *api.ForwardingInfoElement) string {
	capture := fie.ProductionTimestamp.UTC()
	var builder strings.Builder
	builder.Grow(96)
	builder.WriteString(strconv.FormatUint(uint64(fie.ProbingDirectiveID), 10))
	builder.WriteByte(',')
	builder.WriteString(strconv.FormatInt(capture.Unix(), 10))
	appendFIEInfo(&builder, capture, fie.NearInfo)
	appendFIEInfo(&builder, capture, fie.FarInfo)
	builder.WriteByte('\n')
	return builder.String()
}

func appendFIEInfo(builder *strings.Builder, capture time.Time, info *api.Info) {
	builder.WriteByte(',')
	// A reply without an address is encoded as no reply.
	if info == nil || info.ReplyAddress == nil {
		builder.WriteString(`"",0`)
		return
	}
	builder.WriteString(strconv.Quote(info.ReplyAddress.String()))
	builder.WriteByte(',')
	delta := capture.Unix() - info.ReceivedTimestamp.UTC().Unix()
	if delta < 0 {
		delta = 0
	}
	builder.WriteString(strconv.FormatInt(delta, 10))
}

func readCSVRecord(line string) ([]string, error) {
	reader := csv.NewReader(strings.NewReader(line))
	reader.FieldsPerRecord = -1
	record, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if _, err = reader.Read(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple records in one line")
		}
		return nil, err
	}
	return record, nil
}

func parseUint8(value, field string) (uint8, error) {
	parsed, err := strconv.ParseUint(value, 10, 8)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, value, err)
	}
	return uint8(parsed), nil
}

func parseUint16(value, field string) (uint16, error) {
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, value, err)
	}
	return uint16(parsed), nil
}
