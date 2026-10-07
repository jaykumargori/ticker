// Intl.NumberFormat construction is expensive; cache one per (locale, digits).
const cache = new Map<string, Intl.NumberFormat>();

function nf(locale: string, opts: Intl.NumberFormatOptions): Intl.NumberFormat {
  const key = locale + JSON.stringify(opts);
  let f = cache.get(key);
  if (!f) {
    f = new Intl.NumberFormat(locale, opts);
    cache.set(key, f);
  }
  return f;
}

/** Indian digit grouping for Indian instruments, western elsewhere. */
export const localeFor = (exchange: string) => (exchange.startsWith("NSE") ? "en-IN" : "en-US");

export function price(v: number, decimals: number, locale = "en-US"): string {
  if (!v) return "—";
  return nf(locale, { minimumFractionDigits: decimals, maximumFractionDigits: decimals }).format(v);
}

export function signed(v: number, decimals: number, locale = "en-US"): string {
  const s = nf(locale, { minimumFractionDigits: decimals, maximumFractionDigits: decimals }).format(Math.abs(v));
  return (v > 0 ? "+" : v < 0 ? "−" : "") + s;
}

export function pct(ltp: number, close: number): number {
  return close ? ((ltp - close) / close) * 100 : 0;
}

export function compact(v: number): string {
  return nf("en-US", { notation: "compact", maximumFractionDigits: 2 }).format(v);
}

/** Quantities: fractional crypto sizes keep 4 significant digits. */
export function qty(v: number): string {
  if (!v) return "—";
  return v < 1 ? nf("en-US", { maximumSignificantDigits: 4 }).format(v) : compact(v);
}

export function bytes(v: number): string {
  if (v < 1024) return `${v.toFixed(0)} B`;
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`;
  return `${(v / 1024 / 1024).toFixed(2)} MB`;
}

export function clock(ms: number): string {
  if (!ms) return "—";
  return new Date(ms).toLocaleTimeString("en-GB", { hour12: false }) + "." + String(Math.floor(ms % 1000)).padStart(3, "0");
}
