# Capacity of the agent path

What one control plane relays between browsers and agents, measured on
2026-10-23. The earlier published figures (200 concurrent sessions on one
gateway) were taken on the retired gateway's SSH path; these replace them.

Two harnesses, both in the tree, both skipped unless asked for:

```bash
# Whole sessions: browser → control plane → agent → login shell, and back.
ARGUS_TEST_DATABASE_URL=postgres://… ARGUS_CAPACITY=1 \
  go test ./internal/control/ -run CapacityOfTheAgentPath -v -timeout 30m

# One tunnel, both transports, with a round trip added between the ends.
ARGUS_TUNNEL_CAPACITY=1 go test ./internal/tunnel/ -run Capacity -v
```

The session harness's knobs are in the header of
`internal/control/capacity_test.go` (how many sessions, how many agents, which
transport, how long each is held).

## Concurrent sessions

Every session is the whole thing: a ticket, the WebSocket, a grant the agent
verifies itself, a real login shell on a pty, the recording opened before the
shell runs, keystrokes typed and their answers timed, `exit`, and a sealed
chain head in the closing message. A session that does not end with a chain
head counts as failed.

All sessions are opened at the same moment, which is the worst case (a
control plane restart, or a team opening terminals for an incident). Each one
then types a command every 250 ms. Halfway through, eight of them print 16 MB
each as fast as they can, and the rest keep typing.

One machine, 4 vCPU (Xeon 2.1 GHz), 15 GB. The control plane, Postgres, 50
agents, every shell and the load generator all share it, so the open times
below are mostly the cost of starting hundreds of login shells on four cores
at once; on a real fleet each host starts its own. Sessions are spread evenly
over the agents (the 10-session rows used 10 agents).

QUIC (the default):

| sessions | sealed | open p50 / p95 | keystroke p50 / p95 / p99 | keystroke while 8 print in bulk | control plane heap | goroutines |
|---:|---:|---|---|---|---:|---:|
| 10 | 10/10 | 39 / 58 ms | 1.1 / 2.2 / 3.6 ms | 1.4 / 2.1 / 9.4 ms | 7 MB | 100 |
| 100 | 100/100 | 660 / 697 ms | 1.6 / 4.9 / 17 ms | 1.7 / 51 / 82 ms | 19 MB | 710 |
| 250 | 250/250 | 1.36 / 1.77 s | 1.7 / 5.9 / 110 ms | 2.5 / 67 / 91 ms | 33 MB | 1460 |
| 500 | 500/500 | 2.08 / 3.46 s | 1.9 / 7.1 / 134 ms | 4.0 / 77 / 100 ms | 59 MB | 2710 |
| 1000 | 1000/1000 | 4.33 / 6.81 s | 2.3 / 14 / 692 ms | 7.2 / 105 / 137 ms | 110 MB | 5210 |

HTTPS fallback (yamux over TLS):

| sessions | sealed | open p50 / p95 | keystroke p50 / p95 / p99 | keystroke while 8 print in bulk | control plane heap | goroutines |
|---:|---:|---|---|---|---:|---:|
| 10 | 10/10 | 62 / 62 ms | 0.9 / 1.8 / 3.4 ms | 1.2 / 1.8 / 2.9 ms | 6 MB | 105 |
| 100 | 100/100 | 447 / 605 ms | 1.2 / 3.5 / 49 ms | 1.0 / 55 / 140 ms | 15 MB | 755 |
| 250 | 250/250 | 1.11 / 1.67 s | 1.4 / 5.8 / 79 ms | 1.6 / 54 / 83 ms | 30 MB | 1505 |
| 500 | 500/500 | 2.10 / 3.47 s | 1.3 / 17 / 91 ms | 2.0 / 55 / 91 ms | 59 MB | 2755 |
| 1000 | 1000/1000 | 3.84 / 7.06 s | 1.5 / 120 / 1666 ms | 6.1 / 125 / 172 ms | 107 MB | 5255 |

- **Every session at every level opened, ran and sealed.** No panics, no
  stuck streams, nothing left open after `exit`.
- **The control plane costs about 110 KB of heap and five goroutines per open
  terminal**, linear from 10 to 1000. Heap and goroutine figures are this
  process's (control plane plus the load generator's WebSocket clients); the
  agents and shells run in a separate process and are not in them.
- **During a 500-session run the control plane's process used 0.4 of one
  core** (7.1 s of CPU over 17.7 s, from a CPU profile, load generator
  included). It is not what limits the rows above; the shells are.
- **Typing stays at 1–2 ms median at every level.** The tails grow with load
  because every process is on the same four cores; the eight bulk printers are
  `base64` in a shell, and they compete for CPU with everything else.
- Aggregate bulk output through terminals was 29–45 MB/s, bounded by the pty
  and `base64`, not by the tunnel (see below for what a tunnel carries).

### One server's limit

**An agent takes at most 64 streams at once**: sessions, file browsers and the
control plane's 30-second ping together. Measured on one host: 60 sessions all
open; at 70, 64 open and six are refused with a reason the console shows
("this server already has 64 sessions and file transfers open through Argus…").
It is a bound on purpose (`TunnelConfig.MaxStreams`), so a flood of opens
cannot pin a host's memory.

## What one tunnel carries

Both ends are the real ones (`DialUpgrade`, `DialQUIC`, and the configurations
the agent and control plane use). Between them is a proxy that holds every
byte for half the round trip in each direction and does nothing else: no loss
and no bandwidth cap, so what limits a stream is the transport's own window.
Bulk is the agent writing 32 KB terminal-output frames; echo is one keystroke
frame each way, alone and then while a bulk stream runs on the same tunnel.

| round trip | transport | 1 stream | 8 streams | echo p50 | echo p99 | echo p99 under bulk |
|---|---|---:|---:|---:|---:|---:|
| loopback | HTTPS | 447 MB/s | 663 MB/s | 0.12 ms | 0.76 ms | 1.8 ms |
| loopback | QUIC | 210 MB/s | 196 MB/s | 0.16 ms | 0.33 ms | 1.9 ms |
| 20 ms | HTTPS | 22.8 MB/s | 171 MB/s | 20.9 ms | 23.1 ms | 22.5–24 ms |
| 20 ms | QUIC | 29–38 MB/s | 24–36 MB/s | 21.0 ms | 22.4 ms | 25–26 ms |
| 80 ms | HTTPS | 6.1 MB/s | 46 MB/s | 81.2 ms | 83–85 ms | 83–192 ms |
| 80 ms | QUIC | 15–16 MB/s | 27–37 MB/s | 81.8 ms | 83 ms | 85 ms |

Ranges are across three runs where they differed. A keystroke costs the round
trip and almost nothing else, on both transports, even with another stream
saturating the same tunnel. The one noisy figure is the HTTPS tail under bulk
at 80 ms.

## What changed because of these numbers

1. **HTTPS fallback: 4× per-stream throughput.** yamux's default 256 KB window
   held one stream to 1.5 MB/s at an 80 ms round trip (5.9 MB/s at 20 ms), so a
   download or a large `cat` through a Cloudflare Tunnel crawled while QUIC
   carried ten times that. The window is now 1 MB: 6.1 MB/s at 80 ms, 22.8 MB/s
   at 20 ms. It is not larger because the window is also what a stalled reader
   makes the other side buffer: on the control plane a slow browser holds at
   most 1 MB of its session's output. Either side's upgrade helps the direction
   it receives, and the wire format is unchanged, so old and new agents and
   control planes still talk to each other.

2. **Burst opens no longer queue on the audit log one commit at a time.** Every
   session writes `terminal.opened` to the hash-chained audit log before the
   browser is told it is ready, and every append was its own transaction behind
   one lock. With 500 opening at once the median one waited 760 ms for the audit
   write alone. Appends that arrive while one is being written are now
   committed together in the next transaction, in arrival order: one lock, one
   read of the head, one commit. Each caller still returns only once its own
   event is committed. Open p50 at 500 sessions went from 3.70 s to 2.08 s, and
   at 1000 from 7.17 s to 4.33 s. A bad event in a batch fails alone; the rest
   are rewritten one at a time.

3. **Output frames no longer allocate.** `WriteFrame` built every frame in a
   fresh buffer: 40 KB of garbage for each 32 KB a session printed. It reuses
   pooled buffers now (`BenchmarkWriteFrame`: 10.3 µs and 1 alloc → 0.9 µs and
   0 allocs per frame).

4. **The per-host refusal says what to do.** "too many streams" became a
   sentence an operator can act on.

## What the numbers do not justify

**WebRTC or a direct browser-to-agent path.** The relay is not where the time
goes: a keystroke's answer comes back in 1–2 ms through browser, control plane,
tunnel, agent, pty and shell, and the control plane spends 0.4 of a core on 500
sessions. What a direct path would save is the extra distance when the control
plane is far from both the person and the server, which a single-machine
measurement cannot show either way. Worth revisiting with a measurement from a
deployment where the control plane is a long way from its users.

## Not measured

- Real packet loss and bandwidth caps. The delay proxy models neither, so
  QUIC's advantage over TCP under loss, and how a larger window queues behind a
  slow link, are not in these tables.
- Two control-plane replicas, or the control plane on its own machine.
- Remote Desktop and the Windows agent. Every figure here is a Linux
  terminal; a desktop session carries far more bytes and has not been loaded.
- Long soaks. The longest run held 1000 sessions for 20 seconds.
