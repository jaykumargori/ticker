# How it works

This explains how a price gets from an exchange onto your screen, and how the watchlist,
search, reorder, sort and alert features are built on top of that. File references point at
the code that does each step.

---

## 1. The instruments (what you can watch)

| Source | Count | Real or simulated | File |
|---|---|---|---|
| Binance crypto pairs (`CRYPTO`) | top 100 by 24h volume (**dynamic**) | **Real** trade-by-trade data, public API, no key | [binance.go](../backend/internal/feed/binance.go) |
| NSE-style stocks (`NSE-SIM`, segment `EQ`) | 109 | Simulated (NIFTY 50 + Next 50 / midcaps) | [sim.go](../backend/internal/feed/sim.go) |
| Indices: NIFTY 50, NIFTY BANK, NIFTY IT (segment `INDEX`) | 3 | **Calculated** from the simulated member stocks | [sim.go](../backend/internal/feed/sim.go) |
| US stocks (`US`) | optional | Real, needs `FINNHUB_TOKEN` | [finnhub.go](../backend/internal/feed/finnhub.go) |

At startup each feed **registers** its instruments with the hub. The hub hands back a small
integer **token** (1, 2, 3 …), and everything after that refers to instruments by token, not
by name. That's how Kite's `instrument_token` works too. A 4-byte integer is cheaper to send
and look up than a string.

**Dynamic crypto universe.** At startup, one ~1 MB request (`ticker/24hr?type=MINI`) ranks every
USDT pair by 24h quote volume. Stablecoin pairs are excluded, both by name (USDC, FDUSD…) and by
behaviour (price ≈ 1 with a < 0.5% range), so new stablecoins are caught too. The top
`BINANCE_TOP` (default 100) plus any pinned `BINANCE_SYMBOLS` are registered in ranked order, and
the same rows seed their first quotes.
- **Refresh:** every `BINANCE_REFRESH_MIN` (30), the ranking is recomputed. Newly popular pairs
  are registered and subscribed **on the live socket** with Binance's `SUBSCRIBE` method, sent in
  chunks of 100 streams with 250 ms gaps to respect the 5-messages-per-second limit. There is no
  reconnect.
- **Nothing is removed:** pairs that drop out of the ranking stay listed, so tokens and saved
  watchlists stay valid.
- **Caps and fallback:** the total is capped at 500 symbols (Binance allows 1024 streams per
  connection, and each pair uses 2). If discovery fails, a built-in fallback list is used.
- **The browser** polls `GET /api/instruments` every 2 minutes with `If-None-Match`. An
  unchanged catalog is a bodiless 304. New pairs get live rows and a "N new pairs available"
  toast.

**Validating Binance symbols.** Binance rejects the whole request if even one symbol in it is
invalid or delisted. Instead of retrying all ~85 symbols one by one, the list is split in half
recursively until the bad ones are isolated (`resolve()` in binance.go). Two bad symbols cost
about 10 requests instead of 85. This is covered by `TestBinanceResolveBisectsBadSymbols`.

**How the indices are calculated.** A real index is a weighted average of its member stocks
relative to a reference point. The simulator does the same:

```
index = base × Σ wᵢ · (priceᵢ / prevCloseᵢ)        Σ wᵢ = 1
```

- **Weights:** heavier for the larger, earlier names (`1/√rank`), so a few heavyweights move
  the index, as on the real NIFTY.
- **Correlation:** every simulated tick includes a small shared "market" shock. Stocks move
  together, so the index trends instead of averaging out flat.
- **Tests:** if every member is at its reference price, the index equals its base, and if every
  member is +1% the index is +1% (`TestSimIndexTracksMembers`).

---

## 2. A price's journey: exchange → screen

```
 ① Upstream            ② Hub (Go)                     ③ Wire              ④ Browser (Solid)
 ────────────          ─────────────────────────      ─────────           ─────────────────────
 Binance trade ──►     Apply(): update quote      ┐
 Sim tick      ──►     under a mutex, mark dirty  │    every 50 ms
                                                  ▼
                       Frame builder: encode each       binary frame   ──►  decode into plain
                       changed quote ONCE (68 B)        [count][ts]          objects (no render)
                       → new immutable snapshot         [len][packet]…            │
                       → wake every client (O(1))                                 ▼
                                                                      once per animation frame:
                       Per-client writer: send only                   batch() bump the version
                       my subscribed tokens that                      signal of each changed row
                       changed since I last sent                      → only those text nodes
                                                                        update, cell flashes
```

**① Ingest.** Feeds call `hub.Apply(tick)`, which takes about 23 ns. That function updates
the instrument's quote (LTP, OHLC, volume) and adds the token to a "dirty" list
([hub.go](../backend/internal/hub/hub.go)).

**② Conflate and encode.** Every `FLUSH_MS` (50 ms), the builder:
- takes the dirty list;
- encodes each changed quote **once** into a 68-byte packet;
- publishes a new immutable *snapshot* (frame), where each entry records the frame sequence
  number in which it last changed.

If BTC traded 30 times in those 50 ms, only the latest price goes out. That's conflation: a
quote screen shows *state*, not a list of events.

**③ Fan-out.** Publishing a frame closes a Go channel, and every client's writer goroutine wakes
at once ([client.go](../backend/internal/hub/client.go)). Each writer sends the instruments
**it subscribed to** whose sequence number is newer than the last one it sent. A slow client
that missed 10 frames gets **one** message with the latest values, so the server never builds a
queue per client. A client whose write blocks past `WRITE_TIMEOUT` is disconnected.

**④ Render.** The browser decodes the binary frame straight into plain JS objects. That costs
nothing in rendering terms because nothing is reactive yet
([feed.ts](../frontend/src/lib/feed.ts)). Once per `requestAnimationFrame`, it bumps a
`version` signal on each changed row inside one `batch()`. Solid then updates only the text
nodes that read that row. Network rate and paint rate are fully decoupled: the server can send
20 frames/s or 200, and the browser still paints at most 60.

---

## 3. Watchlist features: logic

All watchlist state lives in [watchlists.ts](../frontend/src/lib/watchlists.ts), a Solid
store saved to `localStorage`. The UI is
[WatchlistPanel.tsx](../frontend/src/components/WatchlistPanel.tsx).

### 3.1 Data model
```ts
{ v: 2,
  lists:  [{ id, name, items: ["CRYPTO:BTCUSDT", "NSE-SIM:RELIANCE", …] }],   // ≤10 lists, ≤100 items
  activeId,
  alerts: [{ id, key, op: "above"|"below", price, triggeredAt, triggeredPrice }] }
```

Design decisions:
- **Items are `EXCHANGE:SYMBOL`, not tokens.** Tokens are assigned fresh every time the server
  starts. A saved token would point at a different instrument after a restart.
- **Unknown items are hidden, not deleted.** If the Binance feed is down, the server doesn't
  list crypto. The app hides those items but keeps them in storage, so a temporary outage never
  wipes anyone's list.
- **Untrusted storage is validated** (`sanitize()`). Corrupt JSON, wrong types or oversized
  lists fall back to defaults instead of crashing.
- **Migration.** The old v1 format (a single array) is imported into the first list.
- **In production this lives on the server per user** (as Kite does) so it syncs across devices.
  There is no auth here, so it stays in the browser.

### 3.2 Subscribe only what's visible (ref-counted)
Kite doesn't stream every instrument to every user. The client subscribes to what's on screen.

```ts
createEffect(() => {
  const tokens = shownKeys().map(k => byKey.get(k).token);
  feed.subscribe(tokens);                       // refcount++  → send "subscribe" only on 0→1
  onCleanup(() => feed.unsubscribe(tokens));    // refcount--  → send "unsubscribe" only on 1→0
});
```

Three things can want the same instrument: the visible list, the open detail panel, and an
active alert. The feed keeps a **reference count per token**. Switching tabs unsubscribes the
old list, but BTC keeps streaming if its detail panel is open or an alert is set on it. After a
reconnect, everything with refcount > 0 is resubscribed automatically.

### 3.3 Search (`/` to focus)
[SearchBox.tsx](../frontend/src/components/SearchBox.tsx) ranks matches in this order:
1. exact symbol
2. symbol prefix
3. symbol contains
4. name contains

Within a tier, shorter symbols come first. A linear scan over about 200 instruments takes
microseconds, so no index is needed. Kite searches server-side because it has about 100k
instruments.

Keys: ↑/↓ moves the highlight. **Enter adds the instrument to the active list and opens it.**
Esc closes. The `+`/`✓` button toggles membership without opening.

### 3.4 Reorder: drag, or Alt+↑/↓
**Pointer events**, not HTML5 drag-and-drop, so it works with mouse, pen and touch:

1. `pointerdown` on the ⋮⋮ handle remembers which item is being dragged.
2. `pointermove` (on `window`) calls `elementFromPoint(x, y).closest("[data-key]")` to find the
   row under the pointer. If it's a different row, the store's `move(key, overKey)` runs, which
   splices the item into that position.
3. Near the top or bottom edge the list auto-scrolls on a `requestAnimationFrame` loop.
4. `pointerup` or `pointercancel` cleans up.

**Why it's cheap:** `<For>` is keyed by the item string. When the order changes, Solid **moves
the existing DOM nodes** instead of re-creating them. Rows don't re-render, and prices keep
flashing while you drag.

**Why it doesn't jitter:** right after a swap, the pointer is over the dragged row itself, which
is a no-op. So rows don't oscillate back and forth.

The keyboard alternative is to focus a row and press Alt+↑/↓ (`nudge`). Plain ↑/↓ moves focus,
Enter opens the row, and Delete removes it.

### 3.5 Sort: a one-shot action, not a live mode
"Sort…" rewrites the saved order **once**, by % change, price, symbol or exchange. Kite does
the same.
- **Live sorting** would re-run O(n log n) every frame, and rows would jump under your cursor as
  prices change.
- **The sort uses the live quote values at that moment**, read through a lookup function.
- **Hidden (unknown) items keep their relative order** at the end of the list.

### 3.6 Multiple lists
- **Create:** the `+` tab adds a list and goes straight into rename mode.
- **Rename:** double-click a tab or press Rename. Enter saves, Esc cancels.
- **Delete:** inline confirmation. The last remaining list can't be deleted.
- **Detail-panel menu:** "★ In N watchlists ▾" shows a checkbox per list, so one instrument
  can live in several lists.

### 3.7 Explore
Browse all 194 instruments by category chip (Crypto, NSE stocks, Indices). The category is
derived from `exchange` + `segment`, and `✓`/`+` adds to the active list. Explore subscribes to
the instruments it shows, same as a watchlist.

---

## 4. Price alerts: logic

Code: [alerts.ts](../frontend/src/lib/alerts.ts).

1. **Creating an alert.** You enter a target price. The **direction is inferred** from the
   current price: a target above the LTP becomes "≥", a target below becomes "≤". So the alert
   fires on the crossing, not instantly.
2. **Index.** Active alerts are grouped into a `Map<token, alerts[]>` (a memo that rebuilds only
   when alerts change).
3. **Keep the price flowing.** Each alert's token is subscribed through the same refcount, so
   alerts work for instruments that aren't on screen.
4. **Check on every render flush.** The feed calls `onFlush(changedTokens)`. The engine only
   looks at alerts for tokens **that actually changed**, so the cost is O(changed), not O(all
   alerts).
5. **Trigger.** It marks the alert with the time and price it hit, shows a toast, and, if the tab
   is in the background and you allowed notifications, a desktop notification. Permission is
   requested only when you create your first alert, because browsers require a user gesture.
   Triggered alerts stop being checked, and the bell badge turns amber.

**Limitation:** the browser sees *conflated* prices, at most one per 50 ms frame. A spike that
crosses your target and comes back within that window can be missed. The browser also has to be
open. Real brokers evaluate alerts **server-side on every tick** (Kite alerts and GTT orders) for
exactly this reason. To move this server-side, run the same check inside `hub.Apply` and push
the trigger over the WebSocket as a text message.

---

## 5. Other details

- **Flash on change.** When a row's LTP differs from the last *rendered* value, the cell runs a
  600 ms Web Animations API fade (green up, red down). WAAPI avoids the forced reflow you'd get
  from toggling a CSS class to restart an animation.
- **Day-range bar.** It shows where the LTP sits between the day's low and high:
  `(ltp − low) / (high − low)`.
- **FULL vs LTP mode.** The LTP packet is the first 12 bytes of the FULL packet, so the server
  sends a slice instead of encoding twice. Change % still works in LTP mode, because the
  snapshot sent on subscribe is always FULL and delivers the reference close.
- **Latency figure.** Measured as browser wall-clock time minus the server's send time, which
  is only meaningful when clocks agree. On the same machine the true value is below clock
  resolution, so it reads "<1 ms".

## 6. Dashboard and candle charts

```
┌ Topbar: logo · index strip (NIFTY 50, BANK, IT, BTC, ETH) ·········· ● Live <1 ms · 🔔 · ☀ ┐
├ Watchlists ─┬ Instrument header (symbol, price, change, ★ lists) ─────────┬ Key stats ──┤
│ search      │ Chart card: [Candles|Line|Area]   [1s 1m 5m 15m 1H]        │ range bar   │
│ tabs        │   OHLC legend · candles · volume · prev-close line         ├ Price alert ┤
│ rows        ├ Top movers (NSE|Crypto) ─┬ Heatmap (NIFTY 50|BANK|IT|Crypto) ─────────────┤
└─────────────┴──────────────────────────┴───────────────────────────────────────────────┘
```

### Where candles come from
| Layer | What it does | File |
|---|---|---|
| Server aggregation | `hub.OnTrade` sends every trade to `candles.Store.Trade`, which folds it into 1m/5m/15m/1h bars (500 kept per series) | [candles.go](../backend/internal/candles/candles.go) |
| Real history (crypto) | On a chart's first request, the Binance klines REST endpoint is fetched **once**, even if many users ask at the same moment (single-flight). The result is merged *under* the live bars, then served from memory. Failures retry after 30 s, and live bars are still served meanwhile | `Binance.klines()` |
| Synthetic history (NSE-SIM) | At startup, 500 bars per interval are generated by a backward random walk that ends at the current price. Volatility scales with √interval | `Sim.seedHistory()` |
| Live bar (browser) | After loading history, the browser advances the current bar from streamed ticks, so the chart moves at tick speed without re-fetching. Bar volume is the change in cumulative volume, with negative changes dropped because the 24h window can shrink | [CandleChart.tsx](../frontend/src/components/CandleChart.tsx) |
| `1s` interval | Browser-only. Bars are built from ticks since the chart opened | same |

`GET /api/candles?token=&interval=1m|5m|15m|1h&limit=` returns `[{t,o,h,l,c,v}]`, where `t`
is unix seconds.

**Merging history and live bars.** One time bucket is covered by both the history and the live
bars. That bar takes the history's **open**, the high/low across **both**, the live **close**,
and the **larger** volume. The history's volume already includes trades from before the server
started. This is covered by `TestBackfillMergesOnceUnderConcurrency`.

**Rendering.** The charts use TradingView's open-source *lightweight-charts* (Apache-2.0,
canvas-based). Candles, volume, crosshair and price lines are built in, and it handles thousands
of bars per frame. Hand-rolling candles on a canvas would mean re-implementing axes, zoom/pan
and crosshair snapping. It adds about 60 KB gzipped to the bundle.

### Why the overview panels poll instead of stream
Movers and the heatmap need **every** instrument, but nobody needs them tick-by-tick.
`GET /api/quotes` returns an ~8 KB snapshot that the browser polls every 2 s, and polling stops
while the tab is hidden. Streaming all 194 instruments in FULL mode could cost up to ~250 KB/s
per user. The general rule: **stream what the user is looking at, and poll summaries.**

### Color and accessibility
- **Colorblind check:** standard green/red fails the deutan colorblindness check (ΔE 5.0,
  checked with the palette validator). Traders expect green and red, so they stay the default,
  but **direction never depends on color alone**:
  - every change carries a ▲/▼ and a sign;
  - up candles are **hollow** and down candles are filled.
- **Colorblind-safe palette:** Settings → *Blue / Orange* switches to a palette that passes all
  checks in both themes (CVD ΔE ≥ 25).
- **Heatmap:** a diverging scale, with two hues around a neutral gray at 0%, clipped at ±2% for
  NSE and ±5% for crypto. It has a legend, and each tile prints its value.
- **Theme:** Light, Dark or System. These are attributes on `<html>`. Charts re-read the CSS
  tokens when the theme changes, because canvas can't see CSS.

## 7. Display currency (INR by default in India)

**Rates.** [fx.go](../backend/internal/fx/fx.go) fetches USD-based reference rates
server-side when the server starts, then hourly, and serves them at `GET /api/fx`.
- **Primary source:** ECB rates via Frankfurter (no key).
- **Fallback:** ExchangeRate-API, which has wider coverage and requires its attribution.
- **On failure:** the last good snapshot is kept and the fetch retries in 5 min.
- **Why server-side:** one upstream call for all users, no browser CORS issues, and a single
  place to swap in a paid feed.

**Default currency by location, with no third-party IP lookup**
([currency.ts](../frontend/src/lib/currency.ts)). The first of these that answers wins:
1. **CDN geo header.** The server echoes `CloudFront-Viewer-Country` or `CF-IPCountry` when it
   is deployed behind CloudFront or Cloudflare.
2. **Browser time zone.** `Asia/Kolkata` (or the legacy alias `Asia/Calcutta`) → IN → **INR**.
3. **Locale region.** `en-IN` → IN.
4. **USD.**

The time zone is checked before language because many Indian users run an `en-US` browser. That
was the case on this machine, and INR was still picked correctly. A spoofed header only changes
a default, so it carries no risk.

**Conversion rules**
| Rule | Why |
|---|---|
| `value × rates[display] / rates[native]`, applied at render time | Switching currency re-renders, with no re-fetch. The chart multiplies bars as it draws them |
| % change is never converted | It's a ratio, so the result is identical |
| **Indices are never converted** | NIFTY 50 is in *points*, not rupees. "NIFTY = $257" would be meaningless |
| **Alerts stay in the traded currency** (the card says "in USD"/"in INR") | Otherwise an FX move could fire a price alert |
| USDT/USDC are treated as USD | Close to 1:1. Small de-pegs are ignored, and the footer says so |
| Decimals: the currency's usual digits (₹/$ 2, ¥ 0), or 4 significant digits below 1 | BTC → ₹81,25,398.25, PEPE → ₹0.0003982 |
| INR uses Indian grouping (lakh/crore) | ₹81,25,398, not ₹8,125,398 |
| Historical candles are converted at **today's** rate | A simplification; the label says "≈" |
| No rates available → Native prices | Never show a guessed conversion |

The ₹/$ picker in the top bar offers Auto (shows the detected country and why), Native (each
instrument in its own currency), or a fixed currency. The choice is saved per browser.

## 8. News (with thumbnails, popping in live)

**Sources, merged.** [news.go](../backend/internal/news/news.go) queries every source for an
instrument **concurrently**, then merges the results, de-duplicates them by headline, and sorts
newest first (up to 20 items).

| Instrument | Sources | Notes |
|---|---|---|
| NSE stocks | Bing News ×2 (plain name, name + "stock") + Google News | Bing brings thumbnails and direct publisher links; Google brings breadth |
| Indices | Bing News ×2 + Google News | |
| Crypto | Bing News ×2 + Yahoo Finance (`BTC-USD`) + Google News | |

**Relevance.** Search engines match article *bodies*, so a plain query for "Eternal" returned
an essay about eternity. Two defences:
- **Aliases** for ambiguous names, in [query.go](../backend/internal/news/query.go):
  `"Eternal Ltd" OR Zomato OR Blinkit`, "Titan Company", "Larsen & Toubro", and so on.
- **A title filter.** A headline is kept only if its *title* names the company, as a whole word.

**Caching.** Results are cached for 3 minutes per instrument. Concurrent requests share a single
upstream fetch. At most 4 upstream fetches run at once. A failed refresh serves the stale copy,
and a first fetch that fails is negatively cached for 30 s.

**Untrusted input.** Feed text is HTML-stripped, unescaped and length-capped. Only http(s) links
survive, so `javascript:` links are dropped; this is tested. Bing's click-tracking links are
unwrapped to the publisher URL. Article links open with `rel="noopener noreferrer nofollow"`.

**Image proxy** (`/api/news/img`, [images.go](../backend/internal/news/images.go)). Thumbnails
are relayed through our own origin:
- the viewer's IP never reaches the image host;
- there are no mixed-content warnings.

It is **SSRF-safe**:
- only Bing's `/th` thumbnail endpoint is allowlisted;
- redirects are refused;
- only `image/*` responses up to 1 MB are relayed;
- a 32 MB LRU cache sits in front of upstream.

The tests reject the cloud metadata IP, lookalike hosts and `file://`. Items without an image
show a neutral publisher-initials tile.

**"Popping up."** The card polls every 60 s while the tab is visible. The first load is quiet.
After that, any id not seen before slides in at the top with a **New** badge and raises one
toast. Switching instruments re-keys the card, which starts a fresh "seen" set.

**Layout.** The dashboard is a named-area CSS grid:
```
head  head        ← symbol, price, ★ lists
stats stats       ← day range + key figures, one compact strip
chart side        ← side = Price alert + News, locked to the chart's height (--main-h)
```
News scrolls inside the side column, so neither column leaves whitespace. At ≤1280 px, Alert and
News move below the chart, side by side. On phones everything stacks.

**Limits.** These RSS endpoints suit demos and personal use. A commercial product should use a
licensed feed, such as NSE corporate announcements or a paid news API, behind the same `Feed`
shape. Thumbnail coverage depends on Bing, typically about 40–75% of items. Getting more would
need og:image scraping of publisher pages, with a hardened dialer that blocks private IPs.

## 9. Insights (educational signals, not advice)

**Why there's no BUY/SELL.**
- **Regulation:** in India, buy/sell recommendations to the public fall under SEBI's Research
  Analyst / Investment Adviser regulations, which require registration. SEBI has also acted
  against unregistered "finfluencer" tips.
- **Honesty:** these indicators *describe* the past; they don't predict returns reliably.
- **Simulated data:** for NSE-SIM the prices are simulated, so a "buy" would be fiction.

So the section explains what the data shows and never tells anyone what to do. **Shipping
actual recommendations needs a SEBI-registered RA/IA, or a registered partner, plus legal
review.**

**Engine:** [insights.go](../backend/internal/insights/insights.go), a pure function that is
unit-tested. It is served at `GET /api/insights?token=` and the browser polls it every 60 s.

| Signal | Computed from | Reading |
|---|---|---|
| Trend | LTP vs 20h and 50h simple moving averages (hourly candles) | Above both with 20 > 50 = conventional uptrend; below both with 20 < 50 = downtrend; else no clear trend |
| Momentum | Wilder's RSI(14) on hourly closes | 55–70 positive, 30–45 negative. **≥70 / ≤30 are reported as neutral**: "overbought/oversold" means both strength and a stretched move, so it is not a direction |
| Volatility | Std-dev of hourly log returns over ~5 days × √24 | Always neutral (risk, not direction). Thresholds differ by asset class: 1–2.5%/day for stocks, 2.5–6% for crypto |
| Performance | 24h and 5-day change | Positive or negative beyond ±1%, with a "past performance…" note |
| Activity | Last 24h volume ÷ average of up to 5 prior 24h windows | Always neutral; volume confirms moves but isn't a direction. Not available for indices |
| News tone | Finance-word lexicon over headlines from the last 72h | Positive or negative only with a 2-headline margin. The headlines are listed, so users can see it's a word count ("falls despite strong growth" scores positive; a test documents this) |

- **Lean:** "Signals lean positive/negative" needs a net margin of ≥ 2, otherwise "mixed". It
  reads "not enough data" when fewer than 3 signals are readable.
- **Missing data:** signals without enough history report **No data** instead of guessing.
- **Simulated prices:** NSE-SIM instruments get a banner saying the price-based signals are a
  demo, while the news tone uses real headlines.

**Before you invest:** a generic checklist (goals and horizon, emergency fund, diversification,
exit plan, costs and taxes, a crypto risk note, and "use a SEBI-registered adviser for
personalised advice"). Nothing in it is personalised.

**Position size calculator (fixed-fractional rule).** Size = (capital × risk %) ÷ stop distance.
- **Currency:** capital is typed in the display currency and converted, so ₹ in gives ₹ out.
- **Cap:** the size is capped at capital (more would need leverage).
- **Crypto:** quantities are fractional (4 dp); for shares they round down.
- **Verified:** ₹1,00,000 at 1% risk with a 5% stop → ₹1,000 max loss, a ₹20,000 position
  (20%), and 0.0025 BTC at ₹80.9 lakh.

**Simulator realism fix found along the way:** simulated large-caps were showing ±3.3%/day
volatility, because per-minute volatility had been calibrated against trading hours while the
sim runs 24h. They now show ~±1.3%/day (`simSigma1m` = 1.5%/day ÷ √1440), which is realistic
for an Indian large-cap.

## 10. Verification

- **Go tests:** frame conflation, an end-to-end WebSocket test (snapshot, mode switch, skipped
  frames), index math, symbol bisection.
- **Browser, checked with Playwright:**
  - default lists load;
  - search + Enter adds;
  - Alt+↑ reorders;
  - a real pointer drag moves AXISBANK from 1st to 4th;
  - sort by % change, and the order survives a reload;
  - create, rename and delete-with-confirm a list;
  - Explore shows 194 rows, and the Indices chip shows 3;
  - an alert set at 1381.95 fired at 1382.00 within 500 ms;
  - layout works at 390 px width.
