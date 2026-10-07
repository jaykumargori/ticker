import { createEffect, on } from "solid-js";
import { createStore, produce } from "solid-js/store";

/**
 * Watchlists + price alerts, persisted in localStorage.
 *
 * Items are stored as "EXCHANGE:SYMBOL" keys, never tokens: tokens are
 * assigned per server start and would point at the wrong instrument after a
 * restart. Keys for instruments the server doesn't currently offer (e.g. a
 * feed is down) are kept in storage but hidden, so they are not silently lost.
 *
 * Production note: brokers keep this server-side per user so lists sync
 * across devices. There is no auth here, so it stays in the browser.
 */

export const MAX_ITEMS = 100;
export const MAX_LISTS = 10;

export interface Watchlist {
  id: string;
  name: string;
  items: string[];
}

export interface PriceAlert {
  id: string;
  key: string;
  op: "above" | "below";
  price: number;
  createdAt: number;
  triggeredAt: number | null;
  triggeredPrice: number | null;
}

interface State {
  v: 2;
  lists: Watchlist[];
  activeId: string;
  alerts: PriceAlert[];
}

const KEY = "ticker.state.v2";
const LEGACY_V1 = "ticker.watchlist.v1";

// crypto.randomUUID needs a secure context; plain http on a LAN IP is not one.
const uid = () =>
  globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;

function defaults(): State {
  const lists: Watchlist[] = [
    { id: uid(), name: "Crypto", items: ["BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT", "XRPUSDT", "DOGEUSDT"].map((s) => `CRYPTO:${s}`) },
    {
      id: uid(),
      name: "NIFTY",
      items: ["NIFTY 50", "NIFTY BANK", "NIFTY IT", "RELIANCE", "HDFCBANK", "ICICIBANK", "INFY", "TCS", "SBIN"].map((s) => `NSE-SIM:${s}`),
    },
    { id: uid(), name: "My list", items: [] },
  ];
  return { v: 2, lists, activeId: lists[0].id, alerts: [] };
}

const isStr = (x: unknown): x is string => typeof x === "string";

/** Validates untrusted JSON from storage; anything malformed falls back. */
function sanitize(raw: unknown): State | null {
  if (!raw || typeof raw !== "object") return null;
  const r = raw as Partial<State>;
  if (r.v !== 2 || !Array.isArray(r.lists)) return null;
  const lists = r.lists
    .filter((l): l is Watchlist => !!l && isStr(l.id) && isStr(l.name) && Array.isArray(l.items))
    .slice(0, MAX_LISTS)
    .map((l) => ({ id: l.id, name: l.name.slice(0, 40), items: [...new Set(l.items.filter(isStr))].slice(0, MAX_ITEMS) }));
  if (!lists.length) return null;
  const alerts = (Array.isArray(r.alerts) ? r.alerts : []).filter(
    (a): a is PriceAlert =>
      !!a && isStr(a.id) && isStr(a.key) && (a.op === "above" || a.op === "below") && Number.isFinite(a.price) && a.price > 0,
  );
  const activeId = lists.some((l) => l.id === r.activeId) ? r.activeId! : lists[0].id;
  return { v: 2, lists, activeId, alerts };
}

function load(): State {
  try {
    const s = sanitize(JSON.parse(localStorage.getItem(KEY) ?? "null"));
    if (s) return s;
    // Migrate v1 (single list of keys) into the first list.
    const v1 = JSON.parse(localStorage.getItem(LEGACY_V1) ?? "null");
    const d = defaults();
    if (Array.isArray(v1) && v1.every(isStr) && v1.length) {
      d.lists.unshift({ id: uid(), name: "Watchlist", items: v1.slice(0, MAX_ITEMS) });
      d.activeId = d.lists[0].id;
    }
    return d;
  } catch {
    return defaults(); // storage blocked or corrupt JSON
  }
}

export type SortKey = "symbol" | "pctDesc" | "pctAsc" | "ltpDesc" | "exchange";

export function createWatchlists() {
  const [state, setState] = createStore<State>(load());

  // Persist after every change. JSON.stringify reads every field, so the
  // effect tracks the whole store. Writes happen on user actions only.
  createEffect(
    on(
      () => JSON.stringify(state),
      (json) => {
        try {
          localStorage.setItem(KEY, json);
        } catch {
          /* quota/private mode: state lives for this session only */
        }
      },
      { defer: true },
    ),
  );

  const listIndex = (id: string) => state.lists.findIndex((l) => l.id === id);
  const active = () => state.lists[Math.max(0, listIndex(state.activeId))];

  /** Moves `key` to the position currently held by `toKey`. */
  const move = (listId: string, key: string, toKey: string) => {
    const i = listIndex(listId);
    if (i < 0 || key === toKey) return;
    setState("lists", i, "items", (it) => {
      const from = it.indexOf(key);
      const to = it.indexOf(toKey);
      if (from < 0 || to < 0) return it;
      const next = it.slice();
      next.splice(from, 1);
      next.splice(to, 0, key);
      return next;
    });
  };

  return {
    state,
    active,
    setActive(id: string) {
      if (listIndex(id) >= 0) setState("activeId", id);
    },

    createList(name?: string): string | null {
      if (state.lists.length >= MAX_LISTS) return null;
      const id = uid();
      setState("lists", (ls) => [...ls, { id, name: name?.trim() || `Watchlist ${ls.length + 1}`, items: [] }]);
      setState("activeId", id);
      return id;
    },

    renameList(id: string, name: string) {
      const i = listIndex(id);
      const n = name.trim().slice(0, 40);
      if (i >= 0 && n) setState("lists", i, "name", n);
    },

    deleteList(id: string) {
      if (state.lists.length <= 1) return; // always keep one list
      const i = listIndex(id);
      if (i < 0) return;
      setState(
        produce((s) => {
          s.lists.splice(i, 1);
          if (s.activeId === id) s.activeId = s.lists[Math.max(0, i - 1)].id;
        }),
      );
    },

    /** Returns an error message, or null on success. */
    addItem(key: string, listId = state.activeId): string | null {
      const i = listIndex(listId);
      if (i < 0) return "Watchlist not found";
      const l = state.lists[i];
      if (l.items.includes(key)) return null;
      if (l.items.length >= MAX_ITEMS) return `“${l.name}” is full (${MAX_ITEMS} max)`;
      setState("lists", i, "items", (it) => [...it, key]);
      return null;
    },

    removeItem(key: string, listId = state.activeId) {
      const i = listIndex(listId);
      if (i >= 0) setState("lists", i, "items", (it) => it.filter((k) => k !== key));
    },

    has(key: string, listId = state.activeId): boolean {
      const i = listIndex(listId);
      return i >= 0 && state.lists[i].items.includes(key);
    },

    move,

    /** Moves `key` up (-1) or down (+1) by one place. */
    nudge(listId: string, key: string, delta: -1 | 1, visible: (k: string) => boolean) {
      const i = listIndex(listId);
      if (i < 0) return;
      const it = state.lists[i].items.filter(visible);
      const at = it.indexOf(key);
      const target = it[at + delta];
      if (at >= 0 && target) move(listId, key, target);
    },

    /**
     * One-shot sort: rewrites the saved order, as Kite does. A live re-sort
     * on every tick would make rows jump under the cursor and costs O(n log n)
     * per frame.
     */
    sortList(listId: string, by: SortKey, lookup: (key: string) => { symbol: string; exchange: string; ltp: number; pct: number } | null) {
      const i = listIndex(listId);
      if (i < 0) return;
      const cmp: Record<SortKey, (a: NonNullable<ReturnType<typeof lookup>>, b: NonNullable<ReturnType<typeof lookup>>) => number> = {
        symbol: (a, b) => a.symbol.localeCompare(b.symbol),
        exchange: (a, b) => a.exchange.localeCompare(b.exchange) || a.symbol.localeCompare(b.symbol),
        pctDesc: (a, b) => b.pct - a.pct,
        pctAsc: (a, b) => a.pct - b.pct,
        ltpDesc: (a, b) => b.ltp - a.ltp,
      };
      setState("lists", i, "items", (it) => {
        const known = it.filter((k) => lookup(k));
        const unknown = it.filter((k) => !lookup(k)); // hidden items keep their place at the end
        known.sort((a, b) => cmp[by](lookup(a)!, lookup(b)!));
        return [...known, ...unknown];
      });
    },

    // ---- alerts ----
    addAlert(key: string, price: number, currentLtp: number): PriceAlert {
      // Direction is inferred from where the target sits relative to the
      // current price, so the alert fires on the crossing, not immediately.
      const op: PriceAlert["op"] = price >= currentLtp ? "above" : "below";
      const a: PriceAlert = { id: uid(), key, op, price, createdAt: Date.now(), triggeredAt: null, triggeredPrice: null };
      setState("alerts", (al) => [...al, a]);
      return a;
    },
    removeAlert(id: string) {
      setState("alerts", (al) => al.filter((a) => a.id !== id));
    },
    markTriggered(id: string, ltp: number) {
      const i = state.alerts.findIndex((a) => a.id === id);
      if (i >= 0 && state.alerts[i].triggeredAt === null) {
        setState("alerts", i, { triggeredAt: Date.now(), triggeredPrice: ltp });
      }
    },
    clearTriggered() {
      setState("alerts", (al) => al.filter((a) => a.triggeredAt === null));
    },
  };
}

export type WatchlistStore = ReturnType<typeof createWatchlists>;
