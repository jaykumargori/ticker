# Design notes

How the system is built and why: the broker analysis, stack choice, performance decisions,
measured results and the wire protocol. For feature-level behaviour see
[HOW-IT-WORKS.md](HOW-IT-WORKS.md). For security see [../SECURITY.md](../SECURITY.md).

A working copy of the way broker terminals such as Zerodha Kite or Groww push live prices to
thousands of browsers. A **Go** backend takes in real market data, conflates it, and fans it out
over a **binary WebSocket protocol**. A **SolidJS** frontend renders it with fine-grained
reactivity, at no more than one paint per animation frame.

```
                   ┌──────────────────────── Go server ────────────────────────┐
 Binance WS ──┐    │                                                           │
 (real, free) │    │  feed adapters ──Apply()──► quote table (mutex, dirty set)│
 Finnhub WS ──┼───►│                                   │ every FLUSH_MS        │
 (opt, key)   │    │                                   ▼                       │
 Simulator ───┘    │      frame builder: encode each dirty quote ONCE          │
 (NSE-like)        │      → immutable snapshot []*entry{seq, bytes}            │
                   │      → atomic publish + close(frame.next)  (O(1) wake)    │
                   │                                   │                       │
                   │   per-client writer goroutine ◄───┘                       │
                   │   send entries with seq > lastSent ∩ my subscriptions     │
                   │   (skipped frames are conflated automatically)            │
                   └───────────────────────────┬───────────────────────────────┘
                                               │ binary frames (big-endian)
                                               ▼
                   SolidJS: decode ─► mutate row objects ─► rAF: batch(bump signals)
                            ─► only the changed text nodes update + WAAPI flash
```

---

## 1. Analysis: how Kite and Groww actually do it

| Layer | What brokers do | What this repo does |
|---|---|---|
| Source | Exchange (NSE/BSE) **UDP multicast** tick-by-tick feed over a leased line into colocated feed handlers | Public **Binance** WebSocket (real trades), optional **Finnhub** (US stocks), and an **NSE-like simulator** for load testing |
| Normalisation | Feed handler turns exchange packets into internal quote structs keyed by `instrument_token` | `feed.Tick` → `hub.Apply()` into a dense `[]Quote` indexed by `uint32` token |
| Fan-out | Ticker cluster. A client subscribes to tokens and picks a mode: `ltp` / `quote` / `full` | Same: JSON control `{"a":"subscribe","v":[...]}`, `{"a":"mode","v":["ltp",[...]]}` |
| Wire format | **Kite Ticker is binary**: `[u16 count]` then `[u16 len][packet]…`, big-endian. A packet's length tells you its mode. 1-byte heartbeat | Same framing. LTP = 12 B, FULL = 68 B. The LTP packet is a **prefix** of the FULL packet (zero-copy slice) |
| Rate control | Updates are **conflated**: you get the latest state at a fixed cadence (about 1/s on Kite), not every trade | Global frame cadence (`FLUSH_MS`, default 50 ms). Slow clients skip frames and still converge, because of the per-entry `seq` |
| Client | Batches DOM writes and flashes red or green when the price changes | Decode into plain objects. One `requestAnimationFrame` flush wraps a Solid `batch()`. Web Animations API flash |

**Key point:** a quote screen shows **state**, not an event log. You only ever need the
*latest* price per instrument. That one fact gives you conflation. Conflation then gives you
bounded memory, protection against slow consumers, and CPU cost that grows with the number of
subscriptions instead of with tick rate × subscribers.

## 2. Public API options (critique)

| API | Real-time? | Key | Indian equities? | Verdict |
|---|---|---|---|---|
| **Binance** `data-stream.binance.vision` | Yes, trade by trade | None | No (crypto) | **Default.** Free, no key, high tick rate, open 24/7 so you can always demo it. The market-data-only host works where `binance.com` is blocked. Checked reachable from this machine |
| Finnhub WS | Yes (US trades) | Free key | No | Optional adapter (`FINNHUB_TOKEN`). Data only during US market hours. Free tier caps the symbol count |
| Zerodha Kite Connect | Yes | Paid (about ₹2000/mo) plus a trading account | Yes | The real thing, but paid. The protocol here copies its layout, so writing an adapter would be easy |
| Upstox / Angel SmartAPI | Yes (protobuf / binary) | Free with a trading account plus daily login | Yes | Good for production NSE data. Needs KYC and a TOTP login flow, so it doesn't suit a public demo |
| Yahoo, Alpha Vantage, Twelve Data free tier | Polling or delayed | Varies | Delayed | Not real-time. Rejected |

**Bottom line:** no free, key-less source gives real-time NSE data. Binance delivers real ticks,
and the simulator delivers NSE-shaped symbols and prices (₹0.05 tick size) at any rate you want.
The adapter interface means you can plug in Kite or Upstox later without touching the hub or
the UI.

## 3. Stack choice (critique)

**Backend: Go.**
- The work is I/O fan-out plus small encodes, and that suits goroutines. One goroutine per
  socket is cheap: about 8 KB of stack, and around 10k connections fit in under 200 MB.
- **Rust** (tokio + tungstenite) would cut tail latency (no GC) and memory per connection by
  about 2-3×. But at this tier the bottleneck is the network and the kernel, not CPU. Go
  development speed and its simpler concurrency make it the better default. Choose Rust only if
  you need 100k+ sockets per box or sub-millisecond p99.
- **Python** (asyncio/uvloop) is fine for I/O, but per-message encoding and fan-out is CPU-bound,
  and the GIL caps it at one core per process. You end up running multiple processes behind
  Redis pub/sub. Rejected for the hot path.

**Frontend: SolidJS.**
- A ticker table is the worst case for React. Hundreds of cells change 20×/s, and the VDOM
  re-renders and diffs whole rows. Memoisation helps but is fragile.
- Solid compiles to direct DOM updates driven by signals. A price change touches exactly one
  text node, with no diffing.
- Rendering is also decoupled from the network: decoded data goes into plain objects, and
  signals get bumped once per animation frame inside `batch()`.

## 4. Performance decisions and why

1. **Encode once, fan out many.** Each changed quote is serialised once per frame. Every
   client then appends the same immutable byte slices. Cost per client is `memcpy`, not
   encoding.
2. **O(1) broadcast.** A new frame closes a channel (`frame.next`), and the Go runtime wakes
   every waiting writer. The hub never loops over clients and never blocks on them.
3. **Seq-based conflation.** Every snapshot entry carries the seq of the frame that last
   changed it. A writer sends `entry.seq > lastSent`. A client that was busy writing and missed
   10 frames gets one message with the latest values. No queue builds up per client.
4. **Copy-on-write snapshots.** Each frame copies an `N`-pointer slice (N = instruments, so a
   few KB). Readers never take a lock.
5. **Slow-consumer policy.** Writes have a deadline (`WRITE_TIMEOUT`). A client that can't
   drain gets disconnected, so it never holds server memory.
6. **Binary, big-endian, fixed offsets.** A FULL packet is 68 B. The same data as JSON is about
   250 B and needs parsing. `permessage-deflate` is off on purpose: small binary frames gain
   little from it and it costs CPU on every send.
7. **Client:** decode with `DataView` without allocating per field, apply once per
   `requestAnimationFrame`, cache `Intl.NumberFormat` objects, draw the chart on a canvas
   (no SVG nodes), and flash with WAAPI (no forced reflow from toggling classes).

### Known trade-offs and limits
- **Single node.** For horizontal scale, put the frame builder behind NATS or Redis Streams.
  Each edge node runs its own client writers against a replicated snapshot. The protocol
  doesn't change. Cost: one extra message broker, plus about 1 ms of extra latency.
- `encoding/json` on the Binance ingest is enough for a few thousand msgs/s. For the full
  `!ticker@arr` firehose, switch to a streaming parser (`goccy/go-json` or a hand-rolled one).
- Latency in the UI is measured as `client_now − server_frame_ts`. That's only accurate when
  clocks agree (same machine, or NTP-synced).
- The protocol is float64 throughout. Kite uses int32 paise. Crypto needs ~8 decimals, so f64
  is the simpler universal choice, at a cost of +4 B per field.
- There's no auth yet (no accounts exist). See SECURITY.md for what's required before adding
  any.

## 5. Measured results

Machine: Windows 11, 12 logical cores. The load generator runs **on the same box** and competes
with the server for CPU, so tail latencies here are pessimistic. Feeds were live Binance plus the
sim at about 550 ticks/s in, 20 frames/s out.

| Test | Result |
|---|---|
| `hub.Apply` (1 tick) | **23 ns**, 0 allocs |
| Build a frame with 1000 changed quotes | **63 µs**, 1 alloc per quote |
| Browser (1 tab, 7 rows) | 1.1 ms avg server→decode latency, ≤ 1 render per animation frame |
| 2000 clients × 50 instruments, FULL | 0 failures, 40k msgs/s, ~490k packets/s, 35 MB/s, **p50 1.1 ms / p99 13 ms** |
| 5000 clients × 50, FULL, 200 connects/s ramp | 4985/5000 connected, ~950k packets/s, ~67 MB/s, p50 3 ms. Server RSS about 335 MB (~80 KB/conn) |
| 5000 clients, 625 connects/s ramp | About 20% of connects get a TCP RST |

**Finding:** the limit on a single Windows box is the **connect rate**, not how many connections
the server can hold. A burst of connects overflows the kernel accept backlog before the app ever
sees them, and the server logs no errors. This is the "reconnect storm after a deploy" problem.
Mitigations:
- The client already reconnects with exponential backoff and full jitter.
- On Linux, raise `net.core.somaxconn` and `tcp_max_syn_backlog`.
- In production, terminate connections at an ALB/NLB and roll deploys across instances.

## 6. Wire protocol

**Client → server** (text, JSON, Kite-compatible verbs):
```json
{"a":"subscribe","v":[1,2,3]}
{"a":"unsubscribe","v":[3]}
{"a":"mode","v":["ltp",[1,2]]}      // "ltp" | "full"   (subscribe defaults to full)
```

**Server → client**
- Text: `{"type":"error","data":"..."}`
- Binary, 1 byte: heartbeat (sent if the connection has been idle for 1 s)
- Binary, data frame (big-endian):

```
offset  size  field
0       u16   packet count
2       f64   server send time (unix ms)
10      ...   repeated: u16 packet length, packet bytes

packet (len 12 = LTP, len 68 = FULL)
0   u32 token
4   f64 last traded price        ── LTP packet ends here
12  f64 last traded qty
20  f64 open
28  f64 high
36  f64 low
44  f64 close (previous close / reference for change %)
52  f64 volume
60  f64 exchange timestamp (unix ms)
```

## 7. Deployment notes and risks

- Behind ALB or nginx: raise idle timeouts above the 1 s heartbeat (the defaults are fine).
  Turn on sticky sessions only if you add per-session server state.
- An AWS ALB supports WebSockets natively. Data transfer out is the main cost driver. Rough
  estimate: 10k users × 50 instruments × 20 fps × 68 B ≈ 680 MB/s worst case. Raise `FLUSH_MS`
  to 250-1000 ms (as Kite does) for retail screens. Bandwidth falls in direct proportion.
- **Rollback:** the server is stateless. Redeploy the previous binary, and clients reconnect
  on their own (exponential backoff with jitter) and resubscribe.
