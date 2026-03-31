// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Coverage: 100% of prober.go.
package agent

import (
	"net"
	"strings"
	"testing"
	"time"
)

// -- ProbeResult --------------------------------------------------------------

func TestProbeResult_Success(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		result   ProbeResult
		expected bool
	}{
		{
			name:     "successful probe",
			result:   ProbeResult{TimedOut: false},
			expected: true,
		},
		{
			name:     "timed out probe",
			result:   ProbeResult{TimedOut: true},
			expected: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.result.Success(); got != tt.expected {
				t.Errorf("Success() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestProbeResult_RTT(t *testing.T) {
	t.Parallel()
	now := time.Now()
	delay := 50 * time.Millisecond
	tests := []struct {
		name     string
		result   ProbeResult
		expected time.Duration
	}{
		{
			name: "successful probe",
			result: ProbeResult{
				SentTime:     now,
				ReceivedTime: now.Add(delay),
				TimedOut:     false,
			},
			expected: delay,
		},
		{
			name: "timed out probe",
			result: ProbeResult{
				SentTime: now,
				TimedOut: true,
			},
			expected: 0,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.result.RTT(); got != tt.expected {
				t.Errorf("RTT() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestProbeResult_String(t *testing.T) {
	t.Parallel()
	now := time.Now()
	replyAddr := net.ParseIP("8.8.8.8")
	tests := []struct {
		name     string
		result   ProbeResult
		contains []string
	}{
		{
			name: "successful probe",
			result: ProbeResult{
				ReplyAddress: replyAddr,
				SentTime:     now,
				ReceivedTime: now.Add(50 * time.Millisecond),
				TimedOut:     false,
			},
			contains: []string{"SUCCESS", "8.8.8.8", "RTT="},
		},
		{
			name: "timed out probe",
			result: ProbeResult{
				SentTime: now,
				TimedOut: true,
			},
			contains: []string{"TIMEOUT", "sent at"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.result.String()
			for _, substr := range tt.contains {
				if !strings.Contains(got, substr) {
					t.Errorf("String() = %q, should contain %q", got, substr)
				}
			}
		})
	}
}
