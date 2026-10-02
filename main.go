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
			Address:             "localhost:50050",
			Secret:              os.Getenv("RETINA_SECRET"),
			ConnectTimeout:      5 * time.Second,
			HandshakeTimeout:    5 * time.Second,
			KeepAliveIdle:       30 * time.Second,
			KeepAliveInterval:   10 * time.Second,
			KeepAliveCount:      3,
			WriteBufferSize:     64 * 1024,
			FlushPeriod:         100 * time.Millisecond,
			ReconnectMinBackoff: time.Second,
			ReconnectMaxBackoff: 30 * time.Second,
		},
		Prober: retina.ProberConfig{
			PDQueueSize:               1024,
			FIEQueueSize:              1024,
			DiscardQueuedOnDisconnect: false,
			Mock: retina.MockProberConfig{
				Delay:       100 * time.Millisecond,
				MaxInflight: 100_000,
			},
		},
	}
	flag.StringVar(&config.ID, "id", config.ID, "Unique identifier of this agent")
	flag.StringVar(&config.Orchestrator.Address, "address", config.Orchestrator.Address, "Address of the orchestrator")
	flag.Parse()

	stop := context.AfterFunc(ctx, func() { logger.Info("Shut down signal detected") })
	defer stop()

	agent, err := retina.NewAgent(config, logger)
	if err != nil {
		return err
	}

	return agent.Run(ctx)
}
