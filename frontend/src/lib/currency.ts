import { createMemo, createRoot, createSignal } from "solid-js";
import type { Instrument } from "./feed";

/**
 * Display-currency conversion.
 *
 * - Rates come from the server's /api/fx (daily reference rates, USD base),
 *   refreshed hourly. They are for display only, not settlement.
 * - The default currency follows the viewer's location without calling a
 *   third-party IP service. Order: CDN geo header (sent back by the server) →
 *   browser time zone (Asia/Kolkata → IN) → locale region (en-IN → IN) → USD.
 *   Time zone comes before language because many Indian users run en-US.
 * - Conversion is a pure multiplier, so % changes are unaffected. Alerts stay
 *   in the instrument's native currency, so an FX move can't shift a trigger.
 */

export interface FxSnapshot {
  base: "USD";
  rates: Record<string, number>; // units per 1 USD
  source: string;
  sourceUrl: string;
  asOf: string;
  fetchedAt: string;
  country?: string;
}

export type CurrencyPref = "auto" | "native" | string; // or an ISO code

const PREF_KEY = "ticker.currency";

const TZ_COUNTRY: Record<string, string> = {
  "Asia/Kolkata": "IN", "Asia/Calcutta": "IN",
  "America/New_York": "US", "America/Chicago": "US", "America/Denver": "US", "America/Los_Angeles": "US",
  "America/Phoenix": "US", "America/Anchorage": "US", "Pacific/Honolulu": "US",
  "America/Toronto": "CA", "America/Vancouver": "CA", "America/Edmonton": "CA", "America/Winnipeg": "CA", "America/Halifax": "CA",
  "Europe/London": "GB", "Europe/Berlin": "DE", "Europe/Paris": "FR", "Europe/Madrid": "ES", "Europe/Rome": "IT",
  "Europe/Amsterdam": "NL", "Europe/Brussels": "BE", "Europe/Vienna": "AT", "Europe/Dublin": "IE", "Europe/Lisbon": "PT",
  "Europe/Helsinki": "FI", "Europe/Athens": "GR", "Europe/Zurich": "CH",
  "Asia/Tokyo": "JP", "Asia/Dubai": "AE", "Asia/Singapore": "SG", "Asia/Shanghai": "CN", "Asia/Hong_Kong": "HK",
  "Australia/Sydney": "AU", "Australia/Melbourne": "AU", "Australia/Brisbane": "AU", "Australia/Perth": "AU", "Australia/Adelaide": "AU",
};

const EUROZONE = ["DE", "FR", "ES", "IT", "NL", "BE", "AT", "IE", "PT", "FI", "GR", "SK", "SI", "LU", "LV", "LT", "EE", "CY", "MT", "HR"];
const COUNTRY_CCY: Record<string, string> = {
  IN: "INR", US: "USD", CA: "CAD", GB: "GBP", JP: "JPY", AE: "AED", SG: "SGD", AU: "AUD", CH: "CHF", CN: "CNY", HK: "HKD",
  ...Object.fromEntries(EUROZONE.map((c) => [c, "EUR"])),
};

/** Best-effort viewer country, no network calls. */
export function detectCountry(serverHint?: string): { country: string; via: string } {
  const hint = serverHint?.toUpperCase();
  if (hint && /^[A-Z]{2}$/.test(hint) && hint !== "XX") return { country: hint, via: "network location" };
  try {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (TZ_COUNTRY[tz]) return { country: TZ_COUNTRY[tz], via: `time zone ${tz}` };
  } catch {
    /* Intl unavailable */
  }
  for (const lang of navigator.languages ?? [navigator.language]) {
    const region = /-([A-Z]{2})\b/i.exec(lang)?.[1]?.toUpperCase();
    if (region) return { country: region, via: `browser language ${lang}` };
  }
  return { country: "US", via: "default" };
}

function readPref(): CurrencyPref {
  try {
    return localStorage.getItem(PREF_KEY) || "auto";
  } catch {
    return "auto";
  }
}

export const currency = createRoot(() => {
  const [fx, setFx] = createSignal<FxSnapshot | null>(null);
  const [pref, setPrefSig] = createSignal<CurrencyPref>(readPref());
  const [error, setError] = createSignal(false);

  const detected = createMemo(() => {
    const { country, via } = detectCountry(fx()?.country);
    const rates = fx()?.rates ?? {};
    const ccy = COUNTRY_CCY[country];
    return { country, via, currency: ccy && rates[ccy] ? ccy : "USD" };
  });

  /** Display currency, or null for native (each instrument in its own). */
  const display = createMemo<string | null>(() => {
    const p = pref();
    const rates = fx()?.rates;
    if (p === "native" || !rates) return null; // no rates: never show a guessed conversion
    if (p === "auto") return detected().currency;
    return rates[p] ? p : detected().currency;
  });

  const load = async () => {
    try {
      const r = await fetch("/api/fx");
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      setFx(await r.json());
      setError(false);
    } catch {
      setError(true);
      setTimeout(load, 30_000); // server may still be fetching its first rates
    }
  };
  void load();
  setInterval(load, 60 * 60_000);

  return {
    fx,
    pref,
    error,
    detected,
    display,
    setPref(p: CurrencyPref) {
      setPrefSig(p);
      try {
        localStorage.setItem(PREF_KEY, p);
      } catch {
        /* session-only */
      }
    },
    /** Multiplier from an instrument's currency to the display currency (1 when native/unknown). */
    factor(from: string): number {
      const to = display();
      const rates = fx()?.rates;
      if (!to || !rates || from === to) return 1;
      const a = rates[from];
      const b = rates[to];
      return a && b ? b / a : 1;
    },
    /** True when values for this instrument are actually being converted. */
    converts(from: string): boolean {
      const to = display();
      const rates = fx()?.rates;
      return !!to && to !== from && !!rates?.[from] && !!rates?.[to];
    },
    /** Indices are in points, not money: never converted, never given a symbol. */
    convertsIns(ins: Instrument): boolean {
      return isMoney(ins) && this.converts(ins.currency);
    },
    factorFor(ins: Instrument): number {
      return isMoney(ins) ? this.factor(ins.currency) : 1;
    },
  };
});

export const isMoney = (ins: Instrument) => ins.segment !== "INDEX";

// ---------- formatting ----------

const cache = new Map<string, Intl.NumberFormat>();
function nf(locale: string, opts: Intl.NumberFormatOptions) {
  const k = locale + JSON.stringify(opts);
  let f = cache.get(k);
  if (!f) cache.set(k, (f = new Intl.NumberFormat(locale, opts)));
  return f;
}

const localeOf = (ccy: string | null, ins: Instrument) => (ccy ? (ccy === "INR" ? "en-IN" : "en-US") : ins.exchange.startsWith("NSE") ? "en-IN" : "en-US");

function currencyDigits(ccy: string): number {
  return nf("en-US", { style: "currency", currency: ccy }).resolvedOptions().maximumFractionDigits ?? 2;
}

/**
 * Decimals to show. Native values keep the instrument's tick decimals.
 * Converted values use the currency's usual digits (₹ 2, ¥ 0), or 4
 * significant digits below 1, so tiny prices like PEPE stay readable.
 */
export function displayDecimals(ins: Instrument, convertedValue: number): number {
  const ccy = currency.display();
  if (!currency.convertsIns(ins)) return ins.decimals;
  const v = Math.abs(convertedValue);
  if (!v || v >= 1) return currencyDigits(ccy!);
  return Math.min(10, Math.max(2, Math.ceil(-Math.log10(v)) + 3));
}

/** Formats an instrument-native value in the display currency. */
export function money(v: number, ins: Instrument, opts: { symbol?: boolean } = {}): string {
  if (!v) return "—";
  const converting = currency.convertsIns(ins);
  const disp = currency.display();
  // Show a symbol whenever a display currency is active and the value is money:
  // converted (BTC → ₹) or already in it (RELIANCE in ₹), so the screen is consistent.
  const ccy = converting ? disp : disp && isMoney(ins) && ins.currency === disp ? disp : null;
  const x = v * currency.factorFor(ins);
  const d = displayDecimals(ins, x);
  const base: Intl.NumberFormatOptions = { minimumFractionDigits: d, maximumFractionDigits: d };
  if (ccy && opts.symbol !== false) {
    return nf(localeOf(ccy, ins), { ...base, style: "currency", currency: ccy, currencyDisplay: "narrowSymbol" }).format(x);
  }
  return nf(localeOf(ccy, ins), base).format(x);
}

/** Signed change (+/−), same currency handling as money(). */
export function moneyDelta(v: number, ins: Instrument, ref: number): string {
  const ccy = currency.convertsIns(ins) ? currency.display()! : null;
  const k = currency.factorFor(ins);
  const d = displayDecimals(ins, ref * k);
  const s = nf(localeOf(ccy, ins), { minimumFractionDigits: d, maximumFractionDigits: d }).format(Math.abs(v * k));
  return (v > 0 ? "+" : v < 0 ? "−" : "") + s;
}

export function symbolOf(ccy: string): string {
  try {
    return nf("en-US", { style: "currency", currency: ccy, currencyDisplay: "narrowSymbol" }).formatToParts(0).find((p) => p.type === "currency")?.value ?? ccy;
  } catch {
    return ccy;
  }
}
