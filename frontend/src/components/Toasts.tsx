import { createSignal, For, onCleanup, onMount } from "solid-js";

export type Tone = "info" | "up" | "down" | "error";
interface Toast {
  id: number;
  title: string;
  body?: string;
  tone: Tone;
}

const [toasts, setToasts] = createSignal<Toast[]>([]);
let seq = 0;

/** Fire-and-forget notification; auto-dismisses. */
export function toast(title: string, opts: { body?: string; tone?: Tone; ms?: number } = {}) {
  const id = ++seq;
  setToasts((t) => [...t.slice(-4), { id, title, body: opts.body, tone: opts.tone ?? "info" }]); // keep at most 5
  setTimeout(() => dismiss(id), opts.ms ?? 6000);
}

const dismiss = (id: number) => setToasts((t) => t.filter((x) => x.id !== id));

export function Toasts() {
  return (
    <div class="toasts" role="status" aria-live="polite">
      <For each={toasts()}>
        {(t) => (
          <div class={`toast ${t.tone}`}>
            <div>
              <strong>{t.title}</strong>
              {t.body && <div class="small">{t.body}</div>}
            </div>
            <button class="icon-btn" aria-label="Dismiss" onClick={() => dismiss(t.id)}>
              ×
            </button>
          </div>
        )}
      </For>
    </div>
  );
}

/** Closes a popover on outside pointerdown or Escape. */
export function useDismiss(el: () => HTMLElement | undefined, close: () => void) {
  onMount(() => {
    const onDown = (e: PointerEvent) => {
      const root = el();
      if (root && !root.contains(e.target as Node)) close();
    };
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && close();
    document.addEventListener("pointerdown", onDown, true);
    document.addEventListener("keydown", onKey);
    onCleanup(() => {
      document.removeEventListener("pointerdown", onDown, true);
      document.removeEventListener("keydown", onKey);
    });
  });
}
