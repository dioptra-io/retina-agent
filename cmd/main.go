package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/dioptra-io/retina-agent/pkg/agent"
)

func main() {
	address := flag.String("address", ":50050", "Address of the orchestrator")
	id := flag.String("id", "agent-1", "ID of the agent")
	seed := flag.Int64("seed", 42, "Seed for the generator")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := agent.Run(ctx, *id, *address, *seed); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
