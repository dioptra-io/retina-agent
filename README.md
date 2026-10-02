# Retina Agent

Retina Agent receives probing directives (PDs) from the Retina orchestrator, probes with [caracal](https://github.com/dioptra-io/caracal), and reports forwarding info elements (FIEs).

This is the `research-v1.1.0` branch, a rewrite of the agent. It differs from `main`. [DOCS.md](DOCS.md) describes how the code works.

## Overview

For every PD the agent sends two probes toward the destination, one at the near TTL and one at the near TTL plus one, and reports which address answered each.

```
orchestrator ──TCP──► session ──PD queue──► prober ──stdin──► caracal
orchestrator ◄──TCP── session ◄──FIE queue── prober ◄──stdout── caracal
```

- One TCP connection to the orchestrator: a JSON line each way for the handshake, then PDs and FIEs as CSV lines.
- One long-lived caracal process. It outlives connections: when the orchestrator is lost the agent reconnects with backoff, and what is queued is handled on the next connection.
- Every PD the prober takes gets exactly one FIE, complete as soon as both probes are answered, or with what came once the probe timeout has passed.
- No metrics endpoint. The agent logs its counters in a `Stats` line every 10 seconds and at shutdown.

## Build and run

Go 1.26.1 is needed, and for real probing caracal v0.15.4 in `PATH` with raw socket privileges.

```bash
go build -o retina-agent .
```

```bash
RETINA_SECRET=... ./retina-agent -id agent-1 -address localhost:50050
```

The Dockerfile builds an image with the agent and caracal v0.15.4.

## Configuration

| Setting | Default | Description |
| --- | --- | --- |
| `-id` | `agent-1` | Agent identifier presented to the orchestrator |
| `-address` | `localhost:50050` | Orchestrator address, `host:port` |
| `-max-pd-rate` | `10000` | Most PDs taken from the orchestrator and probed per second |
| `-max-in-flight-pds` | `40000` | Most PDs held at once, from receiving a PD to sending its FIE (0 for no limit) |
| `RETINA_SECRET` (environment) | empty | Shared secret presented in the handshake |

Every other field of the configuration has a flag too:

| Flags | What they set |
| --- | --- |
| `-orchestrator-connect-timeout`, `-orchestrator-handshake-timeout` | Time limits of a connection attempt |
| `-orchestrator-keepalive-idle`, `-orchestrator-keepalive-interval`, `-orchestrator-keepalive-count` | TCP keepalive of the orchestrator connection |
| `-orchestrator-write-buffer-size`, `-orchestrator-flush-period`, `-orchestrator-shutdown-flush-timeout` | Buffering of the FIEs sent to the orchestrator |
| `-orchestrator-reconnect-min-backoff`, `-orchestrator-reconnect-max-backoff` | Wait between attempts to connect again |
| `-pd-queue-size`, `-fie-queue-size`, `-discard-queued-on-disconnect` | The queues between the session and the prober |
| `-caracal-path`, `-caracal-batch-size`, `-caracal-log-level`, `-caracal-rate-limiting-method` | The caracal executable and its options |
| `-caracal-probe-timeout`, `-caracal-write-buffer-size`, `-caracal-stop-timeout` | How the prober uses caracal |
| `-stats-period` | How often the counters are logged |

`./retina-agent -h` lists them with their defaults, which are the values of the one config literal in [main.go](main.go). The `RETINA_*` environment variables of the previous agent, other than `RETINA_SECRET`, are not read.

## Testing

```bash
make test
```

```bash
make smoke
```

`make smoke` builds the agent and runs it between `scripts/mock-orchestrator.sh` and `scripts/mock-caracal.sh`, once on a single connection and once with the connection dropped part of the way. It passes when every PD comes back as a FIE. Arguments for the mock orchestrator go in `SMOKE_ARGS`, and the `MOCK_CARACAL_*` variables configure the mock caracal's replies:

```bash
make smoke SMOKE_ARGS="--count 2000 --seed 7"
```

```bash
make scenarios
```

`make scenarios` runs `tests/scenarios.sh`, which takes the agent through a set of scenarios with the same two mocks: a burst of PDs, PDs above the agent's rate, an orchestrator that stops reading FIEs or reads them slowly, lost and late replies, lines that are not PDs, dropped connections, a stop while PDs are in flight, caracal dying, and a rejected secret. `./tests/scenarios.sh --list` describes them, and `SCENARIOS="burst malformed"` runs only some.

The scripts need bash 5, and the mock orchestrator an OpenBSD-style `nc`. Nothing is probed for real: the mock caracal sends no packets.

## Wire protocol

The handshake is one JSON line each way (`{"agent_id":...,"secret":...}`, answered by `{"authenticated":...,"message":...}`). After it:

```text
# orchestrator → agent
id,"destination",near_ttl,protocol,first_half_word,second_half_word

# agent → orchestrator
id,capture_unix,"near_address",near_delta,"far_address",far_delta
```

The protocol is the IP protocol number: 1 (ICMP), 17 (UDP) or 58 (ICMPv6). The half-words are the UDP source and destination ports; for ICMP only the first tells flows apart. A probe without a reply is reported as `"",0`. The deltas are whole seconds between a reply's capture and `capture_unix`.

## License

MIT License - see [LICENSE](LICENSE) for details
