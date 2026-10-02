# retina-agent — how it works

Internal documentation of the code on `research-v1.1.0`. For the system-level picture and the orchestrator side, see `retina-orchestrator/DOCS.md`.

## 1. What the agent does

It dials the orchestrator over TCP, authenticates with one JSON line each way, and then exchanges CSV lines on that connection:

- orchestrator → agent, a probing directive (PD): `id,"destination",near_ttl,protocol,first_half_word,second_half_word`
- agent → orchestrator, a forwarding info element (FIE): `id,capture_unix,"near_address",near_delta,"far_address",far_delta`

For every PD it sends two probes toward the destination, one at `near_ttl` and one at `near_ttl + 1`, and reports the address that answered each. A probe without a reply is reported as `"",0`. `capture_unix` is when the agent made the FIE, and each delta is the whole seconds between that reply's capture and `capture_unix`. PD IDs are 32-bit.

## 2. Process layout

`main.go` holds every default in one `retina.Config` literal. Two flags exist, `-id` and `-address`; the secret is read from `RETINA_SECRET`. Logs are JSON on the standard output. SIGINT and SIGTERM stop the agent cleanly.

`Agent.Run` (`internal/retina/agent.go`) starts the prober, the stats loop and the session loop. The prober runs for as long as the agent does; connections come and go around it. If the prober stops, the agent stops with an error.

```
conn ──► receivePDs ──pds chan──► Prober.Run ──fies chan──► sendFIEs ──► buffer ──► conn
                                                                flushOrchestrator ──┘
```

### Sessions

`runSessions` keeps one connection at a time. When it is lost or cannot be made, the agent waits and connects again. The wait starts at `ReconnectMinBackoff` (1 s), doubles up to `ReconnectMaxBackoff` (30 s), has 20% jitter either way, and starts over once a connection has lasted 10 s. Every failure is retried, including a rejected secret.

`runSession` dials (`ConnectTimeout` 5 s), does the handshake (`HandshakeTimeout` 5 s) and then runs three loops until the first error:

- `receivePDs` reads PD lines into the PD queue (`PDQueueSize` 1024). It takes them no faster than `MaxPDRate` and only while fewer than `MaxInFlightPDs` are in flight (see below); a full queue blocks it too. PDs it does not take stay with the orchestrator, which is slowed down; nothing is dropped. Blank lines are ignored. A line that is not a PD, and a PD that cannot be probed (a protocol other than 1, 17 or 58, or near TTL 255), are logged, counted and skipped. A line longer than the read buffer, and any read error, ends the session.
- `sendFIEs` takes FIEs from the FIE queue (`FIEQueueSize` 1024) and writes them to a buffer (`WriteBufferSize` 64 KB).
- `flushOrchestrator` sends the buffer every `FlushPeriod` (100 ms); it is also sent whenever it fills up.

Neither direction has a deadline: an idle orchestrator is fine, and one that stops reading makes the agent wait. A dead orchestrator is detected by TCP keepalive (30 s idle, 3 probes 10 s apart), which matches the orchestrator's own values.

### The two limits on PDs

- `MaxPDRate` (10,000, flag `-max-pd-rate`) is the sustained rate PDs are taken at. After a pause, a tenth of a second of PDs is taken at once. Caracal's own rate is set a tenth above it (22,000 packets per second), so caracal does not hold probes back in normal operation and only spreads out a burst.
- `MaxInFlightPDs` (40,000, flag `-max-in-flight-pds`, zero for no limit) bounds the PDs the agent holds: a counter goes up when a PD is received and down when its FIE is taken to be written to the orchestrator. At the limit the agent stops receiving. This is what bounds memory, and stops the probing, when the orchestrator reads FIEs slower than it sends PDs.

### What survives a lost connection

PDs and FIEs in the two queues, and PDs in flight in the prober, are kept and handled on the next connection. A FIE already written to the connection's buffer when it fails is lost. With `DiscardQueuedOnDisconnect` (default false) both queues are emptied at each end of a session instead; PDs the prober has already taken are not discarded.

### Shutdown

When the agent is stopped while connected, it sends the FIEs waiting in the queue and those in the buffer before it closes the connection, within `ShutdownFlushTimeout` (1 s). FIEs of PDs still in flight are not waited for.

## 3. Probers

`Prober` (`internal/retina/prober.go`) has one method, `Run(ctx, pds, fies)`: take PDs, write one FIE per PD. The agent always uses the caracal prober. To run without probing, `scripts/mock-caracal.sh` stands in for caracal itself. The tests of the agent use a fake prober that lives in the test files.

### CaracalProber (`caracal_prober.go`)

Starts one caracal process (`Path`, looked up in `PATH`) with the three options of `CaracalProberConfig` (`--batch-size`, `--log-level`, `--rate-limiting-method`; a zero value leaves one out) and a `--probing-rate` derived from the prober's `MaxPDRate`: two packets per PD, plus a tenth. Caracal is also always given `--n-packets 1 --sniffer-wait-time 1 --meta-round 1 --filter-min-ttl 1 --filter-max-ttl 255`: these are its v0.15.4 defaults, passed explicitly so that a later version with other defaults behaves the same, except `--filter-min-ttl`, which is 1 so that caracal does not send the wake line described below. Its remaining options are never passed. The header caracal writes first is checked against that of v0.15.4. Four loops run until caracal stops or the agent does:

| Loop | Role |
| --- | --- |
| `writeProbes` | registers each PD in the table and writes its two probes to caracal's standard input as `dst_addr,src_port,dst_port,ttl,protocol`. Probes are buffered while PDs keep coming and sent at once when none is waiting. While PDs are in flight and no probe was written for 100 ms, it writes a wake line |
| `readReplies` | reads caracal's output, gives each reply to a record of the table, and sends the FIE of a PD as soon as both of its probes are answered |
| `expirePDs` | every 100 ms sends the FIEs of the PDs whose `ProbeTimeout` (2 s) has passed, with the replies that came |
| `logOutput` | logs caracal's standard error with `source=caracal` |

If caracal exits, the prober returns an error and the agent stops.

### The wake line

Caracal v0.15.4 keeps the replies it captures in its output buffer and only hands them over when it reads its input. This is not documented; it was observed with the real binary on 2026-10-02, and neither `SIGINT` nor `SIGTERM` makes it hand them over. While probes keep coming, replies arrive at once. When they stop, as at the end of a single-issuance measurement, the replies to the last probes would stay in caracal until it exits, and their PDs would be reported unanswered.

So while PDs are in flight and no probe was written for 100 ms, `writeProbes` writes the line `0.0.0.0,0,0,0,udp`. Caracal reads it, which hands over its replies, and does not send it: its TTL of 0 is below `--filter-min-ttl 1`. Caracal counts these lines in `filtered_low_ttl`. In a continuous measurement none is written. `scripts/mock-caracal.sh` holds its replies in the same way.

### The table (`caracal_table.go`)

A reply does not say which line it answers: it carries the probe's protocol, destination, ports and TTL. The table is keyed by `(protocol, dst_addr, src_port, dst_port, ttl)`, one key per probe. The destination port is zero for ICMP and ICMPv6, both in the key and in the line written to caracal.

- Two PDs can make the same probe: the far probe of near TTL `h` is the near probe of near TTL `h+1` of the same flow. So a key has a list of records.
- Each issuance of a PD has its own record. A PD issued again while in flight gets a second record and a second FIE.
- One reply goes to one record: the one with the earliest flush time, then the lowest near TTL, then the one registered first.
- A record's timeout starts at its flush time, which is taken after the write to caracal returns. It is when caracal received the probes, not when they went on the wire.
- Whether a reply is on time is told by caracal's capture timestamp, not by when the agent reads it: a reply captured within the timeout of the flush time is accepted however long it waited to be read. This relies on the capture timestamps being Unix microseconds of the clock the agent uses.
- A record whose timeout has passed is only expired once no reply captured in time can still be on its way: when the reader of caracal's output has read a reply captured after the record's timeout, or when caracal was written to after the record's timeout, at least one expiry period (100 ms) ago, and the reader has been idle for 10 ms. So when the orchestrator stops reading FIEs and the prober pauses, the PDs in flight keep their replies.

Consequences:

- The agent takes PDs no faster than `MaxPDRate`, so probes do not queue in caracal's input while their timeout runs. Without that limit the wait is bounded by what the input pipe holds (about 2,300 probes), which only exceeds the timeout at rates below about 600 PDs per second.
- The table holds at most `MaxInFlightPDs` records.
- When the reply to a probe shared by two PDs is lost, one of the two PDs is reported incomplete.

## 4. Stats log line

There are no metrics. Every `StatsPeriod` (10 s, zero disables it) and once more at shutdown the agent logs `Stats`, with totals since it started:

| Field | Meaning |
| --- | --- |
| `connections` | connections that passed the handshake |
| `pds_received`, `pds_malformed`, `pds_invalid` | PDs received, lines that were not PDs, and PDs that cannot be probed |
| `fies_sent` | FIEs written to a connection |
| `pd_queue`, `fie_queue` | current sizes of the two queues |
| `in_flight`, `in_flight_max` | PDs the agent holds now, and the most it has held |
| `pds_probed`, `pds_unprobeable` | PDs registered in the table, and PDs that could not be probed |
| `pds_in_flight` | PDs currently waiting for their FIE |
| `replies_matched`, `replies_unmatched` | replies given to a PD, and replies no PD waited for |
| `replies_undecodable` | lines of caracal's output that were not replies |
| `fies_complete`, `fies_incomplete` | FIEs made with both replies, and FIEs made at the timeout |

The fields from `pds_probed` on come from the caracal prober. `replies_unmatched` rising together with `fies_incomplete` means replies arrive but too late or for the wrong key, not that the network is silent.

## 5. Interaction with the orchestrator

- **Orchestrator silent**: fine indefinitely; keepalive detects a dead peer in about a minute.
- **Orchestrator stops reading FIEs, or reads them slowly**: the agent's writes wait and the FIE queue fills. The PDs in flight keep their replies. Once `MaxInFlightPDs` are held the agent stops reading PDs, which slows the orchestrator down. Nothing is dropped and the connection stays up.
- **Agent slow to read PDs**: the orchestrator has no send deadline and drops nothing; it sends the overdue PDs once the agent reads again.
- **PD batching**: the orchestrator sends PDs in groups, so they arrive in bursts.

## 6. Repo map

| Path | Contents |
| --- | --- |
| `main.go` | defaults, the two flags, signal handling |
| `internal/retina/agent.go` | `Config`, `Agent`, session loop, shutdown flush, stats line |
| `internal/retina/orchestrator_client.go` | `OrchestratorConn`: handshake, PD and FIE lines |
| `internal/retina/prober.go` | `Prober` interface, `ProberConfig` |
| `internal/retina/caracal_prober.go` | caracal process, probe writing, reply parsing |
| `internal/retina/caracal_table.go` | matching of replies to PDs, expiry |
| `internal/retina/types.go` | `PD`, `FIE` |
| `scripts/mock-caracal.sh` | stand-in for caracal that sends no packets |
| `scripts/mock-orchestrator.sh` | stand-in for the orchestrator, for one agent |
| `scripts/smoke-test.sh` | runs the agent between the two mocks (`make smoke`) |
