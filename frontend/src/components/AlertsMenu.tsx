import { createMemo, createSignal, For, Show } from "solid-js";
import type { Instrument } from "../lib/feed";
import type { WatchlistStore } from "../lib/watchlists";
import { useDismiss } from "./Toasts";

/** Top-bar bell: all alerts across instruments. */
export function AlertsMenu(p: { store: WatchlistStore; byKey: Map<string, Instrument>; onSelect: (token: number) => void }) {
  const [open, setOpen] = createSignal(false);
  let root!: HTMLDivElement;
  useDismiss(() => root, () => setOpen(false));

  const active = createMemo(() => p.store.state.alerts.filter((a) => a.triggeredAt === null).length);
  const hit = createMemo(() => p.store.state.alerts.length - active());
  const sorted = createMemo(() =>
    [...p.store.state.alerts].sort((a, b) => (b.triggeredAt ?? 0) - (a.triggeredAt ?? 0) || b.createdAt - a.createdAt),
  );

  return (
    <div class="menu-wrap" ref={root}>
      <button class="bell-btn" aria-expanded={open()} aria-label={`Alerts: ${active()} active`} onClick={() => setOpen(!open())}>
        🔔
        <Show when={active() + hit()}>
          {/* number = active alerts; amber = some have triggered and are unread */}
          <span class="badge" classList={{ hot: hit() > 0 }} title={`${active()} active, ${hit()} triggered`}>
            {active() || hit()}
          </span>
        </Show>
      </button>
      <Show when={open()}>
        <div class="menu menu-right alerts-menu" role="dialog" aria-label="Price alerts">
          <div class="menu-head">
            <strong>Price alerts</strong>
            <Show when={hit()}>
              <button class="link" onClick={() => p.store.clearTriggered()}>
                Clear triggered
              </button>
            </Show>
          </div>
          <Show when={sorted().length} fallback={<p class="muted small pad">No alerts. Open an instrument to create one.</p>}>
            <ul class="alert-list">
              <For each={sorted()}>
                {(a) => {
                  const ins = p.byKey.get(a.key);
                  const dec = ins?.decimals ?? 2;
                  return (
                    <li classList={{ hit: a.triggeredAt !== null }}>
                      <button
                        class="link"
                        disabled={!ins}
                        onClick={() => {
                          if (ins) p.onSelect(ins.token);
                          setOpen(false);
                        }}
                      >
                        {a.key.split(":")[1]}
                      </button>
                      <span>
                        {a.op === "above" ? "≥" : "≤"} {a.price.toFixed(dec)}
                      </span>
                      <span class="small muted">{a.triggeredAt ? `hit ${a.triggeredPrice?.toFixed(dec)}` : ins ? "active" : "unavailable"}</span>
                      <button class="icon-btn" aria-label="Delete alert" onClick={() => p.store.removeAlert(a.id)}>
                        ×
                      </button>
                    </li>
                  );
                }}
              </For>
            </ul>
          </Show>
        </div>
      </Show>
    </div>
  );
}
