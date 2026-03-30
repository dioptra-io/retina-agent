# Retina Agent

Retina Agent executes coordinated network probes to infer forwarding behavior across distributed vantage points, producing forwarding information elements (FIEs) for topology analysis.

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
┌──────▼──────────────────────────────┐
│         Retina Agent                │
│                                     │
│  ┌────────┐  ┌──────────┐  ┌──────┐ │
│  │ Reader │─▶│Processor │─▶│Writer│ │
│  └────────┘  └─────┬────┘  └──────┘ │
│                    │                │
│              ┌─────▼─────┐          │
│              │  Prober   │          │
│              │ (caracal) │          │
│              └───────────┘          │
└─────────────────────────────────────┘
```

**Three-stage pipeline:**
1. **Reader**: Receives `ProbingDirective` messages from orchestrator
2. **Processor**: Executes two probes per directive (near TTL, far TTL) in parallel, sends FIE once both complete or time out
3. **Writer**: Sends `ForwardingInfoElement` results back to orchestrator

**Key features:**
- Non-blocking probe execution (thousands of concurrent probes, bounded by `--write-queue-size` and OS limits)
- Automatic reconnection with exponential backoff
- Graceful shutdown on SIGINT/SIGTERM

## Quick Start

### Prerequisites

- Go 1.24.4
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

## Testing End-to-End

Use the mock orchestrator to test the complete pipeline:
```bash
# Terminal 1: Start mock orchestrator
go run test/mock_orchestrator.go

# Terminal 2: Start agent with mock prober
./retina-agent --id agent-1 --address localhost:50050 --prober-type mock
```

## Configuration

### Main Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--id` | `agent-1` | Agent identifier |
| `--address` | `localhost:50050` | Orchestrator address (host:port) |
| `--prober-type` | `caracal` | Prober: `caracal` or `mock` |
| `--prober-path` | (searches PATH) | Path to prober executable |
| `--prober-arg` | | Additional argument to pass to the prober (repeatable) |
| `--write-queue-size` | `1000` | Prober write queue buffer size |
| `--cleanup-interval` | `10s` | Prober stale probe cleanup interval |
| `--pds-buffer` | `100` | Directives channel buffer size |
| `--fies-buffer` | `100` | FIEs channel buffer size |
| `--read-deadline` | `10s` | Read timeout for orchestrator connection |
| `--write-deadline` | `5s` | Write timeout for orchestrator connection |
| `--probe-timeout` | `5s` | Timeout for individual probe responses |
| `--max-reconnect-backoff` | `5m` | Maximum wait time between reconnection attempts |
| `--max-consecutive-decode-errors` | `3` | Max consecutive decode errors before reconnecting (0 to disable) |
| `--log-level` | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `--metrics-addr` | `:9312` | Address to expose Prometheus metrics on |

See `--help` for all options.

## How It Works

### Processing Model

For each `ProbingDirective`:

1. **Launch two probes concurrently**:
   - Near probe: TTL = `directive.NearTTL`
   - Far probe: TTL = `directive.NearTTL + 1`
2. **Correlate results** by destination, protocol, header fields, TTL, and send timestamp
3. **Always send a FIE**: if a probe times out, the corresponding `NearInfo` or `FarInfo` field is nil

### Caracal Integration

The caracal prober uses a high-throughput pipeline:
- Multiple goroutines queue probe requests (non-blocking)
- Single writer goroutine sends to caracal stdin (CSV format)
- Single reader goroutine receives from caracal stdout (CSV format)
- Results correlated back to waiting goroutines via shared map

### Error Handling

- **Network errors**: Trigger reconnection with exponential backoff
- **Decode errors**: Log and skip (reconnect after `--max-consecutive-decode-errors` consecutive)
- **Probe timeouts**: Expected behavior, FIE sent with nil NearInfo/FarInfo
- **Context cancellation**: Clean shutdown

## Development

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

## Observability

Metrics are exposed at `--metrics-addr` (default `:9312`) in Prometheus format, covering pipeline throughput (directives received, FIEs sent), probe outcomes (success/timeout/error rates, RTT distribution), connectivity (reconnections, decode errors), and caracal internals (queue depth, in-flight probes, correlation failures). See `internal/agent/metrics.go` for the full list.

## License

MIT