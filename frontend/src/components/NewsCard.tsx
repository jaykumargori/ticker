import { createMemo, createSignal, For, onCleanup, onMount, Show } from "solid-js";
import type { Row } from "../lib/feed";
import { toast } from "./Toasts";

interface NewsItem {
  id: string;
  title: string;
  summary?: string;
  source: string;
  url: string;
  image?: string; // same-origin proxy path (/api/news/img?...)
  published: string;
}

interface NewsResult {
  items: NewsItem[];
  provider: string;
  query: string;
  fetchedAt: string;
  stale?: boolean;
}

const POLL_MS = 60_000; // server caches ~3 min per instrument, so this is cheap
const FRESH_MS = 90_000; // how long a newly arrived headline stays highlighted

const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto", style: "short" });
function ago(iso: string, now: number): string {
  const s = Math.round((new Date(iso).getTime() - now) / 1000);
  const a = Math.abs(s);
  if (a < 60) return "just now";
  if (a < 3600) return rtf.format(Math.round(s / 60), "minute");
  if (a < 86400) return rtf.format(Math.round(s / 3600), "hour");
  return rtf.format(Math.round(s / 86400), "day");
}

// Defence in depth: the server already allows only http(s) links and
// same-origin image paths.
const safeHref = (u: string) => (/^https?:\/\//i.test(u) ? u : undefined);
const safeImg = (u?: string) => (u && u.startsWith("/api/news/img?") ? u : undefined);

/** Image with a quiet monogram fallback (missing image, or load error). */
function Thumb(p: { src?: string; source: string }) {
  const [failed, setFailed] = createSignal(false);
  const initials = () =>
    p.source
      .split(/[\s.]+/)
      .filter(Boolean)
      .slice(0, 2)
      .map((w) => w[0]!.toUpperCase())
      .join("") || "N";
  return (
    <Show when={safeImg(p.src) && !failed()} fallback={<div class="news-thumb mono" aria-hidden="true">{initials()}</div>}>
      <img class="news-thumb" src={safeImg(p.src)} alt="" loading="lazy" decoding="async" width="96" height="72" onError={() => setFailed(true)} />
    </Show>
  );
}

/**
 * Latest headlines for the selected instrument. The first load renders
 * quietly. Later polls diff against already-seen ids: anything new slides in
 * with a "New" badge and raises one toast. The component is keyed per
 * instrument, so switching instruments starts a fresh "seen" set.
 */
export function NewsCard(p: { row: Row }) {
  const r = p.row;
  const [res, setRes] = createSignal<NewsResult | null>(null);
  const [status, setStatus] = createSignal<"loading" | "ready" | "error">("loading");
  const [fresh, setFresh] = createSignal<ReadonlySet<string>>(new Set());
  const [now, setNow] = createSignal(Date.now());
  let seen: Set<string> | null = null;

  const load = async (signal: AbortSignal) => {
    if (document.hidden) return;
    try {
      const resp = await fetch(`/api/news?token=${r.ins.token}`, { signal });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const data: NewsResult = await resp.json();
      const ids = data.items.map((i) => i.id);
      if (seen) {
        const added = data.items.filter((i) => !seen!.has(i.id));
        if (added.length) {
          setFresh((f) => new Set([...f, ...added.map((i) => i.id)]));
          toast(`${added.length > 1 ? `${added.length} new headlines` : "New headline"} · ${r.ins.symbol}`, { body: added[0].title, ms: 8000 });
          setTimeout(() => setFresh((f) => new Set([...f].filter((id) => !added.some((a) => a.id === id)))), FRESH_MS);
        }
        ids.forEach((id) => seen!.add(id));
      } else {
        seen = new Set(ids); // first load: nothing is "new"
      }
      setRes(data);
      setStatus("ready");
    } catch (e) {
      if ((e as Error).name === "AbortError") return;
      if (!res()) setStatus("error"); // keep showing the last good list on a transient failure
    }
  };

  onMount(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    const poll = setInterval(() => void load(ctrl.signal), POLL_MS);
    const tick = setInterval(() => setNow(Date.now()), 30_000);
    const onVis = () => !document.hidden && void load(ctrl.signal);
    document.addEventListener("visibilitychange", onVis);
    onCleanup(() => {
      ctrl.abort();
      clearInterval(poll);
      clearInterval(tick);
      document.removeEventListener("visibilitychange", onVis);
    });
  });

  const shown = createMemo(() => res()?.items ?? []);

  return (
    <section class="card news-card">
      <header class="card-head">
        <h2>News</h2>
        <span class="small muted">
          <Show when={res()} fallback="Latest headlines">
            {(d) => (
              <>
                via {d().provider}
                {d().stale ? " · cached" : ""}
              </>
            )}
          </Show>
        </span>
      </header>

      <Show when={status() !== "loading"} fallback={<NewsSkeleton />}>
        <Show
          when={status() === "ready"}
          fallback={<p class="muted small">News is unavailable right now. Retrying automatically.</p>}
        >
          <Show when={shown().length} fallback={<p class="muted small">No recent headlines for {r.ins.name}.</p>}>
            <ul class="news-list">
              <For each={shown()}>
                {(it) => (
                  <li class="news-item" classList={{ fresh: fresh().has(it.id) }}>
                    <a class="news-link" href={safeHref(it.url)} target="_blank" rel="noopener noreferrer nofollow" title={it.summary || it.title}>
                      <Thumb src={it.image} source={it.source} />
                      <span class="news-body">
                        <span class="news-title">{it.title}</span>
                        <span class="news-meta small muted">
                          <Show when={fresh().has(it.id)}>
                            <span class="new-pill">New</span>
                          </Show>
                          <span class="news-src">{it.source}</span> · <time datetime={it.published}>{ago(it.published, now())}</time>
                        </span>
                      </span>
                    </a>
                  </li>
                )}
              </For>
            </ul>
          </Show>
        </Show>
      </Show>
      <p class="small muted news-foot">
        {r.ins.exchange === "NSE-SIM" ? "Real news; NSE-SIM prices are simulated. " : ""}Not investment advice.
      </p>
    </section>
  );
}

function NewsSkeleton() {
  return (
    <ul class="news-list" aria-busy="true" aria-label="Loading news">
      <For each={[0, 1, 2, 3]}>
        {() => (
          <li class="news-item">
            <div class="news-link">
              <span class="news-thumb skeleton" />
              <span class="news-body">
                <span class="skeleton" style={{ width: "95%" }} />
                <span class="skeleton" style={{ width: "70%" }} />
                <span class="skeleton short" />
              </span>
            </div>
          </li>
        )}
      </For>
    </ul>
  );
}
