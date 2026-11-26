"use client";

import { createContext, useContext, useEffect, useRef, useState } from "react";

import { type StoreApi, useStore } from "zustand";

import { type FontKey, fontRegistry } from "@/lib/fonts/registry";
import { getClientCookie } from "@/lib/cookie.client";
import { getLocalStorageValue } from "@/lib/local-storage.client";
import {
  CONTENT_LAYOUT_VALUES,
  NAVBAR_STYLE_VALUES,
  SIDEBAR_COLLAPSIBLE_VALUES,
  SIDEBAR_VARIANT_VALUES,
} from "@/lib/preferences/layout";
import {
  applyContentLayout,
  applyFont,
  applyNavbarStyle,
  applySidebarCollapsible,
  applySidebarVariant,
} from "@/lib/preferences/layout-utils";
import { THEME_MODE_VALUES, THEME_PRESET_VALUES } from "@/lib/preferences/theme";
import { applyThemeMode, applyThemePreset, subscribeToSystemTheme } from "@/lib/preferences/theme-utils";

import { createPreferencesStore, type PreferencesState } from "./preferences-store";

const PreferencesStoreContext = createContext<StoreApi<PreferencesState> | null>(null);

const FONT_VALUES = Object.keys(fontRegistry) as FontKey[];

function getSafeValue<T extends string>(raw: string | null | undefined, allowed: readonly T[]): T | undefined {
  if (!raw) return undefined;
  return allowed.includes(raw as T) ? (raw as T) : undefined;
}

function readPersistedOrAttr<T extends string>(
  key: string,
  attrName: string,
  allowed: readonly T[],
): T | undefined {
  const fromStorage = getSafeValue(getLocalStorageValue(key), allowed);
  if (fromStorage) return fromStorage;
  const fromCookie = getSafeValue(getClientCookie(key), allowed);
  if (fromCookie) return fromCookie;
  return getSafeValue(document.documentElement.getAttribute(attrName), allowed);
}

function readDomState(): Partial<PreferencesState> {
  const root = document.documentElement;

  const themeMode = readPersistedOrAttr("theme_mode", "data-theme-mode", THEME_MODE_VALUES);
  const themePreset = readPersistedOrAttr("theme_preset", "data-theme-preset", THEME_PRESET_VALUES);
  const font = readPersistedOrAttr("font", "data-font", FONT_VALUES);
  const contentLayout = readPersistedOrAttr("content_layout", "data-content-layout", CONTENT_LAYOUT_VALUES);
  const navbarStyle = readPersistedOrAttr("navbar_style", "data-navbar-style", NAVBAR_STYLE_VALUES);
  const sidebarVariant = readPersistedOrAttr("sidebar_variant", "data-sidebar-variant", SIDEBAR_VARIANT_VALUES);
  const sidebarCollapsible = readPersistedOrAttr(
    "sidebar_collapsible",
    "data-sidebar-collapsible",
    SIDEBAR_COLLAPSIBLE_VALUES,
  );

  // Re-apply persisted attributes in case a route mounted without running ThemeBootScript.
  if (themePreset) applyThemePreset(themePreset);
  if (font) applyFont(font);
  if (contentLayout) applyContentLayout(contentLayout);
  if (navbarStyle) applyNavbarStyle(navbarStyle);
  if (sidebarVariant) applySidebarVariant(sidebarVariant);
  if (sidebarCollapsible) applySidebarCollapsible(sidebarCollapsible);

  const resolvedMode = themeMode ? applyThemeMode(themeMode) : root.classList.contains("dark") ? "dark" : "light";

  return {
    themeMode: themeMode ?? resolvedMode,
    resolvedThemeMode: resolvedMode,
    themePreset,
    font,
    contentLayout,
    navbarStyle,
    sidebarVariant,
    sidebarCollapsible,
  };
}

export const PreferencesStoreProvider = ({
  children,
  themeMode,
  themePreset,
  font,
  contentLayout,
  navbarStyle,
}: {
  children: React.ReactNode;
  themeMode: PreferencesState["themeMode"];
  themePreset: PreferencesState["themePreset"];
  font: PreferencesState["font"];
  contentLayout: PreferencesState["contentLayout"];
  navbarStyle: PreferencesState["navbarStyle"];
}) => {
  const [store] = useState<StoreApi<PreferencesState>>(() =>
    createPreferencesStore({
      themeMode,
      themePreset,
      font,
      contentLayout,
      navbarStyle,
    }),
  );

  const domSnapshotRef = useRef<Partial<PreferencesState> | null>(null);

  useEffect(() => {
    const domState = readDomState();
    domSnapshotRef.current = domState;

    store.setState((prev) => {
      const definedDomState = Object.fromEntries(
        Object.entries(domState).filter(([, value]) => value !== undefined),
      ) as Partial<PreferencesState>;
      return {
        ...prev,
        ...definedDomState,
        isSynced: true,
      };
    });
  }, [store]);

  useEffect(() => {
    let unsubscribeMedia: (() => void) | undefined;

    const applyFromMode = (mode: PreferencesState["themeMode"]) => {
      unsubscribeMedia?.();
      const resolved = applyThemeMode(mode);
      store.setState((prev) => ({ ...prev, resolvedThemeMode: resolved }));

      if (mode === "system") {
        unsubscribeMedia = subscribeToSystemTheme(() => {
          const next = applyThemeMode("system");
          store.setState((prev) => ({ ...prev, resolvedThemeMode: next }));
        });
      }
    };

    const startMode = domSnapshotRef.current?.themeMode ?? store.getState().themeMode;
    applyFromMode(startMode);

    const unsubscribeStore = store.subscribe((s, p) => {
      if (s.themeMode !== p.themeMode) applyFromMode(s.themeMode);
    });

    return () => {
      unsubscribeMedia?.();
      unsubscribeStore();
    };
  }, [store]);

  return <PreferencesStoreContext.Provider value={store}>{children}</PreferencesStoreContext.Provider>;
};

export const usePreferencesStore = <T,>(selector: (state: PreferencesState) => T): T => {
  const store = useContext(PreferencesStoreContext);
  if (!store) throw new Error("Missing PreferencesStoreProvider");
  return useStore(store, selector);
};
