"""3-stage Hybrid Retriever: SQLite FTS5 BM25 + USearch HNSW + RRF + Cross-Encoder Reranker."""

from __future__ import annotations

import math
import time
from typing import Optional

from .embeddings import LocalEmbedder, LocalReranker
from .storage import KnowledgeStore, extract_cves, extract_mitre_ids

RRF_K = 60
VALID_CATEGORIES = {
    "web", "api", "cloud", "ad", "privesc", "mobile", "network",
    "binary", "cve", "mitre", "detection", "exploits", "writeups", "threat-intel", "general",
}


class HybridRetriever:
    """Combines BM25, dense HNSW vectors, RRF fusion, and cross-encoder reranking."""

    def __init__(
        self,
        store: KnowledgeStore,
        embedder: Optional[LocalEmbedder] = None,
        reranker: Optional[LocalReranker] = None,
    ):
        self.store = store
        self.embedder = embedder or LocalEmbedder()
        self.reranker = reranker or LocalReranker()

    def search(
        self,
        query: str,
        *,
        category: Optional[str] = None,
        source: Optional[str] = None,
        top_k: int = 5,
        use_reranker: bool = True,
        max_per_doc: int = 2,
    ) -> dict:
        t0 = time.perf_counter()
        q = (query or "").strip()
        if not q:
            return {"query": q, "results": [], "latency_ms": 0, "mode": "empty"}

        top_k = max(1, min(int(top_k or 5), 12))
        cat = category.strip().lower() if category and category.strip() else None
        src = source.strip().lower() if source and source.strip() else None

        # Stage 1a: Lexical BM25 via FTS5
        fts_hits = self.store.fts_search(q, category=cat, source=src, limit=40)

        # Stage 1b: Dense Vector via USearch HNSW (only if vector index exists)
        vec_hits: list[tuple[int, float]] = []
        vec_available = self.store.get_readonly_vector_index() is not None
        if vec_available:
            try:
                q_vec = self.embedder.embed_query(q)
                vec_hits = self.store.vector_search(q_vec, category=cat, source=src, limit=40)
            except Exception:
                vec_hits = []

        # Stage 2: Reciprocal Rank Fusion (RRF)
        rrf_scores: dict[int, float] = {}
        for rank, (cid, _) in enumerate(fts_hits, start=1):
            rrf_scores[cid] = rrf_scores.get(cid, 0.0) + 1.0 / (RRF_K + rank)
        for rank, (cid, _) in enumerate(vec_hits, start=1):
            rrf_scores[cid] = rrf_scores.get(cid, 0.0) + 1.0 / (RRF_K + rank)

        if not rrf_scores:
            return {
                "query": q,
                "category": cat,
                "source": src,
                "results": [],
                "latency_ms": round((time.perf_counter() - t0) * 1000, 1),
                "mode": "hybrid" if vec_hits else "fts5",
            }

        # Pull top 16 fused candidates for fast sub-second cross-encoder reranking
        fused_ids = [
            cid for cid, _ in sorted(rrf_scores.items(), key=lambda kv: kv[1], reverse=True)[:16]
        ]
        chunks = self.store.get_chunks_by_ids(fused_ids)

        q_cves = set(extract_cves(q))
        q_mitre = set(extract_mitre_ids(q))

        candidates = [chunks[cid] for cid in fused_ids if cid in chunks]

        # Stage 3: Cross-Encoder Reranking (multilingual PT-BR + EN)
        reranked = False
        scores: list[float]
        if use_reranker and self.reranker.enabled and len(candidates) > 1:
            passages = [f"{c.breadcrumb}\n{c.content}" for c in candidates]
            ce_scores = self.reranker.score(q, passages)
            if ce_scores is not None and len(ce_scores) == len(candidates):
                # Normalize raw logits via sigmoid and add small exact-ID boost
                scores = []
                for c, s in zip(candidates, ce_scores):
                    norm_s = 1.0 / (1.0 + math.exp(-max(-20.0, min(20.0, s))))
                    if q_cves and any(cve in c.cves for cve in q_cves):
                        norm_s += 0.15
                    if q_mitre and any(mid in c.tags or mid in c.breadcrumb for mid in q_mitre):
                        norm_s += 0.10
                    scores.append(norm_s)
                reranked = True
            else:
                scores = [rrf_scores[c.chunk_id] for c in candidates]
        else:
            scores = [rrf_scores[c.chunk_id] for c in candidates]

        ranked_pairs = sorted(zip(candidates, scores), key=lambda x: x[1], reverse=True)

        # Diversify: at most max_per_doc chunks per document, no duplicate sections
        results = []
        per_doc: dict[int, int] = {}
        seen_sections: set[int] = set()

        for ch, sc in ranked_pairs:
            if ch.section_id in seen_sections:
                continue
            if per_doc.get(ch.doc_id, 0) >= max_per_doc:
                continue
            seen_sections.add(ch.section_id)
            per_doc[ch.doc_id] = per_doc.get(ch.doc_id, 0) + 1

            excerpt = ch.content
            if len(excerpt) > 1100:
                excerpt = excerpt[:1100].rstrip() + "\n... [truncated; call kb_read_section for full section]"

            results.append(
                {
                    "doc_id": ch.doc_id,
                    "section_id": ch.section_id,
                    "chunk_id": ch.chunk_id,
                    "source": ch.source,
                    "category": ch.category,
                    "title": ch.title,
                    "breadcrumb": ch.breadcrumb,
                    "cves": ch.cves.split() if ch.cves else [],
                    "score": round(float(sc), 4),
                    "excerpt": excerpt,
                }
            )
            if len(results) >= top_k:
                break

        mode = "hybrid+rerank" if (vec_hits and reranked) else (
            "hybrid" if vec_hits else ("fts5+rerank" if reranked else "fts5")
        )
        return {
            "query": q,
            "category": cat,
            "source": src,
            "mode": mode,
            "latency_ms": round((time.perf_counter() - t0) * 1000, 1),
            "results": results,
        }
