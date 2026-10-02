// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dioptra-io/retina-agent/internal/retina"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Agent error", slog.Any("err", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// The defaults. The keepalive values are the orchestrator's defaults for
	// agent connections. The secret is read from the environment only, so that
	// it does not show up in the process list.
	config := &retina.Config{
		ID: "agent-1",
		Orchestrator: retina.OrchestratorConfig{
			Address:              "localhost:50050",
			Secret:               os.Getenv("RETINA_SECRET"),
			ConnectTimeout:       5 * time.Second,
			HandshakeTimeout:     5 * time.Second,
			KeepAliveIdle:        30 * time.Second,
			KeepAliveInterval:    10 * time.Second,
			KeepAliveCount:       3,
			WriteBufferSize:      64 * 1024,
			FlushPeriod:          100 * time.Millisecond,
			ShutdownFlushTimeout: time.Second,
			ReconnectMinBackoff:  time.Second,
			ReconnectMaxBackoff:  30 * time.Second,
		},
		Prober: retina.ProberConfig{
			PDQueueSize:               1024,
			FIEQueueSize:              1024,
			DiscardQueuedOnDisconnect: false,
			MaxPDRate:                 10_000, // caracal sends at most 22,000 packets per second
			MaxInFlightPDs:            40_000, // twice what is probed within one probe timeout
			Caracal: retina.CaracalProberConfig{
				Path: "caracal",

				// Caracal's options. An empty or zero value leaves the
				// option out, so that caracal uses its default.
				BatchSize:          128,
				LogLevel:           "info",
				RateLimitingMethod: "sleep", // "auto" cost caracal 21% of an e2-small vCPU at 3,100 PDs/s

				ProbeTimeout:    2 * time.Second,
				WriteBufferSize: 64 * 1024,
				StopTimeout:     2 * time.Second,
			},
		},
		StatsPeriod: 10 * time.Second,
	}
	// Every field of the config has a flag, with the value above as its
	// default.
	orchestrator, prober, caracal := &config.Orchestrator, &config.Prober, &config.Prober.Caracal
	flag.StringVar(&config.ID, "id", config.ID, "Unique identifier of this agent")
	flag.DurationVar(&config.StatsPeriod, "stats-period", config.StatsPeriod, "Interval at which the agent logs its counters (0 for never)")

	flag.StringVar(&orchestrator.Address, "address", orchestrator.Address, "Address of the orchestrator")
	flag.DurationVar(&orchestrator.ConnectTimeout, "orchestrator-connect-timeout", orchestrator.ConnectTimeout, "Time a connection attempt to the orchestrator may take")
	flag.DurationVar(&orchestrator.HandshakeTimeout, "orchestrator-handshake-timeout", orchestrator.HandshakeTimeout, "Time the handshake with the orchestrator may take")
	flag.DurationVar(&orchestrator.KeepAliveIdle, "orchestrator-keepalive-idle", orchestrator.KeepAliveIdle, "Idle time before TCP keepalive probes are sent on the orchestrator connection")
	flag.DurationVar(&orchestrator.KeepAliveInterval, "orchestrator-keepalive-interval", orchestrator.KeepAliveInterval, "Time between TCP keepalive probes on the orchestrator connection")
	flag.IntVar(&orchestrator.KeepAliveCount, "orchestrator-keepalive-count", orchestrator.KeepAliveCount, "Unanswered TCP keepalive probes before the orchestrator connection is closed")
	flag.IntVar(&orchestrator.WriteBufferSize, "orchestrator-write-buffer-size", orchestrator.WriteBufferSize, "Size in bytes of the buffer FIEs are written to before being sent")
	flag.DurationVar(&orchestrator.FlushPeriod, "orchestrator-flush-period", orchestrator.FlushPeriod, "Interval at which buffered FIEs are sent to the orchestrator; a FIE is delayed by at most this long")
	flag.DurationVar(&orchestrator.ShutdownFlushTimeout, "orchestrator-shutdown-flush-timeout", orchestrator.ShutdownFlushTimeout, "Time the agent has to send its last FIEs when it is stopped")
	flag.DurationVar(&orchestrator.ReconnectMinBackoff, "orchestrator-reconnect-min-backoff", orchestrator.ReconnectMinBackoff, "Wait before the first attempt to connect again")
	flag.DurationVar(&orchestrator.ReconnectMaxBackoff, "orchestrator-reconnect-max-backoff", orchestrator.ReconnectMaxBackoff, "Longest wait between attempts to connect again")

	flag.IntVar(&prober.MaxPDRate, "max-pd-rate", prober.MaxPDRate, "Maximum number of PDs taken from the orchestrator and probed per second")
	flag.IntVar(&prober.MaxInFlightPDs, "max-in-flight-pds", prober.MaxInFlightPDs, "Maximum number of PDs held at once, from receiving a PD to sending its FIE (0 for no limit)")
	flag.IntVar(&prober.PDQueueSize, "pd-queue-size", prober.PDQueueSize, "Number of received PDs that may wait to be probed")
	flag.IntVar(&prober.FIEQueueSize, "fie-queue-size", prober.FIEQueueSize, "Number of FIEs that may wait to be written to the orchestrator")
	flag.BoolVar(&prober.DiscardQueuedOnDisconnect, "discard-queued-on-disconnect", prober.DiscardQueuedOnDisconnect, "Discard the queued PDs and FIEs when the orchestrator connection ends, instead of handling them on the next one")

	flag.StringVar(&caracal.Path, "caracal-path", caracal.Path, "The caracal executable; a name without a slash is looked up in PATH")
	flag.IntVar(&caracal.BatchSize, "caracal-batch-size", caracal.BatchSize, "Caracal's --batch-size (0 for caracal's default)")
	flag.StringVar(&caracal.LogLevel, "caracal-log-level", caracal.LogLevel, "Caracal's --log-level (empty for caracal's default)")
	flag.StringVar(&caracal.RateLimitingMethod, "caracal-rate-limiting-method", caracal.RateLimitingMethod, "Caracal's --rate-limiting-method (empty for caracal's default)")
	flag.DurationVar(&caracal.ProbeTimeout, "caracal-probe-timeout", caracal.ProbeTimeout, "Time a PD waits for its replies before its FIE is sent without them")
	flag.IntVar(&caracal.WriteBufferSize, "caracal-write-buffer-size", caracal.WriteBufferSize, "Size in bytes of the buffer probes are written to before being sent to caracal")
	flag.DurationVar(&caracal.StopTimeout, "caracal-stop-timeout", caracal.StopTimeout, "Time caracal is given to exit once it has been told to stop")
	flag.Parse()

	stop := context.AfterFunc(ctx, func() { logger.Info("Shut down signal detected") })
	defer stop()

	agent, err := retina.NewAgent(config, logger)
	if err != nil {
		return err
	}

	return agent.Run(ctx)
}
