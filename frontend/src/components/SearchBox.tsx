import { createMemo, createSignal, For, onCleanup, onMount, Show } from "solid-js";
import type { Instrument } from "../lib/feed";
import type { WatchlistStore } from "../lib/watchlists";
import { toast, useDismiss } from "./Toasts";

const keyOf = (i: Instrument) => `${i.exchange}:${i.symbol}`;

/**
 * Ranked search: exact symbol > symbol prefix > symbol contains > name
 * contains. A linear scan over ~200 instruments takes microseconds, so no
 * index is needed. Kite does this server-side because it has ~100k instruments.
 */
function rank(list: Instrument[], q: string): Instrument[] {
  const scored: [number, Instrument][] = [];
  for (const i of list) {
    const s = i.symbol.toUpperCase();
    const score =
      s === q ? 0 : s.startsWith(q) ? 1 : s.includes(q) ? 2 : i.name.toUpperCase().includes(q) ? 3 : -1;
    if (score >= 0) scored.push([score * 1000 + s.length, i]); // shorter symbols first within a tier
  }
  return scored.sort((a, b) => a[0] - b[0]).slice(0, 10).map(([, i]) => i);
}

export function SearchBox(p: { instruments: Instrument[]; store: WatchlistStore; onSelect: (token: number) => void }) {
  let input!: HTMLInputElement;
  let root!: HTMLDivElement;
  const [query, setQuery] = createSignal("");
  const [hi, setHi] = createSignal(0);
  const results = createMemo(() => {
    const q = query().trim().toUpperCase();
    return q ? rank(p.instruments, q) : [];
  });

  const close = () => {
    setQuery("");
    setHi(0);
  };
  useDismiss(() => root, close);

  // "/" focuses search from anywhere (Kite shortcut), unless already typing.
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (e.key === "/" && !/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) && !t.isContentEditable) {
        e.preventDefault();
        input.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  const toggle = (i: Instrument) => {
    const k = keyOf(i);
    if (p.store.has(k)) {
      p.store.removeItem(k);
      toast(`Removed ${i.symbol}`, { body: `from “${p.store.active().name}”`, ms: 2500 });
    } else {
      const err = p.store.addItem(k);
      if (err) toast("Can't add", { body: err, tone: "error" });
      else toast(`Added ${i.symbol}`, { body: `to “${p.store.active().name}”`, ms: 2500 });
    }
  };

  const onKeyDown = (e: KeyboardEvent) => {
    const n = results().length;
    if (e.key === "ArrowDown" && n) {
      e.preventDefault();
      setHi((h) => (h + 1) % n);
    } else if (e.key === "ArrowUp" && n) {
      e.preventDefault();
      setHi((h) => (h - 1 + n) % n);
    } else if (e.key === "Enter" && n) {
      const i = results()[hi()];
      if (!p.store.has(keyOf(i))) toggle(i);
      p.onSelect(i.token);
      close();
      input.blur();
    } else if (e.key === "Escape") {
      close();
      input.blur();
    }
  };

  return (
    <div class="search" ref={root}>
      <input
        ref={input}
        type="search"
        placeholder="Search & add  (press / )"
        aria-label="Search instruments"
        aria-expanded={results().length > 0}
        aria-controls="search-results"
        value={query()}
        onInput={(e) => {
          setQuery(e.currentTarget.value);
          setHi(0);
        }}
        onKeyDown={onKeyDown}
      />
      <Show when={results().length}>
        <ul class="results" id="search-results" role="listbox">
          <For each={results()}>
            {(i, idx) => {
              const inList = () => p.store.has(keyOf(i));
              return (
                <li
                  role="option"
                  aria-selected={hi() === idx()}
                  classList={{ hi: hi() === idx() }}
                  onPointerEnter={() => setHi(idx())}
                  onClick={() => {
                    p.onSelect(i.token);
                    close();
                  }}
                >
                  <span class="res-main">
                    <strong>{i.symbol}</strong>
                    <span class="muted small">{i.name}</span>
                  </span>
                  <span class="res-side">
                    <span class="tag">{i.segment === "INDEX" ? "INDEX" : i.exchange}</span>
                    <button
                      class="row-action visible"
                      classList={{ added: inList() }}
                      aria-label={inList() ? `Remove ${i.symbol}` : `Add ${i.symbol}`}
                      onClick={(e) => {
                        e.stopPropagation();
                        toggle(i);
                      }}
                    >
                      {inList() ? "✓" : "+"}
                    </button>
                  </span>
                </li>
              );
            }}
          </For>
          <li class="res-hint muted small" aria-hidden="true">
            ↑↓ navigate · Enter add & open · Esc close
          </li>
        </ul>
      </Show>
    </div>
  );
}
