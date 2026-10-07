import { createEffect, createMemo, createSignal, For, onCleanup, Show } from "solid-js";
import type { Instrument, TickerFeed } from "../lib/feed";
import { pct } from "../lib/format";
import { MAX_ITEMS, MAX_LISTS, type SortKey, type WatchlistStore } from "../lib/watchlists";
import { QuoteRow } from "./QuoteRow";
import { SearchBox } from "./SearchBox";
import { toast } from "./Toasts";

const keyOf = (i: Instrument) => `${i.exchange}:${i.symbol}`;

/** Explore categories derived from instrument metadata. */
const categoryOf = (i: Instrument) =>
  i.segment === "INDEX" ? "Indices" : i.exchange === "CRYPTO" ? "Crypto" : i.exchange === "NSE-SIM" ? "NSE stocks" : i.exchange;

export function WatchlistPanel(p: {
  feed: TickerFeed;
  store: WatchlistStore;
  instruments: Instrument[];
  byKey: Map<string, Instrument>;
  alertKeys: () => Set<string>;
  selected: number;
  onSelect: (token: number) => void;
}) {
  const s = p.store;
  const [view, setView] = createSignal<"list" | "explore">("list");
  const [category, setCategory] = createSignal("All");
  const [editing, setEditing] = createSignal(false);
  const [confirmDelete, setConfirmDelete] = createSignal(false);
  const [dragKey, setDragKey] = createSignal<string | null>(null);
  let listEl!: HTMLUListElement;

  const categories = createMemo(() => ["All", ...new Set(p.instruments.map(categoryOf))]);

  // Keys of the active list that the server currently offers.
  const visibleKeys = createMemo(() => s.active().items.filter((k) => p.byKey.has(k)));
  const exploreKeys = createMemo(() => {
    const c = category();
    return p.instruments.filter((i) => c === "All" || categoryOf(i) === c).map(keyOf);
  });
  const shownKeys = () => (view() === "list" ? visibleKeys() : exploreKeys());

  // Subscribe only to what is on screen, like Kite. The feed ref-counts, so
  // the detail panel and alerts keep their own tokens alive.
  createEffect(() => {
    const tokens = shownKeys().map((k) => p.byKey.get(k)!.token);
    p.feed.subscribe(tokens);
    onCleanup(() => p.feed.unsubscribe(tokens));
  });

  const toggleInActive = (ins: Instrument) => {
    const k = keyOf(ins);
    if (s.has(k)) s.removeItem(k);
    else {
      const err = s.addItem(k);
      if (err) toast("Can't add", { body: err, tone: "error" });
    }
  };

  // ---- drag-to-reorder (pointer events: works for mouse, pen and touch) ----
  // Rows are keyed by item string, so <For> moves existing DOM nodes when the
  // order changes: no row re-renders, and live prices keep flashing mid-drag.
  let stopDrag: (() => void) | null = null;
  const startDrag = (e: PointerEvent, key: string) => {
    if (e.button !== 0) return;
    e.preventDefault();
    const listId = s.state.activeId;
    setDragKey(key);
    let raf = 0;
    const onMove = (ev: PointerEvent) => {
      const over = (document.elementFromPoint(ev.clientX, ev.clientY) as HTMLElement | null)?.closest<HTMLElement>("[data-key]")?.dataset.key;
      if (over && over !== key) s.move(listId, key, over);
      // Auto-scroll near the list edges.
      const box = listEl.getBoundingClientRect();
      const edge = ev.clientY < box.top + 32 ? -1 : ev.clientY > box.bottom - 32 ? 1 : 0;
      cancelAnimationFrame(raf);
      if (edge) {
        const tick = () => {
          listEl.scrollTop += edge * 8;
          raf = requestAnimationFrame(tick);
        };
        raf = requestAnimationFrame(tick);
      }
    };
    const end = () => {
      cancelAnimationFrame(raf);
      setDragKey(null);
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", end);
      window.removeEventListener("pointercancel", end);
      stopDrag = null;
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", end);
    window.addEventListener("pointercancel", end);
    stopDrag = end;
  };
  onCleanup(() => stopDrag?.());

  const sortBy = (by: SortKey) => {
    s.sortList(s.state.activeId, by, (k) => {
      const ins = p.byKey.get(k);
      if (!ins) return null;
      const q = p.feed.rows.get(ins.token)!.q;
      return { symbol: ins.symbol, exchange: ins.exchange, ltp: q.ltp, pct: pct(q.ltp, q.close) };
    });
  };

  const openList = (id: string) => {
    s.setActive(id);
    setView("list");
    setEditing(false);
    setConfirmDelete(false);
  };

  const newList = () => {
    const id = s.createList();
    if (!id) return toast("Limit reached", { body: `Up to ${MAX_LISTS} watchlists`, tone: "error" });
    setView("list");
    setEditing(true);
  };

  return (
    <aside class="sidebar" aria-label="Watchlists">
      <SearchBox instruments={p.instruments} store={s} onSelect={p.onSelect} />

      <nav class="tabs" role="tablist">
        <For each={s.state.lists}>
          {(l) => (
            <button
              role="tab"
              aria-selected={view() === "list" && s.state.activeId === l.id}
              classList={{ on: view() === "list" && s.state.activeId === l.id }}
              onClick={() => openList(l.id)}
              onDblClick={() => {
                openList(l.id);
                setEditing(true);
              }}
              title="Double-click to rename"
            >
              {l.name} <span class="count">{l.items.filter((k) => p.byKey.has(k)).length}</span>
            </button>
          )}
        </For>
        <button class="tab-add" onClick={newList} title="New watchlist" aria-label="New watchlist">
          +
        </button>
        <button role="tab" aria-selected={view() === "explore"} classList={{ on: view() === "explore", explore: true }} onClick={() => setView("explore")}>
          Explore
        </button>
      </nav>

      <Show
        when={view() === "list"}
        fallback={
          <div class="toolbar chips" role="group" aria-label="Category">
            <For each={categories()}>
              {(c) => (
                <button class="chip" classList={{ on: category() === c }} onClick={() => setCategory(c)}>
                  {c}
                </button>
              )}
            </For>
          </div>
        }
      >
        <div class="toolbar">
          <Show
            when={!editing()}
            fallback={
              <form
                class="rename"
                onSubmit={(e) => {
                  e.preventDefault();
                  s.renameList(s.state.activeId, (e.currentTarget.elements.namedItem("name") as HTMLInputElement).value);
                  setEditing(false);
                }}
              >
                <input
                  name="name"
                  value={s.active().name}
                  maxLength={40}
                  aria-label="Watchlist name"
                  ref={(el) => queueMicrotask(() => el.select())}
                  onKeyDown={(e) => e.key === "Escape" && setEditing(false)}
                  onBlur={(e) => {
                    s.renameList(s.state.activeId, e.currentTarget.value);
                    setEditing(false);
                  }}
                />
              </form>
            }
          >
            <span class="muted small">
              {visibleKeys().length} / {MAX_ITEMS}
            </span>
            <Show
              when={!confirmDelete()}
              fallback={
                <span class="confirm small">
                  Delete “{s.active().name}”?
                  <button class="link danger" onClick={() => (s.deleteList(s.state.activeId), setConfirmDelete(false))}>
                    Delete
                  </button>
                  <button class="link" onClick={() => setConfirmDelete(false)}>
                    Cancel
                  </button>
                </span>
              }
            >
              <span class="toolbar-actions">
                <select
                  aria-label="Sort watchlist"
                  value=""
                  onChange={(e) => {
                    const v = e.currentTarget.value as SortKey | "";
                    if (v) sortBy(v);
                    e.currentTarget.selectedIndex = 0; // one-shot action, not a persistent mode
                  }}
                >
                  <option value="" hidden>
                    Sort…
                  </option>
                  <option value="pctDesc">% change ↓</option>
                  <option value="pctAsc">% change ↑</option>
                  <option value="ltpDesc">Price ↓</option>
                  <option value="symbol">Symbol A–Z</option>
                  <option value="exchange">Exchange</option>
                </select>
                <button class="link" onClick={() => setEditing(true)}>
                  Rename
                </button>
                <Show when={s.state.lists.length > 1}>
                  <button class="link danger" onClick={() => setConfirmDelete(true)}>
                    Delete
                  </button>
                </Show>
              </span>
            </Show>
          </Show>
        </div>
      </Show>

      <ul class="list" role="listbox" aria-label="Instruments" ref={listEl} classList={{ "is-dragging": !!dragKey() }}>
        <For
          each={shownKeys()}
          fallback={
            <li class="empty muted">
              This watchlist is empty.
              <br />
              Press <kbd>/</kbd> to search, or open <button class="link" onClick={() => setView("explore")}>Explore</button>.
            </li>
          }
        >
          {(key) => {
            const ins = p.byKey.get(key)!;
            const row = p.feed.rows.get(ins.token)!;
            return (
              <QuoteRow
                row={row}
                itemKey={key}
                variant={view() === "list" ? "watch" : "explore"}
                selected={p.selected === ins.token}
                inList={s.has(key)}
                hasAlert={p.alertKeys().has(key)}
                dragging={dragKey() === key}
                onSelect={() => p.onSelect(ins.token)}
                onToggle={() => toggleInActive(ins)}
                onHandleDown={(e) => startDrag(e, key)}
                onNudge={(d) => s.nudge(s.state.activeId, key, d, (k) => p.byKey.has(k))}
              />
            );
          }}
        </For>
      </ul>
    </aside>
  );
}
