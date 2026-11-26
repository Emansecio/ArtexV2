import * as React from "react";

import { BrainIcon, CpuIcon, ServerIcon, SparklesIcon, TerminalIcon } from "lucide-react";

import type { ProfileFormat } from "./llm-helpers";

/**
 * Antigravity CLI 4-point geometric sparkle (exact Slim Desktop geometry:
 * 4 points with 0.26*R waist around center (12, 12), radius 10).
 */
export function AntigravityIcon({ className = "size-4" }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="currentColor"
      aria-hidden="true"
      className={className}
    >
      <path d="M12 2 L14.6 9.4 L22 12 L14.6 14.6 L12 22 L9.4 14.6 L2 12 L9.4 9.4 Z" />
    </svg>
  );
}

/**
 * Devin / Cognition monochrome hexagonal emblem (adapts to light/dark themes
 * via currentColor, matching Slim Desktop's hexagonal Devin symbol).
 */
export function DevinIcon({ className = "size-4" }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      aria-hidden="true"
      className={className}
    >
      <path
        d="M12 2.25 L20.4 7.1 V16.9 L12 21.75 L3.6 16.9 V7.1 Z"
        stroke="currentColor"
        strokeWidth="1.9"
        strokeLinejoin="round"
      />
      <circle cx="12" cy="8.1" r="1.65" fill="currentColor" />
      <circle cx="8.6" cy="14.2" r="1.65" fill="currentColor" />
      <circle cx="15.4" cy="14.2" r="1.65" fill="currentColor" />
      <path
        d="M12 9.4 L9.3 13.1 M12 9.4 L14.7 13.1 M10 14.2 H14"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function ProviderIcon({
  format,
  className = "size-4",
}: {
  format?: ProfileFormat | string;
  className?: string;
}) {
  switch (format) {
    case "antigravity-cli":
      return <AntigravityIcon className={className} />;
    case "devin":
      return <DevinIcon className={className} />;
    case "anthropic":
    case "anthropic-oauth":
      return <SparklesIcon className={className} />;
    case "ollama":
      return <ServerIcon className={className} />;
    case "opencode-zen":
    case "opencode-go":
      return <TerminalIcon className={className} />;
    case "openai-responses":
    case "chatgpt":
      return <BrainIcon className={className} />;
    default:
      return <CpuIcon className={className} />;
  }
}
