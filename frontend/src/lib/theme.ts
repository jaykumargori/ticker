import { createRoot, createSignal } from "solid-js";

export type ThemePref = "system" | "light" | "dark";
export type PalettePref = "standard" | "cb";

const THEME_KEY = "ticker.theme";
const PALETTE_KEY = "ticker.palette";

function read<T extends string>(key: string, allowed: readonly T[], def: T): T {
  try {
    const v = localStorage.getItem(key) as T | null;
    return v && allowed.includes(v) ? v : def;
  } catch {
    return def;
  }
}

function write(key: string, v: string) {
  try {
    localStorage.setItem(key, v);
  } catch {
    /* private mode: preference lasts for this session */
  }
}

/**
 * Theme and palette are plain attributes on <html>, and the CSS tokens do the
 * rest. Canvas charts can't read CSS, so they track `themeVersion` and
 * re-read tokens through cssVar() when it bumps.
 */
export const theme = createRoot(() => {
  const [pref, setPref] = createSignal<ThemePref>(read(THEME_KEY, ["system", "light", "dark"], "system"));
  const [palette, setPaletteSig] = createSignal<PalettePref>(read(PALETTE_KEY, ["standard", "cb"], "standard"));
  const [version, setVersion] = createSignal(0);

  const apply = () => {
    const root = document.documentElement;
    const p = pref();
    if (p === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", p);
    if (palette() === "cb") root.setAttribute("data-palette", "cb");
    else root.removeAttribute("data-palette");
    setVersion((v) => v + 1);
  };
  apply();
  matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => setVersion((v) => v + 1));

  return {
    pref,
    palette,
    version,
    setPref(p: ThemePref) {
      setPref(p);
      write(THEME_KEY, p);
      apply();
    },
    setPalette(p: PalettePref) {
      setPaletteSig(p);
      write(PALETTE_KEY, p);
      apply();
    },
  };
});

export function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}
