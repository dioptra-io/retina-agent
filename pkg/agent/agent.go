// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
)

var (
	ErrInvalidDirective = errors.New("invalid probing directive")
	ErrProbeTimeout     = errors.New("probe timed out")
)

// agent manages a persistent WebSocket connection to the orchestrator,
// receives probing directives, executes probes, and sends back forwarding info elements.
type agent struct {
	mu              sync.Mutex
	agentID         string
	orchestratorURL string
	rnd             *rand.Rand
}

func Run(parent context.Context, agentID string, orchestratorAddr string, seed int64) error {
	g, ctx := errgroup.WithContext(parent)

	a := &agent{
		agentID:         agentID,
		orchestratorURL: "ws://" + orchestratorAddr + "/agent/" + agentID,
		rnd:             rand.New(rand.NewSource(seed)),
	}

	// Connect to orchestrator WebSocket.
	conn, _, err := websocket.DefaultDialer.Dial(a.orchestratorURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	log.Printf("agent %s connected to orchestrator at %s", a.agentID, a.orchestratorURL)

	// Channel for probing directives
	directives := make(chan *api.ProbingDirective, 100)

	// Goroutine: receive probing directives from orchestrator.
	g.Go(func() error {
		defer close(directives)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()

			default:
				var pd api.ProbingDirective
				if err := conn.ReadJSON(&pd); err != nil {
					return err
				}

				// Only process directives meant for this agent
				if pd.AgentID != a.agentID {
					log.Printf("agent %s: ignoring directive for agent %s", a.agentID, pd.AgentID)
					continue
				}

				select {
				case <-ctx.Done():
					return ctx.Err()

				case directives <- &pd:
				}
			}
		}
	})

	// Goroutine: process directives and send ForwardingInfoElements.
	g.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()

			case pd, ok := <-directives:
				if !ok {
					return nil
				}

				fie, err := a.executeProbe(ctx, pd)
				if err != nil {
					if err == ctx.Err() {
						return err
					}
					log.Printf("agent %s: probe failed: %v", a.agentID, err)
					continue
				}

				data, err := json.Marshal(fie)
				if err != nil {
					return err
				}

				a.mu.Lock()
				err = conn.WriteMessage(websocket.TextMessage, data)
				a.mu.Unlock()

				if err != nil {
					return err
				}
			}
		}
	})

	// Goroutine: listen for context cancellation and disconnect.
	g.Go(func() error {
		<-ctx.Done()
		conn.Close()
		return ctx.Err()
	})

	// Wait until all goroutines end.
	if err := g.Wait(); err != nil && err != ctx.Err() && err != io.ErrUnexpectedEOF {
		log.Printf("agent %s stopped with error: %v", a.agentID, err)
		return err
	}

	log.Printf("agent %s stopped", a.agentID)
	return nil
}

// executeProbe executes a probing directive and returns the resulting ForwardingInfoElement.
func (a *agent) executeProbe(ctx context.Context, pd *api.ProbingDirective) (*api.ForwardingInfoElement, error) {

	return &api.ForwardingInfoElement{
		Agent: api.Agent{
			AgentID: a.agentID,
		},
		IPVersion:           pd.IPVersion,
		Protocol:            pd.Protocol,
		DestinationAddress:  pd.DestinationAddress,
		NearInfo:            randomInfo(),
		FarInfo:             randomInfo(),
		ProductionTimestamp: time.Now().UTC(),
	}, nil
}

func randomInfo() api.Info {
	now := time.Now().UTC()

	return api.Info{
		ProbeTTL:          uint8(rand.Intn(255) + 1),
		ReplyAddress:      net.IPv4(byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256))),
		ProbePayloadSize:  uint16(rand.Intn(1400) + 64),
		SentTimestamp:     now.Add(-time.Duration(rand.Intn(5)) * time.Millisecond),
		ReceivedTimestamp: now,
	}
}
