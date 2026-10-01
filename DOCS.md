# retina-agent — how it works

Internal documentation of the code as it stands. For the system-level picture
and the orchestrator side, see `retina-orchestrator/DOCS.md`.

## 1. What the agent does

It dials the orchestrator over TCP, authenticates, and then exchanges
newline-delimited JSON for authentication, followed by compact CSV records on
that single connection:

- orchestrator → agent: `ProbingDirective` (PD)
- agent → orchestrator: `ForwardingInfoElement` (FIE)

After authentication, PD records contain
`id,"destination",near_ttl,protocol,first_half_word,second_half_word`. FIE
records contain `id,capture_unix,"near_address",near_delta,"far_address",far_delta`.
The header half-words preserve ICMP/ICMPv6 correlation values or UDP ports.
Each FIE delta is `capture_unix - received_timestamp` in whole seconds; missing
observations use `"",0`. Sent timestamps are not transmitted, so the
orchestrator reconstructs them equal to received timestamps.

For every PD it sends two probes toward `destination_address` — one at
`near_ttl`, one at `near_ttl + 1` — and reports which router answered each.

## 2. Process layout

`cmd/retina-agent/main.go`: flags (each with a `RETINA_*` env fallback;
`RETINA_SECRET` is env-only), config validation, Prometheus server on
`--metrics-addr` (default `:9312`, the same default as the orchestrator), then
`runWithReconnect`.

`runWithReconnect` (`cmd/retina-agent/main.go`) calls
`agent.Run` in a loop. Any return other than shutdown counts as a lost
connection: `reconnections_total` is incremented and it sleeps with exponential
backoff (1 s, doubling, capped at `--max-reconnect-backoff` = 5 min; reset when
the previous run lasted ≥ 1 s).

`agent.Run` (`internal/agent/agent.go`) per connection attempt:

1. **Creates a new prober** (for caracal: spawns a new subprocess). It is closed
   — the subprocess killed — when `Run` returns, so every reconnect restarts
   caracal.
2. Dials, enables TCP keepalive (30 s idle, 10 s × 3), authenticates (5 s
   deadlines): sends `AuthRequest{agent_id, secret}`, expects
   `AuthResponse{authenticated: true}`.
3. Runs three goroutines in an `errgroup`; the first error tears all down.

```
conn ──► readerLoop ──pds chan──► processorLoop ──fies chan──► writerLoop ──► conn
                                       │ one goroutine per PD
                                       ▼
                                 prober.Probe ×2 (near, far)
```

### readerLoop (`internal/agent/agent.go`)

- Sets a read deadline of `--read-deadline` (10 s) before each CSV record. A
  timeout is **not** an error: it only lets the loop notice shutdown. Partial
  records are retained across those timeout wakeups.
- Real network errors (including EOF) end the run → reconnect.
- Malformed CSV is counted and skipped. The connection is kept by default; a positive `--max-consecutive-decode-errors` (default 0) ends the run after that many in a row.
- Blank records are ignored. This is required at the JSON/CSV boundary because
  the handshake decoder may leave its terminating newline buffered.
- `validatePD` drops PDs with empty agent ID, nil destination, TTL 0 or 255,
  unsupported protocol, or missing next header (`pds_invalid_total`).
- Closes `pds` on exit, which lets the processor finish.

### processorLoop (`internal/agent/agent.go`)

Spawns one goroutine per active PD (`pd_goroutines` gauge). When
`--max-inflight-pds` is positive, a semaphore enforces that limit; when it is
zero, concurrency is unlimited. A full semaphore stops `processorLoop` from
draining `pds`, which propagates TCP backpressure toward the orchestrator.

### processPD (`internal/agent/agent.go`)

Runs the near and far probes concurrently and waits for both.

- Either probe returning an error → **no FIE at all**. `ErrDuplicatePD` is
  silent; other errors are logged and counted as `probes_total{outcome=error}`.
- A probe that timed out gives a nil `NearInfo` / `FarInfo`; the FIE is still
  sent (so an FIE with both sides nil is normal for an unresponsive path).
- `ProductionTimestamp` is the agent's clock at build time. `SourceAddress` is
  never set.

### writerLoop (`internal/agent/agent.go`)

Encodes each FIE as compact CSV and flushes it. There is no write deadline, so when the orchestrator stops reading the writer waits instead of disconnecting; the connection is closed on shutdown to unblock it. Any encode or flush error ends the run → reconnect.

## 3. Probers

`Prober` interface ([prober.go](internal/agent/prober.go)): `Probe(ctx, pd, ttl)`
blocks until reply, internal timeout (`TimedOut=true`, no error), duplicate
(`ErrDuplicatePD`), or ctx cancellation.

### MockProber

Sleeps 10–100 ms, times out 10 % of the time, otherwise replies from the
destination address itself (so near and far are identical).

### caracalProber (`internal/agent/caracal_prober.go`)

Wraps a long-lived `caracal` subprocess (`--prober-path`, extra args via
repeatable `--prober-arg`). Probes go in as CSV on stdin, replies come back as
CSV on stdout; the first stdout line (header) is consumed at startup.

Four internal goroutines in the prober's own `errgroup`:

| Goroutine | Role |
| --- | --- |
| `writerLoop` | takes requests off `writeQueue` (`--write-queue-size`, 1000), writes `dst,first,second,ttl,proto` and flushes |
| `readerLoop` | reads result rows, builds a key, delivers to the waiting `Probe` |
| `logStderr` | forwards caracal stderr as `Info` logs with `source=caracal` |
| `cleanupLoop` | every `--cleanup-interval` removes in-flight entries older than `ProbeTimeout + 5 s` |

**Correlation.** There is no probe ID. A probe is identified by

```
(dst_addr, first_half_word|src_port, second_half_word|dst_port, ttl, protocol, unix_second)
```

- `Probe` registers the key with `unix_second = time.Now().Unix()` **at queue
  time**.
- A reply's key uses `sent = capture_timestamp − rtt` (caracal reports
  microsecond timestamps and RTT in 0.1 ms units) and is looked up at offsets
  `0, −1, +1, −2, +2` seconds.
- No match → `correlation_failures_total` and the reply is discarded; the
  waiting `Probe` then times out after `--probe-timeout` (5 s) and reports
  `TimedOut`.

Consequences:

- If a probe waits more than ~2 s between being queued and caracal actually
  putting it on the wire (full `writeQueue`, caracal's own `--probing-rate`
  limit, stdin pipe backpressure), its reply cannot be correlated and the FIE
  side comes out nil even though the router answered.
- A second probe with the same key within the same second is rejected with
  `ErrDuplicatePD`; the PD yields no FIE. Its sibling probe (the other TTL) has
  normally already been queued and is still sent.
- If caracal sends several packets per probe, only the first reply is
  delivered; the rest are dropped on the full 1-slot result channel.

**Blocking points in `Probe`.** Sending to `writeQueue` has no timeout (only
ctx); the probe timeout only starts once the request is queued.

**Subprocess death.** If caracal exits, `readerLoop` returns an error, which
cancels the prober's group context and stops its `writerLoop`. Nothing
propagates this to the agent's connection `errgroup`: the agent stays connected,
the next ~`write-queue-size` probes time out, and after that every `Probe`
blocks on the full queue until the connection context ends. `pd_goroutines` and
`write_queue_depth` grow; `fies_sent_total` flatlines.

## 4. Metrics (all labelled `agent_id`)

Pipeline: `pds_received_total`, `pds_invalid_total`, `fies_sent_total`,
`channel_depth{channel=pds|fies}`, `pd_goroutines`.
Probes: `probes_total{outcome=success|timeout|error}`, `probe_rtt_seconds`.
Connection: `reconnections_total`, `decode_errors_total`, `write_errors_total`.
Caracal: `correlation_failures_total`, `duplicate_probes_total`,
`write_queue_depth`, `inflight_probes`, `stale_probes_cleaned_total`.
Reply classification: `reply_address_type_total{type}`,
`icmp_reply_total{type,code}`.

Useful identities when something looks off:

- `pds_received − pds_invalid` should approach `fies_sent` + duplicates + probe
  errors (minus what is in flight).
- `probes_total{timeout}` rising together with `correlation_failures_total`
  means replies are arriving but not being matched (timing), rather than the
  network being silent.

## 5. Interaction with the orchestrator — failure behaviours

- **Orchestrator silent**: fine indefinitely; keepalive detects a dead peer in
  about a minute.
- **Orchestrator stops reading FIEs** (its capture queue is full): the agent's writer waits, with no deadline. The FIE channel fills, in-flight PDs stay in flight, and at `--max-inflight-pds` the agent stops reading PDs. Nothing is dropped and the connection stays up.
- **Same agent ID connected twice**: the orchestrator accepts auth and then closes the second connection; that agent logs "authenticated successfully" followed by "connection lost while reading: EOF" and backs off.
- **Agent slow to read PDs**: the orchestrator has no send deadline and drops nothing. It stops issuing this agent's PDs until the agent reads again, then sends the overdue ones in order, each once. Other agents are not affected.
- **PD batching**: the orchestrator buffers PDs and sends them when its buffer fills or every 100 ms by default, so PDs arrive in groups and up to that much after they were due.
- **PD IDs are 32-bit**: a PD whose ID does not fit in `uint32` is counted as a decode error and skipped.

## 6. Repo map

| Path | Contents |
| --- | --- |
| `cmd/retina-agent/main.go` | flags, metrics server, reconnect loop |
| `cmd/mock-orchestrator/` | legacy JSON-data-phase test utility; incompatible with the current CSV data phase |
| `internal/agent/agent.go` | connection, auth, reader/processor/writer |
| `internal/agent/caracal_prober.go` | caracal subprocess pipeline and correlation |
| `internal/agent/mock_prober.go` | simulated prober |
| `internal/agent/config.go` | `Config`, defaults, validation (secret ≥ 16 chars if set) |
| `internal/agent/metrics.go` | Prometheus metrics |
