// Copyright (c) 2025 Sorbonne Université
// SPDX-License-Identifier: MIT

// metrics.go defines all Prometheus metrics for the retina agent.
//
// Metrics are grouped by concern:
//   - Pipeline Health: PD/FIE flow through the agent pipeline
//   - Probe Outcomes: results of individual probes
//   - Connectivity: orchestrator connection health
//   - Throughput/Resource: goroutine and buffer usage
//   - Caracal Pipeline: internal caracal subprocess health
//   - Responsible Probing: signals that could indicate problematic behavior
//
// Note: caracal probes sent, packets received, and pcap dropped packets are
// available via caracal logs in Loki and do not need to be tracked in Prometheus.
//
// Usage:
//
//	registry := prometheus.NewRegistry()
//	m := NewMetrics(registry, "agent-1")
//	// pass m to agent and CaracalProber constructors
package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the agent.
// It is created once and passed to both agent and CaracalProber via constructors.
type Metrics struct {
	// Pipeline Health
	PDsReceivedTotal prometheus.Counter
	PDsInvalidTotal  prometheus.Counter
	FIEsSentTotal    prometheus.Counter
	ChannelDepth     *prometheus.GaugeVec

	// Probe Outcomes
	ProbesTotal     *prometheus.CounterVec
	ProbeRTTSeconds prometheus.Histogram

	// Connectivity
	ReconnectionsTotal prometheus.Counter
	DecodeErrorsTotal  prometheus.Counter
	WriteErrorsTotal   prometheus.Counter

	// Throughput / Resource
	PDGoroutines prometheus.Gauge

	// Caracal Pipeline
	// Note: probes sent to network and packets received are available via caracal logs in Loki.
	CorrelationFailuresTotal prometheus.Counter
	DuplicateProbesTotal     prometheus.Counter
	WriteQueueDepth          prometheus.Gauge
	InFlightProbes           prometheus.Gauge
	StaleProbesCleanedTotal  prometheus.Counter

	// Responsible Probing
	ReplyAddressTypeTotal *prometheus.CounterVec
	ICMPReplyTotal        *prometheus.CounterVec
}

// NewMetrics creates and registers all agent metrics with the given registry.
// agentID is added as a constant label to all metrics.
//
//nolint:funlen // metric registration is necessarily verbose
func NewMetrics(registry prometheus.Registerer, agentID string) *Metrics {
	factory := promauto.With(registry)

	constLabels := prometheus.Labels{"agent_id": agentID}

	return &Metrics{
		// Pipeline Health
		PDsReceivedTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_pds_received_total",
			Help:        "Total number of probing directives received from the orchestrator.",
			ConstLabels: constLabels,
		}),
		PDsInvalidTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_pds_invalid_total",
			Help:        "Total number of probing directives dropped due to validation failure.",
			ConstLabels: constLabels,
		}),
		FIEsSentTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_fies_sent_total",
			Help:        "Total number of forwarding information elements successfully sent to the orchestrator.",
			ConstLabels: constLabels,
		}),
		ChannelDepth: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "retina_agent_channel_depth",
			Help:        "Current number of items in pipeline channels (pds or fies). Indicates backpressure.",
			ConstLabels: constLabels,
		}, []string{"channel"}),

		// Probe Outcomes
		ProbesTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name:        "retina_agent_probes_total",
			Help:        "Total number of probes by outcome (success, timeout, error).",
			ConstLabels: constLabels,
		}, []string{"outcome"}),
		ProbeRTTSeconds: factory.NewHistogram(prometheus.HistogramOpts{
			Name:        "retina_agent_probe_rtt_seconds",
			Help:        "Round-trip time of individual probes in seconds.",
			ConstLabels: constLabels,
			Buckets:     []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.0},
		}),

		// Connectivity
		ReconnectionsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_reconnections_total",
			Help:        "Total number of reconnections to the orchestrator.",
			ConstLabels: constLabels,
		}),
		DecodeErrorsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_decode_errors_total",
			Help:        "Total number of JSON decode errors when reading directives. Watch if approaching MaxConsecutiveDecodeErrors.",
			ConstLabels: constLabels,
		}),
		WriteErrorsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_write_errors_total",
			Help:        "Total number of errors when writing FIEs to the orchestrator.",
			ConstLabels: constLabels,
		}),

		// Throughput / Resource
		PDGoroutines: factory.NewGauge(prometheus.GaugeOpts{
			Name:        "retina_agent_pd_goroutines",
			Help:        "Current number of goroutines processing probing directives. Abnormal growth indicates a slow prober or fies channel backpressure.",
			ConstLabels: constLabels,
		}),

		// Caracal Pipeline
		CorrelationFailuresTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_correlation_failures_total",
			Help:        "Total number of caracal results that could not be matched to an in-flight probe. Rising rate indicates timing or key-building issues.",
			ConstLabels: constLabels,
		}),
		DuplicateProbesTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_duplicate_probes_total",
			Help:        "Total number of duplicate probes rejected. High rate suggests poor directive randomization.",
			ConstLabels: constLabels,
		}),
		WriteQueueDepth: factory.NewGauge(prometheus.GaugeOpts{
			Name:        "retina_agent_write_queue_depth",
			Help:        "Current number of probe requests queued for caracal. Indicates backpressure on the caracal write pipeline.",
			ConstLabels: constLabels,
		}),
		InFlightProbes: factory.NewGauge(prometheus.GaugeOpts{
			Name:        "retina_agent_inflight_probes",
			Help:        "Current number of probes awaiting a result from caracal. Abnormal growth means results are not coming back.",
			ConstLabels: constLabels,
		}),
		StaleProbesCleanedTotal: factory.NewCounter(prometheus.CounterOpts{
			Name:        "retina_agent_stale_probes_cleaned_total",
			Help:        "Total number of probes removed by the cleanup loop. High rate means probes regularly time out before a result arrives.",
			ConstLabels: constLabels,
		}),

		// Responsible Probing
		ReplyAddressTypeTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name:        "retina_agent_reply_address_type_total",
			Help:        "Total number of probe replies by address type. A spike in private replies could indicate probes staying inside GCP's internal network.",
			ConstLabels: constLabels,
		}, []string{"type"}),
		ICMPReplyTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name:        "retina_agent_icmp_reply_total",
			Help:        "Total number of ICMP replies by type and code. A rising port_unreachable rate means we are hitting end systems rather than routers.",
			ConstLabels: constLabels,
		}, []string{"type", "code"}),
	}
}
