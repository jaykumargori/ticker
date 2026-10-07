import { createEffect, createMemo, on, Show, untrack } from "solid-js";
import type { Row } from "../lib/feed";
import { money, moneyDelta } from "../lib/currency";
import { pct, signed } from "../lib/format";
import { cssVar, theme } from "../lib/theme";

const FLASH_OPTS: KeyframeAnimationOptions = { duration: 600, easing: "ease-out" };

// Keyframes follow the active theme/palette; rebuilt only when it changes.
let flashCache = { v: -1, up: [] as Keyframe[], down: [] as Keyframe[] };
const frames = (color: string): Keyframe[] => [
  { backgroundColor: `color-mix(in srgb, ${color} 28%, transparent)` },
  { backgroundColor: `color-mix(in srgb, ${color} 0%, transparent)` },
];

export function flash(el: HTMLElement, dir: number) {
  if (dir === 0) return;
  const v = untrack(theme.version);
  if (flashCache.v !== v) flashCache = { v, up: frames(cssVar("--up")), down: frames(cssVar("--down")) };
  // WAAPI: no class toggling, no forced reflow to restart a CSS animation.
  el.animate(dir > 0 ? flashCache.up : flashCache.down, FLASH_OPTS);
}

export function QuoteRow(p: {
  row: Row;
  itemKey: string;
  variant: "watch" | "explore";
  selected: boolean;
  inList: boolean;
  hasAlert: boolean;
  dragging?: boolean;
  onSelect: () => void;
  onToggle: () => void;
  onHandleDown?: (e: PointerEvent) => void;
  onNudge?: (delta: -1 | 1) => void;
}) {
  const r = p.row;
  let ltpEl!: HTMLSpanElement;

  // Recomputes only when this row's version is bumped (once per frame at most).
  const view = createMemo(() => {
    r.version();
    const { ltp, close } = r.q;
    const ch = close ? ltp - close : 0;
    return {
      ltp: money(ltp, r.ins),
      ch: moneyDelta(ch, r.ins, ltp),
      pct: signed(pct(ltp, close), 2) + "%",
      tone: ch > 0 ? "up" : ch < 0 ? "down" : "flat",
    };
  });

  createEffect(on(r.version, () => flash(ltpEl, r.dir), { defer: true }));

  const onKeyDown = (e: KeyboardEvent) => {
    const li = e.currentTarget as HTMLElement;
    if (e.key === "Enter") p.onSelect();
    else if (e.altKey && (e.key === "ArrowUp" || e.key === "ArrowDown") && p.onNudge) {
      e.preventDefault();
      p.onNudge(e.key === "ArrowUp" ? -1 : 1);
      queueMicrotask(() => li.focus()); // keep focus on the moved row
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const next = (e.key === "ArrowDown" ? li.nextElementSibling : li.previousElementSibling) as HTMLElement | null;
      next?.focus();
    } else if ((e.key === "Delete" || e.key === "Backspace") && p.variant === "watch") {
      const next = (li.nextElementSibling ?? li.previousElementSibling) as HTMLElement | null;
      p.onToggle();
      next?.focus();
    }
  };

  return (
    <li
      class="row"
      classList={{ selected: p.selected, dragging: p.dragging }}
      data-key={p.itemKey}
      tabIndex={0}
      role="option"
      aria-selected={p.selected}
      onClick={p.onSelect}
      onKeyDown={onKeyDown}
    >
      <Show when={p.variant === "watch"}>
        <span
          class="handle"
          title="Drag to reorder (or Alt+↑/↓)"
          aria-hidden="true"
          onPointerDown={(e) => p.onHandleDown?.(e)}
          onClick={(e) => e.stopPropagation()}
        >
          ⋮⋮
        </span>
      </Show>
      <div class="row-name">
        <span class={`sym ${view().tone}`}>
          {r.ins.symbol}
          <Show when={p.hasAlert}>
            <span class="bell" title="Price alert set" aria-label="alert set">
              ●
            </span>
          </Show>
        </span>
        <span class="exch">
          {r.ins.segment === "INDEX" ? "INDEX" : r.ins.exchange}
        </span>
      </div>
      <div class="row-nums">
        <span class={`ch ${view().tone}`}>{view().ch}</span>
        <span class={`pct ${view().tone}`}>{view().pct}</span>
        <span ref={ltpEl} class={`ltp ${view().tone}`}>{view().ltp}</span>
      </div>
      <button
        class="row-action"
        classList={{ added: p.variant === "explore" && p.inList }}
        title={p.inList ? "Remove from watchlist" : "Add to watchlist"}
        aria-label={p.inList ? `Remove ${r.ins.symbol} from watchlist` : `Add ${r.ins.symbol} to watchlist`}
        onClick={(e) => {
          e.stopPropagation();
          p.onToggle();
        }}
      >
        {p.variant === "watch" ? "−" : p.inList ? "✓" : "+"}
      </button>
    </li>
  );
}
