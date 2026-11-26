"""FastEmbed ONNX query embedder + Apple Silicon MPS bulk embedder + multilingual reranker."""

from __future__ import annotations

import os
import threading
from typing import Iterable, Optional

import numpy as np

from . import DEFAULT_EMBED_MODEL, DEFAULT_RERANK_MODEL, EMBED_DIM

BGE_QUERY_PREFIX = "Represent this sentence for searching relevant passages: "
# Optimal P-core thread count on Apple Silicon (measured: 254 ms at 4 vs 405 ms at 8 / 938 ms at 1).
DEFAULT_ONNX_THREADS = 4


def _parse_onnx_threads(raw: Optional[str]) -> int:
    if not raw:
        return 0
    try:
        val = int(raw.strip())
    except ValueError:
        return 0
    return val if val > 0 else 0


def _l2_normalize(vecs: np.ndarray) -> np.ndarray:
    norms = np.linalg.norm(vecs, axis=-1, keepdims=True)
    norms = np.where(norms == 0, 1.0, norms)
    return vecs / norms


def _finalize_vecs(vecs: np.ndarray, dim: int = EMBED_DIM) -> np.ndarray:
    if vecs.shape[-1] > dim:
        vecs = vecs[..., :dim]
    return _l2_normalize(vecs.astype(np.float32))


class LocalEmbedder:
    """Lazy-loading embedder.

    - For single queries (MCP runtime): uses lightweight FastEmbed ONNX (~67 MB, ~10 ms/query).
    - For bulk document ingestion (prefer_mps=True): automatically uses Apple Silicon Metal GPU
      (PyTorch MPS fp16) with length-sorted batching (~200+ chunks/s, 1.000 cosine parity with
      FastEmbed ONNX), falling back to FastEmbed ONNX when MPS is unavailable.
    """

    def __init__(
        self,
        model_name: Optional[str] = None,
        cache_dir: Optional[str] = None,
        threads: Optional[int] = None,
        prefer_mps: bool = False,
    ):
        self.model_name = (model_name or "").strip() or (os.environ.get("SECRAG_EMBED_MODEL") or "").strip() or DEFAULT_EMBED_MODEL
        self.cache_dir = cache_dir or os.environ.get("SECRAG_MODEL_CACHE") or None
        env_threads = _parse_onnx_threads(os.environ.get("SECRAG_ONNX_THREADS"))
        self.threads = (threads if threads and threads > 0 else 0) or env_threads or DEFAULT_ONNX_THREADS
        self.prefer_mps = prefer_mps
        self._fe_model = None
        self._mps_tok = None
        self._mps_model = None
        self._mps_checked = False
        self._lock = threading.Lock()
        self._infer_lock = threading.Lock()
        low = self.model_name.lower()
        self._uses_nomic_prefix = "nomic-embed" in low
        self._uses_e5_prefix = "e5-" in low
        self._uses_bge_query_prefix = "bge-" in low

    def _ensure_fastembed(self):
        if self._fe_model is not None:
            return self._fe_model
        with self._lock:
            if self._fe_model is None:
                from fastembed import TextEmbedding

                kwargs = {"model_name": self.model_name}
                if self.cache_dir:
                    kwargs["cache_dir"] = self.cache_dir
                if self.threads:
                    kwargs["threads"] = self.threads
                self._fe_model = TextEmbedding(**kwargs)
        return self._fe_model

    def _ensure_mps(self) -> bool:
        if self._mps_checked:
            return self._mps_model is not None
        with self._lock:
            if self._mps_checked:
                return self._mps_model is not None
            try:
                if not self.prefer_mps:
                    return False
                import torch
                from transformers import AutoModel, AutoTokenizer

                if not torch.backends.mps.is_available():
                    return False
                hf_kwargs = {"cache_dir": self.cache_dir} if self.cache_dir else {}
                self._mps_tok = AutoTokenizer.from_pretrained(self.model_name, **hf_kwargs)
                self._mps_model = (
                    AutoModel.from_pretrained(self.model_name, dtype=torch.float16, **hf_kwargs)
                    .to("mps")
                    .eval()
                )
                return True
            except Exception:
                self._mps_tok = None
                self._mps_model = None
                return False
            finally:
                self._mps_checked = True

    def _fmt_doc(self, text: str) -> str:
        t = text[:2400]
        if self._uses_nomic_prefix:
            return "search_document: " + t
        if self._uses_e5_prefix:
            return "passage: " + t
        return t

    def _fmt_query(self, text: str) -> str:
        t = text[:1200]
        if self._uses_nomic_prefix:
            return "search_query: " + t
        if self._uses_e5_prefix:
            return "query: " + t
        if self._uses_bge_query_prefix:
            return BGE_QUERY_PREFIX + t
        return t

    def embed_documents(self, texts: Iterable[str], batch_size: int = 64) -> np.ndarray:
        formatted = [self._fmt_doc(t) for t in texts]
        if not formatted:
            return np.zeros((0, EMBED_DIM), dtype=np.float32)

        if self._ensure_mps():
            import torch

            # Sort by length to eliminate padding waste in Transformer attention
            order = sorted(range(len(formatted)), key=lambda i: len(formatted[i]))
            sorted_texts = [formatted[i] for i in order]
            parts: list[np.ndarray] = []
            for start in range(0, len(sorted_texts), batch_size):
                batch_txt = sorted_texts[start : start + batch_size]
                enc = self._mps_tok(
                    batch_txt,
                    padding=True,
                    truncation=True,
                    max_length=512,
                    return_tensors="pt",
                ).to("mps")
                with torch.no_grad():
                    out = self._mps_model(**enc)
                    # BGE uses CLS pooling; mean pooling otherwise
                    if self._uses_bge_query_prefix:
                        vecs = out.last_hidden_state[:, 0]
                    else:
                        mask = enc["attention_mask"].unsqueeze(-1).type_as(out.last_hidden_state)
                        vecs = (out.last_hidden_state * mask).sum(dim=1) / mask.sum(dim=1).clamp(min=1e-9)
                    vecs = torch.nn.functional.normalize(vecs, p=2, dim=-1)
                parts.append(vecs.cpu().numpy().astype(np.float32))
            stacked = np.concatenate(parts, axis=0)
            restored = np.empty_like(stacked)
            for pos, orig_idx in enumerate(order):
                restored[orig_idx] = stacked[pos]
            return _finalize_vecs(restored, EMBED_DIM)

        model = self._ensure_fastembed()
        raw = np.asarray(list(model.embed(formatted, batch_size=batch_size)), dtype=np.float32)
        return _finalize_vecs(raw, EMBED_DIM)

    def embed_query(self, query: str) -> np.ndarray:
        model = self._ensure_fastembed()
        with self._infer_lock:
            raw = np.asarray(list(model.embed([self._fmt_query(query)], batch_size=1)), dtype=np.float32)
        return _finalize_vecs(raw, EMBED_DIM)[0]


class LocalReranker:
    """Lazy-loading multilingual ONNX cross-encoder reranker (PT-BR + EN)."""

    def __init__(
        self,
        model_name: Optional[str] = None,
        cache_dir: Optional[str] = None,
        threads: Optional[int] = None,
        enabled: bool = True,
    ):
        self.model_name = (model_name or "").strip() or (os.environ.get("SECRAG_RERANK_MODEL") or "").strip() or DEFAULT_RERANK_MODEL
        self.cache_dir = cache_dir or os.environ.get("SECRAG_MODEL_CACHE") or None
        env_threads = _parse_onnx_threads(os.environ.get("SECRAG_ONNX_THREADS"))
        self.threads = (threads if threads and threads > 0 else 0) or env_threads or DEFAULT_ONNX_THREADS
        env_disable = os.environ.get("SECRAG_DISABLE_RERANK", "").strip().lower() in ("1", "true", "yes")
        self.enabled = enabled and not env_disable
        self._model = None
        self._lock = threading.Lock()
        self._infer_lock = threading.Lock()
        self._failed = False

    def _ensure_loaded(self):
        if not self.enabled or self._failed:
            return None
        if self._model is not None:
            return self._model
        with self._lock:
            if self._model is None and not self._failed:
                try:
                    from fastembed.rerank.cross_encoder import TextCrossEncoder

                    kwargs = {"model_name": self.model_name}
                    if self.cache_dir:
                        kwargs["cache_dir"] = self.cache_dir
                    if self.threads:
                        kwargs["threads"] = self.threads
                    self._model = TextCrossEncoder(**kwargs)
                except Exception:
                    self._failed = True
                    return None
        return self._model

    def score(self, query: str, passages: list[str]) -> Optional[list[float]]:
        if not passages:
            return []
        model = self._ensure_loaded()
        if model is None:
            return None
        clipped = [p[:700] for p in passages]
        try:
            with self._infer_lock:
                return [float(s) for s in model.rerank(query[:600], clipped)]
        except Exception:
            return None
