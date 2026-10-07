import { createMemo, createSignal, For, onCleanup, onMount, Show, type Accessor } from "solid-js";
import type { Instrument } from "../lib/feed";
import { money } from "../lib/currency";
import { pct, signed } from "../lib/format";

export interface QuoteLite {
  l: number; // ltp
  c: number; // reference close
  v: number; // volume
}

/**
 * Overview panels don't need tick-level freshness, so they poll a compact
 * snapshot (~8 KB) every 2 s instead of streaming all instruments over the
 * socket. Polling pauses while the tab is hidden.
 */
export function createQuotes(): Accessor<Map<number, QuoteLite>> {
  const [quotes, setQuotes] = createSignal(new Map<number, QuoteLite>());
  onMount(() => {
    const ctrl = new AbortController();
    const poll = async () => {
      if (document.hidden) return;
      try {
        const r = await fetch("/api/quotes", { signal: ctrl.signal });
        if (!r.ok) return;
        const rows: ({ k: number } & QuoteLite)[] = await r.json();
        setQuotes(new Map(rows.map((q) => [q.k, q])));
      } catch {
        /* transient: keep last snapshot */
      }
    };
    void poll();
    const id = setInterval(poll, 2000);
    const onVis = () => !document.hidden && void poll();
    document.addEventListener("visibilitychange", onVis);
    onCleanup(() => {
      clearInterval(id);
      ctrl.abort();
      document.removeEventListener("visibilitychange", onVis);
    });
  });
  return quotes;
}

interface Item {
  ins: Instrument;
  ltp: number;
  ch: number;
}

function items(list: Instrument[], quotes: Map<number, QuoteLite>): Item[] {
  const out: Item[] = [];
  for (const ins of list) {
    const q = quotes.get(ins.token);
    if (q && q.l > 0) out.push({ ins, ltp: q.l, ch: pct(q.l, q.c) });
  }
  return out;
}

function Seg<T extends string>(p: { value: T; options: T[]; onChange: (v: T) => void; label: string }) {
  return (
    <div class="seg" role="group" aria-label={p.label}>
      <For each={p.options}>
        {(o) => (
          <button classList={{ on: p.value === o }} onClick={() => p.onChange(o)}>
            {o}
          </button>
        )}
      </For>
    </div>
  );
}

export function TopMovers(p: { instruments: Instrument[]; quotes: Accessor<Map<number, QuoteLite>>; onSelect: (t: number) => void }) {
  type U = "NSE" | "Crypto";
  const [u, setU] = createSignal<U>("NSE");
  const universe = createMemo(() =>
    u() === "NSE" ? p.instruments.filter((i) => i.exchange === "NSE-SIM" && i.segment === "EQ") : p.instruments.filter((i) => i.exchange === "CRYPTO"),
  );
  const sorted = createMemo(() => items(universe(), p.quotes()).sort((a, b) => b.ch - a.ch));
  const gainers = () => sorted().filter((x) => x.ch > 0).slice(0, 6);
  const losers = () => sorted().filter((x) => x.ch < 0).slice(-6).reverse();

  const List = (lp: { title: string; rows: Item[] }) => (
    <div class="movers-col">
      <h3 class="card-sub">{lp.title}</h3>
      <ul class="movers">
        <For each={lp.rows} fallback={<li class="muted small">No data yet</li>}>
          {(x) => (
            <li>
              <button class="mover" onClick={() => p.onSelect(x.ins.token)} title={x.ins.name}>
                <span class="mover-sym">{x.ins.symbol.replace(/USDT$/, "")}</span>
                <span class="mover-ltp">{money(x.ltp, x.ins)}</span>
                <span class={`mover-ch ${x.ch >= 0 ? "up" : "down"}`}>
                  {x.ch >= 0 ? "▲" : "▼"} {signed(x.ch, 2)}%
                </span>
              </button>
            </li>
          )}
        </For>
      </ul>
    </div>
  );

  return (
    <section class="card">
      <header class="card-head">
        <h2>Top movers</h2>
        <Seg label="Market" value={u()} options={["NSE", "Crypto"]} onChange={setU} />
      </header>
      <div class="movers-grid">
        <List title="Gainers" rows={gainers()} />
        <List title="Losers" rows={losers()} />
      </div>
      <p class="small muted movers-foot">% change vs {u() === "NSE" ? "previous close" : "24h reference"} · refreshes every 2 s</p>
    </section>
  );
}

/**
 * Diverging heatmap: two hues around a neutral gray midpoint, clipped at
 * ±clip%. Every tile also shows its signed % with ▲/▼, so it reads without
 * color too.
 */
export function Heatmap(p: { instruments: Instrument[]; quotes: Accessor<Map<number, QuoteLite>>; onSelect: (t: number) => void }) {
  type U = "NIFTY 50" | "NIFTY BANK" | "NIFTY IT" | "Crypto";
  const [u, setU] = createSignal<U>("NIFTY 50");
  const clip = () => (u() === "Crypto" ? 5 : 2);
  const tiles = createMemo(() => {
    const list = u() === "Crypto" ? p.instruments.filter((i) => i.exchange === "CRYPTO").slice(0, 40) : p.instruments.filter((i) => i.tags?.includes(u()));
    return items(list, p.quotes());
  });
  const bg = (ch: number) => {
    const t = Math.max(-1, Math.min(1, ch / clip()));
    const pole = t >= 0 ? "var(--up)" : "var(--down)";
    return `color-mix(in oklab, ${pole} ${Math.round(Math.abs(t) * 70)}%, var(--tile-neutral))`;
  };
  const breadth = createMemo(() => {
    const t = tiles();
    return { up: t.filter((x) => x.ch > 0).length, down: t.filter((x) => x.ch < 0).length };
  });

  return (
    <section class="card heatmap-card">
      <header class="card-head">
        <h2>Heatmap</h2>
        <Seg label="Universe" value={u()} options={["NIFTY 50", "NIFTY BANK", "NIFTY IT", "Crypto"]} onChange={setU} />
      </header>
      <div class="heatmap-meta">
        <span class="small muted">
          <span class="up">▲ {breadth().up}</span> · <span class="down">▼ {breadth().down}</span> of {tiles().length}
        </span>
        <div class="heat-legend" aria-label={`Color scale from −${clip()}% to +${clip()}%`}>
          <span class="small muted">−{clip()}%</span>
          <span class="heat-ramp" />
          <span class="small muted">+{clip()}%</span>
        </div>
      </div>
      <Show when={tiles().length} fallback={<p class="muted small">Loading…</p>}>
        <div class="heatmap" classList={{ sparse: tiles().length <= 12, medium: tiles().length > 12 && tiles().length <= 24 }} role="list">
          <For each={tiles()}>
            {(x) => (
              <button
                class="tile"
                role="listitem"
                style={{ background: bg(x.ch) }}
                title={`${x.ins.name} · ${money(x.ltp, x.ins)}`}
                onClick={() => p.onSelect(x.ins.token)}
              >
                <span class="tile-sym">{x.ins.symbol.replace(/USDT$/, "")}</span>
                <span class="tile-ch">
                  {x.ch >= 0 ? "▲" : "▼"}
                  {signed(x.ch, 2)}%
                </span>
              </button>
            )}
          </For>
        </div>
      </Show>
    </section>
  );
}
