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
