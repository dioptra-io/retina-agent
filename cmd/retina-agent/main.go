// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT

// Command retina-agent is a network measurement agent that connects to
// an orchestrator to receive probing directives and return measurements.
//
// Usage:
//
//	retina-agent [flags]
//
// Example:
//
//	retina-agent -id agent-1 -address orchestrator.example.com:50050 -prober-type caracal
//
// The agent automatically reconnects on connection loss using exponential
// backoff. Press Ctrl+C or send SIGTERM for graceful shutdown.
//
// Available prober types: caracal, mock
//
// Authentication:
//
//	Set RETINA_SECRET environment variable to enable authentication.
//	Leave unset for local testing without authentication.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dioptra-io/retina-agent/internal/agent"
)

var (
	agentID          = flag.String("id", "agent-1", "Unique identifier for this agent")
	orchestratorAddr = flag.String("address", "localhost:50050", "Orchestrator address (host:port)")

	proberType = flag.String("prober-type", agent.ProberTypeCaracal,
		fmt.Sprintf("Prober implementation (%s, %s)", agent.ProberTypeCaracal, agent.ProberTypeMock))
	proberPath = flag.String("prober-path", "", "Path to prober executable (searches PATH if empty)")

	writeQueueSize  = flag.Int("write-queue-size", 1000, "Prober write queue buffer size")
	cleanupInterval = flag.Duration("cleanup-interval", 10*time.Second, "Prober stale probe cleanup interval")

	pdsBufferSize  = flag.Int("pds-buffer", 100, "Directives channel buffer size")
	fiesBufferSize = flag.Int("fies-buffer", 100, "FIEs channel buffer size")

	readDeadline        = flag.Duration("read-deadline", 10*time.Second, "Read timeout for orchestrator connection")
	writeDeadline       = flag.Duration("write-deadline", 5*time.Second, "Write timeout for orchestrator connection")
	probeTimeout        = flag.Duration("probe-timeout", 5*time.Second, "Timeout for individual probe responses")
	maxReconnectBackoff = flag.Duration("max-reconnect-backoff", 5*time.Minute, "Maximum wait time between reconnection attempts")

	maxConsecutiveDecodeErrors = flag.Int("max-consecutive-decode-errors", 3, "Maximum consecutive decode errors before reconnecting (0 to disable)")

	logLevel = flag.String("log-level", "info", "Log level (debug, info, warn, error)")

	// 9312 is retina-agent's allocated port; 9090 is Prometheus itself.
	metricsAddr = flag.String("metrics-addr", ":9312", "Address to expose Prometheus metrics on")
)

// multiFlag allows a flag to be specified multiple times.
type multiFlag []string

func (f *multiFlag) String() string { return strings.Join(*f, ", ") }
func (f *multiFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

var proberArgs multiFlag

func init() {
	flag.Var(&proberArgs, "prober-arg", "Additional argument to pass to the prober (repeatable)")
}

// agentRun is a variable for dependency injection in tests.
var agentRun = agent.Run

func main() {
	flag.Parse()

	logger := newLogger(*logLevel)

	cfg := &agent.Config{
		AgentID:                    *agentID,
		OrchestratorAddr:           *orchestratorAddr,
		Secret:                     os.Getenv("RETINA_SECRET"),
		ProberType:                 *proberType,
		ProberPath:                 *proberPath,
		ProberArgs:                 proberArgs,
		WriteQueueSize:             *writeQueueSize,
		CleanupInterval:            *cleanupInterval,
		PDsBufferSize:              *pdsBufferSize,
		FIEsBufferSize:             *fiesBufferSize,
		ReadDeadline:               *readDeadline,
		WriteDeadline:              *writeDeadline,
		ProbeTimeout:               *probeTimeout,
		MaxReconnectBackoff:        *maxReconnectBackoff,
		MaxConsecutiveDecodeErrors: *maxConsecutiveDecodeErrors,
	}

	if err := cfg.Validate(); err != nil {
		logger.Error("configuration error", slog.Any("err", err))
		os.Exit(1)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := agent.NewMetrics(registry, *agentID)

	srv, err := startMetricsServer(logger, registry, *metricsAddr)
	if err != nil {
		logger.Error("failed to start metrics server", slog.Any("err", err))
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runWithReconnect(ctx, cfg, logger, metrics)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown failed", slog.Any("err", err))
	}
}

// newLogger creates a JSON logger writing to stdout at the given level.
// Falls back to info if the level string is unrecognised.
func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: l,
	}))
}

// startMetricsServer starts an HTTP server exposing Prometheus metrics at /metrics.
// It binds eagerly so that a port conflict is detected before the agent starts.
func startMetricsServer(logger *slog.Logger, registry *prometheus.Registry, addr string) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics server: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	//nolint:gosec // G112: metrics endpoint is internal-only; timeout omitted intentionally
	srv := &http.Server{Handler: mux}

	go func() {
		logger.Info("Starting metrics server", slog.String("addr", addr))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Metrics server failed", slog.Any("err", err))
		}
	}()

	return srv, nil
}

// runWithReconnect wraps agent.Run with exponential backoff reconnection.
//
// The backoff starts at 1 second and doubles on each failure, up to
// MaxReconnectBackoff. It resets after any run that lasted at least
// initialBackoff, treating it as a successful connection. On intentional
// shutdown (SIGINT or SIGTERM), the function returns immediately without
// retrying.
func runWithReconnect(ctx context.Context, cfg *agent.Config, logger *slog.Logger, metrics *agent.Metrics) {
	const (
		initialBackoff = 1 * time.Second
		backoffFactor  = 2
	)

	agentIDAttr := slog.String("agent_id", cfg.AgentID)

	backoff := initialBackoff
	for {
		logger.Info("Connecting",
			agentIDAttr,
			slog.String("address", cfg.OrchestratorAddr))

		start := time.Now()
		err := agentRun(ctx, cfg, logger, metrics)

		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			logger.Info("Shutdown complete", agentIDAttr)
			return
		}

		if time.Since(start) >= initialBackoff {
			backoff = initialBackoff
		}

		metrics.ReconnectionsTotal.Inc()
		logger.Error("Connection lost", agentIDAttr, slog.Any("err", err))
		logger.Info("Reconnecting", agentIDAttr, slog.Duration("backoff", backoff))

		select {
		case <-time.After(backoff):
			backoff *= backoffFactor
			if backoff > cfg.MaxReconnectBackoff {
				backoff = cfg.MaxReconnectBackoff
			}
		case <-ctx.Done():
			logger.Info("Shutdown during reconnect backoff", agentIDAttr)
			return
		}
	}
}
