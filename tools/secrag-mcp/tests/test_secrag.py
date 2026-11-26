"""Unit & integration tests for ArtexV2 secrag hybrid RAG engine."""

from __future__ import annotations

import numpy as np
import pytest

from secrag.chunker import (
    chunk_markdown,
    parse_cve_v5_json,
    parse_mitre_stix,
    parse_nuclei_yaml,
    parse_poc_in_github_json,
    parse_sigma_yaml,
)
from secrag.retriever import HybridRetriever
from secrag.storage import KnowledgeStore, build_fts5_queries


class _DummyEmbedder:
    """Deterministic bag-of-keywords embedder for fast unit testing."""

    model_name = "dummy-test"

    def _encode_one(self, text: str) -> np.ndarray:
        vec = np.zeros(384, dtype=np.float32)
        low = text.lower()
        for word in low.split():
            h = abs(hash(word)) % 384
            vec[h] += 1.0
        norm = np.linalg.norm(vec)
        return vec / norm if norm > 0 else vec

    def embed_documents(self, texts, batch_size=32):
        return np.stack([self._encode_one(t) for t in texts], axis=0)

    def embed_query(self, query: str) -> np.ndarray:
        return self._encode_one(query)


def test_markdown_chunking_preserves_code_blocks():
    md = """---
title: SSRF Filter Bypass
---
# SSRF Filter Bypass

## IPv6 and Decimal Encoding
Use alternate IP encodings to bypass naive host allowlists.

```bash
# This comment must NOT be treated as a markdown heading
curl -s "http://2130706433/latest/meta-data/"
curl -s "http://[::ffff:169.254.169.254]/latest/meta-data/"
```

## Redirect Chains
Chain an open redirect to reach internal services.
"""
    sections = chunk_markdown(md, doc_title="SSRF Filter Bypass", root_breadcrumb="HackTricks > Web")
    assert len(sections) == 2
    assert "IPv6 and Decimal Encoding" in sections[0][1]
    assert "# This comment must NOT be treated as a markdown heading" in sections[0][2]
    assert "Redirect Chains" in sections[1][1]


def test_fts5_query_sanitizer_handles_special_chars():
    and_q, or_q = build_fts5_queries('CVE-2024-3094 OR (X-Forwarded-Host: "127.0.0.1") ^ *')
    assert "CVE-2024-3094" in and_q
    assert "X-Forwarded-Host" in and_q
    assert or_q != ""


def test_hybrid_search_and_cve_lookup(tmp_path):
    store = KnowledgeStore(data_dir=tmp_path)
    embedder = _DummyEmbedder()

    # 1. Insert HackTricks-style doc
    md = """# SSTI Jinja2 Bypass
## Bypassing Underscore Filters
When underscores are blocked in Jinja2 templates, use request.args or attr() filters:
```python
{{request|attr('application')|attr('\\x5f\\x5fglobals\\x5f\\x5f')}}
```
"""
    secs = chunk_markdown(md, doc_title="SSTI Jinja2 Bypass", root_breadcrumb="PayloadsAllTheThings > SSTI")
    _, cids1, _ = store.upsert_document(
        source="payloads",
        category="web",
        rel_path="ssti/jinja2.md",
        title="SSTI Jinja2 Bypass",
        url="",
        tags=["ssti", "jinja2", "waf-bypass"],
        cves=[],
        full_text=md,
        sections_with_chunks=secs,
    )

    # 2. Insert Nuclei CVE template
    nuclei_doc = parse_nuclei_yaml(
        """
id: CVE-2024-3094
info:
  name: XZ Utils Backdoor Detection
  severity: critical
  description: Malicious backdoor in xz-utils liblzma affecting OpenSSH.
  tags: cve,cve2024,ssh,backdoor
  classification:
    cve-id: CVE-2024-3094
""",
        "http/cves/2024/CVE-2024-3094.yaml",
    )
    assert nuclei_doc is not None
    _, cids2, _ = store.upsert_document(
        source="nuclei",
        category=nuclei_doc["category"],
        rel_path="http/cves/2024/CVE-2024-3094.yaml",
        title=nuclei_doc["title"],
        url=nuclei_doc["url"],
        tags=nuclei_doc["tags"],
        cves=nuclei_doc["cves"],
        full_text=nuclei_doc["full_text"],
        sections_with_chunks=nuclei_doc["sections"],
        cve_entries=nuclei_doc["cve_entries"],
    )
    store.conn.commit()

    # Build vector index
    vec_idx = store.open_writable_vector_index()
    for batch in store.iter_unembedded_chunks(batch_size=32):
        ids = [cid for cid, _ in batch]
        texts = [t for _, t in batch]
        vecs = embedder.embed_documents(texts)
        vec_idx.add(np.asarray(ids, dtype=np.uint64), vecs.astype(np.float16))
        store.mark_chunks_embedded(ids)
    store.save_vector_index_atomic(vec_idx)

    retriever = HybridRetriever(store=store, embedder=embedder, reranker=None)
    # Disable real reranker download in unit test
    retriever.reranker.enabled = False

    res = retriever.search("Jinja2 SSTI bypass underscores attr", category="web", top_k=3)
    assert res["mode"] == "hybrid"
    assert len(res["results"]) >= 1
    assert "Jinja2" in res["results"][0]["title"]

    # Parent section expansion
    sec = store.read_section(res["results"][0]["doc_id"], res["results"][0]["section_id"])
    assert sec is not None
    assert "request|attr" in sec["content"]

    # Direct CVE lookup
    cve_res = store.lookup_cve("cve-2024-3094")
    assert cve_res["cve_id"] == "CVE-2024-3094"
    assert cve_res["match_count"] == 1
    assert cve_res["matches"][0]["source"] == "nuclei"
    store.close()


def test_parse_mitre_and_cve_v5():
    stix = {
        "objects": [
            {
                "type": "attack-pattern",
                "name": "Kerberoasting",
                "description": "Adversaries may abuse valid Kerberos TGS tickets.",
                "x_mitre_platforms": ["Windows"],
                "kill_chain_phases": [{"phase_name": "credential-access"}],
                "external_references": [
                    {"source_name": "mitre-attack", "external_id": "T1558.003", "url": "https://attack.mitre.org/techniques/T1558/003"}
                ],
            }
        ]
    }
    docs = parse_mitre_stix(stix, "enterprise")
    assert len(docs) == 1
    assert docs[0]["title"] == "T1558.003: Kerberoasting"

    cve = parse_cve_v5_json(
        {
            "cveMetadata": {"state": "PUBLISHED", "cveId": "CVE-2025-12345"},
            "containers": {
                "cna": {
                    "title": "Sample Auth Bypass",
                    "descriptions": [{"lang": "en", "value": "An authentication bypass vulnerability in Acme Gateway allows remote attackers to access admin endpoints."}],
                    "affected": [{"vendor": "Acme", "product": "Gateway"}],
                    "metrics": [{"cvssV3_1": {"baseScore": 9.8, "baseSeverity": "CRITICAL"}}],
                }
            },
        },
        "cves/2025/12xxx/CVE-2025-12345.json",
    )
    assert cve is not None
    assert cve["cves"] == ["CVE-2025-12345"]
    assert cve["cve_entries"][0][1] == "critical"
    assert cve["is_high_signal"] is True


def test_parse_sigma_poc_github_and_dual_tier_cve(tmp_path):
    sigma = parse_sigma_yaml(
        """
title: Suspicious Certutil Download
id: 1234-5678
status: stable
level: high
description: Detects certutil used to download remote payloads.
logsource:
  product: windows
  category: process_creation
detection:
  selection:
    Image|endswith: '\\certutil.exe'
    CommandLine|contains: '-urlcache'
  condition: selection
tags:
  - attack.command_and_control
  - attack.t1105
""",
        "windows/process_creation/certutil.yml",
    )
    assert sigma is not None
    assert sigma["category"] == "detection"
    assert "T1105" in sigma["tags"]
    assert "certutil.exe" in sigma["full_text"]

    poc = parse_poc_in_github_json(
        "CVE-2024-3094",
        [
            {"full_name": "a/low", "html_url": "https://github.com/a/low", "stargazers_count": 5, "description": "test"},
            {"full_name": "b/top", "html_url": "https://github.com/b/top", "stargazers_count": 420, "description": "best poc"},
        ],
    )
    assert poc is not None
    assert poc["url"] == "https://github.com/b/top"
    assert "420★" in poc["full_text"]

    # Verify dual-tier lexical-only chunking (embedded=2 skipped by vector pass, found by lookup_cve + FTS5)
    store = KnowledgeStore(data_dir=tmp_path)
    low_cve = parse_cve_v5_json(
        {
            "cveMetadata": {"state": "PUBLISHED", "cveId": "CVE-2024-99999"},
            "containers": {
                "cna": {
                    "title": "Minor Info Leak in TinyWidget",
                    "descriptions": [{"lang": "en", "value": "A low severity information disclosure in TinyWidget allows reading version banners."}],
                    "metrics": [{"cvssV3_1": {"baseScore": 3.1, "baseSeverity": "LOW"}}],
                }
            },
        },
        "cves/2024/99xxx/CVE-2024-99999.json",
    )
    assert low_cve is not None and low_cve["is_high_signal"] is False
    store.upsert_document(
        source="cvelist-v5",
        category="cve",
        rel_path="cves/2024/99xxx/CVE-2024-99999.json",
        title=low_cve["title"],
        url=low_cve["url"],
        tags=low_cve["tags"],
        cves=low_cve["cves"],
        full_text=low_cve["full_text"],
        sections_with_chunks=low_cve["sections"],
        cve_entries=low_cve["cve_entries"],
        embed_chunks=False,
    )
    store.conn.commit()

    # Must NOT be queued for dense embedding
    assert list(store.iter_unembedded_chunks(batch_size=16)) == []
    # Must STILL be found by lookup_cve and FTS5
    res = store.lookup_cve("CVE-2024-99999")
    assert res["match_count"] == 1
    assert len(store.fts_search("TinyWidget version banners")) == 1
    st = store.stats()
    assert st["lexical_only_chunks"] == 1
    assert st["pending_embed_chunks"] == 0
    store.close()


def test_orphan_vector_removal_and_multithread_readonly(tmp_path):
    from concurrent.futures import ThreadPoolExecutor

    store = KnowledgeStore(data_dir=tmp_path)
    embedder = _DummyEmbedder()

    # Insert 2 docs: one in 'writeups', one in 'web'
    md1 = "# Initial Writeup\n## GraphQL IDOR\nOriginal text about graphql idor vulnerability."
    secs1 = chunk_markdown(md1, doc_title="Initial Writeup", root_breadcrumb="H1 > IDOR")
    _, cids1, _ = store.upsert_document(
        source="bugbounty-writeups",
        category="writeups",
        rel_path="h1/idor.md",
        title="Initial Writeup",
        url="",
        tags=["idor"],
        cves=[],
        full_text=md1,
        sections_with_chunks=secs1,
    )
    md2 = "# Web Doc\n## SQLi\nSome unrelated sql injection notes."
    secs2 = chunk_markdown(md2, doc_title="Web Doc", root_breadcrumb="HT > Web")
    _, cids2, _ = store.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="web/sqli.md",
        title="Web Doc",
        url="",
        tags=["sqli"],
        cves=[],
        full_text=md2,
        sections_with_chunks=secs2,
    )
    store.conn.commit()

    # Embed initial chunks
    vec_idx = store.open_writable_vector_index()
    for batch in store.iter_unembedded_chunks(batch_size=32):
        ids = [cid for cid, _ in batch]
        vecs = embedder.embed_documents([t for _, t in batch])
        vec_idx.add(np.asarray(ids, dtype=np.uint64), vecs.astype(np.float16))
        store.mark_chunks_embedded(ids)
    store.save_vector_index_atomic(vec_idx)
    assert cids1[0] in store.get_readonly_vector_index()

    # Update doc1 -> old chunk ID must be recorded in deleted_vectors
    md1_v2 = "# Updated Writeup\n## GraphQL IDOR v2\nUpdated graphql idor bypass payload."
    secs1_v2 = chunk_markdown(md1_v2, doc_title="Updated Writeup", root_breadcrumb="H1 > IDOR")
    _, cids1_v2, removed = store.upsert_document(
        source="bugbounty-writeups",
        category="writeups",
        rel_path="h1/idor.md",
        title="Updated Writeup",
        url="",
        tags=["idor"],
        cves=[],
        full_text=md1_v2,
        sections_with_chunks=secs1_v2,
    )
    store.conn.commit()
    assert removed == cids1
    assert store.get_deleted_vector_ids() == cids1

    # Simulate run_embedding_pass cleanup + add
    vec_idx = store.open_writable_vector_index()
    del_ids = store.get_deleted_vector_ids()
    vec_idx.remove(np.asarray(del_ids, dtype=np.uint64))
    for batch in store.iter_unembedded_chunks(batch_size=32):
        ids = [cid for cid, _ in batch]
        vecs = embedder.embed_documents([t for _, t in batch])
        vec_idx.add(np.asarray(ids, dtype=np.uint64), vecs.astype(np.float16))
        store.mark_chunks_embedded(ids)
    store.save_vector_index_atomic(vec_idx)
    store.clear_deleted_vector_ids(del_ids)

    assert store.get_deleted_vector_ids() == []
    ro_idx = store.get_readonly_vector_index()
    assert cids1[0] not in ro_idx
    assert cids1_v2[0] in ro_idx
    store.close()

    # Concurrent read-only queries across 8 threads
    ro_store = KnowledgeStore(data_dir=tmp_path, read_only=True)
    qv = embedder.embed_query("graphql idor bypass")

    def worker(_i):
        fts = ro_store.fts_search("graphql idor", category="writeups", limit=5)
        vec = ro_store.vector_search(qv, category="writeups", limit=5)
        assert len(fts) == 1 and fts[0][0] == cids1_v2[0]
        assert len(vec) == 1 and vec[0][0] == cids1_v2[0]
        return True

    with ThreadPoolExecutor(max_workers=8) as ex:
        assert all(ex.map(worker, range(32)))
    ro_store.close()


def test_mcp_tools_declare_readonly_annotations():
    import asyncio
    from secrag.server import mcp

    tools = asyncio.run(mcp.list_tools())
    names = {t.name for t in tools}
    assert {"kb_search", "kb_read_section", "kb_lookup_cve", "kb_stats"} <= names
    for t in tools:
        assert t.annotations is not None, f"tool {t.name} missing annotations"
        dumped = t.annotations.model_dump(by_alias=True)
        assert dumped.get("readOnlyHint") is True, f"tool {t.name} missing readOnlyHint=True: {dumped}"


def test_retriever_lru_cache_and_missing_section_fallback(tmp_path):
    store = KnowledgeStore(data_dir=tmp_path)
    embedder = _DummyEmbedder()

    md = "# Redis Security\n## Lua Sandbox Escape\nUse eval script bypass on port 6379."
    secs = chunk_markdown(md, doc_title="Redis Security", root_breadcrumb="HackTricks > Redis")
    doc_id, _, _ = store.upsert_document(
        source="hacktricks",
        category="network",
        rel_path="redis.md",
        title="Redis Security",
        url="",
        tags=["redis", "lua"],
        cves=[],
        full_text=md,
        sections_with_chunks=secs,
    )
    store.conn.commit()

    retriever = HybridRetriever(store=store, embedder=embedder, reranker=None)
    retriever.reranker.enabled = False

    first = retriever.search("Redis Lua sandbox escape", category="network", top_k=3)
    assert len(first["results"]) == 1
    assert not first.get("cached")

    second = retriever.search("Redis Lua sandbox escape", category="network", top_k=3)
    assert second.get("cached") is True
    assert second["results"] == first["results"]

    # Missing section_id on an existing doc_id falls back to overview + outline
    fallback = store.read_section(doc_id, section_id=999999)
    assert fallback is not None
    assert fallback["section_not_found"] == 999999
    assert len(fallback["outline"]) >= 1
    store.close()


def test_server_cold_get_retriever_does_not_deadlock(tmp_path, monkeypatch):
    """Regression test: calling _get_retriever() when _store is None used to
    deadlock on a non-reentrant threading.Lock when kb_search was the first
    tool invoked on a fresh secrag-mcp process, and bootstrapping an empty DB
    must still yield a read-only multi-thread-safe store."""
    from concurrent.futures import ThreadPoolExecutor
    import secrag.server as srv

    monkeypatch.setenv("SECRAG_DATA_DIR", str(tmp_path))
    monkeypatch.setattr(srv, "_store", None)
    monkeypatch.setattr(srv, "_retriever", None)
    retr = srv._get_retriever()
    assert retr is not None
    assert srv._store is not None
    assert srv._store.read_only is True

    # Calling stats() across multiple worker threads must not raise sqlite3.ProgrammingError
    with ThreadPoolExecutor(max_workers=4) as ex:
        counts = list(ex.map(lambda _: srv._store.stats()["documents"], range(8)))
    assert counts == [0] * 8

    # Empty/invalid CVE lookup returns empty cleanly instead of scanning for "CVE-"
    assert srv._store.lookup_cve("")["match_count"] == 0
    assert srv._store.lookup_cve("   ")["match_count"] == 0

    srv._store.close()
    monkeypatch.setattr(srv, "_store", None)
    monkeypatch.setattr(srv, "_retriever", None)


def test_pre_rerank_diversity_prevents_single_doc_starvation(tmp_path):
    """A long document with many matching chunks must not crowd out other
    documents before rerank_k slicing."""
    store = KnowledgeStore(data_dir=tmp_path)
    embedder = _DummyEmbedder()

    # 1 giant doc with 12 SSTI sections
    big_sections = [
        (f"SSTI Section {i}", f"HackTricks > SSTI > Sec {i}", f"jinja2 ssti filter bypass technique {i}", [f"jinja2 ssti filter bypass technique {i}"])
        for i in range(12)
    ]
    store.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="ssti-big.md",
        title="Big SSTI Guide",
        url="",
        tags=["ssti"],
        cves=[],
        full_text="jinja2 ssti filter bypass",
        sections_with_chunks=big_sections,
    )
    # 3 distinct smaller docs also matching
    for j in range(3):
        store.upsert_document(
            source="payloads",
            category="web",
            rel_path=f"ssti-alt-{j}.md",
            title=f"Alt SSTI {j}",
            url="",
            tags=["ssti"],
            cves=[],
            full_text=f"jinja2 ssti filter bypass alt {j}",
            sections_with_chunks=[(f"Alt {j}", f"Payloads > Alt {j}", f"jinja2 ssti filter bypass alt {j}", [f"jinja2 ssti filter bypass alt {j}"])],
        )
    store.conn.commit()

    retriever = HybridRetriever(store=store, embedder=embedder, reranker=None)
    retriever.reranker.enabled = False
    res = retriever.search("jinja2 ssti filter bypass", category="web", top_k=5, max_per_doc=2)
    assert len(res["results"]) == 5
    doc_ids = {r["doc_id"] for r in res["results"]}
    assert len(doc_ids) == 4
    store.close()


def test_strip_frontmatter_dotdotdot_and_eof_delimiters():
    from secrag.chunker import strip_frontmatter

    gtfo = "---\nfunctions:\n  shell:\n    - code: /bin/sh\n...\n"
    meta, body = strip_frontmatter(gtfo)
    assert isinstance(meta, dict) and "functions" in meta
    assert body == ""

    wad_eof = "---\ndescription: Dump hashes\ncommand: secretsdump.py\n---"
    meta2, body2 = strip_frontmatter(wad_eof)
    assert meta2.get("command") == "secretsdump.py"
    assert body2 == ""


def test_fts5_paths_dotfiles_punctuation_and_porter_df(tmp_path):
    store = KnowledgeStore(data_dir=tmp_path)
    docs = [
        ("p1.md", "T1003.001: OS Credential Dumping", "Read /etc/passwd and .git/config and .htaccess files."),
        ("p2.md", "CVE-2024-3094: XZ Backdoor", "Authentication authorization vulnerability exploitation escalation configuration."),
        ("p3.md", "GraphQL Auth", "Authorization and authentication vulnerability in GraphQL endpoint."),
    ]
    for rel, title, text in docs:
        store.upsert_document(
            source="hacktricks",
            category="web",
            rel_path=rel,
            title=title,
            url="",
            tags=["web"],
            cves=[],
            full_text=text,
            sections_with_chunks=[(title, f"HT > {title}", text, [text])],
        )
    store.conn.commit()

    assert len(store.fts_search(".git/config")) == 1
    assert len(store.fts_search("/etc/passwd")) == 1
    assert len(store.fts_search(".htaccess")) == 1
    assert len(store.fts_search("T1003.001")) == 1
    assert len(store.fts_search("CVE-2024-3094")) == 1

    # Porter-stemmed long words must resolve to true doc freq (>= 2), not 0.
    assert store._token_doc_freq("authorization") == 2
    assert store._token_doc_freq("authentication") == 2
    assert store._token_doc_freq("vulnerability") == 2
    assert store._token_doc_freq("configuration") == 1
    assert store._token_doc_freq("escalation") == 1
    assert store._token_doc_freq("exploitation") == 1

    # Verify chunks_au trigger is scoped to text columns (does not refire on embedded=1).
    au_sql = store.conn.execute(
        "SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'chunks_au'"
    ).fetchone()["sql"]
    assert "OF title, breadcrumb, tags, cves, content" in au_sql
    store.close()


def test_vector_search_handles_tuple_with_missing_keys(tmp_path):
    store = KnowledgeStore(data_dir=tmp_path)
    embedder = _DummyEmbedder()
    cids = []
    for i in range(3):
        _, ids, _ = store.upsert_document(
            source="hacktricks",
            category="web",
            rel_path=f"doc{i}.md",
            title=f"Doc {i}",
            url="",
            tags=["web"],
            cves=[],
            full_text=f"ssrf bypass technique {i}",
            sections_with_chunks=[(f"Doc {i}", f"HT > {i}", f"ssrf bypass technique {i}", [f"ssrf bypass technique {i}"])],
        )
        cids.extend(ids)
    store.conn.commit()

    # Mark all 3 embedded in SQLite, but only persist 2 of them in USearch (simulating partial/concurrent state).
    vec_idx = store.open_writable_vector_index()
    vecs = embedder.embed_documents(["ssrf bypass technique 0", "ssrf bypass technique 2"])
    vec_idx.add(np.asarray([cids[0], cids[2]], dtype=np.uint64), vecs.astype(np.float16))
    store.save_vector_index_atomic(vec_idx)
    store.mark_chunks_embedded(cids)

    qv = embedder.embed_query("ssrf bypass technique 0")
    hits = store.vector_search(qv, category="web", limit=5)
    hit_ids = {cid for cid, _ in hits}
    assert cids[0] in hit_ids and cids[2] in hit_ids and cids[1] not in hit_ids
    store.close()


def test_embedding_pass_atomic_checkpoint_and_resume_duplicate_guard(tmp_path, monkeypatch):
    import secrag.ingest as ing

    store = KnowledgeStore(data_dir=tmp_path)
    for i in range(4):
        store.upsert_document(
            source="hacktricks",
            category="web",
            rel_path=f"e{i}.md",
            title=f"E{i}",
            url="",
            tags=["web"],
            cves=[],
            full_text=f"payload technique {i}",
            sections_with_chunks=[(f"E{i}", f"HT > E{i}", f"payload technique {i}", [f"payload technique {i}"])],
        )
    store.conn.commit()

    dummy = _DummyEmbedder()
    dummy._ensure_mps = lambda: False
    monkeypatch.setattr(ing, "LocalEmbedder", lambda prefer_mps=True: dummy)

    # Pre-add chunk 1 to kb.usearch WITHOUT marking it embedded=1 in SQLite (simulates crash after save, before commit).
    vec_idx = store.open_writable_vector_index()
    v1 = dummy.embed_documents(["payload technique 0"])
    vec_idx.add(np.asarray([1], dtype=np.uint64), v1.astype(np.float16))
    store.save_vector_index_atomic(vec_idx)

    # Resuming run_embedding_pass must NOT raise Duplicate keys RuntimeError and must embed remaining 3.
    done = ing.run_embedding_pass(store, batch_size=2, checkpoint_every=2)
    assert done == 3
    pending = store.conn.execute("SELECT COUNT(*) FROM chunks WHERE embedded = 0").fetchone()[0]
    assert pending == 0
    store.close()


def test_streamable_http_main_does_not_crash_on_mcp_v2_settings(monkeypatch):
    import sys
    import secrag.server as srv

    called = {}
    monkeypatch.setattr(srv, "_start_background_warmup", lambda: None)
    monkeypatch.setattr(srv.mcp, "run", lambda **kwargs: called.update(kwargs))
    monkeypatch.setattr(sys, "argv", ["secrag-mcp", "--transport", "streamable-http", "--host", "127.0.0.1", "--port", "8099"])
    srv.main()
    assert called.get("transport") == "streamable-http"
    assert called.get("host") == "127.0.0.1"
    assert called.get("port") == 8099


def test_meta_cache_refreshes_when_marked_embedded_without_vec_rewrite(tmp_path):
    store = KnowledgeStore(data_dir=tmp_path)
    embedder = _DummyEmbedder()
    _, cids, _ = store.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="race.md",
        title="Race Test",
        url="",
        tags=["web"],
        cves=[],
        full_text="prototype pollution __proto__ gadget",
        sections_with_chunks=[("Race Test", "HT > Race Test", "prototype pollution __proto__ gadget", ["prototype pollution __proto__ gadget"])],
    )
    store.conn.commit()

    # Save kb.usearch BEFORE mark_chunks_embedded (matches run_embedding_pass order).
    vec_idx = store.open_writable_vector_index()
    vecs = embedder.embed_documents(["prototype pollution __proto__ gadget"])
    vec_idx.add(np.asarray(cids, dtype=np.uint64), vecs.astype(np.float16))
    store.save_vector_index_atomic(vec_idx)

    # Concurrent read-only reader warms _meta_cache BEFORE SQLite marks embedded=1.
    ro = KnowledgeStore(data_dir=tmp_path, read_only=True)
    qv = embedder.embed_query("prototype pollution")
    assert ro.vector_search(qv, category="web", limit=5) == []

    # Now writer commits embedded=1 WITHOUT rewriting kb.usearch.
    store.mark_chunks_embedded(cids)
    hits = ro.vector_search(qv, category="web", limit=5)
    assert [cid for cid, _ in hits] == cids
    ro.close()
    store.close()


def test_readonly_worker_thread_connections_reaped_on_exit(tmp_path):
    import gc
    import threading

    w = KnowledgeStore(data_dir=tmp_path)
    w.close()

    ro = KnowledgeStore(data_dir=tmp_path, read_only=True)

    def _worker():
        ro.conn.execute("SELECT 1").fetchone()

    threads = [threading.Thread(target=_worker) for _ in range(5)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    del t
    threads.clear()
    gc.collect()

    with ro._conn_lock:
        leaked = len(ro._ro_conns)
    assert leaked == 0, f"expected 0 leaked read-only connections after worker exit, got {leaked}"
    ro.close()


def test_fts5_leading_underscore_and_dash_tokens(tmp_path):
    from secrag.storage import extract_fts5_tokens

    toks = extract_fts5_tokens("__proto__ __class__ -exec --no-sandbox --- ___")
    assert "__proto__" in toks
    assert "__class__" in toks
    assert "-exec" in toks
    assert "--no-sandbox" in toks
    assert "---" not in toks
    assert "___" not in toks

    store = KnowledgeStore(data_dir=tmp_path)
    store.upsert_document(
        source="payloads",
        category="web",
        rel_path="proto.md",
        title="Prototype Pollution & SSTI",
        url="",
        tags=["web"],
        cves=[],
        full_text="Exploit __proto__ and __class__.__subclasses__ and find -exec /bin/sh --no-sandbox",
        sections_with_chunks=[(
            "Proto",
            "Payloads > Proto",
            "Exploit __proto__ and __class__.__subclasses__ and find -exec /bin/sh --no-sandbox",
            ["Exploit __proto__ and __class__.__subclasses__ and find -exec /bin/sh --no-sandbox"],
        )],
    )
    store.conn.commit()

    assert len(store.fts_search("__proto__")) == 1
    assert len(store.fts_search("__class__")) == 1
    assert len(store.fts_search("-exec")) == 1
    assert len(store.fts_search("--no-sandbox")) == 1
    store.close()


def test_gtfobins_v2_schema_contexts_aliases_and_ld_so(tmp_path, monkeypatch):
    import secrag.ingest as ing

    src_dir = tmp_path / "src"
    gtfo_dir = src_dir / "gtfobins" / "_gtfobins"
    gtfo_dir.mkdir(parents=True)
    (src_dir / "lolbas" / "yml").mkdir(parents=True)

    (gtfo_dir / "bash").write_text(
        "---\nfunctions:\n  shell:\n  - code: bash\n    comment: Spawns a shell.\n    contexts:\n      sudo:\n      suid:\n        code: bash -p\n      capabilities:\n        list:\n        - CAP_SETUID\n...\n",
        encoding="utf-8",
    )
    (gtfo_dir / "mawk").write_text(
        "---\nfunctions:\n  shell:\n  - code: mawk 'BEGIN {system(\"/bin/sh\")}'\n    contexts:\n      sudo:\n...\n",
        encoding="utf-8",
    )
    (gtfo_dir / "awk").write_text("---\nalias: mawk\n...\n", encoding="utf-8")
    (gtfo_dir / "ld.so").write_text(
        "---\ncomment: Linux dynamic linker.\nfunctions:\n  shell:\n  - code: /lib/ld.so /bin/sh\n    contexts:\n      suid:\n        code: /lib/ld.so /bin/sh -p\n...\n",
        encoding="utf-8",
    )

    monkeypatch.setattr(ing, "_git_sync", lambda url, dest: "test-rev")
    store = KnowledgeStore(data_dir=tmp_path / "db")
    ing.ingest_privesc_bins(store, src_dir)

    docs = {
        r["rel_path"]: r
        for r in store.conn.execute("SELECT rel_path, title, url, tags, full_text FROM documents").fetchall()
    }
    assert "gtfobins/bash" in docs
    assert "bash -p" in docs["gtfobins/bash"]["full_text"]
    assert "CAP_SETUID" in docs["gtfobins/bash"]["full_text"]
    assert "suid" in docs["gtfobins/bash"]["tags"]
    assert "Spawns a shell." in docs["gtfobins/bash"]["full_text"]

    assert "gtfobins/awk" in docs
    assert "mawk" in docs["gtfobins/awk"]["full_text"]

    assert "gtfobins/ld.so" in docs
    assert docs["gtfobins/ld.so"]["title"] == "GTFOBins: ld.so"
    assert docs["gtfobins/ld.so"]["url"].endswith("/gtfobins/ld.so/")
    assert "Linux dynamic linker." in docs["gtfobins/ld.so"]["full_text"]
    store.close()


def test_lookup_cve_selects_matching_subsection(tmp_path):
    store = KnowledgeStore(data_dir=tmp_path)
    store.upsert_document(
        source="payloads",
        category="web",
        rel_path="python-deser.md",
        title="Python Deserialization",
        url="",
        tags=["web"],
        cves=["CVE-2019-20477"],
        full_text="Intro text\n## PyYAML\nCVE-2019-20477 allows arbitrary code execution via yaml.load.",
        sections_with_chunks=[
            ("Overview", "Payloads > Python Deserialization > Overview", "Generic page introduction without any CVE.", ["Generic page introduction without any CVE."]),
            ("PyYAML", "Payloads > Python Deserialization > PyYAML", "CVE-2019-20477 allows arbitrary code execution via yaml.load.", ["CVE-2019-20477 allows arbitrary code execution via yaml.load."]),
        ],
    )
    store.conn.commit()

    res = store.lookup_cve("CVE-2019-20477")
    assert res["match_count"] == 1
    m = res["matches"][0]
    assert "CVE-2019-20477" in m["excerpt"]
    sec_row = store.conn.execute("SELECT heading FROM sections WHERE id = ?", (m["section_id"],)).fetchone()
    assert sec_row["heading"] == "PyYAML"
    store.close()


def test_parse_cve_v5_json_adp_metrics_and_cwe_fallback():
    from secrag.chunker import parse_cve_v5_json

    data = {
        "cveMetadata": {"cveId": "CVE-2025-99999", "state": "PUBLISHED"},
        "containers": {
            "cna": {
                "title": "ADP-scored RCE",
                "descriptions": [{"lang": "en", "value": "Remote code execution vulnerability in ExampleServer allows unauthenticated attackers to execute arbitrary commands."}],
                "affected": [{"vendor": "Example", "product": "Server"}],
            },
            "adp": [
                {
                    "providerMetadata": {"shortName": "CISA-ADP"},
                    "problemTypes": [{"descriptions": [{"cweId": "CWE-78", "description": "OS Command Injection"}]}],
                    "metrics": [{"cvssV3_1": {"baseScore": 9.8, "baseSeverity": "CRITICAL"}}],
                }
            ],
        },
    }
    parsed = parse_cve_v5_json(data, "2025/9xxx/CVE-2025-99999.json")
    assert parsed is not None
    assert parsed["cvss_float"] == 9.8
    assert parsed["is_high_signal"] is True
    assert "Severity: critical (CVSS 9.8)" in parsed["full_text"]
    assert "CWE-78" in parsed["full_text"]


def test_exact_fts5_stem_df_ignores_typo_prefixes_and_absent_cves(tmp_path):
    """SECRAG-R3-001: _token_doc_freq must use SQLite's exact FTS5 stem rather than
    longest-prefix matching (which gets fooled by typo terms like 'authorizati' or
    shorter CVE prefixes)."""
    store = KnowledgeStore(data_dir=tmp_path)
    # 5 docs containing real English words
    for i in range(5):
        text = f"Authorization and authentication vulnerability deserialization guide {i}"
        store.upsert_document(
            source="hacktricks",
            category="web",
            rel_path=f"doc-{i}.md",
            title=f"Doc {i}",
            url="",
            tags=["web"],
            cves=[],
            full_text=text,
            sections_with_chunks=[(f"Doc {i}", f"HT > {i}", text, [text])],
        )
    # 1 doc containing truncated typo strings + a 4-digit CVE prefix
    typo_text = "Broken authorizati authenticatio vulnerabilit deserializa CVE-2025-1234"
    store.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="typos.md",
        title="Typos",
        url="",
        tags=["web"],
        cves=["CVE-2025-1234"],
        full_text=typo_text,
        sections_with_chunks=[("Typos", "HT > Typos", typo_text, [typo_text])],
    )
    store.conn.commit()

    assert store._token_doc_freq("authorization") == 5
    assert store._token_doc_freq("authentication") == 5
    assert store._token_doc_freq("vulnerability") == 5
    assert store._token_doc_freq("deserialization") == 5
    # Exact absent 5-digit CVE must return 0, not match 4-digit CVE-2025-1234
    assert store._token_doc_freq("CVE-2025-12345") == 0
    assert store._token_doc_freq("CVE-2025-1234") == 1
    store.close()


def test_atomic_fts5_migration_and_transient_vocab_error_recovery(tmp_path, monkeypatch):
    """SECRAG-R3-002: legacy FTS5 migration commits atomically and transient OperationalError
    in _token_doc_freq must not permanently disable vocab lookups."""
    import sqlite3

    store = KnowledgeStore(data_dir=tmp_path)
    store.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="auth.md",
        title="Auth Guide",
        url="",
        tags=["web"],
        cves=[],
        full_text="Authorization bypass via /etc/passwd and .git/config",
        sections_with_chunks=[
            ("Auth", "HT > Auth", "Authorization bypass via /etc/passwd and .git/config", ["Authorization bypass via /etc/passwd and .git/config"])
        ],
    )
    store.conn.commit()

    # Downgrade chunks_fts to the legacy tokenizer ('-_.:/') to test migration.
    store.conn.executescript(
        """
        DROP TABLE chunks_fts_vocab;
        DROP TABLE chunks_fts;
        CREATE VIRTUAL TABLE chunks_fts USING fts5(
            title, breadcrumb, tags, cves, content,
            content='chunks', content_rowid='id',
            tokenize='porter unicode61 tokenchars ''-_.:/'''
        );
        CREATE VIRTUAL TABLE chunks_fts_vocab USING fts5vocab(chunks_fts, row);
        INSERT INTO chunks_fts(chunks_fts) VALUES('rebuild');
        """
    )
    store.close()

    # Re-open triggers atomic migration and preserves indexed rows.
    migrated = KnowledgeStore(data_dir=tmp_path)
    fts_sql = migrated.conn.execute(
        "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'chunks_fts'"
    ).fetchone()["sql"]
    assert "-_.:/" not in fts_sql
    assert len(migrated.fts_search(".git/config")) == 1
    assert migrated._token_doc_freq("authorization") == 1

    # Simulate transient SQLITE_BUSY: must return 1000 once, NOT latch vocab disabled.
    real_single = migrated._single_term_doc_freq
    calls = {"n": 0}

    def flaky_single(low: str) -> int:
        calls["n"] += 1
        if calls["n"] == 1:
            raise sqlite3.OperationalError("database is locked")
        return real_single(low)

    monkeypatch.setattr(migrated, "_single_term_doc_freq", flaky_single)
    assert migrated._token_doc_freq("authorization") == 1000
    assert migrated._vocab_missing_sig is None
    assert migrated._token_doc_freq("authorization") == 1
    migrated.close()


def test_corrupt_vector_index_degrades_cleanly_and_transient_error_not_cached(tmp_path):
    """SECRAG-R3-003 & SECRAG-R3-004: corrupt kb.usearch falls back to FTS5 in search()
    and stats(); transient embedder errors are not cached; stats() counts from SQLite."""
    store = KnowledgeStore(data_dir=tmp_path)
    embedder = _DummyEmbedder()
    _, cids1, _ = store.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="ssrf.md",
        title="SSRF Guide",
        url="",
        tags=["ssrf"],
        cves=[],
        full_text="SSRF metadata 169.254.169.254 bypass",
        sections_with_chunks=[("SSRF", "HT > SSRF", "SSRF metadata 169.254.169.254 bypass", ["SSRF metadata 169.254.169.254 bypass"])],
        embed_chunks=True,
    )
    _, _, _ = store.upsert_document(
        source="cvelist-v5",
        category="cve",
        rel_path="low.json",
        title="Low CVE",
        url="",
        tags=["cve"],
        cves=[],
        full_text="Minor banner disclosure",
        sections_with_chunks=[("Low", "CVE > Low", "Minor banner disclosure", ["Minor banner disclosure"])],
        embed_chunks=False,
    )
    store.conn.commit()
    store.mark_chunks_embedded(cids1)
    store.update_ingestion_state("hacktricks", 1, "done")
    store.update_ingestion_state("cvelist-v5", 2, "done")

    # Write corrupt bytes to kb.usearch -> stats() & search() must degrade cleanly instead of crashing.
    store.vec_path.write_bytes(b"corrupt-usearch-data")
    st = store.stats()
    assert st["chunks"] == 2
    assert st["embedded_chunks"] == 1
    assert st["lexical_only_chunks"] == 1
    assert st["pending_embed_chunks"] == 0
    assert st["vector_index_entries"] == 0

    retriever = HybridRetriever(store=store, embedder=embedder, reranker=None)
    retriever.reranker.enabled = False
    res_corrupt = retriever.search("SSRF metadata bypass", top_k=3)
    assert res_corrupt["mode"] == "fts5"
    assert len(res_corrupt["results"]) == 1

    # Now build a real vector index and simulate a one-time embed_query failure.
    store.vec_path.unlink()
    vec_idx = store.open_writable_vector_index()
    vec = embedder.embed_documents(["SSRF metadata 169.254.169.254 bypass"])
    vec_idx.add(np.asarray(cids1, dtype=np.uint64), vec.astype(np.float16))
    store.save_vector_index_atomic(vec_idx)

    class _FlakyEmbedder(_DummyEmbedder):
        def __init__(self):
            self.failed_once = False

        def embed_query(self, query: str) -> np.ndarray:
            if not self.failed_once:
                self.failed_once = True
                raise RuntimeError("transient ONNX error")
            return super().embed_query(query)

    retriever2 = HybridRetriever(store=store, embedder=_FlakyEmbedder(), reranker=None)
    retriever2.reranker.enabled = False
    first = retriever2.search("SSRF metadata bypass", top_k=3)
    assert first["mode"] == "fts5"
    assert not first.get("cached")
    # Second call must NOT receive a cached degraded fts5 response — it must succeed in hybrid mode.
    second = retriever2.search("SSRF metadata bypass", top_k=3)
    assert second["mode"] == "hybrid"
    assert not second.get("cached")
    store.close()


def test_cvelist_v5_incremental_refresh_preserves_lexical_only_tier(tmp_path, monkeypatch):
    """SECRAG-R4-001: known_cve_ids(exclude_source='cvelist-v5') prevents updated low-signal
    CVEListV5 entries from being silently promoted from lexical-only (embedded=2) to dense embedding."""
    import json
    from secrag import ingest as ing

    src_dir = tmp_path / "sources"
    cve_dir = src_dir / "cvelistV5" / "cves" / "2025" / "1xxx"
    cve_dir.mkdir(parents=True)

    def _make_cve_json(cve_id: str, desc: str, score: float, severity: str) -> str:
        return json.dumps(
            {
                "cveMetadata": {"cveId": cve_id, "state": "PUBLISHED"},
                "containers": {
                    "cna": {
                        "title": f"{cve_id} test",
                        "descriptions": [{"lang": "en", "value": desc}],
                        "affected": [{"vendor": "Example", "product": "App"}],
                        "metrics": [{"cvssV3_1": {"baseScore": score, "baseSeverity": severity}}],
                    }
                },
            }
        )

    low_path = cve_dir / "CVE-2025-1001.json"
    kev_low_path = cve_dir / "CVE-2025-1002.json"
    low_path.write_text(
        _make_cve_json(
            "CVE-2025-1001",
            "Minor information disclosure in Example App banner header allows low-impact fingerprinting.",
            4.3,
            "MEDIUM",
        ),
        encoding="utf-8",
    )
    kev_low_path.write_text(
        _make_cve_json(
            "CVE-2025-1002",
            "Medium flaw in Example App that is actively exploited in the wild and tracked in CISA KEV.",
            5.3,
            "MEDIUM",
        ),
        encoding="utf-8",
    )

    monkeypatch.setattr(ing, "_git_sync", lambda url, dest, sparse_paths=None: "rev-1")
    store = KnowledgeStore(data_dir=tmp_path / "db")
    # Seed CVE-2025-1002 from an external high-signal source (cisa-kev).
    store.upsert_document(
        source="cisa-kev",
        category="cve",
        rel_path="kev/CVE-2025-1002",
        title="CVE-2025-1002 KEV",
        url="",
        tags=["kev"],
        cves=["CVE-2025-1002"],
        full_text="CVE-2025-1002 exploited in the wild.",
        sections_with_chunks=[("KEV", "KEV > CVE-2025-1002", "CVE-2025-1002 exploited in the wild.", ["CVE-2025-1002 exploited in the wild."])],
        cve_entries=[("CVE-2025-1002", "medium", True, "Exploited in the wild")],
    )
    store.conn.commit()

    ing.ingest_cvelist_v5(store, src_dir)
    assert store.known_cve_ids() == {"CVE-2025-1001", "CVE-2025-1002"}
    assert store.known_cve_ids(exclude_source="cvelist-v5") == {"CVE-2025-1002"}

    def _embed_flag(cve_id: str) -> int:
        row = store.conn.execute(
            "SELECT embedded FROM chunks WHERE source = 'cvelist-v5' AND cves LIKE ?",
            (f"%{cve_id}%",),
        ).fetchone()
        assert row is not None
        return int(row["embedded"])

    assert _embed_flag("CVE-2025-1001") == 2
    assert _embed_flag("CVE-2025-1002") == 0

    # Simulate upstream CVEListV5 JSON edit to the low-signal CVE and re-ingest.
    low_path.write_text(
        _make_cve_json(
            "CVE-2025-1001",
            "Updated description: minor information disclosure in Example App banner header (refreshed).",
            4.3,
            "MEDIUM",
        ),
        encoding="utf-8",
    )
    ing.ingest_cvelist_v5(store, src_dir)

    # Low-signal CVE must stay lexical-only (embedded=2), not be promoted to embedded=0.
    assert _embed_flag("CVE-2025-1001") == 2
    assert _embed_flag("CVE-2025-1002") == 0
    store.close()


def test_ingest_privesc_bins_multi_hop_and_cyclic_aliases(tmp_path, monkeypatch):
    """SECRAG-R4-002: GTFOBins multi-hop alias chains (e.g. rview -> view -> vim) must resolve
    completely, invalidate hashes when any hop changes, and safely ignore cyclic/dangling aliases."""
    from secrag import ingest as ing

    src_dir = tmp_path / "sources"
    gtfo_dir = src_dir / "gtfobins" / "_gtfobins"
    gtfo_dir.mkdir(parents=True)

    (gtfo_dir / "vim").write_text(
        "---\ncomment: Classic vi editor.\nfunctions:\n  shell:\n  - code: vim -c ':!/bin/sh'\n    contexts:\n      sudo:\n      suid:\n...\n",
        encoding="utf-8",
    )
    (gtfo_dir / "view").write_text("---\nalias: vim\n...\n", encoding="utf-8")
    (gtfo_dir / "rview").write_text("---\nalias: view\n...\n", encoding="utf-8")
    (gtfo_dir / "cycle-a").write_text("---\nalias: cycle-b\n...\n", encoding="utf-8")
    (gtfo_dir / "cycle-b").write_text("---\nalias: cycle-a\n...\n", encoding="utf-8")
    (gtfo_dir / "self-cycle").write_text("---\nalias: self-cycle\n...\n", encoding="utf-8")
    (gtfo_dir / "dangling").write_text("---\nalias: missing-bin\n...\n", encoding="utf-8")

    monkeypatch.setattr(ing, "_git_sync", lambda url, dest: "test-rev")
    store = KnowledgeStore(data_dir=tmp_path / "db")
    ing.ingest_privesc_bins(store, src_dir)

    docs = {
        r["rel_path"]: r
        for r in store.conn.execute(
            "SELECT rel_path, title, url, tags, content_hash, full_text FROM documents"
        ).fetchall()
    }
    assert "gtfobins/rview" in docs
    assert "gtfobins/view" in docs
    assert "gtfobins/vim" in docs
    assert "gtfobins/cycle-a" not in docs
    assert "gtfobins/cycle-b" not in docs
    assert "gtfobins/self-cycle" not in docs
    assert "gtfobins/dangling" not in docs

    rview = docs["gtfobins/rview"]
    assert rview["url"].endswith("/gtfobins/vim/")
    assert "Alias for `vim`." in rview["full_text"]
    assert "vim -c ':!/bin/sh'" in rview["full_text"]
    assert "rview" in rview["tags"] and "vim" in rview["tags"]
    old_hash = rview["content_hash"]

    # Modifying intermediate hop ('view') or terminal hop ('vim') must change rview's content_hash.
    (gtfo_dir / "view").write_text("---\n# updated intermediate comment\nalias: vim\n...\n", encoding="utf-8")
    ing.ingest_privesc_bins(store, src_dir)
    new_hash = store.conn.execute(
        "SELECT content_hash FROM documents WHERE rel_path = 'gtfobins/rview'"
    ).fetchone()["content_hash"]
    assert new_hash != old_hash
    store.close()


def test_upsert_document_no_cross_section_cve_pollution_and_embed_cap(tmp_path):
    """SECRAG-R7-001: Multi-section docs must not stamp unrelated chunks with doc-level CVEs;
    single-section docs still inherit doc CVEs; iter_unembedded_chunks caps CVE header length."""
    store = KnowledgeStore(data_dir=tmp_path / "db")

    # 1. Multi-section document: only the section mentioning CVE-2026-34606 should carry it.
    store.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="web/example.md",
        title="Example Web Tricks",
        url="https://example.invalid/web",
        tags=["web"],
        cves=["CVE-2026-34606"],
        full_text="## Vuln\nExploit CVE-2026-34606 via header smuggling.\n\n## Other\nGeneral tips.\n\n## References\nhttps://example.invalid",
        sections_with_chunks=[
            ("Vuln", "Example > Vuln", "Exploit CVE-2026-34606 via header smuggling.", ["Exploit CVE-2026-34606 via header smuggling."]),
            ("Other", "Example > Other", "General tips without any vulnerability identifier.", ["General tips without any vulnerability identifier."]),
            ("References", "Example > References", "https://example.invalid/ref", ["https://example.invalid/ref"]),
        ],
    )

    # 2. Single-section document (e.g. Nuclei / CVEListV5 / Exploit-DB) whose chunk doesn't repeat the CVE.
    store.upsert_document(
        source="nuclei",
        category="cve",
        rel_path="http/cves/2026/CVE-2026-99999.yaml",
        title="CVE-2026-99999 Template",
        url="",
        tags=["nuclei"],
        cves=["CVE-2026-99999"],
        full_text="GET /admin HTTP/1.1",
        sections_with_chunks=[
            ("Template", "Nuclei > Template", "GET /admin HTTP/1.1", ["GET /admin HTTP/1.1"]),
        ],
    )

    # 3. Chunk with many CVEs (>8): iter_unembedded_chunks must cap CVE header so body isn't crowded out.
    many_cves = [f"CVE-2026-{10000 + i}" for i in range(25)]
    many_body = "Unique exploit body marker XYZ123. " + " ".join(many_cves)
    store.upsert_document(
        source="bugbounty-writeups",
        category="writeups",
        rel_path="top.md",
        title="Top Writeups",
        url="",
        tags=["writeups"],
        cves=many_cves,
        full_text=many_body,
        sections_with_chunks=[
            ("Top", "Top Writeups", many_body, [many_body]),
        ],
    )
    store.conn.commit()

    rows = {
        r["breadcrumb"]: r["cves"]
        for r in store.conn.execute("SELECT breadcrumb, cves FROM chunks").fetchall()
    }
    assert rows["Example > Vuln"] == "CVE-2026-34606"
    assert rows["Example > Other"] == ""
    assert rows["Example > References"] == ""
    assert rows["Nuclei > Template"] == "CVE-2026-99999"

    # Verify iter_unembedded_chunks caps the CVE bracket header at 8 CVEs.
    embedded_texts = {}
    for batch in store.iter_unembedded_chunks(batch_size=64):
        for cid, txt in batch:
            bc = store.conn.execute("SELECT breadcrumb FROM chunks WHERE id = ?", (cid,)).fetchone()["breadcrumb"]
            embedded_texts[bc] = txt

    top_header = embedded_texts["Top Writeups"].splitlines()[0]
    assert "CVE-2026-10000" in top_header
    assert "CVE-2026-10007" in top_header
    assert "CVE-2026-10008" not in top_header
    assert "Unique exploit body marker XYZ123." in embedded_texts["Top Writeups"][:2400]
    store.close()


def test_chunk_markdown_fences_in_lists_blockquotes_and_nested_backticks():
    """SECRAG-R7-002: List/blockquote fences, info-string lines inside open fences,
    shorter nested fences inside 4-backtick fences, and inline triple-backtick spans
    must never invert fence state in chunk_markdown or _split_section_to_chunks."""
    from secrag.chunker import _split_section_to_chunks, chunk_markdown

    md = "\n".join([
        "# Root Guide",
        "Introductory text long enough to exceed the minimum section threshold.",
        "",
        "## List Fence Section",
        "   - ```json",
        '     {"key": "# not a heading"}',
        "     ```",
        "",
        "## Blockquote Fence Section",
        "> ```bash",
        "> # bash comment should stay inside code block",
        "> echo ok",
        "> ```",
        "",
        "## Nested Four-Backtick Section",
        "````markdown",
        "```sql",
        "## sql comment not a heading",
        "SELECT 1;",
        "```",
        "````",
        "",
        "## Info String Inside Open Fence",
        "```yaml",
        "```yaml",
        "# yaml comment inside open block",
        "key: val",
        "```",
        "",
        "## Inline Backticks Section",
        "- ```inline``` should not open a fence at all",
        "",
        "## Final Real Heading",
        "This final heading must be parsed as its own section after all code blocks.",
    ])

    sections = chunk_markdown(md, doc_title="Root Guide", root_breadcrumb="Guide")
    headings = [h for h, _, _, _ in sections]
    assert headings == [
        "Root Guide",
        "List Fence Section",
        "Blockquote Fence Section",
        "Nested Four-Backtick Section",
        "Info String Inside Open Fence",
        "Inline Backticks Section",
        "Final Real Heading",
    ]
    assert all("sql comment" not in h.lower() and "bash comment" not in h.lower() for h in headings)

    # Also exercise _split_section_to_chunks on an oversized section with nested 4-backtick and list fences.
    big_section = "\n".join([
        "## Big Section",
        "Lead paragraph " + ("a " * 200),
        "",
        "   - ```json",
        '     {"a": 1}',
        "     ```",
        "",
        "````markdown",
        "```sql",
        "SELECT 1;",
        "```",
        "````",
        "",
        "Trailing paragraph " + ("b " * 200),
    ])
    chunks = _split_section_to_chunks(big_section, target=300, hard_max=500)
    assert len(chunks) >= 2
    joined = "\n\n".join(chunks)
    assert "SELECT 1;" in joined
    assert "Trailing paragraph" in joined
    # The nested 4-backtick block must stay atomic in a single chunk.
    assert any("````markdown" in c and "````" in c.splitlines()[-1] for c in chunks)


def test_ingest_nuclei_includes_exposures_files_dir(tmp_path, monkeypatch):
    """SECRAG-R7-003: Nuclei templates under http/exposures/files/ must not be skipped."""
    from secrag import ingest as ing

    src_dir = tmp_path / "sources"
    files_dir = src_dir / "nuclei-templates" / "http" / "exposures" / "files"
    files_dir.mkdir(parents=True)
    (files_dir / "appsettings-file-disclosure.yaml").write_text(
        "\n".join([
            "id: appsettings-file-disclosure",
            "info:",
            "  name: ASP.NET appsettings.json Disclosure",
            "  author: test",
            "  severity: medium",
            "  description: Detects exposed appsettings.json configuration files containing secrets.",
            "  tags: exposure,files,config",
            "http:",
            "  - method: GET",
            "    path:",
            '      - "{{BaseURL}}/appsettings.json"',
        ]),
        encoding="utf-8",
    )

    monkeypatch.setattr(ing, "_git_sync", lambda url, dest: "nuclei-rev")
    store = KnowledgeStore(data_dir=tmp_path / "db")
    ing.ingest_nuclei(store, src_dir)

    rows = store.conn.execute(
        "SELECT rel_path, title FROM documents WHERE source = 'nuclei'"
    ).fetchall()
    rel_paths = [r["rel_path"] for r in rows]
    assert "http/exposures/files/appsettings-file-disclosure.yaml" in rel_paths
    store.close()


def test_infer_category_from_path_spaces_and_word_boundaries():
    from secrag.chunker import infer_category_from_path

    # PayloadsAllTheThings spaced/full-name directories must map to accurate categories.
    assert infer_category_from_path("payloads", "SQL Injection/README.md") == "web"
    assert infer_category_from_path("payloads", "Server Side Template Injection/README.md") == "web"
    assert infer_category_from_path("payloads", "Server Side Request Forgery/README.md") == "web"
    assert infer_category_from_path("payloads", "Cross-Site Request Forgery/README.md") == "web"
    assert infer_category_from_path("payloads", "Insecure Direct Object References/README.md") == "web"
    assert infer_category_from_path("payloads", "File Inclusion/README.md") == "web"
    assert infer_category_from_path("payloads", "Command Injection/README.md") == "web"
    assert infer_category_from_path("payloads", "Request Smuggling/README.md") == "web"
    assert infer_category_from_path("payloads", "Upload Insecure Files/README.md") == "web"
    assert infer_category_from_path("payloads", "Prototype Pollution/README.md") == "web"
    assert infer_category_from_path("payloads", "Open Redirect/README.md") == "web"
    assert infer_category_from_path("payloads", "CORS Misconfiguration/README.md") == "web"
    assert infer_category_from_path("payloads", "CRLF Injection/README.md") == "web"
    assert infer_category_from_path("payloads", "Directory Traversal/README.md") == "web"
    assert infer_category_from_path("payloads", "Active Directory Attack/README.md") == "ad"
    assert infer_category_from_path("payloads", "Methodology and Resources/Linux - Privilege Escalation.md") == "privesc"
    assert infer_category_from_path("payloads", "GraphQL Injection/README.md") == "api"
    assert infer_category_from_path("payloads", "API Key Leaks/README.md") == "api"

    # Substring false positives must NOT be misclassified into 'api'.
    assert infer_category_from_path("payloads", "Methodology and Resources/Windows - DPAPI.md") != "api"
    assert infer_category_from_path("hacktricks", "escaping-from-gui-applications.md") != "api"
    assert infer_category_from_path("hacktricks", "interesting-windows-registry-keys.md") != "api"
    assert infer_category_from_path("hacktricks", "prestashop.md") != "api"


def test_lookup_cve_canonicalization_and_no_fts_false_matches(tmp_path):
    store = KnowledgeStore(data_dir=tmp_path)
    md1 = "# CVE-2021-44228 Log4Shell\n\nApache Log4j2 JNDI RCE vulnerability."
    store.upsert_document(
        source="cisa-kev",
        category="cve",
        rel_path="kev/CVE-2021-44228.md",
        title="CVE-2021-44228: Apache Log4j2",
        url="",
        tags=["cve", "kev"],
        cves=["CVE-2021-44228"],
        full_text=md1,
        sections_with_chunks=chunk_markdown(md1, doc_title="CVE-2021-44228"),
        cve_entries=[("CVE-2021-44228", "critical", True, "Log4Shell")],
    )
    md2 = "# CVE-2021-30807 Apple IOMobileFrameBuffer\n\nMemory corruption in 2021 kernel driver."
    store.upsert_document(
        source="cvelist-v5",
        category="cve",
        rel_path="2021/CVE-2021-30807.md",
        title="CVE-2021-30807",
        url="",
        tags=["cve"],
        cves=["CVE-2021-30807"],
        full_text=md2,
        sections_with_chunks=chunk_markdown(md2, doc_title="CVE-2021-30807"),
        cve_entries=[("CVE-2021-30807", "high", False, "Apple bug")],
    )
    store.conn.commit()

    for variant in [
        "CVE 2021 44228",
        "cve_2021_44228",
        "CVE-2021-44228 (Log4Shell)",
        "2021-44228",
    ]:
        res = store.lookup_cve(variant)
        assert res["cve_id"] == "CVE-2021-44228"
        assert res["is_kev"] is True
        assert res["match_count"] == 1
        assert res["matches"][0]["title"].startswith("CVE-2021-44228")

    # Non-CVE inputs with digits must NOT invoke relaxed FTS5 and match unrelated 2021 docs.
    bad = store.lookup_cve("CVE 2021 invalid")
    assert bad["match_count"] == 0
    assert bad["matches"] == []
    store.close()


def test_retriever_diversifies_beyond_40_chunks_in_rrf_pool(tmp_path):
    store = KnowledgeStore(data_dir=tmp_path)
    embedder = _DummyEmbedder()

    # Insert 2 huge docs with 25 chunks each (50 chunks total matching the query) plus a 3rd doc.
    for doc_idx in (1, 2):
        sections = [
            (
                f"Section {s}",
                f"LongDoc{doc_idx} > Section {s}",
                f"graphql introspection auth bypass payload {doc_idx}-{s}",
                [f"graphql introspection auth bypass payload {doc_idx}-{s}"],
            )
            for s in range(25)
        ]
        store.upsert_document(
            source="hacktricks",
            category="api",
            rel_path=f"api/long{doc_idx}.md",
            title=f"Long GraphQL Doc {doc_idx}",
            url="",
            tags=["graphql", "api"],
            cves=[],
            full_text="graphql introspection auth bypass",
            sections_with_chunks=sections,
        )
    # 3rd doc matches only weakly (rank > 40 in FTS/vector) unless all 80 RRF candidates are inspected.
    store.upsert_document(
        source="payloads",
        category="api",
        rel_path="api/graphql_third.md",
        title="Third GraphQL Doc",
        url="",
        tags=["graphql"],
        cves=[],
        full_text="graphql bypass",
        sections_with_chunks=[("Third", "Third", "graphql bypass note", ["graphql bypass note"])],
    )
    store.conn.commit()

    vec_idx = store.open_writable_vector_index()
    for batch in store.iter_unembedded_chunks(batch_size=64):
        ids = [cid for cid, _ in batch]
        texts = [t for _, t in batch]
        vecs = embedder.embed_documents(texts)
        vec_idx.add(np.asarray(ids, dtype=np.uint64), vecs.astype(np.float16))
        store.mark_chunks_embedded(ids)
    store.save_vector_index_atomic(vec_idx)

    # Mock fts_search & vector_search to return 40 chunks from docs 1+2 and doc 3 at rank 45.
    all_chunks = store.conn.execute("SELECT id, doc_id FROM chunks ORDER BY id").fetchall()
    long_ids = [int(r["id"]) for r in all_chunks if int(r["doc_id"]) in (1, 2)]
    third_id = [int(r["id"]) for r in all_chunks if int(r["doc_id"]) == 3][0]
    store.fts_search = lambda *a, **kw: [(cid, -float(50 - i)) for i, cid in enumerate(long_ids[:40])]
    store.vector_search = lambda *a, **kw: [(cid, 0.9) for cid in long_ids[20:50]] + [(third_id, 0.5)]

    retriever = HybridRetriever(store=store, embedder=embedder, reranker=None)
    retriever.reranker.enabled = False
    res = retriever.search("graphql introspection auth bypass", category="api", top_k=5, max_per_doc=2)
    doc_ids = {r["doc_id"] for r in res["results"]}
    assert len(res["results"]) == 5
    assert 3 in doc_ids
    store.close()


def test_embeddings_empty_onnx_threads_and_mps_cache_dir_and_dcl(monkeypatch):
    import sys
    import threading
    import types
    from secrag.embeddings import DEFAULT_ONNX_THREADS, LocalEmbedder, LocalReranker

    # 1. Empty / invalid SECRAG_ONNX_THREADS must not crash with ValueError.
    for bad_val in ("", "   ", "not-an-int", "-2", "0"):
        monkeypatch.setenv("SECRAG_ONNX_THREADS", bad_val)
        emb = LocalEmbedder()
        rer = LocalReranker()
        assert emb.threads == DEFAULT_ONNX_THREADS
        assert rer.threads == DEFAULT_ONNX_THREADS

    monkeypatch.setenv("SECRAG_ONNX_THREADS", "6")
    assert LocalEmbedder().threads == 6
    assert LocalReranker().threads == 6

    # 2. _ensure_mps must pass cache_dir and hold _mps_checked=False until load finishes.
    entered = threading.Event()
    release = threading.Event()
    captured_kwargs: list[dict] = []

    class _FakeModel:
        def to(self, dev):
            return self
        def eval(self):
            return self

    class _FakeAutoTokenizer:
        @staticmethod
        def from_pretrained(name, **kwargs):
            captured_kwargs.append(dict(kwargs))
            entered.set()
            release.wait(timeout=3)
            return object()

    class _FakeAutoModel:
        @staticmethod
        def from_pretrained(name, **kwargs):
            captured_kwargs.append(dict(kwargs))
            return _FakeModel()

    fake_torch = types.SimpleNamespace(
        float16="fp16",
        backends=types.SimpleNamespace(mps=types.SimpleNamespace(is_available=lambda: True)),
    )
    fake_transformers = types.SimpleNamespace(AutoModel=_FakeAutoModel, AutoTokenizer=_FakeAutoTokenizer)
    monkeypatch.setitem(sys.modules, "torch", fake_torch)
    monkeypatch.setitem(sys.modules, "transformers", fake_transformers)

    emb = LocalEmbedder(cache_dir="/tmp/custom-hf-cache", prefer_mps=True)
    results: list[bool] = []
    t1 = threading.Thread(target=lambda: results.append(emb._ensure_mps()))
    t1.start()
    assert entered.wait(timeout=2)
    # Concurrent caller must block on _lock rather than seeing _mps_checked=True with _mps_model=None!
    t2_res: list[bool] = []
    t2 = threading.Thread(target=lambda: t2_res.append(emb._ensure_mps()))
    t2.start()
    release.set()
    t1.join(timeout=3)
    t2.join(timeout=3)
    assert results == [True]
    assert t2_res == [True]
    assert all(kw.get("cache_dir") == "/tmp/custom-hf-cache" for kw in captured_kwargs)


def test_ingest_unknown_source_exits_nonzero(monkeypatch, capsys):
    import sys
    from secrag import ingest as ing

    monkeypatch.setattr(sys, "argv", ["secrag-ingest", "--source", "gtfobins"])
    with pytest.raises(SystemExit) as excinfo:
        ing.main()
    assert excinfo.value.code != 0
    err = capsys.readouterr().err
    assert "gtfobins" in err
    assert "privesc-bins" in err


def test_ingest_stats_opens_readonly_and_skips_sources_dir(tmp_path, monkeypatch, capsys):
    import json
    import sqlite3
    import sys
    from secrag import ingest as ing

    # Seed an existing DB and hold a concurrent write lock via BEGIN IMMEDIATE.
    seed = KnowledgeStore(data_dir=tmp_path)
    seed.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="web/stats.md",
        title="Stats Test",
        url="",
        tags=["web"],
        cves=[],
        full_text="# Stats\n\nBody text.",
        sections_with_chunks=chunk_markdown("# Stats\n\nBody text.", doc_title="Stats Test"),
    )
    seed.conn.commit()
    seed.close()

    # Hold an active write lock from another connection (simulating an active ingest run).
    writer = sqlite3.connect(str(tmp_path / "kb.sqlite"), timeout=1.0)
    writer.execute("BEGIN IMMEDIATE")
    try:
        orig_ks = ing.KnowledgeStore
        seen_ro: list[bool] = []

        def _spy_ks(*a, **kw):
            seen_ro.append(bool(kw.get("read_only", False)))
            return orig_ks(*a, **kw)

        monkeypatch.setattr(ing, "KnowledgeStore", _spy_ks)
        monkeypatch.setattr(sys, "argv", ["secrag-ingest", "--data-dir", str(tmp_path), "--stats"])
        ing.main()

        assert seen_ro == [True]
        out = json.loads(capsys.readouterr().out)
        assert out["documents"] == 1
        assert not (tmp_path / "_sources").exists()
    finally:
        writer.rollback()
        writer.close()


def test_ingest_error_rolls_back_partial_transaction_and_savepoint(tmp_path, monkeypatch):
    import sys
    from secrag import ingest as ing

    # 1. KnowledgeStore.upsert_document must remain atomic via SAVEPOINT on mid-upsert error.
    store = KnowledgeStore(data_dir=tmp_path)
    store.upsert_document(
        source="hacktricks",
        category="web",
        rel_path="web/atomic.md",
        title="Original Atomic Doc",
        url="",
        tags=["web"],
        cves=[],
        full_text="# Original\n\nIntact body.",
        sections_with_chunks=[("Original", "Original Atomic Doc > Original", "Intact body.", ["Intact body."])],
    )
    store.conn.commit()

    # Passing a malformed cve_entries tuple raises AttributeError AFTER DELETE FROM documents.
    with pytest.raises(AttributeError):
        store.upsert_document(
            source="hacktricks",
            category="web",
            rel_path="web/atomic.md",
            title="Replacement That Fails",
            url="",
            tags=["web"],
            cves=[],
            full_text="# Replacement\n\nDifferent content.",
            sections_with_chunks=[("Replacement", "Replacement", "Different content.", ["Different content."])],
            cve_entries=[(None, "high", False, "boom")],  # type: ignore[list-item]
        )
    # Even if a caller commits afterwards, the original document and its chunks must be intact.
    store.conn.commit()
    row = store.conn.execute(
        "SELECT title FROM documents WHERE source = 'hacktricks' AND rel_path = 'web/atomic.md'"
    ).fetchone()
    assert row is not None and row["title"] == "Original Atomic Doc"
    chunk_cnt = store.conn.execute("SELECT COUNT(*) FROM chunks WHERE source = 'hacktricks'").fetchone()[0]
    assert chunk_cnt == 1

    # 2. ingest_bugbounty_writeups must roll back uncommitted partial writes when a repo fails.
    def _fake_git_sync(url, dest, **kw):
        dest.mkdir(parents=True, exist_ok=True)
        return "deadbeef"

    call_idx = {"n": 0}

    def _fake_ingest_tree(st, **kw):
        call_idx["n"] += 1
        if call_idx["n"] == 1:
            st.upsert_document(
                source="bugbounty-writeups",
                category="writeups",
                rel_path="repo1/partial.md",
                title="Partial Uncommitted",
                url="",
                tags=["writeups"],
                cves=[],
                full_text="partial body",
                sections_with_chunks=[("H", "H", "partial body", ["partial body"])],
            )
            raise RuntimeError("simulated repo1 failure before commit")
        return 0

    monkeypatch.setattr(ing, "_git_sync", _fake_git_sync)
    monkeypatch.setattr(ing, "_ingest_markdown_tree", _fake_ingest_tree)
    ing.ingest_bugbounty_writeups(store, tmp_path / "_sources")
    store.conn.commit()
    assert (
        store.conn.execute(
            "SELECT COUNT(*) FROM documents WHERE source = 'bugbounty-writeups'"
        ).fetchone()[0]
        == 0
    )
    store.close()

    # 3. ingest.main() must roll back uncommitted writes before update_ingestion_state(..., "error") commits.
    def _failing_source(st: KnowledgeStore, src_dir):
        st.upsert_document(
            source="hacktricks",
            category="web",
            rel_path="web/uncommitted.md",
            title="Should Roll Back",
            url="",
            tags=["web"],
            cves=[],
            full_text="should not survive",
            sections_with_chunks=[("H", "H", "should not survive", ["should not survive"])],
        )
        raise RuntimeError("mid-batch crash")

    monkeypatch.setattr(ing, "SOURCES", [("hacktricks", 1, _failing_source)])
    monkeypatch.setattr(
        sys,
        "argv",
        ["secrag-ingest", "--data-dir", str(tmp_path), "--source", "hacktricks", "--skip-embed"],
    )
    ing.main()

    check = KnowledgeStore(data_dir=tmp_path, read_only=True)
    try:
        assert (
            check.conn.execute(
                "SELECT COUNT(*) FROM documents WHERE rel_path = 'web/uncommitted.md'"
            ).fetchone()[0]
            == 0
        )
        st_row = check.conn.execute(
            "SELECT status, error FROM ingestion_state WHERE source = 'hacktricks'"
        ).fetchone()
        assert st_row["status"] == "error"
        assert "mid-batch crash" in st_row["error"]
    finally:
        check.close()


def test_embeddings_blank_model_env_falls_back_to_default(monkeypatch):
    from secrag import DEFAULT_EMBED_MODEL, DEFAULT_RERANK_MODEL
    from secrag.embeddings import LocalEmbedder, LocalReranker

    for blank in ("", "   ", "\t\n"):
        monkeypatch.setenv("SECRAG_EMBED_MODEL", blank)
        monkeypatch.setenv("SECRAG_RERANK_MODEL", blank)
        assert LocalEmbedder().model_name == DEFAULT_EMBED_MODEL
        assert LocalReranker().model_name == DEFAULT_RERANK_MODEL
        assert LocalEmbedder(model_name=blank).model_name == DEFAULT_EMBED_MODEL
        assert LocalReranker(model_name=blank).model_name == DEFAULT_RERANK_MODEL

    monkeypatch.setenv("SECRAG_EMBED_MODEL", "  BAAI/bge-small-en-v1.5  ")
    monkeypatch.setenv("SECRAG_RERANK_MODEL", "  jinaai/jina-reranker-v2-base-multilingual  ")
    assert LocalEmbedder().model_name == "BAAI/bge-small-en-v1.5"
    assert LocalEmbedder(model_name="   ").model_name == "BAAI/bge-small-en-v1.5"
    assert LocalReranker().model_name == "jinaai/jina-reranker-v2-base-multilingual"
    assert LocalReranker(model_name="   ").model_name == "jinaai/jina-reranker-v2-base-multilingual"
