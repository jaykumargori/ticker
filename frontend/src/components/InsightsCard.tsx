import { createMemo, createSignal, For, onCleanup, onMount, Show } from "solid-js";
import { currency, money } from "../lib/currency";
import type { Row } from "../lib/feed";

type Tone = "positive" | "negative" | "neutral" | "unknown";

interface Signal {
  key: string;
  label: string;
  value: string;
  detail: string;
  tone: Tone;
}

interface InsightResult {
  signals: Signal[];
  lean: { positive: number; negative: number; neutral: number; summary: string };
  headlines: { title: string; source: string; url: string; tone: Tone; published: string }[];
  basis: string;
  simulated: boolean;
  generatedAt: string;
}

const POLL_MS = 60_000;
const ICON: Record<Tone, string> = { positive: "▲", negative: "▼", neutral: "●", unknown: "○" };
const WORD: Record<Tone, string> = { positive: "Positive", negative: "Negative", neutral: "Neutral", unknown: "No data" };
const safeHref = (u: string) => (/^https?:\/\//i.test(u) ? u : undefined);

/**
 * Educational, rule-based insights. Deliberately no buy/sell verdict: every
 * signal shows its number and what it conventionally means, the summary only
 * says which way the signals lean, and the headlines behind the news tone are
 * listed so users can judge them.
 */
export function InsightsCard(p: { row: Row }) {
  const r = p.row;
  const [data, setData] = createSignal<InsightResult | null>(null);
  const [failed, setFailed] = createSignal(false);

  onMount(() => {
    const ctrl = new AbortController();
    const load = async () => {
      if (document.hidden) return;
      try {
        const res = await fetch(`/api/insights?token=${r.ins.token}`, { signal: ctrl.signal });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        setData(await res.json());
        setFailed(false);
      } catch (e) {
        if ((e as Error).name !== "AbortError" && !data()) setFailed(true);
      }
    };
    void load();
    const id = setInterval(load, POLL_MS);
    onCleanup(() => {
      clearInterval(id);
      ctrl.abort();
    });
  });

  const leanTone = createMemo<Tone>(() => {
    const s = data()?.lean.summary ?? "";
    return s.includes("positive") ? "positive" : s.includes("negative") ? "negative" : "neutral";
  });

  return (
    <section class="card insights-card" aria-label={`Insights for ${r.ins.symbol}`}>
      <header class="card-head">
        <h2>Insights</h2>
        <span class="small muted">Educational, rule-based signals. Not investment advice.</span>
      </header>

      <Show when={data()} fallback={<p class="muted small">{failed() ? "Insights are unavailable right now. Retrying." : "Analysing…"}</p>}>
        {(d) => (
          <>
            <Show when={d().simulated}>
              <p class="sim-banner small" role="note">
                Price-based signals for {r.ins.symbol} are computed from <strong>simulated prices</strong> and are a
                demonstration only. The news tone uses real headlines.
              </p>
            </Show>

            <div class="lean">
              <span class={`lean-label ${leanTone()}`}>
                {ICON[leanTone()]} {d().lean.summary}
              </span>
              <LeanBar lean={d().lean} />
              <span class="small muted">{d().basis}</span>
            </div>

            <div class="insights-grid">
              <div>
                <h3 class="card-sub">Signals</h3>
                <ul class="signal-list">
                  <For each={d().signals}>
                    {(s) => (
                      <li class="signal">
                        <span class={`tone-chip ${s.tone}`} title={WORD[s.tone]} aria-label={WORD[s.tone]}>
                          {ICON[s.tone]}
                        </span>
                        <div class="signal-body">
                          <div class="signal-top">
                            <span class="signal-label">{s.label}</span>
                            <span class="signal-value">{s.value}</span>
                          </div>
                          <p class="small muted">{s.detail}</p>
                        </div>
                      </li>
                    )}
                  </For>
                </ul>
              </div>

              <div>
                <h3 class="card-sub">Headlines behind the news tone</h3>
                <Show when={d().headlines.length} fallback={<p class="small muted">No recent headlines.</p>}>
                  <ul class="tone-heads">
                    <For each={d().headlines}>
                      {(h) => (
                        <li>
                          <span class={`tone-dot ${h.tone}`} title={WORD[h.tone]} aria-label={WORD[h.tone]}>
                            {ICON[h.tone]}
                          </span>
                          <a href={safeHref(h.url)} target="_blank" rel="noopener noreferrer nofollow">
                            {h.title}
                          </a>
                          <span class="small muted">{h.source}</span>
                        </li>
                      )}
                    </For>
                  </ul>
                </Show>
                <p class="small muted method">
                  Tone counts finance words in each headline ("surges", "downgrade"…). It reads wording, not meaning:
                  "falls despite strong growth" can score positive. Always read the story.
                </p>
              </div>

              <BeforeYouInvest row={r} />
            </div>
          </>
        )}
      </Show>
    </section>
  );
}

/** Stacked bar of signal counts (positive · neutral · negative), with a text legend. */
function LeanBar(p: { lean: InsightResult["lean"] }) {
  const total = () => p.lean.positive + p.lean.neutral + p.lean.negative || 1;
  const seg = (n: number) => `${(n / total()) * 100}%`;
  return (
    <div class="lean-bar-wrap">
      <div
        class="lean-bar"
        role="img"
        aria-label={`${p.lean.positive} positive, ${p.lean.neutral} neutral, ${p.lean.negative} negative signals`}
      >
        <Show when={p.lean.positive}>
          <span class="seg positive" style={{ width: seg(p.lean.positive) }} />
        </Show>
        <Show when={p.lean.neutral}>
          <span class="seg neutral" style={{ width: seg(p.lean.neutral) }} />
        </Show>
        <Show when={p.lean.negative}>
          <span class="seg negative" style={{ width: seg(p.lean.negative) }} />
        </Show>
      </div>
      <span class="lean-legend small">
        <span class="up">▲ {p.lean.positive}</span> <span class="muted">● {p.lean.neutral}</span> <span class="down">▼ {p.lean.negative}</span>
      </span>
    </div>
  );
}

/**
 * Generic investing education plus a position-size calculator (the "fixed
 * fractional" risk rule). Nothing here is personalised.
 */
function BeforeYouInvest(p: { row: Row }) {
  const r = p.row;
  const crypto = r.ins.exchange === "CRYPTO";
  const isIndex = r.ins.segment === "INDEX";
  const [capital, setCapital] = createSignal("100000");
  const [riskPct, setRiskPct] = createSignal("1");
  const [stopPct, setStopPct] = createSignal("5");

  const calc = createMemo(() => {
    r.version(); // track live price
    // Capital is typed in the display currency; the maths runs in the instrument's currency.
    const cap = Number(capital()) / currency.factorFor(r.ins);
    const risk = Number(riskPct());
    const stop = Number(stopPct());
    const ltp = r.q.ltp;
    if (!(cap > 0) || !(risk > 0 && risk <= 100) || !(stop > 0 && stop < 100) || !ltp) return null;
    const maxLoss = (cap * risk) / 100;
    let position = maxLoss / (stop / 100);
    const capped = position > cap;
    if (capped) position = cap;
    const qty = position / ltp;
    return { maxLoss, position, pctOfCap: (position / cap) * 100, qty, capped, stopPrice: ltp * (1 - stop / 100) };
  });

  const fmtQty = (q: number) => (crypto ? q.toLocaleString("en-US", { maximumFractionDigits: 4 }) : Math.floor(q).toLocaleString("en-IN"));

  return (
    <div class="before-invest">
      <h3 class="card-sub">Before you invest</h3>
      <ul class="checklist small">
        <li>Know your goal and time horizon. Short-term trading and long-term investing carry very different risks.</li>
        <li>Invest only money you won't need soon. Keep an emergency fund first.</li>
        <li>Diversify. Avoid putting a large share of your savings into one stock or coin.</li>
        <li>Decide your exit before you enter: where you'd cut a loss, and where you'd take profit.</li>
        <li>Account for brokerage, taxes and other costs. Check the current tax rules for this asset.</li>
        <Show when={crypto}>
          <li>Crypto is highly volatile and isn't regulated like stocks in India. Size it as money you can afford to lose.</li>
        </Show>
        <li>
          For personalised advice, use a SEBI-registered investment adviser, and verify their registration on SEBI's website.
        </li>
      </ul>

      <Show when={!isIndex} fallback={<p class="small muted">Indices can't be bought directly; index funds and ETFs track them.</p>}>
        <form class="sizer" onSubmit={(e) => e.preventDefault()} aria-label="Position size calculator">
          <h3 class="card-sub">Position size calculator</h3>
          <label>
            <span class="small muted">Capital ({currency.convertsIns(r.ins) ? currency.display() : r.ins.currency})</span>
            <input type="number" min="0" step="any" inputMode="decimal" value={capital()} onInput={(e) => setCapital(e.currentTarget.value)} />
          </label>
          <label title="Share of capital you're willing to lose on this one position">
            <span class="small muted">Risk (%)</span>
            <input type="number" min="0.1" max="100" step="0.1" inputMode="decimal" value={riskPct()} onInput={(e) => setRiskPct(e.currentTarget.value)} />
          </label>
          <label title="How far below your entry price you'd exit to cap the loss">
            <span class="small muted">Stop-loss (%)</span>
            <input type="number" min="0.1" max="99" step="0.1" inputMode="decimal" value={stopPct()} onInput={(e) => setStopPct(e.currentTarget.value)} />
          </label>
          <Show when={calc()} fallback={<p class="small muted">Enter capital, risk and stop-loss to see a size.</p>}>
            {(c) => (
              <dl class="sizer-out">
                <div><dt>Max loss if stopped out</dt><dd>{money(c().maxLoss, r.ins)}</dd></div>
                <div><dt>Position size</dt><dd>{money(c().position, r.ins)} <span class="muted small">({c().pctOfCap.toFixed(0)}% of capital)</span></dd></div>
                <div><dt>≈ Quantity at {money(r.q.ltp, r.ins)}</dt><dd>{fmtQty(c().qty)}</dd></div>
                <div><dt>Stop price</dt><dd>{money(c().stopPrice, r.ins)}</dd></div>
              </dl>
            )}
          </Show>
          <Show when={calc()?.capped}>
            <p class="small muted">Capped at your full capital. A wider stop or a higher risk % would require leverage.</p>
          </Show>
          <p class="small muted method">
            Rule of thumb: risk a small, fixed % of capital per position (often 1–2%). Size = max loss ÷ stop distance.
          </p>
        </form>
      </Show>
    </div>
  );
}
