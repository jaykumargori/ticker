import { createMemo, createSignal, For, onCleanup, onMount, Show, type JSX } from "solid-js";
import type { Instrument, Row, TickerFeed } from "../lib/feed";
import { currency, money, symbolOf } from "../lib/currency";
import { bytes, pct, signed } from "../lib/format";
import { theme, type PalettePref, type ThemePref } from "../lib/theme";
import { useDismiss } from "./Toasts";

const STRIP_KEYS = ["NSE-SIM:NIFTY 50", "NSE-SIM:NIFTY BANK", "NSE-SIM:NIFTY IT", "CRYPTO:BTCUSDT", "CRYPTO:ETHUSDT"];

export function Topbar(p: { feed: TickerFeed; byKey: Map<string, Instrument>; onSelect: (token: number) => void; children?: JSX.Element }) {
  const strip = STRIP_KEYS.map((k) => p.byKey.get(k)).filter((i): i is Instrument => !!i);
  const tokens = strip.map((i) => i.token);
  onMount(() => p.feed.subscribe(tokens));
  onCleanup(() => p.feed.unsubscribe(tokens));

  return (
    <header class="topbar">
      <div class="brand">
        <svg width="18" height="18" viewBox="0 0 16 16" aria-hidden="true">
          <polyline points="1,12 5,7 9,10 15,3" fill="none" stroke="var(--accent)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
        <strong>Ticker</strong>
      </div>
      <nav class="index-strip" aria-label="Market indices">
        <For each={strip}>{(ins) => <IndexChip row={p.feed.rows.get(ins.token)!} onClick={() => p.onSelect(ins.token)} />}</For>
      </nav>
      <div class="topbar-right">
        <CurrencyMenu />
        <StatusPill feed={p.feed} />
        {p.children}
        <Settings />
      </div>
    </header>
  );
}

function IndexChip(p: { row: Row; onClick: () => void }) {
  const r = p.row;
  const v = createMemo(() => {
    r.version();
    const ch = pct(r.q.ltp, r.q.close);
    return { ltp: money(r.q.ltp, r.ins), ch, tone: ch > 0 ? "up" : ch < 0 ? "down" : "flat" };
  });
  return (
    <button class="index-chip" onClick={p.onClick} title={r.ins.name}>
      <span class="index-name">{r.ins.segment === "INDEX" ? r.ins.symbol : r.ins.symbol.replace(/USDT$/, "")}</span>
      <span class="index-val">{v().ltp}</span>
      <span class={`index-ch ${v().tone}`}>
        {v().ch >= 0 ? "▲" : "▼"} {signed(v().ch, 2)}%
      </span>
    </button>
  );
}

interface ServerStats {
  clients: number;
  ticksPerSec: number;
  framesPerSec: number;
  bytesPerSec: number;
  slowClientsDropped: number;
}

/** Minimal by default ("● Live <1 ms"); the full metrics live in the popover. */
function StatusPill(p: { feed: TickerFeed }) {
  const [open, setOpen] = createSignal(false);
  const [server, setServer] = createSignal<ServerStats | null>(null);
  let root!: HTMLDivElement;
  useDismiss(() => root, () => setOpen(false));
  const s = () => p.feed.stats();

  onMount(() => {
    const ctrl = new AbortController();
    const poll = async () => {
      if (!open()) return; // only poll while the panel is visible
      try {
        const r = await fetch("/api/stats", { signal: ctrl.signal });
        if (r.ok) setServer(await r.json());
      } catch {
        /* status dot already reflects connectivity */
      }
    };
    const id = setInterval(poll, 2000);
    onCleanup(() => {
      clearInterval(id);
      ctrl.abort();
    });
  });

  const label = () => ({ open: "Live", connecting: "Connecting", closed: "Reconnecting" })[p.feed.status()];
  const latency = () => (s().latencyAvg < 1 ? "<1 ms" : `${s().latencyAvg.toFixed(0)} ms`);

  return (
    <div class="menu-wrap" ref={root}>
      <button
        class="pill"
        aria-expanded={open()}
        onClick={() => {
          setOpen(!open());
          if (open()) fetch("/api/stats").then((r) => r.json()).then(setServer).catch(() => {});
        }}
      >
        <span class={`dot ${p.feed.status()}`} aria-hidden="true" />
        {label()}
        <Show when={p.feed.status() === "open"}>
          <span class="muted">· {latency()}</span>
        </Show>
      </button>
      <Show when={open()}>
        <div class="menu menu-right stats-menu" role="dialog" aria-label="Connection details">
          <div class="menu-head">
            <strong>Connection</strong>
            <span class="muted small">this browser</span>
          </div>
          <dl class="kv">
            <dt>Messages</dt><dd>{s().msgsPerSec}/s</dd>
            <dt>Price updates</dt><dd>{s().packetsPerSec}/s</dd>
            <dt>Bandwidth</dt><dd>{bytes(s().bytesPerSec)}/s</dd>
            <dt>Latency avg / max</dt><dd>{latency()} / {s().latencyMax.toFixed(1)} ms</dd>
            <dt>Renders</dt><dd>{s().rendersPerSec}/s</dd>
          </dl>
          <Show when={server()}>
            {(sv) => (
              <>
                <div class="menu-head sub"><strong>Server</strong></div>
                <dl class="kv">
                  <dt>Ticks in</dt><dd>{sv().ticksPerSec}/s</dd>
                  <dt>Frames out</dt><dd>{sv().framesPerSec}/s</dd>
                  <dt>Clients</dt><dd>{sv().clients}</dd>
                  <dt>Bandwidth out</dt><dd>{bytes(sv().bytesPerSec)}/s</dd>
                </dl>
              </>
            )}
          </Show>
          <div class="menu-foot">
            <span class="small muted">Streaming mode</span>
            <div class="seg" role="group" aria-label="Streaming mode">
              <button classList={{ on: p.feed.mode() === "full" }} onClick={() => p.feed.setMode("full")} title="68-byte packets: OHLC, volume, time">
                Full
              </button>
              <button classList={{ on: p.feed.mode() === "ltp" }} onClick={() => p.feed.setMode("ltp")} title="12-byte packets: last price only">
                LTP
              </button>
            </div>
          </div>
        </div>
      </Show>
    </div>
  );
}

function Settings() {
  const [open, setOpen] = createSignal(false);
  let root!: HTMLDivElement;
  useDismiss(() => root, () => setOpen(false));
  const themes: [ThemePref, string][] = [["system", "System"], ["light", "Light"], ["dark", "Dark"]];
  const palettes: [PalettePref, string][] = [["standard", "Green / Red"], ["cb", "Blue / Orange"]];

  return (
    <div class="menu-wrap" ref={root}>
      <button class="icon-btn lg" aria-label="Display settings" aria-expanded={open()} onClick={() => setOpen(!open())}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
        </svg>
      </button>
      <Show when={open()}>
        <div class="menu menu-right settings-menu" role="dialog" aria-label="Display settings">
          <div class="setting">
            <span class="small muted">Theme</span>
            <div class="seg">
              <For each={themes}>
                {([id, label]) => (
                  <button classList={{ on: theme.pref() === id }} onClick={() => theme.setPref(id)}>
                    {label}
                  </button>
                )}
              </For>
            </div>
          </div>
          <div class="setting">
            <span class="small muted">Up / down colors</span>
            <div class="seg">
              <For each={palettes}>
                {([id, label]) => (
                  <button classList={{ on: theme.palette() === id }} onClick={() => theme.setPalette(id)}>
                    {label}
                  </button>
                )}
              </For>
            </div>
            <span class="small muted">Blue / Orange stays distinguishable with red-green color blindness.</span>
          </div>
        </div>
      </Show>
    </div>
  );
}

const CCY_NAMES: Record<string, string> = {
  INR: "Indian Rupee", USD: "US Dollar", EUR: "Euro", GBP: "British Pound", JPY: "Japanese Yen", AED: "UAE Dirham",
  SGD: "Singapore Dollar", AUD: "Australian Dollar", CAD: "Canadian Dollar", CHF: "Swiss Franc", CNY: "Chinese Yuan", HKD: "Hong Kong Dollar",
};

function regionName(code: string): string {
  try {
    return new Intl.DisplayNames(["en"], { type: "region" }).of(code) ?? code;
  } catch {
    return code;
  }
}

/** Display currency: Auto (location) · Native · a specific currency. */
function CurrencyMenu() {
  const [open, setOpen] = createSignal(false);
  let root!: HTMLDivElement;
  useDismiss(() => root, () => setOpen(false));
  const c = currency;
  const available = createMemo(() => Object.keys(c.fx()?.rates ?? {}).sort((a, b) => (a === "INR" ? -1 : b === "INR" ? 1 : a.localeCompare(b))));
  const label = () => {
    const d = c.display();
    return d ? `${symbolOf(d)} ${d}` : "Native";
  };
  const pick = (v: string) => {
    c.setPref(v);
    setOpen(false);
  };

  return (
    <div class="menu-wrap" ref={root}>
      <button class="pill" aria-expanded={open()} aria-label={`Display currency: ${label()}`} onClick={() => setOpen(!open())}>
        {label()}
        <span class="muted" aria-hidden="true">▾</span>
      </button>
      <Show when={open()}>
        <div class="menu menu-right ccy-menu" role="menu" aria-label="Display currency">
          <button class="menu-item" classList={{ on: c.pref() === "auto" }} role="menuitemradio" aria-checked={c.pref() === "auto"} onClick={() => pick("auto")}>
            <span class="ccy-sym">◎</span>
            <span class="ccy-text">
              Auto · {c.detected().currency}
              <span class="small muted">
                {regionName(c.detected().country)} (from {c.detected().via})
              </span>
            </span>
          </button>
          <button class="menu-item" classList={{ on: c.pref() === "native" }} role="menuitemradio" aria-checked={c.pref() === "native"} onClick={() => pick("native")}>
            <span class="ccy-sym">≡</span>
            <span class="ccy-text">
              Native
              <span class="small muted">Each instrument in its traded currency</span>
            </span>
          </button>
          <div class="menu-sep" />
          <div class="ccy-list">
            <For each={available()}>
              {(code) => (
                <button class="menu-item" classList={{ on: c.pref() === code }} role="menuitemradio" aria-checked={c.pref() === code} onClick={() => pick(code)}>
                  <span class="ccy-sym">{symbolOf(code)}</span>
                  <span class="ccy-text">
                    {code}
                    <span class="small muted">{CCY_NAMES[code] ?? code}</span>
                  </span>
                  <span class="small muted ccy-rate">{code === "USD" ? "1" : c.fx()!.rates[code].toLocaleString("en-US", { maximumFractionDigits: 3 })}</span>
                </button>
              )}
            </For>
          </div>
          <div class="menu-foot ccy-foot small muted">
            <Show when={c.fx()} fallback={<span>{c.error() ? "Exchange rates unavailable. Showing native prices." : "Loading rates…"}</span>}>
              {(fx) => (
                <span>
                  Rates per 1 USD ·{" "}
                  <a href={fx().sourceUrl} target="_blank" rel="noopener noreferrer">
                    {fx().source}
                  </a>{" "}
                  · {fx().asOf.slice(0, 16)}. Reference rates for display only; USDT is treated as USD.
                </span>
              )}
            </Show>
          </div>
        </div>
      </Show>
    </div>
  );
}
