// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

// Package retina implements the Retina agent.
package retina

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
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
	// StatsPeriod is how often the agent logs its counters. Zero means never.
	StatsPeriod time.Duration `json:"stats_period"`
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
	if c.StatsPeriod < 0 {
		return fmt.Errorf("stats period cannot be negative: got %v", c.StatsPeriod)
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
	// The counters of the stats log line, all since the agent started:
	// connections that passed the handshake, PDs and malformed PD lines
	// received, and FIEs written to a connection.
	connections  atomic.Uint64
	pdsReceived  atomic.Uint64
	pdsMalformed atomic.Uint64
	fiesSent     atomic.Uint64
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
		prober: NewCaracalProber(&config.Prober.Caracal, logger),
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

	statsDone := make(chan struct{})
	go func() {
		defer close(statsDone)
		a.logStats(runCtx)
	}()

	a.runSessions(runCtx)

	err := <-proberDone
	cancel()
	<-statsDone
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

	// At shutdown the last FIEs are sent before the connection is closed,
	// unless it is still in its handshake.
	var established atomic.Bool
	stop := context.AfterFunc(ctx, func() {
		if established.Load() {
			a.sendLastFIEs(conn)
		}
		end(ctx.Err())
	})
	defer stop()

	if err := conn.Handshake(a.config.ID); err != nil {
		return 0, err
	}
	established.Store(true)
	a.connections.Add(1)
	start := time.Now()
	a.logger.Info("Connected to orchestrator", slog.String("remote_addr", conn.RemoteAddr().String()))

	// The queues are emptied at both ends of the session: the prober went on
	// filling the FIE queue while there was no connection.
	if a.config.Prober.DiscardQueuedOnDisconnect {
		a.discardQueues()
		defer a.discardQueues()
	}

	var group sync.WaitGroup
	group.Go(func() { a.sendFIEs(conn, end, done) })
	group.Go(func() { a.flushOrchestrator(conn, end, done) })
	a.receivePDs(conn, end, done)
	group.Wait()
	return time.Since(start), sessionErr
}

// sendFIEs buffers the prober's FIEs for the flusher. A FIE it has taken when
// the connection fails is lost. It returns when done is closed, and ends the
// session when the connection fails.
func (a *Agent) sendFIEs(conn *OrchestratorConn, end func(error), done <-chan struct{}) {
	for {
		select {
		case fie := <-a.fies:
			if err := conn.SendFIE(&fie); err != nil {
				end(err)
				return
			}
			a.fiesSent.Add(1)
		case <-done:
			return
		}
	}
}

// receivePDs hands the orchestrator's PDs to the prober. A full PD queue
// blocks it, which slows the orchestrator down. A line that is not a PD is
// logged and skipped. It returns when done is closed, and ends the session
// when the connection fails.
func (a *Agent) receivePDs(conn *OrchestratorConn, end func(error), done <-chan struct{}) {
	for {
		pd, err := conn.ReceivePD()
		if errors.Is(err, ErrMalformedPD) {
			a.pdsMalformed.Add(1)
			a.logger.Warn("Skipping PD", slog.Any("err", err))
			continue
		}
		if err != nil {
			end(err)
			return
		}
		a.pdsReceived.Add(1)
		select {
		case a.pds <- pd:
		case <-done:
			return
		}
	}
}

// sendLastFIEs sends the FIEs that wait in the queue and those already
// buffered, within the shutdown flush timeout. It is called at shutdown,
// before the connection is closed. FIEs of PDs still in flight are not waited
// for.
func (a *Agent) sendLastFIEs(conn *OrchestratorConn) {
	err := conn.SetWriteDeadline(deadline(a.config.Orchestrator.ShutdownFlushTimeout))
	for err == nil {
		select {
		case fie := <-a.fies:
			if err = conn.SendFIE(&fie); err == nil {
				a.fiesSent.Add(1)
			}
			continue
		default:
		}
		err = conn.Flush()
		break
	}
	if err != nil {
		a.logger.Warn("Cannot send the last FIEs", slog.Any("err", err))
	}
}

// logStats logs the agent's counters every stats period, and once more when
// ctx is done. The counters are totals since the agent started; the queue
// sizes are those of the moment.
func (a *Agent) logStats(ctx context.Context) {
	if a.config.StatsPeriod <= 0 {
		return
	}
	ticker := time.NewTicker(a.config.StatsPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
		}
		attrs := []slog.Attr{
			slog.Uint64("connections", a.connections.Load()),
			slog.Uint64("pds_received", a.pdsReceived.Load()),
			slog.Uint64("pds_malformed", a.pdsMalformed.Load()),
			slog.Uint64("fies_sent", a.fiesSent.Load()),
			slog.Int("pd_queue", len(a.pds)),
			slog.Int("fie_queue", len(a.fies)),
		}
		if prober, ok := a.prober.(statsProber); ok {
			attrs = append(attrs, prober.stats()...)
		}
		a.logger.LogAttrs(context.Background(), slog.LevelInfo, "Stats", attrs...)
		if ctx.Err() != nil {
			return
		}
	}
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
