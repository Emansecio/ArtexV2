"use client";

import { setClientCookie } from "../cookie.client";
import { setLocalStorageValue } from "../local-storage.client";
import { PREFERENCE_PERSISTENCE, type PreferenceKey } from "./preferences-config";

export async function persistPreference(key: PreferenceKey, value: string) {
  const mode = PREFERENCE_PERSISTENCE[key];

  switch (mode) {
    case "none":
      return;

    // Static export + macOS WKWebView: write to BOTH localStorage and client cookie so
    // theme and layout preferences reliably survive desktop app termination & restarts.
    case "server-cookie":
    case "client-cookie":
      setLocalStorageValue(key, value);
      setClientCookie(key, value);
      return;

    case "localStorage":
      setLocalStorageValue(key, value);
      setClientCookie(key, value);
      return;

    default:
      return;
  }
}
