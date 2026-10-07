import { createMemo, createResource, createSignal, onCleanup, onMount, Show } from "solid-js";
import { AlertsMenu } from "./components/AlertsMenu";
import { AlertsCard, InstrumentView, KeyStats } from "./components/Detail";
import { createQuotes, Heatmap, TopMovers } from "./components/MarketPanels";
import { InsightsCard } from "./components/InsightsCard";
import { NewsCard } from "./components/NewsCard";
import { toast, Toasts } from "./components/Toasts";
import { Topbar } from "./components/Topbar";
import { WatchlistPanel } from "./components/WatchlistPanel";
import { startAlertEngine, systemNotify } from "./lib/alerts";
import { TickerFeed, wsURL, type Instrument } from "./lib/feed";
import { localeFor, price } from "./lib/format";
import { createWatchlists } from "./lib/watchlists";

async function fetchInstruments(): Promise<Instrument[]> {
  const r = await fetch("/api/instruments");
  if (!r.ok) throw new Error(`instruments: HTTP ${r.status}`);
  return r.json();
}

export default function App() {
  const [instruments, { refetch }] = createResource(fetchInstruments);
  return (
    <Show
      when={instruments()}
      fallback={
        <div class="center">
          <Show when={instruments.error} fallback={<p class="muted">Loading instruments…</p>}>
            <p>Could not reach the ticker server. Is the backend running on :8080?</p>
            <button class="btn" onClick={refetch}>
              Retry
            </button>
          </Show>
        </div>
      }
    >
      {(list) => <Dashboard instruments={list()} />}
    </Show>
  );
}

function Dashboard(p: { instruments: Instrument[] }) {
  const feed = new TickerFeed(wsURL(), p.instruments);
  feed.start();
  onCleanup(() => feed.stop());

  // The catalog can grow at runtime (the server re-ranks Binance pairs), so it is
  // a signal. byKey is derived from it, and every consumer re-reads it reactively.
  const [instruments, setInstruments] = createSignal(p.instruments);
  const byKey = createMemo(() => new Map(instruments().map((i) => [`${i.exchange}:${i.symbol}`, i])));
  pollCatalog(feed, setInstruments);
  const store = createWatchlists();
  const quotes = createQuotes();

  const firstVisible = store.active().items.find((k) => byKey().has(k));
  const [selected, setSelected] = createSignal<number>(byKey().get(firstVisible ?? "")?.token ?? p.instruments[0]?.token ?? 0);
  const select = (t: number) => {
    setSelected(t);
    document.querySelector(".main")?.scrollTo({ top: 0, behavior: "smooth" });
  };

  const alertKeys = createMemo(() => new Set(store.state.alerts.filter((a) => a.triggeredAt === null).map((a) => a.key)));

  startAlertEngine(feed, store, byKey, (a, row) => {
    const px = price(row.q.ltp, row.ins.decimals, localeFor(row.ins.exchange));
    const title = `${row.ins.symbol} ${a.op === "above" ? "≥" : "≤"} ${a.price}`;
    toast(title, { body: `Last traded ${px} ${row.ins.currency}`, tone: a.op === "above" ? "up" : "down", ms: 10000 });
    systemNotify(title, `Last traded ${px}`);
  });

  const selectedRow = createMemo(() => feed.rows.get(selected()));

  return (
    <div class="app">
      <Topbar feed={feed} byKey={byKey()} onSelect={select}>
        <AlertsMenu store={store} byKey={byKey()} onSelect={select} />
      </Topbar>
      <Show when={feed.lastError()}>
        <div class="banner" role="status">
          {feed.lastError()}
        </div>
      </Show>
      <div class="layout">
        <WatchlistPanel
          feed={feed}
          store={store}
          instruments={instruments()}
          byKey={byKey()}
          alertKeys={alertKeys}
          selected={selected()}
          onSelect={select}
        />
        <main class="main">
          <Show when={selectedRow()} keyed fallback={<div class="center muted">Select an instrument</div>}>
            {(row) => (
              // Named grid areas: head / stats span both columns; chart and the
              // side column (alert + news) share one height, so nothing dangles.
              <div class="dash">
                <InstrumentView feed={feed} row={row} store={store} />
                <KeyStats row={row} />
                <aside class="dash-side">
                  <AlertsCard store={store} row={row} />
                  <NewsCard row={row} />
                </aside>
                <InsightsCard row={row} />
              </div>
            )}
          </Show>
          <div class="dash-market">
            <TopMovers instruments={instruments()} quotes={quotes} onSelect={select} />
            <Heatmap instruments={instruments()} quotes={quotes} onSelect={select} />
          </div>
        </main>
      </div>
      <Toasts />
    </div>
  );
}

const CATALOG_POLL_MS = 120_000;

/**
 * Polls the instrument catalog with If-None-Match. An unchanged catalog costs
 * a bodiless 304. New instruments get live rows and a toast. Instruments are
 * never removed server-side, so the list only grows.
 */
function pollCatalog(feed: TickerFeed, setInstruments: (l: Instrument[]) => void) {
  onMount(() => {
    let etag = "";
    const ctrl = new AbortController();
    const poll = async () => {
      if (document.hidden) return;
      try {
        const r = await fetch("/api/instruments", { headers: etag ? { "If-None-Match": etag } : {}, signal: ctrl.signal });
        if (r.status === 304 || !r.ok) return;
        etag = r.headers.get("ETag") ?? "";
        const list: Instrument[] = await r.json();
        const added = feed.addInstruments(list);
        if (added.length) {
          setInstruments(list);
          toast(`${added.length} new ${added.length > 1 ? "pairs" : "pair"} available`, {
            body: added.slice(0, 5).map((i) => i.symbol).join(", ") + (added.length > 5 ? "…" : "") + ". Find them in Explore.",
            ms: 6000,
          });
        }
      } catch {
        /* transient: try again next interval */
      }
    };
    const id = setInterval(poll, CATALOG_POLL_MS);
    onCleanup(() => {
      clearInterval(id);
      ctrl.abort();
    });
  });
}
