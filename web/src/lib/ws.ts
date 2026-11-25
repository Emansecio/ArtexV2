import { sseUrl } from "@/lib/api";

export interface WSSubscribeOpts {
  since?: number;
  onEvent: (event: string, data: unknown) => void;
  onState?: (connected: boolean) => void;
}

interface SubEntry {
  since: number;
  onEvent: (event: string, data: unknown) => void;
  onState?: (connected: boolean) => void;
}

const channels = new Map<string, Set<SubEntry>>();
const connListeners = new Set<(connected: boolean) => void>();
const dirtyListeners = new Set<(channel: string, topic?: string) => void>();

let socket: WebSocket | null = null;
let connected = false;
let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
let reconnectMs = 1000;
let dirtyTimer: ReturnType<typeof setTimeout> | undefined;
const pendingDirty = new Map<string, string | undefined>();

function buildWSUrl(): string {
  const raw = sseUrl("/api/ws");
  if (raw.startsWith("http://") || raw.startsWith("https://")) {
    return raw.replace(/^http/, "ws");
  }
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${raw}`;
}

function sendControl(msg: { op: "sub" | "unsub" | "ping"; channel?: string; since?: number }) {
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify(msg));
  }
}

function notifyConnection(next: boolean) {
  if (connected === next) return;
  connected = next;
  for (const fn of connListeners) fn(next);
  for (const subs of channels.values()) {
    for (const s of subs) s.onState?.(next);
  }
}

function notifyDirty(channel: string, topic?: string) {
  pendingDirty.set(`${channel}\0${topic ?? ""}`, topic);
  if (dirtyTimer !== undefined) return;
  dirtyTimer = setTimeout(() => {
    dirtyTimer = undefined;
    const items = [...pendingDirty.entries()];
    pendingDirty.clear();
    for (const [k, tp] of items) {
      const ch = k.slice(0, k.indexOf("\0"));
      for (const fn of dirtyListeners) fn(ch, tp);
    }
  }, 600);
}

function ensureSocket() {
  if (typeof window === "undefined" || channels.size === 0) return;
  if (socket && (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING)) {
    return;
  }
  if (reconnectTimer !== undefined) {
    clearTimeout(reconnectTimer);
    reconnectTimer = undefined;
  }

  let ws: WebSocket;
  try {
    ws = new WebSocket(buildWSUrl());
  } catch {
    scheduleReconnect();
    return;
  }
  socket = ws;

  ws.onopen = () => {
    if (socket !== ws) return;
    reconnectMs = 1000;
    notifyConnection(true);
    for (const [ch, subs] of channels) {
      let minSince = 0;
      for (const s of subs) {
        if (s.since > 0 && (minSince === 0 || s.since < minSince)) minSince = s.since;
      }
      sendControl({ op: "sub", channel: ch, since: minSince || undefined });
    }
  };

  const dispatchFrame = (msg: { channel?: string; event?: string; data?: unknown }) => {
    if (msg.event === "batch" && Array.isArray(msg.data)) {
      for (const item of msg.data) {
        if (item && typeof item === "object") {
          dispatchFrame(item as { channel?: string; event?: string; data?: unknown });
        }
      }
      return;
    }
    const ch = msg.channel;
    const event = msg.event;
    if (!ch || !event) return;
    if (event === "resync") {
      notifyDirty(ch);
      return;
    }
    const subs = channels.get(ch);
    if (subs) {
      for (const s of subs) s.onEvent(event, msg.data);
    }
    if (ch === "global" && event === "topic") {
      const tp = msg.data && typeof msg.data === "object" ? (msg.data as { topic?: string }).topic : undefined;
      notifyDirty(ch, tp);
    } else if (event === "activity" || event === "status" || event === "side") {
      notifyDirty(ch);
    }
  };

  ws.onmessage = (ev) => {
    if (typeof ev.data !== "string") return;
    let msg: { channel?: string; event?: string; data?: unknown };
    try {
      msg = JSON.parse(ev.data) as { channel?: string; event?: string; data?: unknown };
    } catch {
      return;
    }
    dispatchFrame(msg);
  };

  const handleClose = () => {
    if (socket !== ws) return;
    socket = null;
    notifyConnection(false);
    if (channels.size > 0) scheduleReconnect();
  };

  ws.onclose = handleClose;
  ws.onerror = () => {
    for (const subs of channels.values()) {
      for (const s of subs) s.onState?.(false);
    }
    try {
      ws.close();
    } catch {
      handleClose();
    }
  };
}

function scheduleReconnect() {
  if (reconnectTimer !== undefined || channels.size === 0) return;
  const wait = reconnectMs;
  reconnectMs = Math.min(reconnectMs * 2, 15000);
  reconnectTimer = setTimeout(() => {
    reconnectTimer = undefined;
    ensureSocket();
  }, wait);
}

export function isWSConnected(): boolean {
  return connected;
}

export function onWSConnectionChange(fn: (connected: boolean) => void): () => void {
  connListeners.add(fn);
  return () => connListeners.delete(fn);
}

export function onWSDirty(fn: (channel: string, topic?: string) => void): () => void {
  dirtyListeners.add(fn);
  return () => dirtyListeners.delete(fn);
}

export function subscribeWS(channel: string, opts: WSSubscribeOpts): () => void {
  const entry: SubEntry = {
    since: opts.since ?? 0,
    onEvent: opts.onEvent,
    onState: opts.onState,
  };
  let set = channels.get(channel);
  const first = !set || set.size === 0;
  if (!set) {
    set = new Set();
    channels.set(channel, set);
  }
  set.add(entry);

  if (connected) {
    entry.onState?.(true);
    if (first) sendControl({ op: "sub", channel, since: entry.since || undefined });
  } else {
    ensureSocket();
  }

  let closed = false;
  return () => {
    if (closed) return;
    closed = true;
    const cur = channels.get(channel);
    if (!cur) return;
    cur.delete(entry);
    if (cur.size === 0) {
      channels.delete(channel);
      sendControl({ op: "unsub", channel });
    }
    if (channels.size === 0 && socket) {
      const ws = socket;
      socket = null;
      notifyConnection(false);
      ws.close();
    }
  };
}
