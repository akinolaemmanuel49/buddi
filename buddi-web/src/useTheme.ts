import { useCallback, useEffect, useState } from "react";

const THEME_KEY = "buddi.theme";

export type Theme = "dark" | "light";

/**
 * useTheme keeps the colour scheme in a data attribute so the stylesheet owns both
 * themes.
 *
 * The choice is applied to the document element rather than kept in React state
 * alone, because a token-driven theme has to reach CSS that renders outside the
 * component tree, and it is persisted because a theme that resets on every reload
 * reads as a bug.
 *
 * The stored value is only trusted once it has been validated: it is attacker-
 * reachable in the sense that any script that can write localStorage could put
 * anything there, and a bad value would silently fall back with no way to tell.
 */
export function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(() => readStoredTheme());

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem(THEME_KEY, theme);
  }, [theme]);

  const toggle = useCallback(() => {
    setTheme((current) => (current === "dark" ? "light" : "dark"));
  }, []);

  return [theme, toggle];
}

function readStoredTheme(): Theme {
  try {
    const stored = localStorage.getItem(THEME_KEY);
    if (stored === "light" || stored === "dark") {
      return stored;
    }
  } catch {
    // Storage can be unavailable in private modes; the default still applies.
  }

  // Following the system is the least surprising default, but only when the stored
  // preference is absent.
  return window.matchMedia?.("(prefers-color-scheme: light)").matches ? "light" : "dark";
}
