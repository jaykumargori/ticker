import { createEffect, createMemo, onCleanup } from "solid-js";
import type { Instrument, Row, TickerFeed } from "./feed";
import type { PriceAlert, WatchlistStore } from "./watchlists";

/**
 * Client-side price alerts.
 *
 * - Active alerts are indexed by token, so each render flush costs
 *   O(changed tokens), not O(alerts).
 * - Alert tokens are subscribed through the feed's ref-count, so an alert
 *   keeps receiving prices even when its instrument isn't on screen.
 * - Limitation: prices arrive conflated (server FLUSH_MS + one render frame).
 *   A spike that crosses and reverts inside one window can be missed. Brokers
 *   evaluate alerts server-side on every tick for that reason (Kite's alerts
 *   and GTT orders).
 */
export function startAlertEngine(
  feed: TickerFeed,
  store: WatchlistStore,
  byKey: () => Map<string, Instrument>, // accessor: the catalog can grow at runtime
  onTrigger: (alert: PriceAlert, row: Row) => void,
) {
  const index = createMemo(() => {
    const m = new Map<number, PriceAlert[]>();
    for (const a of store.state.alerts) {
      if (a.triggeredAt !== null) continue;
      const ins = byKey().get(a.key);
      if (!ins) continue;
      const list = m.get(ins.token);
      if (list) list.push(a);
      else m.set(ins.token, [a]);
    }
    return m;
  });

  createEffect(() => {
    const tokens = [...index().keys()];
    feed.subscribe(tokens);
    onCleanup(() => feed.unsubscribe(tokens));
  });

  const off = feed.onFlush((changed) => {
    const idx = index();
    if (!idx.size) return;
    for (const token of changed) {
      const alerts = idx.get(token);
      if (!alerts) continue;
      const row = feed.rows.get(token)!;
      const ltp = row.q.ltp;
      for (const a of alerts) {
        if (a.op === "above" ? ltp >= a.price : ltp <= a.price) {
          store.markTriggered(a.id, ltp);
          onTrigger(a, row);
        }
      }
    }
  });
  onCleanup(off);
}

/** Desktop notification when the tab is in the background (permission is opt-in). */
export function systemNotify(title: string, body: string) {
  try {
    if (typeof Notification !== "undefined" && Notification.permission === "granted" && document.hidden) {
      new Notification(title, { body, tag: title });
    }
  } catch {
    /* some browsers throw when constructing Notification outside a SW */
  }
}

/** Ask once, from a user gesture (creating the first alert). */
export function requestNotifyPermission() {
  try {
    if (typeof Notification !== "undefined" && Notification.permission === "default") void Notification.requestPermission();
  } catch {
    /* unsupported */
  }
}
