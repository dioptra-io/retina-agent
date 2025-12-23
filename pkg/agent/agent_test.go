// Copyright (c) 2025 Dioptra
// SPDX-License-Identifier: MIT
package agent

import (
	"context"
	"encoding/json"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dioptra-io/retina-commons/pkg/api/v1"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateDirective_Valid(t *testing.T) {
	tests := []struct {
		name string
		pd   *api.ProbingDirective
	}{
		{
			name: "valid IPv4 ICMP",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.ICMP,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("192.168.1.1").To4(),
				NearTTL:            10,
				NextHeader: api.NextHeader{
					ICMPNextHeader: &api.ICMPNextHeader{
						FirstHalfWord:  0x0800,
						SecondHalfWord: 0x0001,
					},
				},
			},
		},
		{
			name: "valid IPv4 UDP",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.UDP,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("10.0.0.1").To4(),
				NearTTL:            15,
				NextHeader: api.NextHeader{
					UDPNextHeader: &api.UDPNextHeader{
						SourcePort:      12345,
						DestinationPort: 33434,
					},
				},
			},
		},
		{
			name: "valid IPv6 ICMPv6",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv6,
				Protocol:           api.ICMPv6,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("2001:db8::1"),
				NearTTL:            20,
				NextHeader: api.NextHeader{
					ICMPv6NextHeader: &api.ICMPv6NextHeader{
						FirstHalfWord:  0x8000,
						SecondHalfWord: 0x0001,
					},
				},
			},
		},
		{
			name: "valid IPv6 UDP",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv6,
				Protocol:           api.UDP,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("2001:db8::2"),
				NearTTL:            25,
				NextHeader: api.NextHeader{
					UDPNextHeader: &api.UDPNextHeader{
						SourcePort:      54321,
						DestinationPort: 33435,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDirective(tt.pd)
			assert.NoError(t, err)
		})
	}
}

func TestValidateDirective_Invalid(t *testing.T) {
	tests := []struct {
		name string
		pd   *api.ProbingDirective
	}{
		{
			name: "nil directive",
			pd:   nil,
		},
		{
			name: "empty agent ID",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.ICMP,
				AgentID:            "",
				DestinationAddress: net.ParseIP("192.168.1.1").To4(),
				NearTTL:            10,
				NextHeader: api.NextHeader{
					ICMPNextHeader: &api.ICMPNextHeader{},
				},
			},
		},
		{
			name: "nil destination address",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.ICMP,
				AgentID:            "agent-1",
				DestinationAddress: nil,
				NearTTL:            10,
				NextHeader: api.NextHeader{
					ICMPNextHeader: &api.ICMPNextHeader{},
				},
			},
		},
		{
			name: "IPv4 with IPv6 address",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.ICMP,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("2001:db8::1"),
				NearTTL:            10,
				NextHeader: api.NextHeader{
					ICMPNextHeader: &api.ICMPNextHeader{},
				},
			},
		},
		{
			name: "IPv6 with IPv4 address",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv6,
				Protocol:           api.ICMPv6,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("192.168.1.1").To4(),
				NearTTL:            10,
				NextHeader: api.NextHeader{
					ICMPv6NextHeader: &api.ICMPv6NextHeader{},
				},
			},
		},
		{
			name: "ICMP with IPv6",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv6,
				Protocol:           api.ICMP,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("2001:db8::1"),
				NearTTL:            10,
				NextHeader: api.NextHeader{
					ICMPNextHeader: &api.ICMPNextHeader{},
				},
			},
		},
		{
			name: "ICMPv6 with IPv4",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.ICMPv6,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("192.168.1.1").To4(),
				NearTTL:            10,
				NextHeader: api.NextHeader{
					ICMPv6NextHeader: &api.ICMPv6NextHeader{},
				},
			},
		},
		{
			name: "ICMP without next header",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.ICMP,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("192.168.1.1").To4(),
				NearTTL:            10,
				NextHeader:         api.NextHeader{},
			},
		},
		{
			name: "UDP without next header",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.UDP,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("192.168.1.1").To4(),
				NearTTL:            10,
				NextHeader:         api.NextHeader{},
			},
		},
		{
			name: "zero TTL",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.ICMP,
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("192.168.1.1").To4(),
				NearTTL:            0,
				NextHeader: api.NextHeader{
					ICMPNextHeader: &api.ICMPNextHeader{},
				},
			},
		},
		{
			name: "invalid protocol",
			pd: &api.ProbingDirective{
				IPVersion:          api.TypeIPv4,
				Protocol:           api.Protocol(99),
				AgentID:            "agent-1",
				DestinationAddress: net.ParseIP("192.168.1.1").To4(),
				NearTTL:            10,
				NextHeader:         api.NextHeader{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDirective(tt.pd)
			assert.ErrorIs(t, err, ErrInvalidDirective)
		})
	}
}

func TestAgent_GetSourceAddress(t *testing.T) {
	a := &agent{
		sourceAddressV4: net.ParseIP("192.168.1.100").To4(),
		sourceAddressV6: net.ParseIP("2001:db8::100"),
	}

	t.Run("IPv4", func(t *testing.T) {
		addr := a.getSourceAddress(api.TypeIPv4)
		assert.Equal(t, a.sourceAddressV4, addr)
	})

	t.Run("IPv6", func(t *testing.T) {
		addr := a.getSourceAddress(api.TypeIPv6)
		assert.Equal(t, a.sourceAddressV6, addr)
	})

	t.Run("invalid version", func(t *testing.T) {
		addr := a.getSourceAddress(api.IPVersion(99))
		assert.Nil(t, addr)
	})
}

func TestAgent_CalculatePayloadSize(t *testing.T) {
	a := &agent{}

	tests := []struct {
		protocol api.Protocol
		expected uint16
	}{
		{api.ICMP, 64},
		{api.ICMPv6, 64},
		{api.UDP, 32},
		{api.Protocol(99), 0},
	}

	for _, tt := range tests {
		t.Run(tt.protocol.String(), func(t *testing.T) {
			pd := &api.ProbingDirective{Protocol: tt.protocol}
			size := a.calculatePayloadSize(pd)
			assert.Equal(t, tt.expected, size)
		})
	}
}

func TestAgent_ExecuteProbe(t *testing.T) {
	a := &agent{
		agentID:         "test-agent",
		sourceAddressV4: net.ParseIP("192.168.1.100").To4(),
		sourceAddressV6: net.ParseIP("2001:db8::100"),
		rnd:             rand.New(rand.NewSource(42)),
		probeTimeout:    100 * time.Millisecond,
	}

	t.Run("valid IPv4 ICMP probe", func(t *testing.T) {
		pd := &api.ProbingDirective{
			IPVersion:          api.TypeIPv4,
			Protocol:           api.ICMP,
			AgentID:            "test-agent",
			DestinationAddress: net.ParseIP("8.8.8.8").To4(),
			NearTTL:            10,
			NextHeader: api.NextHeader{
				ICMPNextHeader: &api.ICMPNextHeader{
					FirstHalfWord:  0x0800,
					SecondHalfWord: 0x0001,
				},
			},
		}

		ctx := context.Background()
		fie, err := a.executeProbe(ctx, pd)

		require.NoError(t, err)
		require.NotNil(t, fie)

		assert.Equal(t, "test-agent", fie.Agent.AgentID)
		assert.Equal(t, api.TypeIPv4, fie.IPVersion)
		assert.Equal(t, api.ICMP, fie.Protocol)
		assert.Equal(t, a.sourceAddressV4, fie.SourceAddress)
		assert.Equal(t, pd.DestinationAddress, fie.DestinationAddress)

		assert.Equal(t, uint8(10), fie.NearInfo.ProbeTTL)
		assert.Equal(t, uint8(11), fie.FarInfo.ProbeTTL)

		assert.NotNil(t, fie.NearInfo.ReplyAddress)
		assert.NotNil(t, fie.FarInfo.ReplyAddress)

		assert.Equal(t, uint16(64), fie.NearInfo.ProbePayloadSize)
		assert.Equal(t, uint16(64), fie.FarInfo.ProbePayloadSize)

		assert.False(t, fie.ProductionTimestamp.IsZero())
		assert.True(t, fie.NearInfo.ReceivedTimestamp.After(fie.NearInfo.SentTimestamp))
		assert.True(t, fie.FarInfo.ReceivedTimestamp.After(fie.FarInfo.SentTimestamp))
	})

	t.Run("valid IPv6 UDP probe", func(t *testing.T) {
		pd := &api.ProbingDirective{
			IPVersion:          api.TypeIPv6,
			Protocol:           api.UDP,
			AgentID:            "test-agent",
			DestinationAddress: net.ParseIP("2001:4860:4860::8888"),
			NearTTL:            15,
			NextHeader: api.NextHeader{
				UDPNextHeader: &api.UDPNextHeader{
					SourcePort:      12345,
					DestinationPort: 33434,
				},
			},
		}

		ctx := context.Background()
		fie, err := a.executeProbe(ctx, pd)

		require.NoError(t, err)
		require.NotNil(t, fie)

		assert.Equal(t, api.TypeIPv6, fie.IPVersion)
		assert.Equal(t, api.UDP, fie.Protocol)
		assert.Equal(t, a.sourceAddressV6, fie.SourceAddress)
		assert.Equal(t, uint16(32), fie.NearInfo.ProbePayloadSize)
	})

	t.Run("context cancelled", func(t *testing.T) {
		pd := &api.ProbingDirective{
			IPVersion:          api.TypeIPv4,
			Protocol:           api.ICMP,
			AgentID:            "test-agent",
			DestinationAddress: net.ParseIP("8.8.8.8").To4(),
			NearTTL:            10,
			NextHeader: api.NextHeader{
				ICMPNextHeader: &api.ICMPNextHeader{},
			},
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		fie, err := a.executeProbe(ctx, pd)
		assert.Nil(t, fie)
		assert.Equal(t, context.Canceled, err)
	})

	t.Run("invalid directive", func(t *testing.T) {
		pd := &api.ProbingDirective{
			IPVersion: api.TypeIPv4,
			Protocol:  api.ICMP,
			AgentID:   "", // Invalid: empty agent ID
		}

		ctx := context.Background()
		fie, err := a.executeProbe(ctx, pd)
		assert.Nil(t, fie)
		assert.ErrorIs(t, err, ErrInvalidDirective)
	})

	t.Run("no source address", func(t *testing.T) {
		agentNoSource := &agent{
			agentID:         "test-agent",
			sourceAddressV4: nil, // No IPv4 source
			rnd:             rand.New(rand.NewSource(42)),
			probeTimeout:    100 * time.Millisecond,
		}

		pd := &api.ProbingDirective{
			IPVersion:          api.TypeIPv4,
			Protocol:           api.ICMP,
			AgentID:            "test-agent",
			DestinationAddress: net.ParseIP("8.8.8.8").To4(),
			NearTTL:            10,
			NextHeader: api.NextHeader{
				ICMPNextHeader: &api.ICMPNextHeader{},
			},
		}

		ctx := context.Background()
		fie, err := agentNoSource.executeProbe(ctx, pd)
		assert.Nil(t, fie)
		assert.ErrorIs(t, err, ErrInvalidDirective)
	})
}

func TestAgent_SendProbe(t *testing.T) {
	a := &agent{
		agentID:      "test-agent",
		rnd:          rand.New(rand.NewSource(42)),
		probeTimeout: 100 * time.Millisecond,
	}

	t.Run("successful probe", func(t *testing.T) {
		pd := &api.ProbingDirective{
			IPVersion: api.TypeIPv4,
			Protocol:  api.ICMP,
		}

		ctx := context.Background()
		info, err := a.sendProbe(ctx, pd, 10)

		require.NoError(t, err)
		require.NotNil(t, info)

		assert.Equal(t, uint8(10), info.ProbeTTL)
		assert.NotNil(t, info.ReplyAddress)
		assert.False(t, info.SentTimestamp.IsZero())
		assert.False(t, info.ReceivedTimestamp.IsZero())
		assert.True(t, info.ReceivedTimestamp.After(info.SentTimestamp))
	})

	t.Run("context cancelled during probe", func(t *testing.T) {
		pd := &api.ProbingDirective{
			IPVersion: api.TypeIPv4,
			Protocol:  api.ICMP,
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		info, err := a.sendProbe(ctx, pd, 10)
		assert.Nil(t, info)
		assert.Equal(t, context.Canceled, err)
	})
}

func TestAgent_SimulateReplyAddress(t *testing.T) {
	a := &agent{
		rnd: rand.New(rand.NewSource(42)),
	}

	t.Run("IPv4 reply", func(t *testing.T) {
		pd := &api.ProbingDirective{IPVersion: api.TypeIPv4}
		addr := a.simulateReplyAddress(pd, 10)

		assert.NotNil(t, addr)
		assert.NotNil(t, addr.To4(), "should be IPv4 address")
	})

	t.Run("IPv6 reply", func(t *testing.T) {
		pd := &api.ProbingDirective{IPVersion: api.TypeIPv6}
		addr := a.simulateReplyAddress(pd, 10)

		assert.NotNil(t, addr)
		assert.Len(t, addr, 16, "should be 16-byte IPv6 address")
	})
}

func TestRun_Integration(t *testing.T) {
	var upgrader = websocket.Upgrader{}
	var receivedFIEs []api.ForwardingInfoElement
	var mu sync.Mutex

	// Create a test directive
	testDirective := &api.ProbingDirective{
		IPVersion:          api.TypeIPv4,
		Protocol:           api.ICMP,
		AgentID:            "test-agent-1",
		DestinationAddress: net.ParseIP("8.8.8.8").To4(),
		NearTTL:            10,
		NextHeader: api.NextHeader{
			ICMPNextHeader: &api.ICMPNextHeader{
				FirstHalfWord:  0x0800,
				SecondHalfWord: 0x0001,
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent" {
			http.NotFound(w, r)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		// Send a few directives
		for i := 0; i < 3; i++ {
			err = conn.WriteJSON(testDirective)
			require.NoError(t, err)
		}

		// Receive ForwardingInfoElements
		for i := 0; i < 3; i++ {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}

			var fie api.ForwardingInfoElement
			err = json.Unmarshal(msg, &fie)
			require.NoError(t, err)

			mu.Lock()
			receivedFIEs = append(receivedFIEs, fie)
			mu.Unlock()
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	addr := strings.TrimPrefix(server.URL, "http://")

	cfg := Config{
		AgentID:          "test-agent-1",
		OrchestratorAddr: addr,
		SourceAddressV4:  net.ParseIP("192.168.1.100").To4(),
		SourceAddressV6:  net.ParseIP("2001:db8::100"),
		Seed:             42,
		ProbeTimeout:     100 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg)
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()

	err := <-done
	assert.True(t, err == nil || err == context.Canceled || err == context.DeadlineExceeded)

	mu.Lock()
	defer mu.Unlock()

	assert.GreaterOrEqual(t, len(receivedFIEs), 1, "should have received at least one FIE")

	for _, fie := range receivedFIEs {
		assert.Equal(t, "test-agent-1", fie.Agent.AgentID)
		assert.Equal(t, api.TypeIPv4, fie.IPVersion)
		assert.Equal(t, api.ICMP, fie.Protocol)
		assert.Equal(t, uint8(10), fie.NearInfo.ProbeTTL)
		assert.Equal(t, uint8(11), fie.FarInfo.ProbeTTL)
	}
}

func TestRun_IgnoresOtherAgentDirectives(t *testing.T) {
	var upgrader = websocket.Upgrader{}
	var receivedFIEs []api.ForwardingInfoElement
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent" {
			http.NotFound(w, r)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		// Send directive for different agent (should be ignored)
		otherDirective := &api.ProbingDirective{
			IPVersion:          api.TypeIPv4,
			Protocol:           api.ICMP,
			AgentID:            "other-agent",
			DestinationAddress: net.ParseIP("8.8.8.8").To4(),
			NearTTL:            10,
			NextHeader: api.NextHeader{
				ICMPNextHeader: &api.ICMPNextHeader{},
			},
		}
		err = conn.WriteJSON(otherDirective)
		require.NoError(t, err)

		// Send directive for our agent
		ourDirective := &api.ProbingDirective{
			IPVersion:          api.TypeIPv4,
			Protocol:           api.ICMP,
			AgentID:            "my-agent",
			DestinationAddress: net.ParseIP("8.8.4.4").To4(),
			NearTTL:            15,
			NextHeader: api.NextHeader{
				ICMPNextHeader: &api.ICMPNextHeader{},
			},
		}
		err = conn.WriteJSON(ourDirective)
		require.NoError(t, err)

		// Should only receive one FIE
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := conn.ReadMessage()
		if err == nil {
			var fie api.ForwardingInfoElement
			json.Unmarshal(msg, &fie)
			mu.Lock()
			receivedFIEs = append(receivedFIEs, fie)
			mu.Unlock()
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	addr := strings.TrimPrefix(server.URL, "http://")

	cfg := Config{
		AgentID:          "my-agent",
		OrchestratorAddr: addr,
		SourceAddressV4:  net.ParseIP("192.168.1.100").To4(),
		Seed:             42,
	}

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg)
	}()

	time.Sleep(600 * time.Millisecond)
	cancel()

	<-done

	mu.Lock()
	defer mu.Unlock()

	// Should have received exactly 1 FIE (for our agent's directive)
	require.Len(t, receivedFIEs, 1)
	assert.Equal(t, "my-agent", receivedFIEs[0].Agent.AgentID)
	assert.Equal(t, uint8(15), receivedFIEs[0].NearInfo.ProbeTTL)
}

func TestRun_ConnectionError(t *testing.T) {
	ctx := context.Background()

	cfg := Config{
		AgentID:          "test-agent",
		OrchestratorAddr: "localhost:99999",
		Seed:             42,
	}

	err := Run(ctx, cfg)
	assert.Error(t, err)
}

func BenchmarkExecuteProbe(b *testing.B) {
	a := &agent{
		agentID:         "bench-agent",
		sourceAddressV4: net.ParseIP("192.168.1.100").To4(),
		sourceAddressV6: net.ParseIP("2001:db8::100"),
		rnd:             rand.New(rand.NewSource(42)),
		probeTimeout:    1 * time.Millisecond, // Fast for benchmarking
	}

	pd := &api.ProbingDirective{
		IPVersion:          api.TypeIPv4,
		Protocol:           api.ICMP,
		AgentID:            "bench-agent",
		DestinationAddress: net.ParseIP("8.8.8.8").To4(),
		NearTTL:            10,
		NextHeader: api.NextHeader{
			ICMPNextHeader: &api.ICMPNextHeader{
				FirstHalfWord:  0x0800,
				SecondHalfWord: 0x0001,
			},
		},
	}

	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = a.executeProbe(ctx, pd)
	}
}

func BenchmarkValidateDirective(b *testing.B) {
	pd := &api.ProbingDirective{
		IPVersion:          api.TypeIPv4,
		Protocol:           api.ICMP,
		AgentID:            "agent-1",
		DestinationAddress: net.ParseIP("192.168.1.1").To4(),
		NearTTL:            10,
		NextHeader: api.NextHeader{
			ICMPNextHeader: &api.ICMPNextHeader{
				FirstHalfWord:  0x0800,
				SecondHalfWord: 0x0001,
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = validateDirective(pd)
	}
}
