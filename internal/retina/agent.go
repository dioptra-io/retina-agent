// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

// Package retina implements the Retina agent.
package retina

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// stableSession is how long a connection must last for the reconnect backoff
// to start over.
const stableSession = 10 * time.Second

// Config is the configuration of the agent.
type Config struct {
	// ID is the agent ID presented to the orchestrator.
	ID           string             `json:"id"`
	Orchestrator OrchestratorConfig `json:"orchestrator"`
	Prober       ProberConfig       `json:"prober"`
}

// Validate reports whether the configuration is usable.
func (c *Config) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("id cannot be empty")
	}
	if err := c.Orchestrator.validate(); err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}
	if err := c.Prober.validate(); err != nil {
		return fmt.Errorf("prober: %w", err)
	}
	return nil
}

// Agent receives PDs from the orchestrator, probes, and reports FIEs.
type Agent struct {
	config *Config
	logger *slog.Logger
	prober Prober
	// pds carries the PDs received from the orchestrator to the prober, and
	// fies the prober's FIEs back. Both outlive the connections: what is in
	// them when a connection is lost is handled on the next one.
	pds  chan PD
	fies chan FIE
}

// NewAgent creates an agent from the given configuration.
func NewAgent(config *Config, logger *slog.Logger) (*Agent, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Agent{
		config: config,
		logger: logger,
		prober: NewMockProber(&config.Prober.Mock),
		pds:    make(chan PD, config.Prober.PDQueueSize),
		fies:   make(chan FIE, config.Prober.FIEQueueSize),
	}, nil
}

// Run runs the agent until ctx is done or the prober fails. It returns nil on
// a clean shutdown.
//
// The prober runs for as long as the agent does. Connections to the
// orchestrator come and go around it: a lost connection does not restart it.
func (a *Agent) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	proberDone := make(chan error, 1)
	go func() {
		proberDone <- a.prober.Run(runCtx, a.pds, a.fies)
		// Without a prober there is nothing to stay connected for.
		cancel()
	}()

	a.runSessions(runCtx)

	err := <-proberDone
	if ctx.Err() != nil {
		a.logger.Info("Shutting down")
		return nil
	}
	if err == nil {
		return fmt.Errorf("prober stopped")
	}
	return fmt.Errorf("prober failed: %w", err)
}

// runSessions keeps one connection to the orchestrator at a time, until ctx is
// done. When the connection is lost, or cannot be made, it waits and connects
// again. The wait starts at the min backoff and doubles up to the max backoff;
// it goes back to the min once a connection has lasted stableSession.
func (a *Agent) runSessions(ctx context.Context) {
	config := &a.config.Orchestrator
	backoff := config.ReconnectMinBackoff
	for {
		connected, err := a.runSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected >= stableSession {
			backoff = config.ReconnectMinBackoff
		}

		wait := jitter(backoff)
		a.logger.Warn("Not connected to orchestrator", slog.Any("err", err), slog.String("retry_in", wait.Round(time.Millisecond).String()))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, config.ReconnectMaxBackoff)
	}
}

// runSession connects to the orchestrator and serves the connection until it
// fails or ctx is done. It returns how long the session lasted after the
// handshake, and the error that ended it.
func (a *Agent) runSession(ctx context.Context) (time.Duration, error) {
	conn, err := DialOrchestrator(ctx, &a.config.Orchestrator)
	if err != nil {
		return 0, err
	}

	// end ends the session with its first error. Closing the connection is
	// what unblocks the calls on it, and done what unblocks the waits on the
	// prober's channels.
	var (
		once       sync.Once
		sessionErr error
		done       = make(chan struct{})
	)
	end := func(err error) {
		once.Do(func() {
			sessionErr = err
			close(done)
			_ = conn.Close()
		})
	}
	defer end(nil)

	stop := context.AfterFunc(ctx, func() { end(ctx.Err()) })
	defer stop()

	if err := conn.Handshake(a.config.ID); err != nil {
		return 0, err
	}
	start := time.Now()
	a.logger.Info("Connected to orchestrator", slog.String("remote_addr", conn.RemoteAddr().String()))

	// The queues are emptied at both ends of the session: the prober went on
	// filling the FIE queue while there was no connection.
	if a.config.Prober.DiscardOnDisconnect {
		a.discardQueues()
		defer a.discardQueues()
	}

	var group sync.WaitGroup

	// The sender buffers the prober's FIEs for the flusher. A FIE it has
	// taken when the connection fails is lost.
	group.Go(func() {
		for {
			select {
			case fie := <-a.fies:
				if err := conn.SendFIE(&fie); err != nil {
					end(err)
					return
				}
			case <-done:
				return
			}
		}
	})

	group.Go(func() {
		a.flushOrchestrator(conn, end, done)
	})

	// A full PD queue blocks here, which slows the orchestrator down.
receive:
	for {
		pd, err := conn.ReceivePD()
		if err != nil {
			end(err)
			break
		}
		select {
		case a.pds <- pd:
		case <-done:
			break receive
		}
	}

	group.Wait()
	return time.Since(start), sessionErr
}

// discardQueues drops the PDs and FIEs waiting in the queues.
func (a *Agent) discardQueues() {
	for {
		select {
		case <-a.pds:
		case <-a.fies:
		default:
			return
		}
	}
}

// flushOrchestrator sends the buffered FIEs every flush period, so that FIEs
// are written to the orchestrator in groups, not one by one. It returns when
// done is closed, and ends the session when the connection fails.
func (a *Agent) flushOrchestrator(conn *OrchestratorConn, end func(error), done <-chan struct{}) {
	ticker := time.NewTicker(a.config.Orchestrator.FlushPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := conn.Flush(); err != nil {
				end(err)
				return
			}
		case <-done:
			return
		}
	}
}

// jitter returns the backoff changed by up to 20% either way, so that agents
// that lost the orchestrator together do not all come back together.
func jitter(backoff time.Duration) time.Duration {
	return time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: not a security use
}
