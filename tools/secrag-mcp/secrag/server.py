"""MCP Server exposing ArtexV2's local Cybersecurity Knowledge Base (secrag)."""

from __future__ import annotations

import argparse
import os
import sys
import threading
from typing import Optional

from mcp.types import ToolAnnotations

try:
    from mcp.server.mcpserver import MCPServer as _MCPServer
except ImportError:
    from mcp.server.fastmcp import FastMCP as _MCPServer  # type: ignore

from .embeddings import LocalEmbedder, LocalReranker
from .retriever import VALID_CATEGORIES, VALID_FOCUSES, HybridRetriever
from .storage import KnowledgeStore, resolve_data_dir

READONLY_ANNOTATIONS = ToolAnnotations(
    readOnlyHint=True,
    destructiveHint=False,
    idempotentHint=True,
    openWorldHint=False,
)

mcp = _MCPServer(
    "secrag",
    instructions=(
        "Local offline Cybersecurity Knowledge Base (HackTricks, HackTricks Cloud, The Hacker Recipes, "
        "HackingTheCloud, Stratus Red Team, PayloadsAllTheThings, InternalAllTheThings, PortSwigger Labs, "
        "HackerOne Disclosed Reports, HowToHunt, OWASP WSTG/MASTG/API/ASVS/LLM/CheatSheets, GTFOBins, LOLBAS, "
        "WADComs, MITRE ATT&CK, Atomic Red Team, SigmaHQ, Semgrep Whitebox Rules, Nuclei Templates, Vulhub PoCs, "
        "Metasploit Module Docs, CISA KEV, Exploit-DB, Trickest CVE PoCs, PoC-in-GitHub, GHSA Advisories, CVEListV5). "
        "Use concise English technical keywords in kb_search, then kb_read_section to "
        "expand promising hits, or kb_lookup_cve for exact CVE intelligence."
    ),
)

# RLock is required because _get_retriever() calls _get_store() while holding _init_lock.
_init_lock = threading.RLock()
_retriever: Optional[HybridRetriever] = None
_store: Optional[KnowledgeStore] = None


def _get_store() -> KnowledgeStore:
    global _store
    if _store is not None:
        return _store
    with _init_lock:
        if _store is None:
            db_file = resolve_data_dir() / "kb.sqlite"
            if not db_file.exists():
                # Bootstrap empty schema once, then close and reopen strictly read-only
                # so every anyio worker thread uses its own TLS SQLite connection cleanly.
                init_store = KnowledgeStore(read_only=False)
                init_store.close()
            _store = KnowledgeStore(read_only=True)
    return _store


def _get_retriever() -> HybridRetriever:
    global _retriever
    if _retriever is not None:
        return _retriever
    with _init_lock:
        if _retriever is None:
            _retriever = HybridRetriever(
                store=_get_store(),
                embedder=LocalEmbedder(),
                reranker=LocalReranker(),
            )
    return _retriever


def _start_background_warmup() -> None:
    """Pre-warm SQLite metadata cache, USearch mmap, and ONNX models in a daemon
    thread after MCP startup so the first agent kb_search hits warm caches (~330ms)
    instead of paying a ~3.3s cold start, without delaying initialize/tools/list."""
    if os.environ.get("SECRAG_DISABLE_WARMUP", "").strip().lower() in ("1", "true", "yes"):
        return
    db_file = resolve_data_dir() / "kb.sqlite"
    if not db_file.exists():
        return

    def _run() -> None:
        try:
            retr = _get_retriever()
            idx, sig = retr.store.get_readonly_vector_index_with_sig()
            if idx is not None:
                retr.store._get_meta_cache(sig)
            retr.embedder._ensure_fastembed()
            retr.reranker._ensure_loaded()
        except Exception:
            pass

    threading.Thread(target=_run, name="secrag-warmup", daemon=True).start()


VALID_SOURCES = {
    "hacktricks", "hacktricks-cloud", "payloads", "owasp", "privesc-bins",
    "mitre-attack", "atomic-red-team", "nuclei", "cisa-kev",
    "bugbounty-writeups", "poc-in-github", "sigma-rules", "exploitdb",
    "cvelist-v5", "primus-seed",
    # Phase 3 — Deep Anti-Duplicate Bug Bounty, PoC & Advisory Pack
    "hackerone-disclosed", "portswigger-labs", "bounty-methodology",
    "the-hacker-recipes", "hacking-the-cloud", "vulhub-pocs",
    "metasploit-docs", "trickest-cve", "ghsa-advisories", "semgrep-rules",
}


CATEGORY_ALIASES = {
    "bounty": "writeups",
    "bugbounty": "writeups",
    "bug-bounty": "writeups",
    "hackerone": "writeups",
    "reports": "writeups",
    "whitebox": "detection",
    "code": "detection",
    "sast": "detection",
    "semgrep": "detection",
    "sigma": "detection",
    "k8s": "cloud",
    "kubernetes": "cloud",
    "aws": "cloud",
    "gcp": "cloud",
    "azure": "cloud",
    "container": "cloud",
    "active-directory": "ad",
    "kerberos": "ad",
    "adcs": "ad",
    "poc": "exploits",
    "pocs": "exploits",
    "exploit": "exploits",
    "metasploit": "exploits",
    "vulhub": "exploits",
    "cves": "cve",
    "advisory": "cve",
    "advisories": "cve",
    "ghsa": "cve",
    "package": "cve",
    "waf": "web",
    "bypass": "web",
    "http": "web",
    "llm": "web",
    "ai": "web",
}


@mcp.tool(annotations=READONLY_ANNOTATIONS)
def kb_search(
    query: str,
    category: str = "",
    source: str = "",
    focus: str = "",
    top_k: int = 5,
) -> str:
    """Search ArtexV2's local offline Cybersecurity Knowledge Base (~482k docs) with intent-aware 3-stage hybrid retrieval (BM25 + synonym expansion + dense vectors + cross-encoder reranker + cross-source diversity).

    Always available, instant (<500ms), zero network noise. Use concise English technical keywords
    (e.g. "SSTI Jinja2 filter bypass", "OAuth redirect_uri account takeover", "single-packet race condition",
    "HTTP/2 request smuggling cache poisoning", "Kubernetes RBAC privilege escalation", "ADCS ESC8 relay",
    "Next.js middleware auth bypass").
    Optional focus (auto-detected if omitted): bounty, exploit, whitebox, cloud, ad, bypass, cve.
    Optional category filter: web, api, cloud, ad, privesc, mobile, network, binary, cve, mitre, detection, exploits, writeups, threat-intel (aliases like bounty/whitebox/k8s/poc/ghsa are auto-normalized).
    Optional source filter: hacktricks, hacktricks-cloud, the-hacker-recipes, hacking-the-cloud, payloads, portswigger-labs, bounty-methodology, hackerone-disclosed, bugbounty-writeups, owasp, privesc-bins, mitre-attack, atomic-red-team, nuclei, vulhub-pocs, metasploit-docs, cisa-kev, exploitdb, trickest-cve, poc-in-github, ghsa-advisories, cvelist-v5, sigma-rules, semgrep-rules.
    Returns ranked cross-source excerpts with doc_id and section_id; call kb_read_section(doc_id, section_id) to read the full section of the winning hit.
    """
    cat = category.strip().lower()
    if cat in CATEGORY_ALIASES:
        cat = CATEGORY_ALIASES[cat]
    if cat and cat not in VALID_CATEGORIES:
        return (
            f"Invalid category '{category}'. Valid categories: "
            + ", ".join(sorted(VALID_CATEGORIES))
        )
    src = source.strip().lower()
    if src and src not in VALID_SOURCES:
        return (
            f"Invalid source '{source}'. Valid sources: "
            + ", ".join(sorted(VALID_SOURCES))
        )
    foc = focus.strip().lower()
    if foc and foc not in VALID_FOCUSES:
        return (
            f"Invalid focus '{focus}'. Valid focus values: "
            + ", ".join(sorted(VALID_FOCUSES))
        )

    res = _get_retriever().search(
        query=query,
        category=cat or None,
        source=src or None,
        focus=foc or None,
        top_k=max(1, min(int(top_k or 5), 10)),
    )
    hits = res.get("results") or []
    if not hits:
        return (
            f"No knowledge-base matches found for query={query!r}"
            + (f" category={cat!r}" if cat else "")
            + (f" source={source!r}" if source else "")
            + (f" focus={foc!r}" if foc else "")
            + ". Try broader English technical terms or remove filters."
        )

    intent_tag = f", intent={res['intent']}" if res.get("intent") else ""
    lines = [
        f"Found {len(hits)} match(es) [{res.get('mode')}{intent_tag}, {res.get('latency_ms')} ms] for: {query}"
    ]
    surfaced_cves: list[str] = []
    for idx, h in enumerate(hits, start=1):
        cves_h = h.get("cves") or []
        for c in cves_h[:2]:
            if c not in surfaced_cves:
                surfaced_cves.append(c)
        cve_part = f" | CVEs: {', '.join(cves_h[:4])}" if cves_h else ""
        lines.append(
            f"\n### [{idx}] {h['breadcrumb']}\n"
            f"- ref: `doc_id={h['doc_id']}`, `section_id={h['section_id']}` | "
            f"source: `{h['source']}` | category: `{h['category']}` | score: {h['score']:.3f}{cve_part}\n\n"
            f"{h['excerpt']}"
        )
    if surfaced_cves and res.get("intent") != "cve":
        lines.append(
            f"\n> Next-step tip: call `kb_read_section(doc_id, section_id)` for full payloads/commands, "
            f"or `kb_lookup_cve({surfaced_cves[0]!r})` for full KEV/Vulhub/Nuclei/Metasploit/PoC intelligence."
        )
    return "\n".join(lines)


@mcp.tool(annotations=READONLY_ANNOTATIONS)
def kb_read_section(doc_id: int, section_id: int = 0) -> str:
    """Read the full content of a document section returned by kb_search or kb_lookup_cve.

    Pass both doc_id and section_id for the complete section (including full command blocks and bypasses).
    Pass section_id=0 to read the document overview and see its section outline.
    """
    sec_arg = int(section_id) if section_id and int(section_id) > 0 else None
    data = _get_store().read_section(int(doc_id), section_id=sec_arg)
    if data is None:
        return f"Document/section not found (doc_id={doc_id}, section_id={section_id})."

    lines = [
        f"# {data['title']}",
        f"- source: `{data['source']}` | category: `{data['category']}` | path: `{data['path']}`",
    ]
    if data.get("section_not_found") is not None:
        lines.append(
            f"- note: `section_id={data['section_not_found']}` was not found in `doc_id={data['doc_id']}`; "
            "showing document overview and available sections below."
        )
    if data.get("breadcrumb"):
        lines.append(f"- section: {data['breadcrumb']} (`section_id={data.get('section_id')}`)")
    if data.get("cves"):
        lines.append(f"- cves: {data['cves']}")
    if data.get("url"):
        lines.append(f"- url: {data['url']}")
    lines.append("")
    lines.append(data["content"])
    if data.get("truncated"):
        lines.append("\n[Note: content truncated at 12,000 chars; inspect individual sections from the outline below.]")

    outline = data.get("outline") or []
    if len(outline) > 1:
        lines.append("\n## Document Sections Outline")
        for s in outline[:25]:
            lines.append(f"- `section_id={s['section_id']}`: {s['heading']} ({s['chars']} chars)")
    return "\n".join(lines)


@mcp.tool(annotations=READONLY_ANNOTATIONS)
def kb_lookup_cve(cve_id: str) -> str:
    """Direct indexed lookup for a specific CVE (e.g. 'CVE-2024-3094') across CISA KEV, Vulhub PoCs, Nuclei Templates, Metasploit Docs, Exploit-DB, Trickest CVE PoCs, PoC-in-GitHub, HackerOne Disclosed Reports, GHSA Advisories, CVEListV5, and HackTricks.

    Prefer this tool whenever you encounter a concrete CVE identifier or want to check whether a CVE is in CISA KEV or has a Nuclei template / Vulhub / Metasploit / GitHub PoC.
    """
    if not cve_id or not cve_id.strip():
        return "Please provide a valid CVE identifier (e.g. 'CVE-2024-3094')."
    res = _get_store().lookup_cve(cve_id)
    matches = res.get("matches") or []
    if not matches:
        return f"No indexed records found for {res['cve_id']}. Try kb_search with the product/software name."

    kev_badge = "YES (Active Exploitation — CISA KEV)" if res["is_kev"] else "No"
    lines = [
        f"# {res['cve_id']} Intelligence ({res['match_count']} record(s))",
        f"- CISA KEV: **{kev_badge}**",
    ]
    for idx, m in enumerate(matches, start=1):
        sev = f" | severity: `{m['severity']}`" if m.get("severity") else ""
        url = f" | url: {m['url']}" if m.get("url") else ""
        lines.append(
            f"\n### [{idx}] [{m['source']}] {m['title']}\n"
            f"- ref: `doc_id={m['doc_id']}`, `section_id={m.get('section_id') or 0}`{sev}{url}\n\n"
            f"{m['excerpt']}"
        )
    return "\n".join(lines)


@mcp.tool(annotations=READONLY_ANNOTATIONS)
def kb_stats() -> str:
    """Inspect loaded knowledge-base sources, document/chunk/vector counts, and available categories."""
    st = _get_store().stats()
    lines = [
        "# ArtexV2 Cybersecurity Knowledge Base Status",
        f"- Data dir: `{st['data_dir']}`",
        f"- Documents: **{st['documents']:,}** | Sections: **{st['sections']:,}** | Chunks: **{st['chunks']:,}**",
        f"- Embedded vectors: **{st['embedded_chunks']:,}** (USearch HNSW entries: **{st['vector_index_entries']:,}**) | Lexical-only chunks: **{st.get('lexical_only_chunks', 0):,}**",
        f"- Unique CVEs indexed: **{st['unique_cves']:,}**",
        f"- Disk size: SQLite `{st['sqlite_mb']} MB` + USearch `{st['usearch_mb']} MB`",
        "",
        "## Categories",
    ]
    for c in st["categories"]:
        lines.append(f"- `{c['category']}`: {c['documents']:,} docs")
    lines.append("\n## Sources")
    for s in st["sources"]:
        err = f" (error: {s['error']})" if s.get("error") else ""
        lines.append(
            f"- `[phase {s['phase']}] {s['source']}`: **{s['status']}** — "
            f"{s['documents']:,} docs, {s['chunks']:,} chunks{err}"
        )
    return "\n".join(lines)


def main() -> None:
    parser = argparse.ArgumentParser(description="ArtexV2 Cybersecurity RAG MCP Server")
    parser.add_argument("--data-dir", default=None, help="Path to secrag data directory")
    parser.add_argument(
        "--transport",
        choices=["stdio", "streamable-http"],
        default="stdio",
        help="MCP transport (default: stdio)",
    )
    parser.add_argument("--host", default="127.0.0.1", help="HTTP host (when transport=streamable-http)")
    parser.add_argument("--port", type=int, default=8090, help="HTTP port (when transport=streamable-http)")
    args = parser.parse_args()

    if args.data_dir:
        os.environ["SECRAG_DATA_DIR"] = args.data_dir

    _start_background_warmup()

    if args.transport == "streamable-http":
        if hasattr(mcp, "settings") and hasattr(mcp.settings, "host"):
            mcp.settings.host = args.host
            mcp.settings.port = args.port
            mcp.run(transport="streamable-http")
        else:
            mcp.run(transport="streamable-http", host=args.host, port=args.port)
    else:
        mcp.run(transport="stdio")


if __name__ == "__main__":
    main()
