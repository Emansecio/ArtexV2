"""SQLite (WAL + FTS5 BM25 + CVE index) and USearch HNSW (f16 mmap) storage layer."""

from __future__ import annotations

import hashlib
import os
import re
import sqlite3
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Optional

import numpy as np
from usearch.index import Index

from . import EMBED_DIM

DEFAULT_DATA_DIR = Path(__file__).resolve().parents[3] / "data" / "secrag"

CVE_RE = re.compile(r"\bCVE-\d{4}-\d{4,7}\b", re.IGNORECASE)
MITRE_RE = re.compile(r"\bT\d{4}(?:\.\d{3})?\b")
TOKEN_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_\-./:+]*")

# Subsets with <= this many embedded chunks use exact SIMD matmul (100% recall in ~15-40 ms)
# instead of approximate global HNSW over-fetching.
EXACT_SUBSET_THRESHOLD = 15_000
FTS_SELECTIVE_DF_CAP = 25_000

STOPWORDS = {
    "a", "an", "and", "are", "as", "at", "be", "by", "como", "da", "das", "de",
    "do", "dos", "e", "em", "for", "from", "how", "in", "is", "it", "na", "nas",
    "no", "nos", "o", "os", "of", "on", "or", "para", "por", "que", "se", "the",
    "to", "um", "uma", "using", "via", "what", "when", "where", "which", "with",
}


@dataclass
class ChunkRecord:
    chunk_id: int
    doc_id: int
    section_id: int
    source: str
    category: str
    title: str
    breadcrumb: str
    tags: str
    cves: str
    content: str


@dataclass
class _MetaCache:
    mtime: float
    max_id: int
    cat_to_id: dict[str, int]
    src_to_id: dict[str, int]
    cat_arr: np.ndarray  # uint8[max_id + 1]
    src_arr: np.ndarray  # uint8[max_id + 1]
    cat_embed_ids: dict[str, np.ndarray]  # only populated for small categories
    src_embed_ids: dict[str, np.ndarray]  # only populated for small sources


def resolve_data_dir(data_dir: Optional[str | Path] = None) -> Path:
    if data_dir:
        return Path(data_dir).expanduser().resolve()
    env_dir = os.environ.get("SECRAG_DATA_DIR", "").strip()
    if env_dir:
        return Path(env_dir).expanduser().resolve()
    return DEFAULT_DATA_DIR


def sha256_text(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8", errors="ignore")).hexdigest()


def extract_cves(text: str) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for m in CVE_RE.findall(text):
        norm = m.upper()
        if norm not in seen:
            seen.add(norm)
            out.append(norm)
    return out


def extract_mitre_ids(text: str) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for m in MITRE_RE.findall(text):
        if m not in seen:
            seen.add(m)
            out.append(m)
    return out


def extract_fts5_tokens(query: str) -> list[str]:
    """Extract deduplicated technical tokens safe for FTS5 quoting."""
    raw_tokens = TOKEN_RE.findall(query)
    tokens: list[str] = []
    seen: set[str] = set()
    for tok in raw_tokens:
        low = tok.lower()
        if len(tok) < 2 and not tok.isdigit():
            continue
        if low in STOPWORDS and len(raw_tokens) > 2:
            continue
        if low not in seen:
            seen.add(low)
            tokens.append(tok)
    if not tokens:
        tokens = [t for t in raw_tokens if t]
    return tokens[:12]


def _quote_fts_token(tok: str) -> str:
    return '"' + tok.replace('"', '""') + '"'


def build_fts5_queries(query: str) -> tuple[str, str]:
    """Return (strict_and_query, relaxed_or_query) safely quoted for FTS5."""
    tokens = extract_fts5_tokens(query)
    if not tokens:
        return "", ""
    quoted = [_quote_fts_token(t) for t in tokens]
    return " AND ".join(quoted), " OR ".join(quoted)


class KnowledgeStore:
    """Thread-safe SQLite (WAL + FTS5) + USearch HNSW (f16 mmap) storage."""

    def __init__(self, data_dir: Optional[str | Path] = None, read_only: bool = False):
        self.data_dir = resolve_data_dir(data_dir)
        self.data_dir.mkdir(parents=True, exist_ok=True)
        self.db_path = self.data_dir / "kb.sqlite"
        self.vec_path = self.data_dir / "kb.usearch"
        self.read_only = read_only and self.db_path.exists()

        self._tls = threading.local()
        self._conn_lock = threading.Lock()
        self._all_conns: list[sqlite3.Connection] = []
        self._write_conn: Optional[sqlite3.Connection] = None

        if not self.read_only:
            self._write_conn = self._open_connection(read_only=False)
            self._init_schema()

        self._vec_lock = threading.RLock()
        self._vec_index: Optional[Index] = None
        self._vec_mtime: float = 0.0
        self._meta_cache: Optional[_MetaCache] = None
        self._has_vocab: Optional[bool] = None

    def _open_connection(self, read_only: bool) -> sqlite3.Connection:
        uri = f"file:{self.db_path}" + ("?mode=ro" if read_only else "")
        conn = sqlite3.connect(
            uri,
            uri=True,
            timeout=30.0,
            check_same_thread=not read_only,
        )
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA foreign_keys = ON")
        conn.execute("PRAGMA busy_timeout = 15000")
        if not read_only:
            conn.execute("PRAGMA journal_mode = WAL")
            conn.execute("PRAGMA synchronous = NORMAL")
        conn.execute("PRAGMA temp_store = MEMORY")
        conn.execute("PRAGMA cache_size = -65536")
        conn.execute("PRAGMA mmap_size = 268435456")
        with self._conn_lock:
            self._all_conns.append(conn)
        return conn

    @property
    def conn(self) -> sqlite3.Connection:
        if not self.read_only and self._write_conn is not None:
            return self._write_conn
        c = getattr(self._tls, "conn", None)
        if c is None:
            c = self._open_connection(read_only=True)
            self._tls.conn = c
        return c

    def _init_schema(self) -> None:
        self.conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS documents (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                source TEXT NOT NULL,
                category TEXT NOT NULL,
                rel_path TEXT NOT NULL,
                title TEXT NOT NULL,
                url TEXT NOT NULL DEFAULT '',
                tags TEXT NOT NULL DEFAULT '',
                cves TEXT NOT NULL DEFAULT '',
                content_hash TEXT NOT NULL,
                full_text TEXT NOT NULL,
                updated_at INTEGER NOT NULL,
                UNIQUE(source, rel_path)
            );

            CREATE INDEX IF NOT EXISTS idx_documents_source ON documents(source);
            CREATE INDEX IF NOT EXISTS idx_documents_category ON documents(category);

            CREATE TABLE IF NOT EXISTS sections (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
                section_idx INTEGER NOT NULL,
                heading TEXT NOT NULL,
                breadcrumb TEXT NOT NULL,
                content TEXT NOT NULL
            );

            CREATE INDEX IF NOT EXISTS idx_sections_doc ON sections(doc_id, section_idx);

            CREATE TABLE IF NOT EXISTS chunks (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
                section_id INTEGER NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
                source TEXT NOT NULL,
                category TEXT NOT NULL,
                title TEXT NOT NULL,
                breadcrumb TEXT NOT NULL,
                tags TEXT NOT NULL DEFAULT '',
                cves TEXT NOT NULL DEFAULT '',
                content TEXT NOT NULL,
                content_hash TEXT NOT NULL,
                embedded INTEGER NOT NULL DEFAULT 0
            );

            CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(doc_id);
            CREATE INDEX IF NOT EXISTS idx_chunks_source ON chunks(source);
            CREATE INDEX IF NOT EXISTS idx_chunks_category ON chunks(category);
            CREATE INDEX IF NOT EXISTS idx_chunks_embedded ON chunks(embedded) WHERE embedded = 0;

            CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
                title,
                breadcrumb,
                tags,
                cves,
                content,
                content='chunks',
                content_rowid='id',
                tokenize='porter unicode61 tokenchars ''-_.:/'''
            );

            CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts_vocab USING fts5vocab(chunks_fts, row);

            CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
                INSERT INTO chunks_fts(rowid, title, breadcrumb, tags, cves, content)
                VALUES (new.id, new.title, new.breadcrumb, new.tags, new.cves, new.content);
            END;

            CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
                INSERT INTO chunks_fts(chunks_fts, rowid, title, breadcrumb, tags, cves, content)
                VALUES ('delete', old.id, old.title, old.breadcrumb, old.tags, old.cves, old.content);
            END;

            CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
                INSERT INTO chunks_fts(chunks_fts, rowid, title, breadcrumb, tags, cves, content)
                VALUES ('delete', old.id, old.title, old.breadcrumb, old.tags, old.cves, old.content);
                INSERT INTO chunks_fts(rowid, title, breadcrumb, tags, cves, content)
                VALUES (new.id, new.title, new.breadcrumb, new.tags, new.cves, new.content);
            END;

            CREATE TABLE IF NOT EXISTS cve_index (
                cve_id TEXT NOT NULL,
                doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
                source TEXT NOT NULL,
                severity TEXT NOT NULL DEFAULT '',
                is_kev INTEGER NOT NULL DEFAULT 0,
                summary TEXT NOT NULL DEFAULT '',
                PRIMARY KEY (cve_id, doc_id)
            );

            CREATE INDEX IF NOT EXISTS idx_cve_lookup ON cve_index(cve_id);

            CREATE TABLE IF NOT EXISTS deleted_vectors (
                chunk_id INTEGER PRIMARY KEY
            );

            CREATE TABLE IF NOT EXISTS ingestion_state (
                source TEXT PRIMARY KEY,
                phase INTEGER NOT NULL DEFAULT 1,
                status TEXT NOT NULL DEFAULT 'pending',
                doc_count INTEGER NOT NULL DEFAULT 0,
                chunk_count INTEGER NOT NULL DEFAULT 0,
                revision TEXT NOT NULL DEFAULT '',
                updated_at INTEGER NOT NULL DEFAULT 0,
                error TEXT NOT NULL DEFAULT ''
            );
            """
        )
        self.conn.commit()

    def close(self) -> None:
        with self._vec_lock:
            self._vec_index = None
            self._meta_cache = None
        with self._conn_lock:
            for c in self._all_conns:
                try:
                    c.close()
                except Exception:
                    pass
            self._all_conns.clear()
            self._write_conn = None

    # ---- Write / Ingestion API ----

    def document_unchanged(self, source: str, rel_path: str, content_hash: str) -> bool:
        row = self.conn.execute(
            "SELECT content_hash FROM documents WHERE source = ? AND rel_path = ?",
            (source, rel_path),
        ).fetchone()
        return row is not None and row["content_hash"] == content_hash

    def upsert_document(
        self,
        *,
        source: str,
        category: str,
        rel_path: str,
        title: str,
        url: str,
        tags: list[str],
        cves: list[str],
        full_text: str,
        sections_with_chunks: list[tuple[str, str, str, list[str]]],
        cve_entries: Optional[list[tuple[str, str, bool, str]]] = None,
        content_hash: Optional[str] = None,
        embed_chunks: bool = True,
    ) -> tuple[int, list[int], list[int]]:
        """Insert or replace one document, its sections, chunks, and CVE entries.

        sections_with_chunks: list of (heading, breadcrumb, section_content, [chunk_texts])
        cve_entries: optional list of (cve_id, severity, is_kev, summary)
        embed_chunks: when False, marks chunks embedded=2 (lexical-only: searchable via
            FTS5 BM25 and cve_index without diluting the dense HNSW vector index).
        Returns (doc_id, new_chunk_ids, removed_chunk_ids).
        """
        c_hash = content_hash or sha256_text(full_text)
        tags_str = " ".join(dict.fromkeys(t.strip() for t in tags if t and t.strip()))
        cves_str = " ".join(dict.fromkeys(c.upper() for c in cves if c))
        now = int(time.time())

        removed_chunk_ids: list[int] = []
        existing = self.conn.execute(
            "SELECT id, content_hash FROM documents WHERE source = ? AND rel_path = ?",
            (source, rel_path),
        ).fetchone()
        if existing is not None:
            if existing["content_hash"] == c_hash:
                return int(existing["id"]), [], []
            removed_rows = self.conn.execute(
                "SELECT id, embedded FROM chunks WHERE doc_id = ?", (existing["id"],)
            ).fetchall()
            removed_chunk_ids = [int(r["id"]) for r in removed_rows]
            removed_vec_ids = [(int(r["id"]),) for r in removed_rows if int(r["embedded"]) == 1]
            if removed_vec_ids:
                self.conn.executemany(
                    "INSERT OR IGNORE INTO deleted_vectors(chunk_id) VALUES (?)",
                    removed_vec_ids,
                )
            self.conn.execute("DELETE FROM documents WHERE id = ?", (existing["id"],))

        cur = self.conn.execute(
            """
            INSERT INTO documents(source, category, rel_path, title, url, tags, cves, content_hash, full_text, updated_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (source, category, rel_path, title, url, tags_str, cves_str, c_hash, full_text, now),
        )
        doc_id = int(cur.lastrowid)

        new_chunk_ids: list[int] = []
        for idx, (heading, breadcrumb, sec_content, chunk_texts) in enumerate(sections_with_chunks):
            sec_cur = self.conn.execute(
                """
                INSERT INTO sections(doc_id, section_idx, heading, breadcrumb, content)
                VALUES (?, ?, ?, ?, ?)
                """,
                (doc_id, idx, heading, breadcrumb, sec_content),
            )
            section_id = int(sec_cur.lastrowid)
            for ch in chunk_texts:
                ch_clean = ch.strip()
                if not ch_clean:
                    continue
                ch_cves = extract_cves(breadcrumb + "\n" + ch_clean)
                ch_cves_str = " ".join(ch_cves) if ch_cves else cves_str
                embed_flag = 0 if embed_chunks else 2
                ch_cur = self.conn.execute(
                    """
                    INSERT INTO chunks(doc_id, section_id, source, category, title, breadcrumb, tags, cves, content, content_hash, embedded)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        doc_id,
                        section_id,
                        source,
                        category,
                        title,
                        breadcrumb,
                        tags_str,
                        ch_cves_str,
                        ch_clean,
                        sha256_text(ch_clean),
                        embed_flag,
                    ),
                )
                new_chunk_ids.append(int(ch_cur.lastrowid))

        if cve_entries:
            for cve_id, severity, is_kev, summary in cve_entries:
                self.conn.execute(
                    """
                    INSERT OR REPLACE INTO cve_index(cve_id, doc_id, source, severity, is_kev, summary)
                    VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (cve_id.upper(), doc_id, source, severity or "", 1 if is_kev else 0, summary[:500]),
                )
        elif cves:
            summary = title[:240]
            for cve_id in cves[:20]:
                self.conn.execute(
                    """
                    INSERT OR IGNORE INTO cve_index(cve_id, doc_id, source, severity, is_kev, summary)
                    VALUES (?, ?, ?, '', 0, ?)
                    """,
                    (cve_id.upper(), doc_id, source, summary),
                )

        return doc_id, new_chunk_ids, removed_chunk_ids

    def get_deleted_vector_ids(self) -> list[int]:
        try:
            rows = self.conn.execute("SELECT chunk_id FROM deleted_vectors ORDER BY chunk_id ASC").fetchall()
            return [int(r["chunk_id"]) for r in rows]
        except sqlite3.OperationalError:
            return []

    def clear_deleted_vector_ids(self, chunk_ids: list[int]) -> None:
        if not chunk_ids:
            return
        self.conn.executemany(
            "DELETE FROM deleted_vectors WHERE chunk_id = ?", [(cid,) for cid in chunk_ids]
        )
        self.conn.commit()

    def update_ingestion_state(
        self,
        source: str,
        phase: int,
        status: str,
        revision: str = "",
        error: str = "",
    ) -> None:
        doc_count = int(
            self.conn.execute(
                "SELECT COUNT(*) FROM documents WHERE source = ?", (source,)
            ).fetchone()[0]
        )
        chunk_count = int(
            self.conn.execute(
                "SELECT COUNT(*) FROM chunks WHERE source = ?", (source,)
            ).fetchone()[0]
        )
        self.conn.execute(
            """
            INSERT INTO ingestion_state(source, phase, status, doc_count, chunk_count, revision, updated_at, error)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(source) DO UPDATE SET
                phase = excluded.phase,
                status = excluded.status,
                doc_count = excluded.doc_count,
                chunk_count = excluded.chunk_count,
                revision = CASE WHEN excluded.revision != '' THEN excluded.revision ELSE ingestion_state.revision END,
                updated_at = excluded.updated_at,
                error = excluded.error
            """,
            (source, phase, status, doc_count, chunk_count, revision, int(time.time()), error[:500]),
        )
        self.conn.commit()

    def known_cve_ids(self) -> set[str]:
        """Return all CVE IDs currently present in cve_index."""
        return {
            r["cve_id"]
            for r in self.conn.execute("SELECT DISTINCT cve_id FROM cve_index").fetchall()
        }

    def iter_unembedded_chunks(self, batch_size: int = 256) -> Iterable[list[tuple[int, str]]]:
        """Yield batches of (chunk_id, text_for_embedding) where embedded = 0."""
        last_id = 0
        while True:
            rows = self.conn.execute(
                """
                SELECT id, breadcrumb, tags, cves, content
                FROM chunks
                WHERE embedded = 0 AND id > ?
                ORDER BY id ASC
                LIMIT ?
                """,
                (last_id, batch_size),
            ).fetchall()
            if not rows:
                break
            batch: list[tuple[int, str]] = []
            for r in rows:
                last_id = int(r["id"])
                header = r["breadcrumb"]
                if r["cves"]:
                    header = f"{header} [{r['cves']}]"
                text = f"{header}\n{r['content']}"
                batch.append((last_id, text))
            yield batch

    def mark_chunks_embedded(self, chunk_ids: list[int]) -> None:
        if not chunk_ids:
            return
        self.conn.executemany(
            "UPDATE chunks SET embedded = 1 WHERE id = ?", [(cid,) for cid in chunk_ids]
        )
        self.conn.commit()

    # ---- Vector Index (USearch HNSW f16) & Lightweight Filter Cache ----

    def open_writable_vector_index(self) -> Index:
        idx = Index(
            ndim=EMBED_DIM,
            metric="cos",
            dtype="f16",
            connectivity=16,
            expansion_add=128,
            expansion_search=64,
        )
        if self.vec_path.exists() and self.vec_path.stat().st_size > 0:
            idx.load(str(self.vec_path))
        return idx

    def save_vector_index_atomic(self, idx: Index) -> None:
        tmp_path = self.vec_path.with_suffix(".usearch.tmp")
        idx.save(str(tmp_path))
        os.replace(tmp_path, self.vec_path)
        with self._vec_lock:
            self._vec_index = None
            self._vec_mtime = 0.0
            self._meta_cache = None

    def get_readonly_vector_index(self) -> Optional[Index]:
        if not self.vec_path.exists() or self.vec_path.stat().st_size == 0:
            return None
        mtime = self.vec_path.stat().st_mtime
        with self._vec_lock:
            if self._vec_index is not None and self._vec_mtime == mtime:
                return self._vec_index
            idx = Index(
                ndim=EMBED_DIM,
                metric="cos",
                dtype="f16",
                connectivity=16,
                expansion_add=128,
                expansion_search=64,
            )
            idx.view(str(self.vec_path))
            self._vec_index = idx
            self._vec_mtime = mtime
            self._meta_cache = None
            return idx

    def _get_meta_cache(self, mtime: float) -> _MetaCache:
        with self._vec_lock:
            if self._meta_cache is not None and self._meta_cache.mtime == mtime:
                return self._meta_cache

            rows = self.conn.execute("SELECT id, category, source, embedded FROM chunks").fetchall()
            if not rows:
                cache = _MetaCache(mtime, 0, {}, {}, np.zeros(1, dtype=np.uint8), np.zeros(1, dtype=np.uint8), {}, {})
                self._meta_cache = cache
                return cache

            max_id = max(int(r["id"]) for r in rows)
            cat_to_id: dict[str, int] = {}
            src_to_id: dict[str, int] = {}
            cat_arr = np.zeros(max_id + 1, dtype=np.uint8)
            src_arr = np.zeros(max_id + 1, dtype=np.uint8)
            cat_lists: dict[str, list[int]] = {}
            src_lists: dict[str, list[int]] = {}

            for r in rows:
                cid = int(r["id"])
                cat = r["category"]
                src = r["source"]
                cnum = cat_to_id.get(cat)
                if cnum is None:
                    cnum = len(cat_to_id) + 1
                    cat_to_id[cat] = cnum
                snum = src_to_id.get(src)
                if snum is None:
                    snum = len(src_to_id) + 1
                    src_to_id[src] = snum
                cat_arr[cid] = cnum
                src_arr[cid] = snum
                if int(r["embedded"]) == 1:
                    cat_lists.setdefault(cat, []).append(cid)
                    src_lists.setdefault(src, []).append(cid)

            cat_embed_ids = {
                k: np.asarray(v, dtype=np.uint64)
                for k, v in cat_lists.items()
                if len(v) <= EXACT_SUBSET_THRESHOLD
            }
            src_embed_ids = {
                k: np.asarray(v, dtype=np.uint64)
                for k, v in src_lists.items()
                if len(v) <= EXACT_SUBSET_THRESHOLD
            }
            cache = _MetaCache(
                mtime=mtime,
                max_id=max_id,
                cat_to_id=cat_to_id,
                src_to_id=src_to_id,
                cat_arr=cat_arr,
                src_arr=src_arr,
                cat_embed_ids=cat_embed_ids,
                src_embed_ids=src_embed_ids,
            )
            self._meta_cache = cache
            return cache

    # ---- Read / Search API ----

    def _token_doc_freq(self, token: str) -> int:
        """Estimate FTS5 document frequency via chunks_fts_vocab in <0.1 ms."""
        if self._has_vocab is False:
            return 1000
        low = token.lower()
        try:
            row = self.conn.execute(
                "SELECT doc FROM chunks_fts_vocab WHERE term = ?", (low,)
            ).fetchone()
            if row is not None:
                self._has_vocab = True
                return int(row["doc"])
            # Porter stem fallback (e.g. 'authorization' -> 'author')
            stem_prefix = low[: max(3, len(low) - 4)]
            row = self.conn.execute(
                "SELECT MAX(doc) AS d FROM chunks_fts_vocab WHERE term >= ? AND term <= ?",
                (stem_prefix, low),
            ).fetchone()
            self._has_vocab = True
            if row is not None and row["d"] is not None:
                return int(row["d"])
            return 0
        except sqlite3.OperationalError:
            self._has_vocab = False
            return 1000

    def fts_search(
        self,
        query: str,
        *,
        category: Optional[str] = None,
        source: Optional[str] = None,
        limit: int = 40,
    ) -> list[tuple[int, float]]:
        tokens = extract_fts5_tokens(query)
        if not tokens:
            return []

        where_extra = []
        params_extra: list[object] = []
        if category:
            where_extra.append("c.category = ?")
            params_extra.append(category.lower())
        if source:
            where_extra.append("c.source = ?")
            params_extra.append(source.lower())
        extra_sql = (" AND " + " AND ".join(where_extra)) if where_extra else ""

        sql = f"""
            SELECT c.id, bm25(chunks_fts, 4.0, 3.0, 2.0, 6.0, 1.0) AS score
            FROM chunks_fts
            JOIN chunks c ON c.id = chunks_fts.rowid
            WHERE chunks_fts MATCH ?{extra_sql}
            ORDER BY score ASC
            LIMIT ?
        """

        results: list[tuple[int, float]] = []
        seen: set[int] = set()

        def _run_and_merge(fts_expr: str) -> None:
            if not fts_expr or len(results) >= limit:
                return
            rows = self.conn.execute(sql, [fts_expr, *params_extra, limit]).fetchall()
            for r in rows:
                cid = int(r["id"])
                if cid not in seen:
                    seen.add(cid)
                    results.append((cid, float(r["score"])))
                    if len(results) >= limit:
                        break

        # 1. Exact full AND (always fast)
        _run_and_merge(" AND ".join(_quote_fts_token(t) for t in tokens))
        if len(results) >= min(limit, 12) or len(tokens) == 1:
            return results

        # 2. IDF-aware progressive relaxation: rank tokens rarest-first so common words
        #    ('object', 'authorization', 'level', 'file') never trigger a 3s full-table OR scan.
        scored_tokens = sorted(
            ((self._token_doc_freq(t), -len(t), t) for t in tokens),
            key=lambda x: (x[0] if x[0] > 0 else 1_000_000_000, x[1]),
        )
        present_tokens = [(df, t) for df, _, t in scored_tokens if df > 0]
        if not present_tokens:
            present_tokens = [(df, t) for df, _, t in scored_tokens]

        if len(present_tokens) >= 3:
            _run_and_merge(" AND ".join(_quote_fts_token(t) for _, t in present_tokens[:3]))
        if len(results) < min(limit, 12) and len(present_tokens) >= 2:
            _run_and_merge(" AND ".join(_quote_fts_token(t) for _, t in present_tokens[:2]))
        if len(results) < min(limit, 12):
            selective = [t for df, t in present_tokens if df <= FTS_SELECTIVE_DF_CAP][:3]
            if not selective:
                selective = [t for _, t in present_tokens[:2]]
            _run_and_merge(" OR ".join(_quote_fts_token(t) for t in selective))

        return results

    def vector_search(
        self,
        query_vec: np.ndarray,
        *,
        category: Optional[str] = None,
        source: Optional[str] = None,
        limit: int = 40,
    ) -> list[tuple[int, float]]:
        idx = self.get_readonly_vector_index()
        if idx is None or len(idx) == 0:
            return []

        cat = category.lower() if category else None
        src = source.lower() if source else None
        q = query_vec.astype(np.float32)

        with self._vec_lock:
            mtime = self._vec_mtime

        if cat or src:
            meta = self._get_meta_cache(mtime)
            # Small-subset fast path: exact SIMD dot product -> 100% recall in ~15-40 ms!
            subset: Optional[np.ndarray] = None
            if cat and not src and cat in meta.cat_embed_ids:
                subset = meta.cat_embed_ids[cat]
            elif src and not cat and src in meta.src_embed_ids:
                subset = meta.src_embed_ids[src]
            elif cat and src:
                base = meta.cat_embed_ids.get(cat)
                if base is None:
                    base = meta.src_embed_ids.get(src)
                if base is not None and len(base) > 0:
                    want_c = meta.cat_to_id.get(cat, 0)
                    want_s = meta.src_to_id.get(src, 0)
                    b_int = base.astype(np.int64)
                    mask = (b_int <= meta.max_id) & (meta.cat_arr[b_int] == want_c) & (meta.src_arr[b_int] == want_s)
                    subset = base[mask]

            if subset is not None:
                if len(subset) == 0:
                    return []
                vecs = np.asarray(idx.get(subset), dtype=np.float32)
                if vecs.ndim == 1:
                    vecs = vecs.reshape(1, -1)
                # Guard against missing/removed keys returning NaN or zero vectors
                valid = np.isfinite(vecs).all(axis=1) & (np.abs(vecs).sum(axis=1) > 0)
                if not np.any(valid):
                    return []
                subset = subset[valid]
                vecs = vecs[valid]
                sims = vecs @ q
                k = min(limit, len(sims))
                if k == len(sims):
                    order = np.argsort(sims)[::-1]
                else:
                    part = np.argpartition(sims, -k)[-k:]
                    order = part[np.argsort(sims[part])[::-1]]
                return [(int(subset[i]), float(1.0 - sims[i])) for i in order]

            # Large category/source: global HNSW + O(1) uint8 array filtering (zero SQLite round-trip)
            want_c = meta.cat_to_id.get(cat, -1) if cat else 0
            want_s = meta.src_to_id.get(src, -1) if src else 0
            if (cat and want_c < 0) or (src and want_s < 0):
                return []
            fetch_k = min(len(idx), max(limit * 6, 300))
            matches = idx.search(q, fetch_k)
            keys = np.asarray(matches.keys, dtype=np.int64)
            dists = np.asarray(matches.distances, dtype=np.float32)
            in_bounds = (keys >= 0) & (keys <= meta.max_id)
            safe_keys = np.where(in_bounds, keys, 0)
            mask = in_bounds
            if cat:
                mask = mask & (meta.cat_arr[safe_keys] == want_c)
            if src:
                mask = mask & (meta.src_arr[safe_keys] == want_s)
            filtered_keys = keys[mask][:limit]
            filtered_dists = dists[mask][:limit]
            return [(int(k), float(d)) for k, d in zip(filtered_keys, filtered_dists)]

        fetch_k = min(limit, len(idx))
        matches = idx.search(q, fetch_k)
        return [(int(m.key), float(m.distance)) for m in matches]

    def get_chunks_by_ids(self, chunk_ids: list[int]) -> dict[int, ChunkRecord]:
        if not chunk_ids:
            return {}
        placeholders = ",".join("?" for _ in chunk_ids)
        rows = self.conn.execute(
            f"""
            SELECT id, doc_id, section_id, source, category, title, breadcrumb, tags, cves, content
            FROM chunks
            WHERE id IN ({placeholders})
            """,
            chunk_ids,
        ).fetchall()
        return {
            int(r["id"]): ChunkRecord(
                chunk_id=int(r["id"]),
                doc_id=int(r["doc_id"]),
                section_id=int(r["section_id"]),
                source=r["source"],
                category=r["category"],
                title=r["title"],
                breadcrumb=r["breadcrumb"],
                tags=r["tags"],
                cves=r["cves"],
                content=r["content"],
            )
            for r in rows
        }

    def read_section(
        self, doc_id: int, section_id: Optional[int] = None, max_chars: int = 12000
    ) -> Optional[dict]:
        doc = self.conn.execute(
            "SELECT id, source, category, rel_path, title, url, tags, cves, full_text FROM documents WHERE id = ?",
            (doc_id,),
        ).fetchone()
        if doc is None:
            return None

        all_sections = self.conn.execute(
            "SELECT id, section_idx, heading, breadcrumb, length(content) AS chars FROM sections WHERE doc_id = ? ORDER BY section_idx ASC",
            (doc_id,),
        ).fetchall()

        outline = [
            {
                "section_id": int(s["id"]),
                "heading": s["heading"],
                "breadcrumb": s["breadcrumb"],
                "chars": int(s["chars"]),
            }
            for s in all_sections
        ]

        if section_id is not None:
            sec = self.conn.execute(
                "SELECT id, heading, breadcrumb, content FROM sections WHERE id = ? AND doc_id = ?",
                (section_id, doc_id),
            ).fetchone()
            if sec is None:
                return None
            content = sec["content"]
            truncated = len(content) > max_chars
            return {
                "doc_id": int(doc["id"]),
                "section_id": int(sec["id"]),
                "source": doc["source"],
                "category": doc["category"],
                "title": doc["title"],
                "breadcrumb": sec["breadcrumb"],
                "path": doc["rel_path"],
                "url": doc["url"],
                "cves": doc["cves"],
                "content": content[:max_chars],
                "truncated": truncated,
                "outline": outline[:25],
            }

        full_text = doc["full_text"]
        truncated = len(full_text) > max_chars
        return {
            "doc_id": int(doc["id"]),
            "source": doc["source"],
            "category": doc["category"],
            "title": doc["title"],
            "path": doc["rel_path"],
            "url": doc["url"],
            "cves": doc["cves"],
            "content": full_text[:max_chars],
            "truncated": truncated,
            "outline": outline[:30],
        }

    def lookup_cve(self, cve_id: str, limit: int = 15) -> dict:
        norm = cve_id.strip().upper()
        if not norm.startswith("CVE-"):
            norm = "CVE-" + norm

        rows = self.conn.execute(
            """
            SELECT ci.cve_id, ci.doc_id, ci.source, ci.severity, ci.is_kev, ci.summary,
                   d.category, d.title, d.rel_path, d.url
            FROM cve_index ci
            JOIN documents d ON d.id = ci.doc_id
            WHERE ci.cve_id = ?
            ORDER BY ci.is_kev DESC,
                     CASE ci.source
                         WHEN 'cisa-kev' THEN 1
                         WHEN 'nuclei' THEN 2
                         WHEN 'exploitdb' THEN 3
                         WHEN 'poc-in-github' THEN 4
                         WHEN 'cvelist-v5' THEN 5
                         WHEN 'hacktricks' THEN 6
                         ELSE 7
                     END ASC
            LIMIT ?
            """,
            (norm, limit),
        ).fetchall()

        # Batch-load the first section of all matched documents in 1 query (no N+1)
        doc_ids = [int(r["doc_id"]) for r in rows]
        first_sections: dict[int, sqlite3.Row] = {}
        if doc_ids:
            placeholders = ",".join("?" for _ in doc_ids)
            for s in self.conn.execute(
                f"SELECT doc_id, id, breadcrumb, content FROM sections WHERE doc_id IN ({placeholders}) AND section_idx = 0",
                doc_ids,
            ).fetchall():
                first_sections[int(s["doc_id"])] = s

        matches = []
        seen_docs: set[int] = set()
        for r in rows:
            did = int(r["doc_id"])
            seen_docs.add(did)
            sec = first_sections.get(did)
            matches.append(
                {
                    "doc_id": did,
                    "section_id": int(sec["id"]) if sec else None,
                    "source": r["source"],
                    "category": r["category"],
                    "title": r["title"],
                    "severity": r["severity"],
                    "is_kev": bool(r["is_kev"]),
                    "summary": r["summary"],
                    "url": r["url"],
                    "excerpt": (sec["content"][:900] if sec else ""),
                }
            )

        # Also check FTS5 mentions not yet in cve_index
        if len(matches) < limit:
            fts_hits = self.fts_search(norm, limit=limit)
            cids = [cid for cid, _ in fts_hits]
            chunks = self.get_chunks_by_ids(cids)
            for cid in cids:
                ch = chunks.get(cid)
                if not ch or ch.doc_id in seen_docs:
                    continue
                seen_docs.add(ch.doc_id)
                matches.append(
                    {
                        "doc_id": ch.doc_id,
                        "section_id": ch.section_id,
                        "source": ch.source,
                        "category": ch.category,
                        "title": ch.title,
                        "severity": "",
                        "is_kev": False,
                        "summary": ch.breadcrumb,
                        "url": "",
                        "excerpt": ch.content[:900],
                    }
                )
                if len(matches) >= limit:
                    break

        return {
            "cve_id": norm,
            "is_kev": any(m["is_kev"] for m in matches),
            "match_count": len(matches),
            "matches": matches,
        }

    def stats(self) -> dict:
        total_docs = int(self.conn.execute("SELECT COUNT(*) FROM documents").fetchone()[0])
        total_sections = int(self.conn.execute("SELECT COUNT(*) FROM sections").fetchone()[0])
        total_chunks = int(self.conn.execute("SELECT COUNT(*) FROM chunks").fetchone()[0])
        embedded_chunks = int(
            self.conn.execute("SELECT COUNT(*) FROM chunks WHERE embedded = 1").fetchone()[0]
        )
        lexical_only_chunks = int(
            self.conn.execute("SELECT COUNT(*) FROM chunks WHERE embedded = 2").fetchone()[0]
        )
        pending_embed_chunks = int(
            self.conn.execute("SELECT COUNT(*) FROM chunks WHERE embedded = 0").fetchone()[0]
        )
        total_cves = int(
            self.conn.execute("SELECT COUNT(DISTINCT cve_id) FROM cve_index").fetchone()[0]
        )
        categories = [
            {"category": r["category"], "documents": int(r["cnt"])}
            for r in self.conn.execute(
                "SELECT category, COUNT(*) AS cnt FROM documents GROUP BY category ORDER BY cnt DESC"
            ).fetchall()
        ]
        sources = [
            {
                "source": r["source"],
                "phase": int(r["phase"]),
                "status": r["status"],
                "documents": int(r["doc_count"]),
                "chunks": int(r["chunk_count"]),
                "revision": r["revision"],
                "updated_at": int(r["updated_at"]),
                "error": r["error"],
            }
            for r in self.conn.execute(
                "SELECT * FROM ingestion_state ORDER BY phase ASC, source ASC"
            ).fetchall()
        ]
        vec_idx = self.get_readonly_vector_index()
        vec_count = len(vec_idx) if vec_idx is not None else 0
        db_mb = round(self.db_path.stat().st_size / (1024 * 1024), 1) if self.db_path.exists() else 0.0
        vec_mb = round(self.vec_path.stat().st_size / (1024 * 1024), 1) if self.vec_path.exists() else 0.0

        return {
            "data_dir": str(self.data_dir),
            "documents": total_docs,
            "sections": total_sections,
            "chunks": total_chunks,
            "embedded_chunks": embedded_chunks,
            "lexical_only_chunks": lexical_only_chunks,
            "pending_embed_chunks": pending_embed_chunks,
            "vector_index_entries": vec_count,
            "unique_cves": total_cves,
            "sqlite_mb": db_mb,
            "usearch_mb": vec_mb,
            "categories": categories,
            "sources": sources,
        }
