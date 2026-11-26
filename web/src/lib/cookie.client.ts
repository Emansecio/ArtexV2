// Client-side cookie utilities.
// These functions manage cookies in the browser only.
// Server actions handle cookie updates on the server side.

function writeClientCookie(serializedCookie: string) {
  // biome-ignore lint/suspicious/noDocumentCookie: This project still uses document.cookie for broad browser support.
  document.cookie = serializedCookie;
}

export function setClientCookie(key: string, value: string, days = 365) {
  const expires = new Date(Date.now() + days * 864e5).toUTCString();
  writeClientCookie(`${key}=${encodeURIComponent(value)}; expires=${expires}; path=/; SameSite=Lax`);
}

export function getClientCookie(key: string) {
  const raw = document.cookie
    .split("; ")
    .find((row) => row.startsWith(`${key}=`))
    ?.slice(key.length + 1);
  if (raw === undefined) return undefined;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

export function deleteClientCookie(key: string) {
  writeClientCookie(`${key}=; expires=Thu, 01 Jan 1970 00:00:00 UTC; path=/`);
}
