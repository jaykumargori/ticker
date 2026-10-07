import { createEffect, createMemo, createSignal, For, on, onCleanup, onMount, Show } from "solid-js";
import { requestNotifyPermission } from "../lib/alerts";
import type { Row, TickerFeed } from "../lib/feed";
import { currency, money, moneyDelta, symbolOf } from "../lib/currency";
import { clock, compact, pct, qty, signed } from "../lib/format";
import type { WatchlistStore } from "../lib/watchlists";
import { CandleChart, INTERVALS, type ChartKind, type Interval } from "./CandleChart";
import { flash } from "./QuoteRow";
import { toast, useDismiss } from "./Toasts";

const PREF_KEY = "ticker.chart.v1";
function loadPrefs(): { interval: Interval; kind: ChartKind } {
  try {
    const v = JSON.parse(localStorage.getItem(PREF_KEY) ?? "null");
    if (v && INTERVALS.some((i) => i.id === v.interval) && ["candles", "line", "area"].includes(v.kind)) return v;
  } catch {
    /* fall through */
  }
  return { interval: "1m", kind: "candles" };
}

/** Header + chart. The side cards (stats, alerts) are separate so the grid can lay them out. */
export function InstrumentView(p: { feed: TickerFeed; row: Row; store: WatchlistStore }) {
  const r = p.row;
  const key = `${r.ins.exchange}:${r.ins.symbol}`;
  let ltpEl!: HTMLSpanElement;

  onMount(() => p.feed.subscribe([r.ins.token]));
  onCleanup(() => p.feed.unsubscribe([r.ins.token]));
  createEffect(on(r.version, () => flash(ltpEl, r.dir), { defer: true }));

  const prefs = loadPrefs();
  const [interval, setIv] = createSignal<Interval>(prefs.interval);
  const [kind, setKind] = createSignal<ChartKind>(prefs.kind);
  createEffect(() => {
    try {
      localStorage.setItem(PREF_KEY, JSON.stringify({ interval: interval(), kind: kind() }));
    } catch {
      /* ignore */
    }
  });

  const v = createMemo(() => {
    r.version();
    const ch = r.q.close ? r.q.ltp - r.q.close : 0;
    return { ltp: money(r.q.ltp, r.ins), ch: moneyDelta(ch, r.ins, r.q.ltp), pct: signed(pct(r.q.ltp, r.q.close), 2) + "%", tone: ch > 0 ? "up" : ch < 0 ? "down" : "flat" };
  });

  return (
    <>
      <header class="inst-head">
        <div class="inst-id">
          <div class="inst-title">
            <h1>{r.ins.symbol}</h1>
            <span class="tag">{r.ins.segment === "INDEX" ? "INDEX" : r.ins.exchange}</span>
          </div>
          <p class="muted">{r.ins.name}</p>
        </div>
        <div class="inst-price">
          <span ref={ltpEl} class={`big ${v().tone}`}>{v().ltp}</span>
          <span class={`inst-ch ${v().tone}`}>
            {v().tone === "down" ? "▼" : "▲"} {v().ch} ({v().pct})
          </span>
          <Show when={currency.convertsIns(r.ins)}>
            <span class="fx-note small muted" title={`${currency.fx()?.source}, rates as of ${currency.fx()?.asOf}`}>
              ≈ from {r.ins.currency} at 1 {r.ins.currency} = {symbolOf(currency.display()!)}
              {currency.factorFor(r.ins).toLocaleString("en-US", { maximumSignificantDigits: 5 })}
            </span>
          </Show>
        </div>
        <div class="inst-actions">
          <WatchlistMenu store={p.store} itemKey={key} />
        </div>
      </header>

      <section class="card chart-card">
        <div class="chart-toolbar">
          <div class="seg" role="group" aria-label="Chart type">
            <For each={[["candles", "Candles"], ["line", "Line"], ["area", "Area"]] as [ChartKind, string][]}>
              {([id, label]) => (
                <button classList={{ on: kind() === id }} onClick={() => setKind(id)}>
                  {label}
                </button>
              )}
            </For>
          </div>
          <div class="seg" role="group" aria-label="Interval">
            <For each={INTERVALS}>
              {(i) => (
                <button classList={{ on: interval() === i.id }} onClick={() => setIv(i.id)} title={i.id === "1s" ? "Live 1-second bars from incoming ticks" : `${i.label} bars`}>
                  {i.label}
                </button>
              )}
            </For>
          </div>
        </div>
        <CandleChart row={r} interval={interval()} kind={kind()} />
      </section>
    </>
  );
}

export function KeyStats(p: { row: Row }) {
  const r = p.row;
  const isIndex = r.ins.segment === "INDEX";
  const v = createMemo(() => {
    r.version();
    const q = r.q;
    const span = q.high - q.low;
    return {
      open: money(q.open, r.ins),
      high: money(q.high, r.ins),
      low: money(q.low, r.ins),
      close: money(q.close, r.ins),
      vol: q.volume ? compact(q.volume) : "—",
      qty: qty(q.lastQty),
      ts: clock(q.ts),
      pos: span > 0 ? Math.min(100, Math.max(0, ((q.ltp - q.low) / span) * 100)) : 50,
    };
  });

  // Compact full-width strip: day range + key figures on one line.
  return (
    <section class="card stats-strip" aria-label="Key stats">
      <div class="range" aria-label="Day range">
        <div class="range-labels small muted">
          <span>{r.ins.exchange === "CRYPTO" ? "24h low" : "Day low"}</span>
          <span>{r.ins.exchange === "CRYPTO" ? "24h high" : "Day high"}</span>
        </div>
        <div class="range-track">
          <div class="range-dot" style={{ left: `${v().pos}%` }} />
        </div>
        <div class="range-labels">
          <span>{v().low}</span>
          <span>{v().high}</span>
        </div>
      </div>
      <dl class="stat-grid">
        <div><dt>Open</dt><dd>{v().open}</dd></div>
        <div><dt>{r.ins.exchange === "CRYPTO" ? "24h reference" : "Prev close"}</dt><dd>{v().close}</dd></div>
        <Show when={!isIndex}>
          <div><dt>Volume</dt><dd>{v().vol}</dd></div>
          <div><dt>Last trade qty</dt><dd>{v().qty}</dd></div>
        </Show>
        <div><dt>Last update</dt><dd>{v().ts}</dd></div>
        <div class="stat-note"><dt>Data</dt><dd class="muted">
          {r.ins.exchange === "NSE-SIM" ? (isIndex ? "Calculated from simulated members" : "Simulated prices") : "Live Binance trades"}
        </dd></div>
      </dl>
    </section>
  );
}

/** "★ In N watchlists ▾" with per-list checkboxes. */
function WatchlistMenu(p: { store: WatchlistStore; itemKey: string }) {
  const [open, setOpen] = createSignal(false);
  let root!: HTMLDivElement;
  useDismiss(() => root, () => setOpen(false));
  const count = () => p.store.state.lists.filter((l) => l.items.includes(p.itemKey)).length;

  return (
    <div class="menu-wrap" ref={root}>
      <button class="btn" classList={{ starred: count() > 0 }} aria-expanded={open()} onClick={() => setOpen(!open())}>
        {count() ? `★ In ${count()} list${count() > 1 ? "s" : ""}` : "☆ Add to watchlist"} ▾
      </button>
      <Show when={open()}>
        <div class="menu menu-right" role="menu">
          <For each={p.store.state.lists}>
            {(l) => {
              const checked = () => l.items.includes(p.itemKey);
              return (
                <label class="menu-item" role="menuitemcheckbox" aria-checked={checked()}>
                  <input
                    type="checkbox"
                    checked={checked()}
                    onChange={() => {
                      if (checked()) p.store.removeItem(p.itemKey, l.id);
                      else {
                        const err = p.store.addItem(p.itemKey, l.id);
                        if (err) toast("Can't add", { body: err, tone: "error" });
                      }
                    }}
                  />
                  {l.name}
                  <span class="muted small">{l.items.length}</span>
                </label>
              );
            }}
          </For>
        </div>
      </Show>
    </div>
  );
}

export function AlertsCard(p: { store: WatchlistStore; row: Row }) {
  const r = p.row;
  const key = `${r.ins.exchange}:${r.ins.symbol}`;
  const dec = r.ins.decimals;
  const [target, setTarget] = createSignal("");
  const mine = createMemo(() => p.store.state.alerts.filter((a) => a.key === key));

  const create = (e: Event) => {
    e.preventDefault();
    const px = Number(target());
    const ltp = r.q.ltp;
    if (!Number.isFinite(px) || px <= 0) return toast("Enter a valid price", { tone: "error" });
    if (!ltp) return toast("No live price yet", { body: "Try again in a moment", tone: "error" });
    if (px === ltp) return toast("Target equals current price", { tone: "error" });
    requestNotifyPermission(); // user gesture: browsers only allow the prompt here
    const a = p.store.addAlert(key, px, ltp);
    toast(`Alert set for ${r.ins.symbol}`, { body: `when price goes ${a.op} ${px.toFixed(dec)}`, ms: 3000 });
    setTarget("");
  };

  return (
    <section class="card">
      <header class="card-head">
        <h2>Price alert</h2>
        <span class="small muted" title="Alerts use the traded currency, so exchange-rate moves can't trigger them">in {r.ins.currency}</span>
      </header>
      <form class="alert-form" onSubmit={create}>
        <input
          type="number"
          inputMode="decimal"
          step="any"
          min="0"
          aria-label={`Alert price in ${r.ins.currency}`}
          placeholder={r.q.ltp ? r.q.ltp.toFixed(dec) : "Target price"}
          value={target()}
          onInput={(e) => setTarget(e.currentTarget.value)}
        />
        <button class="btn primary" type="submit">
          Set alert
        </button>
      </form>
      <Show when={mine().length} fallback={<p class="small muted note">Get notified when the price crosses a level.</p>}>
        <ul class="alert-list">
          <For each={mine()}>
            {(a) => (
              <li classList={{ hit: a.triggeredAt !== null }}>
                <span>
                  {a.op === "above" ? "≥" : "≤"} {a.price.toFixed(dec)}
                </span>
                <span class="small muted">
                  {a.triggeredAt ? `hit ${a.triggeredPrice?.toFixed(dec)} · ${new Date(a.triggeredAt).toLocaleTimeString()}` : "active"}
                </span>
                <button class="icon-btn" aria-label="Delete alert" onClick={() => p.store.removeAlert(a.id)}>
                  ×
                </button>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </section>
  );
}
