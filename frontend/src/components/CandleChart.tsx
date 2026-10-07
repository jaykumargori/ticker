import {
  AreaSeries,
  CandlestickSeries,
  ColorType,
  createChart,
  CrosshairMode,
  HistogramSeries,
  LineSeries,
  LineStyle,
  type IChartApi,
  type IPriceLine,
  type ISeriesApi,
  type MouseEventParams,
  type SeriesType,
  type UTCTimestamp,
} from "lightweight-charts";
import { createEffect, createSignal, on, onCleanup, onMount, Show, untrack } from "solid-js";
import type { Row } from "../lib/feed";
import { currency, displayDecimals, money } from "../lib/currency";
import { compact, pct, signed } from "../lib/format";
import { cssVar, theme } from "../lib/theme";

export type Interval = "1s" | "1m" | "5m" | "15m" | "1h";
export type ChartKind = "candles" | "line" | "area";

export const INTERVALS: { id: Interval; label: string; sec: number }[] = [
  { id: "1s", label: "1s", sec: 1 },
  { id: "1m", label: "1m", sec: 60 },
  { id: "5m", label: "5m", sec: 300 },
  { id: "15m", label: "15m", sec: 900 },
  { id: "1h", label: "1H", sec: 3600 },
];

interface Bar {
  t: number; // bucket start, unix seconds (UTC)
  o: number;
  h: number;
  l: number;
  c: number;
  v: number;
}

const MAX_BARS = 1500;
// lightweight-charts renders UTC; shift timestamps so axes read local time (IST here).
const TZ_SHIFT = -new Date().getTimezoneOffset() * 60;
const toTime = (t: number) => (t + TZ_SHIFT) as UTCTimestamp;

/** "#rrggbb" + alpha → rgba(). The chart library parses colors itself, so stick to plain rgba. */
function alpha(hex: string, a: number): string {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255},${(n >> 8) & 255},${n & 255},${a})`;
}

/**
 * History comes from /api/candles (server-aggregated, Binance-backfilled for
 * crypto). The live bar is then advanced client-side from streamed ticks, so
 * the chart moves at tick speed without re-fetching. "1s" is live-only:
 * built from ticks since the chart opened.
 */
export function CandleChart(p: { row: Row; interval: Interval; kind: ChartKind }) {
  let host!: HTMLDivElement;
  let chart: IChartApi;
  let priceSeries: ISeriesApi<SeriesType> | null = null;
  let volSeries: ISeriesApi<"Histogram">;
  let closeLine: IPriceLine | null = null;
  let bars: Bar[] = [];
  let lastVolume = 0;
  let loadSeq = 0;

  const r = p.row;
  const hasVolume = r.ins.segment !== "INDEX";
  const [legend, setLegend] = createSignal<Bar | null>(null);
  const [hovering, setHovering] = createSignal(false);
  const [status, setStatus] = createSignal<"loading" | "ready" | "error">("loading");

  const ivSec = () => INTERVALS.find((i) => i.id === p.interval)!.sec;

  const colors = () => ({
    up: cssVar("--up"),
    down: cssVar("--down"),
    text: cssVar("--muted"),
    grid: cssVar("--grid"),
    border: cssVar("--border"),
    panel: cssVar("--panel"),
    accent: cssVar("--accent"),
  });

  const volPoint = (b: Bar, c = colors()) => ({
    time: toTime(b.t),
    value: b.v,
    color: alpha(b.c >= b.o ? c.up : c.down, 0.35),
  });
  // Bars stay in the instrument's native currency; conversion is applied at
  // render time so a currency switch is a re-render, not a re-fetch.
  let k = 1;
  let prec = r.ins.decimals;
  const pricePoint = (b: Bar) =>
    p.kind === "candles"
      ? { time: toTime(b.t), open: b.o * k, high: b.h * k, low: b.l * k, close: b.c * k }
      : { time: toTime(b.t), value: b.c * k };

  const createPriceSeries = () => {
    if (priceSeries) chart.removeSeries(priceSeries);
    closeLine = null;
    const c = colors();
    k = untrack(() => currency.factorFor(r.ins));
    prec = untrack(() => displayDecimals(r.ins, (r.q.ltp || bars.at(-1)?.c || 1) * k));
    const fmt = { type: "price" as const, precision: prec, minMove: 10 ** -prec };
    if (p.kind === "candles") {
      // Hollow up / filled down: direction stays readable without color (CVD-safe).
      priceSeries = chart.addSeries(CandlestickSeries, {
        upColor: "rgba(0,0,0,0)",
        downColor: c.down,
        borderUpColor: c.up,
        borderDownColor: c.down,
        wickUpColor: c.up,
        wickDownColor: c.down,
        priceFormat: fmt,
      });
    } else if (p.kind === "line") {
      priceSeries = chart.addSeries(LineSeries, { color: c.accent, lineWidth: 2, priceFormat: fmt });
    } else {
      priceSeries = chart.addSeries(AreaSeries, {
        lineColor: c.accent,
        lineWidth: 2,
        topColor: alpha(c.accent, 0.22),
        bottomColor: alpha(c.accent, 0),
        priceFormat: fmt,
      });
    }
    drawCloseLine();
  };

  const drawCloseLine = () => {
    if (!priceSeries) return;
    if (closeLine) priceSeries.removePriceLine(closeLine);
    closeLine = null;
    const close = r.q.close;
    if (!close) return;
    closeLine = priceSeries.createPriceLine({
      price: close * k,
      color: cssVar("--muted"),
      lineWidth: 1,
      lineStyle: LineStyle.Dashed,
      axisLabelVisible: true,
      title: r.ins.exchange === "CRYPTO" ? "24h ref" : "Prev close",
    });
  };

  const setAll = () => {
    const c = colors();
    priceSeries?.setData(bars.map(pricePoint) as never);
    volSeries.setData(hasVolume ? bars.map((b) => volPoint(b, c)) : []);
    setLegend(bars.at(-1) ?? null);
  };

  const applyTheme = () => {
    const c = colors();
    chart.applyOptions({
      layout: { background: { type: ColorType.Solid, color: c.panel }, textColor: c.text, fontSize: 12 },
      grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
      rightPriceScale: { borderColor: c.border },
      timeScale: { borderColor: c.border },
    });
    createPriceSeries();
    setAll();
  };

  /** Folds the latest tick into the current bar (or opens a new one). */
  const onTick = () => {
    const ltp = r.q.ltp;
    if (!ltp || !priceSeries) return;
    const iv = ivSec();
    const now = Math.floor(Date.now() / 1000);
    const t = Math.floor(now / iv) * iv;
    // Volume arrives cumulative (day / 24h); bar volume is the delta. A
    // rolling 24h window can shrink, so negative deltas are dropped.
    let dv = 0;
    if (r.q.volume > 0) {
      if (lastVolume > 0 && r.q.volume > lastVolume) dv = r.q.volume - lastVolume;
      lastVolume = r.q.volume;
    }
    let last = bars.at(-1);
    if (!last || t > last.t) {
      last = { t, o: ltp, h: ltp, l: ltp, c: ltp, v: dv };
      bars.push(last);
      if (bars.length > MAX_BARS) bars.splice(0, bars.length - MAX_BARS);
    } else if (t === last.t) {
      last.h = Math.max(last.h, ltp);
      last.l = Math.min(last.l, ltp);
      last.c = ltp;
      last.v += dv;
    } else return; // clock went backwards: ignore
    priceSeries.update(pricePoint(last) as never);
    if (hasVolume) volSeries.update(volPoint(last));
    if (!hovering()) setLegend({ ...last });
  };

  const load = async () => {
    const seq = ++loadSeq;
    bars = [];
    lastVolume = 0;
    setAll();
    if (p.interval === "1s") {
      setStatus("ready");
      return;
    }
    setStatus("loading");
    try {
      const res = await fetch(`/api/candles?token=${r.ins.token}&interval=${p.interval}&limit=500`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const hist: Bar[] = await res.json();
      if (seq !== loadSeq) return; // a newer interval was picked meanwhile
      // Keep live bars that formed while the request was in flight.
      const lastHist = hist.at(-1)?.t ?? -Infinity;
      bars = [...hist, ...bars.filter((b) => b.t > lastHist)];
      setAll();
      chart.timeScale().setVisibleLogicalRange({ from: Math.max(0, bars.length - 120), to: bars.length + 4 });
      setStatus("ready");
    } catch (e) {
      if (seq !== loadSeq) return;
      console.warn("candles load failed", e);
      setStatus("error"); // live bars still build from ticks
    }
  };

  onMount(() => {
    const c = colors();
    chart = createChart(host, {
      autoSize: true,
      layout: { background: { type: ColorType.Solid, color: c.panel }, textColor: c.text, fontSize: 12, attributionLogo: true },
      grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
      crosshair: { mode: CrosshairMode.Normal },
      rightPriceScale: { borderColor: c.border, scaleMargins: { top: 0.08, bottom: hasVolume ? 0.22 : 0.08 } },
      timeScale: { borderColor: c.border, timeVisible: true, secondsVisible: false, rightOffset: 4 },
      localization: { priceFormatter: (v: number) => v.toFixed(prec) },
    });
    volSeries = chart.addSeries(HistogramSeries, { priceScaleId: "vol", priceFormat: { type: "volume" }, lastValueVisible: false, priceLineVisible: false });
    chart.priceScale("vol").applyOptions({ scaleMargins: { top: 0.82, bottom: 0 } });
    createPriceSeries();

    const onMove = (param: MouseEventParams) => {
      const d = priceSeries && param.time ? param.seriesData.get(priceSeries) : undefined;
      if (!d) {
        setHovering(false);
        setLegend(bars.at(-1) ?? null);
        return;
      }
      setHovering(true);
      const t = (param.time as number) - TZ_SHIFT;
      setLegend(bars.find((b) => b.t === t) ?? null);
    };
    chart.subscribeCrosshairMove(onMove);
    onCleanup(() => {
      chart.unsubscribeCrosshairMove(onMove);
      chart.remove();
    });
  });

  // Reload on interval change; restyle on chart type or theme change.
  createEffect(on(() => p.interval, () => void load()));
  createEffect(on(() => p.kind, () => (createPriceSeries(), setAll()), { defer: true }));
  createEffect(on(theme.version, applyTheme, { defer: true }));
  // Re-render on currency switch and on the hourly rate refresh.
  createEffect(on(() => [currency.display(), currency.fx()], () => (createPriceSeries(), setAll()), { defer: true }));
  createEffect(on(r.version, onTick, { defer: true }));
  // Reference close rarely changes (24h window for crypto); redraw only when it does.
  let lastClose = 0;
  createEffect(
    on(r.version, () => {
      if (r.q.close !== lastClose) {
        lastClose = r.q.close;
        drawCloseLine();
      }
    }),
  );

  const legendView = () => {
    const b = legend();
    if (!b) return null;
    return { o: money(b.o, r.ins), h: money(b.h, r.ins), l: money(b.l, r.ins), c: money(b.c, r.ins), v: compact(b.v), chg: pct(b.c, b.o), up: b.c >= b.o };
  };

  return (
    <div class="chart-host">
      <div class="chart-legend" aria-live="off">
        <Show when={legendView()}>
          {(l) => (
            <>
              <span><i>O</i>{l().o}</span>
              <span><i>H</i>{l().h}</span>
              <span><i>L</i>{l().l}</span>
              <span><i>C</i>{l().c}</span>
              <span class={l().up ? "up" : "down"}>
                {l().up ? "▲" : "▼"} {signed(l().chg, 2)}%
              </span>
              <Show when={hasVolume}>
                <span><i>Vol</i>{l().v}</span>
              </Show>
            </>
          )}
        </Show>
        <Show when={status() === "loading"}>
          <span class="muted">Loading history…</span>
        </Show>
        <Show when={status() === "error"}>
          <span class="muted">History unavailable. Showing live bars only.</span>
        </Show>
        <Show when={p.interval === "1s" && !legend()}>
          <span class="muted">Live 1-second bars build from incoming ticks.</span>
        </Show>
      </div>
      <div class="chart-canvas" ref={host} role="img" aria-label={`${r.ins.symbol} ${p.interval} ${p.kind} chart`} />
    </div>
  );
}
