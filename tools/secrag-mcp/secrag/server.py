"""MCP Server exposing ArtexV2's local Cybersecurity Knowledge Base (secrag)."""

from __future__ import annotations

import argparse
import os
import sys
from typing import Optional

try:
    from mcp.server.mcpserver import MCPServer as _MCPServer
except ImportError:
    from mcp.server.fastmcp import FastMCP as _MCPServer  # type: ignore

from .embeddings import LocalEmbedder, LocalReranker
from .retriever import VALID_CATEGORIES, HybridRetriever
from .storage import KnowledgeStore

mcp = _MCPServer(
    "secrag",
    instructions=(
        "Local offline Cybersecurity Knowledge Base (HackTricks, HackTricks Cloud, "
        "PayloadsAllTheThings, InternalAllTheThings, OWASP WSTG/MASTG/API/CheatSheets, "
        "GTFOBins, LOLBAS, WADComs, MITRE ATT&CK, Atomic Red Team, SigmaHQ, Nuclei Templates, "
        "CISA KEV, Exploit-DB, PoC-in-GitHub, Bug Bounty writeups, CVEListV5, optional Primus-Seed). "
        "Use concise English technical keywords in kb_search, then kb_read_section to "
        "expand promising hits, or kb_lookup_cve for exact CVE intelligence."
    ),
)

_retriever: Optional[HybridRetriever] = None
_store: Optional[KnowledgeStore] = None


def _get_store() -> KnowledgeStore:
    global _store
    if _store is None:
        _store = KnowledgeStore(read_only=False)
    return _store


def _get_retriever() -> HybridRetriever:
    global _retriever
    if _retriever is None:
        _retriever = HybridRetriever(
            store=_get_store(),
            embedder=LocalEmbedder(),
            reranker=LocalReranker(),
        )
    return _retriever


@mcp.tool()
def kb_search(
    query: str,
    category: str = "",
    source: str = "",
    top_k: int = 5,
) -> str:
    """Search the local cybersecurity knowledge base using 3-stage hybrid retrieval (BM25 + dense vectors + multilingual cross-encoder reranker).

    Use concise English technical keywords (e.g. "SSTI Jinja2 filter bypass", "Kubernetes RBAC privilege escalation",
    "GraphQL introspection auth bypass", "ADCS ESC8 relay", "Redis Lua sandbox escape").
    Optional category filter: web, api, cloud, ad, privesc, mobile, network, binary, cve, mitre, detection, exploits, writeups, threat-intel.
    Optional source filter: hacktricks, hacktricks-cloud, payloads, owasp, privesc-bins, mitre-attack, atomic-red-team, nuclei, cisa-kev, bugbounty-writeups, poc-in-github, sigma-rules, exploitdb, cvelist-v5, primus-seed.
    Returns ranked excerpts with doc_id and section_id; call kb_read_section(doc_id, section_id) to read the full section of a relevant hit.
    """
    cat = category.strip().lower()
    if cat and cat not in VALID_CATEGORIES:
        return (
            f"Invalid category '{category}'. Valid categories: "
            + ", ".join(sorted(VALID_CATEGORIES))
        )

    res = _get_retriever().search(
        query=query,
        category=cat or None,
        source=source.strip().lower() or None,
        top_k=max(1, min(int(top_k or 5), 10)),
    )
    hits = res.get("results") or []
    if not hits:
        return (
            f"No knowledge-base matches found for query={query!r}"
            + (f" category={cat!r}" if cat else "")
            + (f" source={source!r}" if source else "")
            + ". Try broader English technical terms or remove filters."
        )

    lines = [
        f"Found {len(hits)} match(es) [{res.get('mode')}, {res.get('latency_ms')} ms] for: {query}"
    ]
    for idx, h in enumerate(hits, start=1):
        cve_part = f" | CVEs: {', '.join(h['cves'][:4])}" if h.get("cves") else ""
        lines.append(
            f"\n### [{idx}] {h['breadcrumb']}\n"
            f"- ref: `doc_id={h['doc_id']}`, `section_id={h['section_id']}` | "
            f"source: `{h['source']}` | category: `{h['category']}` | score: {h['score']:.3f}{cve_part}\n\n"
            f"{h['excerpt']}"
        )
    return "\n".join(lines)


@mcp.tool()
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


@mcp.tool()
def kb_lookup_cve(cve_id: str) -> str:
    """Direct indexed lookup for a specific CVE (e.g. 'CVE-2024-3094') across CISA KEV, Nuclei Templates, Exploit-DB, CVEListV5, and HackTricks.

    Prefer this tool whenever you encounter a concrete CVE identifier or want to check whether a CVE is in CISA KEV or has a Nuclei template / PoC.
    """
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


@mcp.tool()
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

    if args.transport == "streamable-http":
        if hasattr(mcp, "settings"):
            mcp.settings.host = args.host
            mcp.settings.port = args.port
            mcp.run(transport="streamable-http")
        else:
            mcp.run(transport="streamable-http")
    else:
        mcp.run(transport="stdio")


if __name__ == "__main__":
    main()
