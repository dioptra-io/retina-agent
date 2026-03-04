// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestNewMetrics(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	m := NewMetrics(registry, "test-agent")

	if m.PDsReceivedTotal == nil {
		t.Error("PDsReceivedTotal is nil")
	}
	if m.PDsInvalidTotal == nil {
		t.Error("PDsInvalidTotal is nil")
	}
	if m.FIEsSentTotal == nil {
		t.Error("FIEsSentTotal is nil")
	}
	if m.ChannelDepth == nil {
		t.Error("ChannelDepth is nil")
	}
	if m.ProbesTotal == nil {
		t.Error("ProbesTotal is nil")
	}
	if m.ProbeRTTSeconds == nil {
		t.Error("ProbeRTTSeconds is nil")
	}
	if m.ReconnectionsTotal == nil {
		t.Error("ReconnectionsTotal is nil")
	}
	if m.DecodeErrorsTotal == nil {
		t.Error("DecodeErrorsTotal is nil")
	}
	if m.WriteErrorsTotal == nil {
		t.Error("WriteErrorsTotal is nil")
	}
	if m.PDGoroutines == nil {
		t.Error("ActiveProbeGoroutines is nil")
	}
	if m.CorrelationFailuresTotal == nil {
		t.Error("CorrelationFailuresTotal is nil")
	}
	if m.DuplicateProbesTotal == nil {
		t.Error("DuplicateProbesTotal is nil")
	}
	if m.WriteQueueDepth == nil {
		t.Error("WriteQueueDepth is nil")
	}
	if m.InFlightProbes == nil {
		t.Error("InFlightProbes is nil")
	}
	if m.StaleProbesCleanedTotal == nil {
		t.Error("StaleProbesCleanedTotal is nil")
	}
	if m.ReplyAddressTypeTotal == nil {
		t.Error("ReplyAddressTypeTotal is nil")
	}
	if m.ICMPReplyTypeTotal == nil {
		t.Error("ICMPReplyTypeTotal is nil")
	}
}

func TestNewMetrics_NoPanic_MultipleRegistries(t *testing.T) {
	t.Parallel()

	// Verify that creating metrics on separate registries does not panic
	// (would panic if accidentally using the global registry).
	registry1 := prometheus.NewRegistry()
	registry2 := prometheus.NewRegistry()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("NewMetrics panicked on registry1: %v", r)
			}
		}()
		NewMetrics(registry1, "agent-1")
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("NewMetrics panicked on registry2: %v", r)
			}
		}()
		NewMetrics(registry2, "agent-2")
	}()
}
