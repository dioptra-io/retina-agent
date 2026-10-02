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

	config := &retina.Config{}
	flag.StringVar(&config.ID, "id", "agent-1", "Unique identifier of this agent")
	flag.StringVar(&config.Orchestrator.Address, "address", "localhost:50050", "Address of the orchestrator")
	flag.Parse()

	// The secret is read from the environment only, so that it does not show
	// up in the process list.
	config.Orchestrator.Secret = os.Getenv("RETINA_SECRET")

	stop := context.AfterFunc(ctx, func() { logger.Info("Shut down signal detected") })
	defer stop()

	agent, err := retina.NewAgent(config, logger)
	if err != nil {
		return err
	}

	return agent.Run(ctx)
}
