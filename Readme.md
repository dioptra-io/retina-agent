# Retina Agent

Network probing agent for the Retina distributed measurement system.

## Overview

The agent connects to an orchestrator via TCP, receives probing directives, executes network probes, and returns forwarding information elements (FIEs).

**Part of the Retina system:**
- **Generator**: Creates probing directives
- **Orchestrator**: Distributes directives to agents, collects FIEs
- **Agent**: Executes network probes (this component)

## Architecture
```
┌─────────────┐
│Orchestrator │
└──────┬──────┘
       │ TCP (JSON over newline-delimited stream)
       │
┌──────▼──────────────────────────┐
│         Retina Agent            │
│                                 │
│  ┌────────┐  ┌──────────┐  ┌──────┐│
│  │ Reader │─▶│Processor │─▶│Writer││
│  └────────┘  └─────┬────┘  └──────┘│
│                    │                │
│              ┌─────▼─────┐          │
│              │  Prober   │          │
│              │ (caracal) │          │
│              └───────────┘          │
└─────────────────────────────────────┘
```

**Three-stage pipeline:**
1. **Reader**: Receives `ProbingDirective` messages from orchestrator
2. **Processor**: Executes two probes per directive (near TTL, far TTL) in parallel, sends FIE when both complete
3. **Writer**: Sends `ForwardingInfoElement` results back to orchestrator

**Key features:**
- Non-blocking probe execution (thousands of concurrent probes)
- Automatic reconnection with exponential backoff
- Graceful shutdown on SIGINT/SIGTERM

## Quick Start

### Prerequisites

- Go 1.21+
- For production: [caracal](https://github.com/dioptra-io/caracal) and raw socket privileges

### Installation
```bash
git clone https://github.com/dioptra-io/retina-agent
cd retina-agent
go build -o retina-agent ./cmd/retina-agent
```

### Running with Mock Prober
```bash
./retina-agent --id agent-1 --address localhost:50050 --prober-type mock
```

**Example output:**
```
2026/01/21 17:02:54 Agent agent-1: Connected to orchestrator at localhost:50050
2026/01/21 17:02:54 Agent agent-1: ← Directive for 8.8.8.8 (TTL 10 → 11)
2026/01/21 17:02:54 Agent agent-1: → FIE for 8.8.8.8 | Near(TTL10) | Far(TTL11)
2026/01/21 17:02:55 Agent agent-1: ← Directive for 1.1.1.1 (TTL 15 → 16)
2026/01/21 17:02:55 Agent agent-1: → FIE for 1.1.1.1 | Near(TTL15) | Far(TTL16)
```

## Testing End-to-End

Use the mock orchestrator to test the complete pipeline:
```bash
# Terminal 1: Start mock orchestrator
go run test/mock_orchestrator.go

# Terminal 2: Start agent with mock prober
./retina-agent --id agent-1 --address localhost:50050 --prober-type mock
```

You should see directives flowing in and FIEs flowing out in both terminals.

## Configuration

### Main Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--id` | `agent-1` | Agent identifier |
| `--address` | `localhost:50050` | Orchestrator address (host:port) |
| `--prober-type` | `caracal` | Prober: `caracal` or `mock` |
| `--prober-path` | (searches PATH) | Path to prober executable |
| `--probe-timeout` | `5s` | Timeout for probe responses |
| `--directives-buffer` | `100` | Directives channel buffer |
| `--fies-buffer` | `100` | FIEs channel buffer |
| `--max-consecutive-decode-errors` | `3` | Max decode errors before reconnecting |

See `--help` for all options.

### Advanced Configuration

**Caracal-specific arguments** can be set programmatically via `Config.ProberArgs`:
```go
cfg.ProberArgs = []string{"--n-packets", "3", "--interface", "eth0"}
```
Not exposed as CLI flag for MVP - modify code if needed.

## How It Works

### Directive Processing

For each `ProbingDirective`:

1. **Launch two probes concurrently**:
   - Near probe: TTL = `directive.NearTTL`
   - Far probe: TTL = `directive.NearTTL + 1`
2. **Correlate results** by destination, protocol, header fields, TTL, and timestamp
3. **If both succeed**: Build and send FIE
4. **If either times out**: Discard (no FIE)

### Caracal Prober Architecture

The caracal prober uses a high-throughput pipeline:
- Multiple goroutines queue probe requests (non-blocking)
- Single writer goroutine sends to caracal stdin (CSV format)
- Single reader goroutine receives from caracal stdout (CSV format)
- Results correlated back to waiting goroutines via shared map
- Supports thousands of concurrent probes without blocking

### Error Handling

- **Network errors**: Trigger reconnection with exponential backoff
- **Decode errors**: Log and skip (reconnect after 3 consecutive)
- **Probe timeouts**: Expected behavior, no FIE created
- **Context cancellation**: Clean shutdown

## Development

### Project Structure
```
retina-agent/
├── cmd/retina-agent/     # Main entry point
├── internal/agent/
│   ├── agent.go          # Core pipeline logic
│   ├── config.go         # Configuration
│   ├── prober.go         # Prober interface
│   ├── caracal_prober.go # Caracal implementation
│   ├── mock_prober.go    # Mock for testing
│   └── agent_test.go     # Tests
└── test/
    └── mock_orchestrator.go  # For end-to-end testing
```

### Running Tests
```bash
# All tests
go test ./...

# Specific test
go test -v ./internal/agent -run TestAgentPipeline

# With race detection
go test -race ./...
```

### Adding a New Prober

1. Implement the `Prober` interface:
```go
type Prober interface {
    Probe(ctx context.Context, pd *api.ProbingDirective, ttl uint8) (*ProbeResult, error)
    Close() error
}
```

2. Add to `createProber()` in `agent.go`:
```go
case "myprober":
    return NewMyProber(cfg), nil
```

3. Use it:
```bash
./retina-agent --prober-type myprober
```

## Troubleshooting

### Agent keeps reconnecting

**Cause**: Orchestrator is unreachable.

**Check**: `nc -zv orchestrator.example.com 50050`

### No FIEs produced

**Cause**: Probes timing out (expected with mock prober's 10% timeout rate).

**Check logs**: You should see directives received (←) but some won't produce FIEs (→).

### "unknown prober type"

**Fix**: Use `--prober-type mock` or `--prober-type caracal`

### "permission denied" (caracal)

**Fix**: Run as root or grant capabilities:
```bash
sudo setcap cap_net_raw+ep /path/to/caracal
```

## Design Decisions

### Why two probes per directive?

Each FIE contains consecutive hop information (near and far TTL) needed for topology analysis.

### Why both must succeed?

Partial FIEs complicate downstream processing. Clean failure (no FIE) is simpler than partial success.

### Why non-blocking probes?

Sequential probing is too slow. Each directive spawns a goroutine that launches both probes in parallel and waits for results. This allows processing thousands of directives concurrently without blocking the main pipeline.

### Why interface for Prober?

Allows testing with mock prober and easy addition of new implementations without changing agent code.

### Why separate TTL parameter?

Passing TTL separately (rather than embedding in the directive) allows probing multiple hops with the same directive without duplicating the directive structure. This keeps the API simple and flexible.

## License

MIT