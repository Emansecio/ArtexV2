"""SQLite (WAL + FTS5 BM25 + CVE index) and USearch HNSW (f16 mmap) storage layer."""

from __future__ import annotations

import hashlib
import os
import re
import sqlite3
import threading
import time
import weakref
from collections import OrderedDict
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Optional

import numpy as np
from usearch.index import Index

from . import EMBED_DIM

DEFAULT_DATA_DIR = Path(__file__).resolve().parents[3] / "data" / "secrag"

CVE_RE = re.compile(r"\bCVE-\d{4}-\d{4,7}\b", re.IGNORECASE)
CVE_PARSE_RE = re.compile(r"(?:CVE[-_\s]+)?(\d{4})[-_\s]+(\d{4,7})", re.IGNORECASE)
MITRE_RE = re.compile(r"\bT\d{4}(?:\.\d{3})?\b")
TOKEN_RE = re.compile(r"[A-Za-z0-9_\-][A-Za-z0-9_\-./:+]*")

# Subsets with <= this many embedded chunks use exact SIMD matmul (100% recall in ~15-60 ms)
# instead of approximate global HNSW over-fetching. 55,000 covers 'writeups' (~49k),
# 'cloud' (~28k), and 'web' (~28k) — the three core pentest & bug-bounty domains.
EXACT_SUBSET_THRESHOLD = 55_000
FTS_SELECTIVE_DF_CAP = 25_000
DEFAULT_FTS_TOKENIZE = "porter unicode61 tokenchars '-_'"
_TOKENIZE_CLAUSE_RE = re.compile(r"tokenize\s*=\s*'((?:''|[^'])*)'", re.IGNORECASE)
_STEM_CACHE_MAX = 4096

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


CacheSignature = tuple[int, int, int, int, int, int]


@dataclass
class _MetaCache:
    sig: CacheSignature
    max_id: int
    cat_to_id: dict[str, int]
    src_to_id: dict[str, int]
    cat_arr: np.ndarray  # uint8[max_id + 1]
    src_arr: np.ndarray  # uint8[max_id + 1]
    cat_embed_ids: dict[str, np.ndarray]  # only populated for small categories
    src_embed_ids: dict[str, np.ndarray]  # only populated for small sources


def _reap_ro_conn(
    conn: sqlite3.Connection,
    store_ref: weakref.ReferenceType["KnowledgeStore"],
) -> None:
    store = store_ref()
    if store is not None:
        with store._conn_lock:
            store._ro_conns.discard(conn)
    try:
        conn.close()
    except Exception:
        pass


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
    raw_tokens = [t.strip(".:/") for t in TOKEN_RE.findall(query)]
    raw_tokens = [t for t in raw_tokens if t and any(ch.isalnum() for ch in t)]
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


# High-signal offensive security acronym/synonym pairs used for Stage 1a BM25
# recall expansion when exact AND yields too few matches.
SECURITY_SYNONYMS: dict[str, tuple[str, ...]] = {
    "bola": ("idor", "object", "authorization"),
    "idor": ("bola", "authorization"),
    "ato": ("account", "takeover"),
    "wcd": ("cache", "deception"),
    "wcp": ("cache", "poisoning"),
    "ssti": ("template", "injection"),
    "ssrf": ("request", "forgery", "metadata"),
    "xxe": ("xml", "external", "entity"),
    "sqli": ("sql", "injection"),
    "rce": ("remote", "code", "execution"),
    "lfi": ("path", "traversal", "inclusion"),
    "csrf": ("request", "forgery"),
    "cswsh": ("websocket", "hijacking"),
    "toctou": ("race", "condition"),
    "desync": ("request", "smuggling"),
    "adcs": ("certificate", "esc1", "esc8"),
    "imds": ("metadata", "169.254.169.254"),
    "imdsv2": ("metadata", "hop-limit"),
    "jwt": ("json", "web", "token"),
    "k8s": ("kubernetes", "rbac"),
    "privesc": ("privilege", "escalation"),
    "oauth": ("redirect_uri", "authorization"),
    "saml": ("assertion", "signature"),
}


def expand_fts5_synonyms(tokens: list[str], max_extra: int = 4) -> list[str]:
    """Return supplementary canonical security terms for BM25 recall expansion."""
    seen = {t.lower() for t in tokens}
    extra: list[str] = []
    for tok in tokens:
        for syn in SECURITY_SYNONYMS.get(tok.lower(), ()):
            if syn not in seen:
                seen.add(syn)
                extra.append(syn)
                if len(extra) >= max_extra:
                    return extra
    return extra
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
        self._ro_conns: set[sqlite3.Connection] = set()
        self._write_conn: Optional[sqlite3.Connection] = None

        if not self.read_only:
            self._write_conn = self._open_connection(read_only=False)
            self._init_schema()

        self._vec_lock = threading.RLock()
        self._vec_index: Optional[Index] = None
        self._vec_mtime: float = 0.0
        self._vec_key: tuple[int, int, int] = (0, 0, 0)
        self._meta_cache: Optional[_MetaCache] = None
        self._stats_cache_key: Optional[CacheSignature] = None
        self._stats_cache: Optional[dict] = None
        self._has_vocab: Optional[bool] = None
        self._vocab_missing_sig: Optional[CacheSignature] = None
        self._stem_lock = threading.Lock()
        self._stem_conn: Optional[sqlite3.Connection] = None
        self._stem_conn_spec: Optional[str] = None
        self._stem_spec: Optional[str] = None
        self._stem_spec_sig: Optional[CacheSignature] = None
        self._stem_cache: OrderedDict[tuple[str, str], tuple[str, ...]] = OrderedDict()

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
        if read_only:
            with self._conn_lock:
                self._ro_conns.add(conn)
            try:
                weakref.finalize(threading.current_thread(), _reap_ro_conn, conn, weakref.ref(self))
            except TypeError:
                pass
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
            CREATE INDEX IF NOT EXISTS idx_chunks_meta_embedded ON chunks(category, source, id) WHERE embedded = 1;

            CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
                title,
                breadcrumb,
                tags,
                cves,
                content,
                content='chunks',
                content_rowid='id',
                tokenize='porter unicode61 tokenchars ''-_'''
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

            CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE OF title, breadcrumb, tags, cves, content ON chunks BEGIN
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
        # Migrate existing DBs whose chunks_au trigger fires on non-text column updates (e.g. embedded=1).
        au_row = self.conn.execute(
            "SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'chunks_au'"
        ).fetchone()
        if au_row is not None and "OF title" not in (au_row["sql"] or ""):
            self.conn.commit()
            self.conn.execute("BEGIN IMMEDIATE")
            try:
                self.conn.execute("DROP TRIGGER IF EXISTS chunks_au")
                self.conn.execute(
                    """
                    CREATE TRIGGER chunks_au AFTER UPDATE OF title, breadcrumb, tags, cves, content ON chunks BEGIN
                        INSERT INTO chunks_fts(chunks_fts, rowid, title, breadcrumb, tags, cves, content)
                        VALUES ('delete', old.id, old.title, old.breadcrumb, old.tags, old.cves, old.content);
                        INSERT INTO chunks_fts(rowid, title, breadcrumb, tags, cves, content)
                        VALUES (new.id, new.title, new.breadcrumb, new.tags, new.cves, new.content);
                    END
                    """
                )
                self.conn.commit()
            except BaseException:
                if self.conn.in_transaction:
                    self.conn.rollback()
                raise
        # Migrate existing DBs using the legacy punctuation-gluing FTS5 tokenizer ('-_.:/').
        fts_row = self.conn.execute(
            "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'chunks_fts'"
        ).fetchone()
        if fts_row is not None and "-_.:/" in (fts_row["sql"] or ""):
            self.conn.commit()
            self.conn.execute("BEGIN IMMEDIATE")
            try:
                self.conn.execute("DROP TABLE IF EXISTS chunks_fts_vocab")
                self.conn.execute("DROP TABLE IF EXISTS chunks_fts")
                self.conn.execute(
                    """
                    CREATE VIRTUAL TABLE chunks_fts USING fts5(
                        title,
                        breadcrumb,
                        tags,
                        cves,
                        content,
                        content='chunks',
                        content_rowid='id',
                        tokenize='porter unicode61 tokenchars ''-_'''
                    )
                    """
                )
                self.conn.execute(
                    "CREATE VIRTUAL TABLE chunks_fts_vocab USING fts5vocab(chunks_fts, row)"
                )
                self.conn.execute("INSERT INTO chunks_fts(chunks_fts) VALUES('rebuild')")
                self.conn.commit()
            except BaseException:
                if self.conn.in_transaction:
                    self.conn.rollback()
                raise
        self.conn.commit()

    def close(self) -> None:
        with self._vec_lock:
            self._vec_index = None
            self._vec_mtime = 0.0
            self._vec_key = (0, 0, 0)
            self._meta_cache = None
            self._stats_cache = None
            self._stats_cache_key = None
        with self._stem_lock:
            stem_conn = self._stem_conn
            self._stem_conn = None
            self._stem_conn_spec = None
            self._stem_spec = None
            self._stem_spec_sig = None
            self._stem_cache.clear()
        if stem_conn is not None:
            try:
                stem_conn.close()
            except Exception:
                pass
        with self._conn_lock:
            conns = list(self._ro_conns)
            self._ro_conns.clear()
            write_conn = self._write_conn
            self._write_conn = None
        if write_conn is not None:
            conns.append(write_conn)
        for c in conns:
            try:
                c.close()
            except Exception:
                pass

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
        if existing is not None and existing["content_hash"] == c_hash:
            return int(existing["id"]), [], []

        if not self.conn.in_transaction:
            self.conn.execute("BEGIN")
        self.conn.execute("SAVEPOINT upsert_document")
        try:
            if existing is not None:
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

            single_section = len(sections_with_chunks) == 1
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
                    if ch_cves:
                        ch_cves_str = " ".join(ch_cves)
                    elif single_section:
                        ch_cves_str = cves_str
                    else:
                        ch_cves_str = ""
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

            self.conn.execute("RELEASE SAVEPOINT upsert_document")
        except Exception:
            try:
                self.conn.execute("ROLLBACK TO SAVEPOINT upsert_document")
                self.conn.execute("RELEASE SAVEPOINT upsert_document")
            except sqlite3.Error:
                pass
            raise

        with self._vec_lock:
            self._meta_cache = None
            self._stats_cache = None
            self._stats_cache_key = None
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

    def known_cve_ids(self, exclude_source: Optional[str] = None) -> set[str]:
        """Return all CVE IDs currently present in cve_index, optionally excluding one source."""
        if exclude_source:
            rows = self.conn.execute(
                "SELECT DISTINCT cve_id FROM cve_index WHERE source != ?",
                (exclude_source,),
            ).fetchall()
        else:
            rows = self.conn.execute("SELECT DISTINCT cve_id FROM cve_index").fetchall()
        return {r["cve_id"] for r in rows}

    def iter_unembedded_chunks(self, batch_size: int = 256) -> Iterable[list[tuple[int, str]]]:
        """Yield batches of (chunk_id, text_for_embedding) where embedded = 0."""
        last_id = 0
        while True:
            rows = self.conn.execute(
                """
                SELECT id, source, category, breadcrumb, tags, cves, content
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
                header = f"[{r['category']}|{r['source']}] {r['breadcrumb']}"
                if r["tags"]:
                    tag_tokens = r["tags"].split()[:6]
                    if tag_tokens:
                        header = f"{header} ({', '.join(tag_tokens)})"
                if r["cves"]:
                    cve_tokens = r["cves"].split()[:8]
                    if cve_tokens:
                        header = f"{header} [{' '.join(cve_tokens)}]"
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
        with self._vec_lock:
            self._meta_cache = None
            self._stats_cache = None
            self._stats_cache_key = None

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
            self._vec_key = (0, 0, 0)
            self._meta_cache = None

    def get_readonly_vector_index_with_sig(self) -> tuple[Optional[Index], CacheSignature]:
        with self._vec_lock:
            sig = self.cache_signature()
            try:
                vst = self.vec_path.stat()
            except OSError:
                return None, sig
            if vst.st_size == 0:
                return None, sig
            vec_key = (vst.st_mtime_ns, vst.st_size, vst.st_ino)
            if self._vec_index is not None and self._vec_key == vec_key:
                return self._vec_index, sig
            idx = Index(
                ndim=EMBED_DIM,
                metric="cos",
                dtype="f16",
                connectivity=16,
                expansion_add=128,
                expansion_search=64,
            )
            try:
                idx.view(str(self.vec_path))
            except (OSError, RuntimeError):
                return None, sig
            self._vec_index = idx
            self._vec_mtime = vst.st_mtime
            self._vec_key = vec_key
            self._meta_cache = None
            return idx, self.cache_signature()

    def get_readonly_vector_index(self) -> Optional[Index]:
        idx, _ = self.get_readonly_vector_index_with_sig()
        return idx

    def cache_signature(self) -> CacheSignature:
        """Lightweight (db_mtime_ns, db_size, wal_mtime_ns, wal_size, vec_mtime_ns, vec_ino)
        signature for read caches. Including the WAL file and vector inode ensures caches
        invalidate immediately during concurrent secrag-ingest before a WAL checkpoint occurs."""
        try:
            st = self.db_path.stat()
            db_m, db_s = st.st_mtime_ns, st.st_size
        except OSError:
            db_m, db_s = 0, 0
        wal_path = Path(str(self.db_path) + "-wal")
        try:
            wst = wal_path.stat()
            wal_m, wal_s = wst.st_mtime_ns, wst.st_size
        except OSError:
            wal_m, wal_s = 0, 0
        try:
            vst = self.vec_path.stat()
            vec_m, vec_ino = vst.st_mtime_ns, vst.st_ino
        except OSError:
            vec_m, vec_ino = 0, 0
        return (db_m, db_s, wal_m, wal_s, vec_m, vec_ino)

    def _get_meta_cache(self, sig: Optional[CacheSignature] = None) -> _MetaCache:
        if sig is None:
            sig = self.cache_signature()
        with self._vec_lock:
            if self._meta_cache is not None and self._meta_cache.sig == sig:
                return self._meta_cache

            # Only embedded=1 chunks exist in the USearch HNSW index; skipping
            # lexical-only (embedded=2) rows and sqlite3.Row wrappers cuts cold
            # load time and memory dramatically.
            cur = self.conn.cursor()
            cur.row_factory = None
            rows = cur.execute("SELECT id, category, source FROM chunks WHERE embedded = 1").fetchall()
            if not rows:
                cache = _MetaCache(sig, 0, {}, {}, np.zeros(1, dtype=np.uint8), np.zeros(1, dtype=np.uint8), {}, {})
                self._meta_cache = cache
                return cache

            max_id = max(int(r[0]) for r in rows)
            cat_to_id: dict[str, int] = {}
            src_to_id: dict[str, int] = {}
            cat_arr = np.zeros(max_id + 1, dtype=np.uint8)
            src_arr = np.zeros(max_id + 1, dtype=np.uint8)
            cat_lists: dict[str, list[int]] = {}
            src_lists: dict[str, list[int]] = {}

            for cid_raw, cat, src in rows:
                cid = int(cid_raw)
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
                sig=sig,
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

    def _get_fts_tokenize_spec(self) -> str:
        sig = self.cache_signature()
        with self._stem_lock:
            if self._stem_spec is not None and self._stem_spec_sig == sig:
                return self._stem_spec
        spec = DEFAULT_FTS_TOKENIZE
        try:
            row = self.conn.execute(
                "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'chunks_fts'"
            ).fetchone()
            if row is not None and row["sql"]:
                m = _TOKENIZE_CLAUSE_RE.search(row["sql"])
                if m:
                    spec = m.group(1).replace("''", "'")
        except sqlite3.OperationalError:
            pass
        with self._stem_lock:
            self._stem_spec = spec
            self._stem_spec_sig = sig
        return spec

    def _fts5_stems(self, text: str) -> tuple[str, ...]:
        """Tokenize and Porter-stem text using SQLite FTS5 with chunks_fts's exact tokenizer."""
        spec = self._get_fts_tokenize_spec()
        cache_key = (spec, text)
        with self._stem_lock:
            cached = self._stem_cache.get(cache_key)
            if cached is not None:
                self._stem_cache.move_to_end(cache_key)
                return cached

            if self._stem_conn is None or self._stem_conn_spec != spec:
                if self._stem_conn is not None:
                    try:
                        self._stem_conn.close()
                    except Exception:
                        pass
                sconn = sqlite3.connect(":memory:", check_same_thread=False)
                escaped = spec.replace("'", "''")
                sconn.execute(f"CREATE VIRTUAL TABLE stem_fts USING fts5(x, tokenize='{escaped}')")
                sconn.execute("CREATE VIRTUAL TABLE stem_vocab USING fts5vocab(stem_fts, row)")
                self._stem_conn = sconn
                self._stem_conn_spec = spec

            self._stem_conn.execute("DELETE FROM stem_fts")
            self._stem_conn.execute("INSERT INTO stem_fts(rowid, x) VALUES (1, ?)", (text,))
            stems = tuple(
                str(r[0])
                for r in self._stem_conn.execute("SELECT term FROM stem_vocab").fetchall()
            )
            self._stem_conn.execute("DELETE FROM stem_fts")

            self._stem_cache[cache_key] = stems
            self._stem_cache.move_to_end(cache_key)
            while len(self._stem_cache) > _STEM_CACHE_MAX:
                self._stem_cache.popitem(last=False)
            return stems

    def _single_term_doc_freq(self, low: str) -> int:
        stems = self._fts5_stems(low)
        if not stems:
            return 0
        if len(stems) == 1:
            row = self.conn.execute(
                "SELECT doc FROM chunks_fts_vocab WHERE term = ?", (stems[0],)
            ).fetchone()
            self._has_vocab = True
            self._vocab_missing_sig = None
            return int(row["doc"]) if row is not None else 0
        placeholders = ",".join("?" for _ in stems)
        rows = self.conn.execute(
            f"SELECT term, doc FROM chunks_fts_vocab WHERE term IN ({placeholders})",
            stems,
        ).fetchall()
        self._has_vocab = True
        self._vocab_missing_sig = None
        doc_by_term = {str(r["term"]): int(r["doc"]) for r in rows}
        if any(s not in doc_by_term for s in stems):
            return 0
        return min(doc_by_term[s] for s in stems)

    def _token_doc_freq(self, token: str) -> int:
        """Return exact FTS5 document frequency via SQLite Porter stemming + chunks_fts_vocab."""
        if self._vocab_missing_sig is not None:
            if self._vocab_missing_sig == self.cache_signature():
                return 1000
            self._vocab_missing_sig = None
            self._has_vocab = None
        low = token.lower().strip(".:/")
        if not low:
            return 0
        try:
            return self._single_term_doc_freq(low)
        except sqlite3.OperationalError as exc:
            if "no such table" in str(exc).lower():
                self._has_vocab = False
                self._vocab_missing_sig = self.cache_signature()
            return 1000

    def fts_search(
        self,
        query: str,
        *,
        category: Optional[str] = None,
        source: Optional[str] = None,
        exclude_categories: Optional[tuple[str, ...]] = None,
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
        if exclude_categories and not category:
            ex = [c.lower() for c in exclude_categories if c]
            if ex:
                placeholders = ",".join("?" for _ in ex)
                where_extra.append(f"c.category NOT IN ({placeholders})")
                params_extra.extend(ex)
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
            try:
                rows = self.conn.execute(sql, [fts_expr, *params_extra, limit]).fetchall()
            except sqlite3.OperationalError:
                return
            for r in rows:
                cid = int(r["id"])
                if cid not in seen:
                    seen.add(cid)
                    results.append((cid, float(r["score"])))
                    if len(results) >= limit:
                        break

        # 1. Exact full AND (always fast)
        _run_and_merge(" AND ".join(_quote_fts_token(t) for t in tokens))
        if len(results) >= min(limit, 12):
            return results

        # 1b. Security acronym/synonym recall expansion (e.g. BOLA->IDOR, ATO->account takeover,
        #     WCD->cache deception, TOCTOU->race condition) when exact AND is sparse.
        if len(results) < min(limit, 8):
            syns = expand_fts5_synonyms(tokens)
            if syns:
                base_non_acro = [t for t in tokens if t.lower() not in SECURITY_SYNONYMS][:2]
                syn_terms = base_non_acro + syns[:2]
                if len(syn_terms) >= 2:
                    _run_and_merge(" AND ".join(_quote_fts_token(t) for t in syn_terms))
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
        exclude_categories: Optional[tuple[str, ...]] = None,
        limit: int = 40,
    ) -> list[tuple[int, float]]:
        idx, sig = self.get_readonly_vector_index_with_sig()
        if idx is None or len(idx) == 0:
            return []

        cat = category.lower() if category else None
        src = source.lower() if source else None
        excl_cats = tuple(c.lower() for c in (exclude_categories or ()) if c) if not cat else ()
        q = query_vec.astype(np.float32)

        if cat or src or excl_cats:
            meta = self._get_meta_cache(sig)
            # Small-subset fast path: exact SIMD dot product -> 100% recall in ~15-60 ms!
            subset: Optional[np.ndarray] = None
            if cat and not src and cat in meta.cat_embed_ids:
                subset = meta.cat_embed_ids[cat]
            elif src and not cat and not excl_cats and src in meta.src_embed_ids:
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
                raw_vecs = idx.get(subset)
                if raw_vecs is None:
                    return []
                vecs: Optional[np.ndarray] = None
                if isinstance(raw_vecs, np.ndarray):
                    vecs = raw_vecs.astype(np.float32, copy=False)
                else:
                    try:
                        candidate = np.asarray(raw_vecs, dtype=np.float32)
                        if candidate.ndim == 2 and candidate.shape[1] == EMBED_DIM:
                            vecs = candidate
                        elif candidate.ndim == 1 and len(subset) == 1 and candidate.shape[0] == EMBED_DIM:
                            vecs = candidate.reshape(1, -1)
                    except (ValueError, TypeError):
                        vecs = None
                    if vecs is None:
                        keep_pairs = [(int(k), v) for k, v in zip(subset, raw_vecs) if v is not None]
                        if not keep_pairs:
                            return []
                        subset = np.asarray([p[0] for p in keep_pairs], dtype=np.uint64)
                        vecs = np.asarray([p[1] for p in keep_pairs], dtype=np.float32)
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

            # Large category/source or exclude-category pass: global HNSW + O(1) uint8 array filtering
            want_c = meta.cat_to_id.get(cat, -1) if cat else 0
            want_s = meta.src_to_id.get(src, -1) if src else 0
            if (cat and want_c < 0) or (src and want_s < 0):
                return []
            excl_c_ids = [meta.cat_to_id[c] for c in excl_cats if c in meta.cat_to_id]
            fetch_k = min(len(idx), max(limit * 6, 300))
            for _attempt in range(2):
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
                for ec_id in excl_c_ids:
                    mask = mask & (meta.cat_arr[safe_keys] != ec_id)
                filtered_keys = keys[mask][:limit]
                filtered_dists = dists[mask][:limit]
                if len(filtered_keys) >= limit or fetch_k >= len(idx):
                    return [(int(k), float(d)) for k, d in zip(filtered_keys, filtered_dists)]
                fetch_k = min(len(idx), fetch_k * 4)
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
                # Document exists, but section_id did not match — return a helpful
                # fallback with the document overview + section outline so agents
                # can pick a valid section_id without an extra search turn.
                full_text = doc["full_text"]
                return {
                    "doc_id": int(doc["id"]),
                    "section_id": None,
                    "section_not_found": int(section_id),
                    "source": doc["source"],
                    "category": doc["category"],
                    "title": doc["title"],
                    "path": doc["rel_path"],
                    "url": doc["url"],
                    "cves": doc["cves"],
                    "content": full_text[: min(max_chars, 4000)],
                    "truncated": len(full_text) > min(max_chars, 4000),
                    "outline": outline[:30],
                }
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
        raw = (cve_id or "").strip().upper()
        m = CVE_PARSE_RE.search(raw)
        if not m:
            return {"cve_id": raw, "is_kev": False, "match_count": 0, "matches": []}
        norm = f"CVE-{m.group(1)}-{m.group(2)}"

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
                         WHEN 'vulhub-pocs' THEN 2
                         WHEN 'nuclei' THEN 3
                         WHEN 'metasploit-docs' THEN 4
                         WHEN 'exploitdb' THEN 5
                         WHEN 'trickest-cve' THEN 6
                         WHEN 'poc-in-github' THEN 7
                         WHEN 'hackerone-disclosed' THEN 8
                         WHEN 'ghsa-advisories' THEN 9
                         WHEN 'cvelist-v5' THEN 10
                         WHEN 'hacktricks' THEN 11
                         ELSE 12
                     END ASC
            LIMIT ?
            """,
            (norm, limit),
        ).fetchall()

        # Batch-load the first section mentioning this CVE per matched document,
        # falling back to section_idx = 0 only when no specific section matches.
        doc_ids = [int(r["doc_id"]) for r in rows]
        first_sections: dict[int, sqlite3.Row] = {}
        if doc_ids:
            placeholders = ",".join("?" for _ in doc_ids)
            for s in self.conn.execute(
                f"""
                SELECT doc_id, id, section_idx, breadcrumb, content
                FROM sections
                WHERE doc_id IN ({placeholders})
                  AND (instr(upper(content), ?) > 0 OR instr(upper(breadcrumb), ?) > 0)
                ORDER BY doc_id ASC, section_idx ASC
                """,
                [*doc_ids, norm, norm],
            ).fetchall():
                did = int(s["doc_id"])
                if did not in first_sections:
                    first_sections[did] = s

            missing_ids = [did for did in doc_ids if did not in first_sections]
            if missing_ids:
                m_placeholders = ",".join("?" for _ in missing_ids)
                for s in self.conn.execute(
                    f"SELECT doc_id, id, section_idx, breadcrumb, content FROM sections WHERE doc_id IN ({m_placeholders}) AND section_idx = 0",
                    missing_ids,
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

        # Also check FTS5 mentions not yet in cve_index (exact canonical CVE token only)
        if len(matches) < limit:
            fts_hits = self.fts_search(norm, limit=limit)
            cids = [cid for cid, _ in fts_hits]
            chunks = self.get_chunks_by_ids(cids)
            for cid in cids:
                ch = chunks.get(cid)
                if not ch or ch.doc_id in seen_docs:
                    continue
                haystack = f"{ch.cves} {ch.title} {ch.breadcrumb} {ch.content}".upper()
                if norm not in haystack:
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
        sig = self.cache_signature()
        with self._vec_lock:
            if self._stats_cache is not None and self._stats_cache_key == sig:
                return self._stats_cache

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
        categories = [
            {"category": r["category"], "documents": int(r["cnt"])}
            for r in self.conn.execute(
                "SELECT category, COUNT(*) AS cnt FROM documents GROUP BY category ORDER BY cnt DESC"
            ).fetchall()
        ]
        total_docs = sum(c["documents"] for c in categories)
        total_sections = int(self.conn.execute("SELECT COUNT(*) FROM sections").fetchone()[0])
        # All three counts use covering indexes (idx_chunks_doc, idx_chunks_meta_embedded,
        # idx_chunks_embedded), avoiding unindexed full-table scans on embedded=2 and
        # staying exact even when kb.usearch lags SQLite or ingestion is in flight.
        total_chunks = int(self.conn.execute("SELECT COUNT(*) FROM chunks").fetchone()[0])
        embedded_chunks = int(
            self.conn.execute("SELECT COUNT(*) FROM chunks WHERE embedded = 1").fetchone()[0]
        )
        pending_embed_chunks = int(
            self.conn.execute("SELECT COUNT(*) FROM chunks WHERE embedded = 0").fetchone()[0]
        )
        lexical_only_chunks = max(0, total_chunks - embedded_chunks - pending_embed_chunks)
        vec_idx = self.get_readonly_vector_index()
        vec_count = len(vec_idx) if vec_idx is not None else 0

        total_cves = int(
            self.conn.execute("SELECT COUNT(DISTINCT cve_id) FROM cve_index").fetchone()[0]
        )
        db_mb = round(self.db_path.stat().st_size / (1024 * 1024), 1) if self.db_path.exists() else 0.0
        vec_mb = round(self.vec_path.stat().st_size / (1024 * 1024), 1) if self.vec_path.exists() else 0.0

        result = {
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
        with self._vec_lock:
            self._stats_cache = result
            self._stats_cache_key = sig
        return result
