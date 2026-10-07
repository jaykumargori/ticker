import { batch, createSignal, type Accessor, type Setter } from "solid-js";
import { applyPacket, decodeFrame, emptyQuote, type Quote } from "./protocol";

export interface Instrument {
  token: number;
  exchange: string;
  segment: "EQ" | "INDEX" | "CRYPTO" | string;
  /** ISO currency the instrument is quoted in (USDT reported as USD). */
  currency: string;
  /** Index membership, e.g. ["NIFTY 50", "NIFTY BANK"]. */
  tags?: string[];
  symbol: string;
  name: string;
  decimals: number;
}

/**
 * One row of live state. `q` is a plain mutable object written by the decoder
 * at network rate; `version` is the only reactive part and is bumped at most
 * once per animation frame. Components read `version()` then fields of `q`.
 */
export interface Row {
  ins: Instrument;
  q: Quote;
  version: Accessor<number>;
  setVersion: Setter<number>;
  /** LTP at last render, used to derive the flash direction. */
  shownLtp: number;
  /** +1 up, -1 down, 0 unchanged since the previous render. */
  dir: number;
}

export type Status = "connecting" | "open" | "closed";
export type Mode = "full" | "ltp";

export interface NetStats {
  msgsPerSec: number;
  packetsPerSec: number;
  bytesPerSec: number;
  latencyAvg: number;
  latencyMax: number;
  rendersPerSec: number;
  rowUpdatesPerSec: number;
}

const zeroStats: NetStats = {
  msgsPerSec: 0, packetsPerSec: 0, bytesPerSec: 0, latencyAvg: 0, latencyMax: 0, rendersPerSec: 0, rowUpdatesPerSec: 0,
};

// Wall clock, because the server stamps frames with its wall clock.
// performance.timeOrigin + now() drifts from it over long sessions (it went negative in testing).
const now = () => Date.now();

export class TickerFeed {
  readonly rows = new Map<number, Row>();
  readonly status: Accessor<Status>;
  readonly stats: Accessor<NetStats>;
  readonly mode: Accessor<Mode>;
  readonly lastError: Accessor<string>;

  private setStatus: Setter<Status>;
  private setStats: Setter<NetStats>;
  private setModeSig: Setter<Mode>;
  private setLastError: Setter<string>;

  private ws: WebSocket | null = null;
  private refs = new Map<number, number>(); // token → subscriber refcount
  private pending = new Set<number>(); // tokens changed since last render
  private rafId = 0;
  private retry = 0;
  private retryTimer = 0;
  private stopped = false;
  private flushListeners = new Set<(changed: ReadonlySet<number>) => void>();

  // per-second counters
  private c = { msgs: 0, packets: 0, bytes: 0, latSum: 0, latN: 0, latMax: 0, renders: 0, rowUpdates: 0 };
  private statsTimer = 0;

  constructor(private url: string, instruments: Instrument[]) {
    [this.status, this.setStatus] = createSignal<Status>("connecting");
    [this.stats, this.setStats] = createSignal<NetStats>(zeroStats);
    [this.mode, this.setModeSig] = createSignal<Mode>("full");
    [this.lastError, this.setLastError] = createSignal("");
    for (const ins of instruments) {
      const [version, setVersion] = createSignal(0, { equals: false });
      this.rows.set(ins.token, { ins, q: emptyQuote(), version, setVersion, shownLtp: 0, dir: 0 });
    }
  }

  /** Adds rows for instruments that appeared after startup (dynamic catalog). */
  addInstruments(list: Instrument[]): Instrument[] {
    const added: Instrument[] = [];
    for (const ins of list) {
      if (this.rows.has(ins.token)) continue;
      const [version, setVersion] = createSignal(0, { equals: false });
      this.rows.set(ins.token, { ins, q: emptyQuote(), version, setVersion, shownLtp: 0, dir: 0 });
      added.push(ins);
    }
    return added;
  }

  start() {
    this.stopped = false;
    this.connect();
    this.statsTimer = window.setInterval(() => this.rollStats(), 1000);
  }

  stop() {
    this.stopped = true;
    clearTimeout(this.retryTimer);
    clearInterval(this.statsTimer);
    cancelAnimationFrame(this.rafId);
    this.ws?.close();
  }

  /** Ref-counted: several views may watch the same token. */
  subscribe(tokens: number[]) {
    const fresh = tokens.filter((t) => {
      const n = this.refs.get(t) ?? 0;
      this.refs.set(t, n + 1);
      return n === 0 && this.rows.has(t);
    });
    if (fresh.length) this.sendSubscribe(fresh);
  }

  unsubscribe(tokens: number[]) {
    const gone = tokens.filter((t) => {
      const n = this.refs.get(t) ?? 0;
      if (n <= 1) {
        this.refs.delete(t);
        return n === 1;
      }
      this.refs.set(t, n - 1);
      return false;
    });
    if (gone.length) this.send({ a: "unsubscribe", v: gone });
  }

  /**
   * Called once per render flush with the tokens that changed (non-reactive).
   * Used by the alert engine so it costs O(changed) per frame, not O(alerts).
   */
  onFlush(fn: (changed: ReadonlySet<number>) => void): () => void {
    this.flushListeners.add(fn);
    return () => this.flushListeners.delete(fn);
  }

  setMode(mode: Mode) {
    this.setModeSig(mode);
    const all = [...this.refs.keys()];
    if (all.length) this.send({ a: "mode", v: [mode, all] });
  }

  private sendSubscribe(tokens: number[]) {
    // Server sends a FULL snapshot on subscribe, so LTP-mode rows still get
    // their reference close for change% before switching to LTP packets.
    this.send({ a: "subscribe", v: tokens });
    if (this.mode() === "ltp") this.send({ a: "mode", v: ["ltp", tokens] });
  }

  private send(msg: unknown) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(msg));
    // When closed, state lives in `refs` and is replayed on reconnect.
  }

  private connect() {
    this.setStatus("connecting");
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.url);
    } catch (e) {
      this.setLastError(String(e));
      this.scheduleReconnect();
      return;
    }
    ws.binaryType = "arraybuffer";
    this.ws = ws;

    ws.onopen = () => {
      this.retry = 0;
      this.setStatus("open");
      this.setLastError("");
      const all = [...this.refs.keys()];
      if (all.length) this.sendSubscribe(all);
    };
    ws.onmessage = (ev) => this.onMessage(ev.data);
    ws.onclose = () => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.setStatus("closed");
      this.scheduleReconnect();
    };
    ws.onerror = () => this.setLastError("connection error");
  }

  private scheduleReconnect() {
    if (this.stopped) return;
    // Exponential backoff with full jitter avoids a thundering herd after a deploy.
    const base = Math.min(10_000, 500 * 2 ** this.retry++);
    this.retryTimer = window.setTimeout(() => this.connect(), Math.random() * base + 250);
  }

  private onMessage(data: ArrayBuffer | string) {
    if (typeof data === "string") {
      try {
        const m = JSON.parse(data);
        if (m.type === "error") this.setLastError(String(m.data));
      } catch {
        /* ignore malformed text frames */
      }
      return;
    }
    const c = this.c;
    c.msgs++;
    c.bytes += data.byteLength;
    const info = decodeFrame(data, (token, dv, off, len) => {
      const row = this.rows.get(token);
      if (!row) return;
      applyPacket(row.q, dv, off, len);
      this.pending.add(token);
    });
    if (info.packets === -2) {
      this.setLastError("malformed frame");
      return;
    }
    if (info.packets <= 0) return; // heartbeat
    c.packets += info.packets;
    // Same-host latency is below clock resolution and can read slightly negative.
    const lat = Math.max(0, now() - info.serverTs);
    c.latSum += lat;
    c.latN++;
    if (lat > c.latMax) c.latMax = lat;
    // Render is decoupled from the network: at most one flush per frame.
    // While the tab is hidden rAF pauses and `pending` simply conflates.
    if (!this.rafId) this.rafId = requestAnimationFrame(this.flush);
  }

  private flush = () => {
    this.rafId = 0;
    if (!this.pending.size) return;
    this.c.renders++;
    this.c.rowUpdates += this.pending.size;
    batch(() => {
      for (const token of this.pending) {
        const row = this.rows.get(token)!;
        const ltp = row.q.ltp;
        row.dir = row.shownLtp === 0 ? 0 : ltp > row.shownLtp ? 1 : ltp < row.shownLtp ? -1 : 0;
        row.shownLtp = ltp;
        row.setVersion(0);
      }
    });
    for (const fn of this.flushListeners) {
      try {
        fn(this.pending);
      } catch (e) {
        console.error("flush listener failed", e); // one bad listener must not stop rendering
      }
    }
    this.pending.clear();
  };

  private rollStats() {
    const c = this.c;
    this.setStats({
      msgsPerSec: c.msgs,
      packetsPerSec: c.packets,
      bytesPerSec: c.bytes,
      latencyAvg: c.latN ? c.latSum / c.latN : 0,
      latencyMax: c.latMax,
      rendersPerSec: c.renders,
      rowUpdatesPerSec: c.rowUpdates,
    });
    this.c = { msgs: 0, packets: 0, bytes: 0, latSum: 0, latN: 0, latMax: 0, renders: 0, rowUpdates: 0 };
  }
}

export function wsURL(): string {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  return `${proto}://${location.host}/ws`;
}
