"""3-stage Hybrid Retriever: SQLite FTS5 BM25 + USearch HNSW + RRF + Cross-Encoder Reranker."""

from __future__ import annotations

from collections import OrderedDict
import math
import re
import threading
import time
from typing import Optional

from .embeddings import LocalEmbedder, LocalReranker
from .storage import CacheSignature, KnowledgeStore, extract_cves, extract_mitre_ids

RRF_K = 60
SEARCH_CACHE_MAX = 128
VALID_CATEGORIES = {
    "web", "api", "cloud", "ad", "privesc", "mobile", "network",
    "binary", "cve", "mitre", "detection", "exploits", "writeups", "threat-intel", "general",
}

_GHSA_RE = re.compile(r"\bGHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}\b", re.IGNORECASE)
_VERSION_RE = re.compile(r"\b\d+\.\d+(?:\.\d+)?\b")

VALID_FOCUSES = {"bounty", "exploit", "whitebox", "cloud", "ad", "bypass", "cve"}

_INTENT_KEYWORDS: dict[str, tuple[str, ...]] = {
    "bounty": (
        "bounty", "writeup", "hackerone", "chain", "account takeover", "ato",
        "idor", "bola", "bfla", "business logic", "race condition", "toctou",
        "single-packet", "cache poisoning", "cache deception", "wcd", "wcp",
        "request smuggling", "desync", "oauth", "oidc", "saml", "sso",
        "prototype pollution", "dom clobbering", "postmessage", "cors", "cswsh",
        "graphql", "jwt", "webhook", "mass assignment", "open redirect",
        "subdomain takeover", "host header", "coupon", "checkout", "billing",
    ),
    "whitebox": (
        "whitebox", "source code", "semgrep", "sink", "ast", "npm", "pypi",
        "maven", "composer", "packagist", "rubygems", "nuget", "crates",
        "package.json", "requirements.txt", "go.mod", "express", "nextjs",
        "next.js", "django", "flask", "spring", "laravel", "rails", "webpack",
        "middleware", "dependency", "ghsa", "bundle",
    ),
    "cloud": (
        "aws", "iam", "s3", "cognito", "sts", "lambda", "eks", "ec2", "imds",
        "imdsv2", "gcp", "azure", "entra", "kubernetes", "k8s", "rbac",
        "helm", "container", "docker", "terraform", "github actions", "cloud",
    ),
    "ad": (
        "active directory", "kerberos", "adcs", "esc1", "esc4", "esc8",
        "ntlm", "relay", "bloodhound", "dcsync", "kerberoast", "asreproast",
        "rbcd", "delegation", "gpo", "ldap", "smb", "golden ticket", "silver ticket",
    ),
    "bypass": (
        "bypass", "waf", "filter", "evasion", "csp", "sandbox", "without",
        "obfuscate", "encoding", "blacklist", "allowlist", "blocked", "403",
    ),
    "exploit": (
        "exploit", "poc", "rce", "metasploit", "nuclei", "vulhub", "0day",
        "unauthenticated", "pre-auth", "deserialization", "jndi", "gadget",
    ),
}

# Intent-specific source boosts applied cleanly after cross-encoder sigmoid scoring.
INTENT_SOURCE_BOOSTS: dict[str, dict[str, float]] = {
    "bounty": {
        "hackerone-disclosed": 0.09,
        "portswigger-labs": 0.09,
        "bounty-methodology": 0.07,
        "bugbounty-writeups": 0.07,
        "hacktricks": 0.05,
        "payloads": 0.05,
        "owasp": 0.04,
    },
    "whitebox": {
        "semgrep-rules": 0.10,
        "ghsa-advisories": 0.10,
        "vulhub-pocs": 0.06,
        "portswigger-labs": 0.05,
        "hackerone-disclosed": 0.05,
    },
    "cloud": {
        "hacking-the-cloud": 0.10,
        "hacktricks-cloud": 0.09,
        "the-hacker-recipes": 0.06,
        "payloads": 0.04,
        "hackerone-disclosed": 0.04,
    },
    "ad": {
        "the-hacker-recipes": 0.10,
        "payloads": 0.08,
        "hacktricks": 0.06,
        "privesc-bins": 0.06,
        "atomic-red-team": 0.05,
    },
    "bypass": {
        "payloads": 0.09,
        "portswigger-labs": 0.08,
        "hacktricks": 0.07,
        "bounty-methodology": 0.07,
        "hackerone-disclosed": 0.06,
        "bugbounty-writeups": 0.06,
    },
    "exploit": {
        "vulhub-pocs": 0.10,
        "metasploit-docs": 0.08,
        "nuclei": 0.08,
        "cisa-kev": 0.08,
        "exploitdb": 0.06,
        "ghsa-advisories": 0.06,
        "hackerone-disclosed": 0.05,
    },
    "cve": {
        "cisa-kev": 0.10,
        "vulhub-pocs": 0.09,
        "nuclei": 0.08,
        "metasploit-docs": 0.07,
        "trickest-cve": 0.06,
        "ghsa-advisories": 0.06,
        "hackerone-disclosed": 0.06,
    },
}


def classify_query_intent(query: str, focus: Optional[str] = None) -> str:
    """Deterministically classify offensive query intent in <0.1 ms."""
    f = (focus or "").strip().lower()
    if f in VALID_FOCUSES:
        return f
    q_cves = extract_cves(query)
    if q_cves or _GHSA_RE.search(query):
        return "cve"
    low = query.lower()
    scores: dict[str, int] = {}
    for intent, kws in _INTENT_KEYWORDS.items():
        hits = sum(1 for kw in kws if kw in low)
        if hits:
            scores[intent] = hits
    if _VERSION_RE.search(query) and "exploit" not in scores:
        scores["exploit"] = 1
    if not scores:
        return "tactical"
    # Prefer specific domain intents cleanly on ties (bounty > whitebox > cloud > ad > bypass > exploit)
    priority = ("bounty", "whitebox", "cloud", "ad", "bypass", "exploit")
    return max(scores.keys(), key=lambda k: (scores[k], -priority.index(k)))


# Deep tactical playbooks, real bug-bounty writeups, and concrete PoC walkthroughs
# get a gentle boost on unfiltered technique queries so generic CVE catalog stubs
# never drown out actionable methodologies and anti-duplicate attack chains.
TACTICAL_SOURCES = {
    "portswigger-labs",
    "hackerone-disclosed",
    "hacktricks",
    "hacktricks-cloud",
    "the-hacker-recipes",
    "hacking-the-cloud",
    "payloads",
    "bounty-methodology",
    "bugbounty-writeups",
    "owasp",
    "privesc-bins",
    "vulhub-pocs",
    "metasploit-docs",
    "semgrep-rules",
}

# Raw CVE/link catalog stubs shine in kb_lookup_cve or explicit CVE queries,
# but should be capped on unfiltered technique/methodology searches.
CVE_STUB_SOURCES = {
    "cvelist-v5",
    "trickest-cve",
    "poc-in-github",
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
        self._cache_lock = threading.Lock()
        self._cache_sig: Optional[CacheSignature] = None
        self._cache: OrderedDict[tuple, dict] = OrderedDict()

    def search(
        self,
        query: str,
        *,
        category: Optional[str] = None,
        source: Optional[str] = None,
        focus: Optional[str] = None,
        top_k: int = 5,
        use_reranker: bool = True,
        max_per_doc: int = 2,
    ) -> dict:
        t0 = time.perf_counter()
        q = (query or "").strip()
        if not q:
            return {"query": q, "results": [], "latency_ms": 0, "mode": "empty", "intent": "empty"}

        top_k = max(1, min(int(top_k or 5), 12))
        cat = category.strip().lower() if category and category.strip() else None
        src = source.strip().lower() if source and source.strip() else None
        foc = focus.strip().lower() if focus and focus.strip() else None
        if foc not in VALID_FOCUSES:
            foc = None

        cache_key = (q.lower(), cat, src, foc, top_k, bool(use_reranker and self.reranker.enabled), max_per_doc)
        sig = self.store.cache_signature()
        with self._cache_lock:
            if self._cache_sig != sig:
                self._cache.clear()
                self._cache_sig = sig
            cached = self._cache.get(cache_key)
            if cached is not None:
                self._cache.move_to_end(cache_key)
                out = dict(cached)
                out["results"] = list(cached["results"])
                out["latency_ms"] = round((time.perf_counter() - t0) * 1000, 1)
                out["cached"] = True
                return out

        q_cves = set(extract_cves(q))
        q_mitre = set(extract_mitre_ids(q))
        intent = classify_query_intent(q, focus=foc)
        is_advisory_query = bool(q_cves or _GHSA_RE.search(q) or intent == "cve")
        # On unfiltered technique/bug-bounty queries (no explicit category/source and no
        # CVE/GHSA ID), run dual-lane retrieval: primary non-CVE tactical lane + secondary
        # CVE/advisory lane so massive CVE catalogs (~50% of chunks) never drown out deep
        # playbooks, PortSwigger labs, and disclosed HackerOne writeups.
        use_tactical_dual_lane = (cat is None and src is None and not is_advisory_query)

        # Stage 1a: Lexical BM25 via FTS5
        if use_tactical_dual_lane:
            fts_hits = self.store.fts_search(q, exclude_categories=("cve",), limit=35)
            fts_cve_hits = self.store.fts_search(q, category="cve", limit=10)
        else:
            fts_hits = self.store.fts_search(q, category=cat, source=src, limit=40)
            fts_cve_hits = []

        # Stage 1b: Dense Vector via USearch HNSW (only if vector index exists)
        vec_hits: list[tuple[int, float]] = []
        vec_cve_hits: list[tuple[int, float]] = []
        degraded = False
        try:
            vec_available = self.store.get_readonly_vector_index() is not None
            if vec_available:
                q_vec = self.embedder.embed_query(q)
                if use_tactical_dual_lane:
                    vec_hits = self.store.vector_search(q_vec, exclude_categories=("cve",), limit=35)
                    vec_cve_hits = self.store.vector_search(q_vec, category="cve", limit=10)
                else:
                    vec_hits = self.store.vector_search(q_vec, category=cat, source=src, limit=40)
        except Exception:
            vec_hits = []
            vec_cve_hits = []
            degraded = True

        # Stage 1c: If the query names an explicit CVE, inject top cve_index chunks directly
        # so kb_search("CVE-YYYY-NNNN") surfaces KEV/Vulhub/Nuclei/Metasploit/H1 immediately.
        cve_direct_ids: list[int] = []
        if q_cves and src is None:
            for cve_id in sorted(q_cves)[:2]:
                try:
                    lk = self.store.lookup_cve(cve_id, limit=6)
                    for m in lk.get("matches") or []:
                        sec_id = m.get("section_id")
                        if sec_id:
                            row = self.store.conn.execute(
                                "SELECT id FROM chunks WHERE section_id = ? ORDER BY id ASC LIMIT 1",
                                (sec_id,),
                            ).fetchone()
                            if row is not None:
                                cve_direct_ids.append(int(row["id"]))
                except Exception:
                    pass

        # Stage 2: Reciprocal Rank Fusion (RRF)
        rrf_scores: dict[int, float] = {}
        for rank, cid in enumerate(cve_direct_ids, start=1):
            rrf_scores[cid] = rrf_scores.get(cid, 0.0) + 1.5 / (RRF_K + rank)
        for rank, (cid, _) in enumerate(fts_hits, start=1):
            rrf_scores[cid] = rrf_scores.get(cid, 0.0) + 1.0 / (RRF_K + rank)
        for rank, (cid, _) in enumerate(vec_hits, start=1):
            rrf_scores[cid] = rrf_scores.get(cid, 0.0) + 1.0 / (RRF_K + rank)
        cve_lane_weight = 0.85 if intent in ("whitebox", "exploit") else 0.70
        for rank, (cid, _) in enumerate(fts_cve_hits, start=1):
            rrf_scores[cid] = rrf_scores.get(cid, 0.0) + cve_lane_weight / (RRF_K + rank)
        for rank, (cid, _) in enumerate(vec_cve_hits, start=1):
            rrf_scores[cid] = rrf_scores.get(cid, 0.0) + cve_lane_weight / (RRF_K + rank)

        has_vec_hits = bool(vec_hits or vec_cve_hits)
        if not rrf_scores:
            return {
                "query": q,
                "category": cat,
                "source": src,
                "intent": intent,
                "results": [],
                "latency_ms": round((time.perf_counter() - t0) * 1000, 1),
                "mode": "hybrid" if has_vec_hits else "fts5",
            }

        # Pull top adaptive fused candidates (10 for default top_k=5, up to 16 for larger top_k)
        # for fast ~330 ms cross-encoder reranking. Pre-diversify before slicing rerank_k so a
        # single long document or single source cannot consume all reranker slots.
        rerank_k = min(16, max(10, top_k * 2))
        sorted_ids = [
            cid for cid, _ in sorted(rrf_scores.items(), key=lambda kv: kv[1], reverse=True)
        ]
        chunks = self.store.get_chunks_by_ids(sorted_ids)
        intent_boosts = INTENT_SOURCE_BOOSTS.get(intent, {})

        # Pre-sort RRF pool gently by intent affinity before slicing rerank_k
        if intent_boosts and src is None:
            sorted_ids.sort(
                key=lambda cid: rrf_scores[cid] * (1.0 + intent_boosts.get(chunks[cid].source, 0.0) * 2.0)
                if cid in chunks
                else rrf_scores[cid],
                reverse=True,
            )

        candidates = []
        pre_per_doc: dict[int, int] = {}
        pre_per_src: dict[str, int] = {}
        pre_seen_sections: set[int] = set()
        pre_cve_stubs = 0
        max_pre_cve_stubs = 2 if use_tactical_dual_lane else rerank_k
        max_pre_per_src = 4 if (src is None and top_k <= 6) else rerank_k
        deferred_overflow: list = []

        for cid in sorted_ids:
            ch = chunks.get(cid)
            if ch is None:
                continue
            if ch.section_id in pre_seen_sections:
                continue
            if pre_per_doc.get(ch.doc_id, 0) >= max_per_doc:
                continue
            if use_tactical_dual_lane and ch.source in CVE_STUB_SOURCES:
                if pre_cve_stubs >= max_pre_cve_stubs:
                    continue
                pre_cve_stubs += 1
            if pre_per_src.get(ch.source, 0) >= max_pre_per_src:
                deferred_overflow.append(ch)
                continue
            pre_seen_sections.add(ch.section_id)
            pre_per_doc[ch.doc_id] = pre_per_doc.get(ch.doc_id, 0) + 1
            pre_per_src[ch.source] = pre_per_src.get(ch.source, 0) + 1
            candidates.append(ch)
            if len(candidates) >= rerank_k:
                break
        if len(candidates) < rerank_k and deferred_overflow:
            for ch in deferred_overflow:
                if ch.section_id in pre_seen_sections or pre_per_doc.get(ch.doc_id, 0) >= max_per_doc:
                    continue
                pre_seen_sections.add(ch.section_id)
                pre_per_doc[ch.doc_id] = pre_per_doc.get(ch.doc_id, 0) + 1
                candidates.append(ch)
                if len(candidates) >= rerank_k:
                    break

        # Stage 3: Cross-Encoder Reranking (multilingual PT-BR + EN)
        reranked = False
        scores: list[float]
        if use_reranker and self.reranker.enabled and len(candidates) > 1:
            passages = [f"{c.breadcrumb}\n{c.content}" for c in candidates]
            try:
                ce_scores = self.reranker.score(q, passages)
            except Exception:
                ce_scores = None
                degraded = True
            if ce_scores is not None and len(ce_scores) == len(candidates):
                # Normalize raw logits via sigmoid and add intent-aware signal boosts
                scores = []
                for c, s in zip(candidates, ce_scores):
                    norm_s = 1.0 / (1.0 + math.exp(-max(-20.0, min(20.0, s))))
                    if q_cves and any(cve in c.cves for cve in q_cves):
                        norm_s += 0.15
                    if q_mitre and any(mid in c.tags or mid in c.breadcrumb for mid in q_mitre):
                        norm_s += 0.10
                    if src is None and c.source in intent_boosts:
                        norm_s += intent_boosts[c.source]
                    elif use_tactical_dual_lane:
                        if c.source in TACTICAL_SOURCES:
                            norm_s += 0.05
                    if use_tactical_dual_lane and c.source in CVE_STUB_SOURCES:
                        norm_s -= 0.08
                    scores.append(norm_s)
                reranked = True
            else:
                if not getattr(self.reranker, "_failed", False):
                    degraded = True
                scores = [rrf_scores[c.chunk_id] for c in candidates]
        else:
            scores = []
            for c in candidates:
                sc = rrf_scores[c.chunk_id]
                if q_cves and any(cve in c.cves for cve in q_cves):
                    sc += 0.02
                if q_mitre and any(mid in c.tags or mid in c.breadcrumb for mid in q_mitre):
                    sc += 0.01
                if src is None and c.source in intent_boosts:
                    sc *= 1.0 + intent_boosts[c.source] * 2.5
                elif use_tactical_dual_lane and c.source in TACTICAL_SOURCES:
                    sc *= 1.15
                if use_tactical_dual_lane and c.source in CVE_STUB_SOURCES:
                    sc *= 0.65
                scores.append(sc)

        ranked_pairs = sorted(zip(candidates, scores), key=lambda x: x[1], reverse=True)

        # Diversify: at most max_per_doc chunks per document, no duplicate sections,
        # cross-source diversity (max 2 per source on unfiltered queries when other
        # sources exist), and at most 1 raw CVE catalog stub on non-CVE searches.
        results = []
        per_doc: dict[int, int] = {}
        per_source: dict[str, int] = {}
        seen_sections: set[int] = set()
        cve_stub_count = 0
        max_per_source = 2 if (src is None and top_k >= 4) else top_k
        overflow_pairs: list[tuple] = []

        def _append_hit(ch, sc):
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

        for ch, sc in ranked_pairs:
            if ch.section_id in seen_sections:
                continue
            if per_doc.get(ch.doc_id, 0) >= max_per_doc:
                continue
            if use_tactical_dual_lane and ch.source in CVE_STUB_SOURCES and cve_stub_count >= 1:
                continue
            if per_source.get(ch.source, 0) >= max_per_source:
                overflow_pairs.append((ch, sc))
                continue
            seen_sections.add(ch.section_id)
            per_doc[ch.doc_id] = per_doc.get(ch.doc_id, 0) + 1
            per_source[ch.source] = per_source.get(ch.source, 0) + 1
            if ch.source in CVE_STUB_SOURCES:
                cve_stub_count += 1
            _append_hit(ch, sc)
            if len(results) >= top_k:
                break

        if len(results) < top_k and overflow_pairs:
            for ch, sc in overflow_pairs:
                if ch.section_id in seen_sections or per_doc.get(ch.doc_id, 0) >= max_per_doc:
                    continue
                seen_sections.add(ch.section_id)
                per_doc[ch.doc_id] = per_doc.get(ch.doc_id, 0) + 1
                _append_hit(ch, sc)
                if len(results) >= top_k:
                    break

        mode = "hybrid+rerank" if (has_vec_hits and reranked) else (
            "hybrid" if has_vec_hits else ("fts5+rerank" if reranked else "fts5")
        )
        payload = {
            "query": q,
            "category": cat,
            "source": src,
            "intent": intent,
            "mode": mode,
            "latency_ms": round((time.perf_counter() - t0) * 1000, 1),
            "results": results,
        }
        if not degraded:
            with self._cache_lock:
                if self._cache_sig == sig:
                    self._cache[cache_key] = payload
                    self._cache.move_to_end(cache_key)
                    while len(self._cache) > SEARCH_CACHE_MAX:
                        self._cache.popitem(last=False)
        return payload
