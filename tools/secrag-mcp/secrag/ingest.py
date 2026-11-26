"""Two-phase resumable ingestion pipeline for ArtexV2's Cybersecurity Knowledge Base."""

from __future__ import annotations

import argparse
import csv
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Callable, Optional

import httpx
import numpy as np
import yaml

from .chunker import (
    _split_section_to_chunks,
    chunk_markdown,
    infer_category_from_path,
    parse_atomic_yaml,
    parse_cve_v5_json,
    parse_mitre_stix,
    parse_nuclei_yaml,
    parse_poc_in_github_json,
    parse_sigma_yaml,
    strip_frontmatter,
)
from .embeddings import LocalEmbedder
from .storage import KnowledgeStore, extract_cves, extract_mitre_ids, resolve_data_dir, sha256_text

SKIP_DIR_NAMES = {
    ".git", ".github", "node_modules", "images", "img", "assets", "files",
    "__pycache__", ".gitbook", "static", "media", "videos", "pdfs",
}

SKIP_MD_NAMES = {
    "summary.md", "contributing.md", "license.md", "code_of_conduct.md",
    "changelog.md", "security.md",
}


def log(msg: str) -> None:
    print(f"[secrag-ingest] {msg}", flush=True)


def _git_sync(url: str, dest: Path, sparse_paths: Optional[list[str]] = None) -> str:
    """Shallow-clone or fast-forward pull a git repository. Returns short HEAD sha."""
    env = {**os.environ, "GIT_TERMINAL_PROMPT": "0"}
    if (dest / ".git").exists():
        subprocess.run(
            ["git", "-C", str(dest), "pull", "--ff-only", "--quiet"],
            check=False,
            env=env,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
        )
    else:
        dest.parent.mkdir(parents=True, exist_ok=True)
        if sparse_paths:
            subprocess.run(
                ["git", "clone", "--depth", "1", "--filter=blob:none", "--sparse", url, str(dest)],
                check=True,
                env=env,
            )
            subprocess.run(
                ["git", "-C", str(dest), "sparse-checkout", "set", *sparse_paths],
                check=True,
                env=env,
            )
        else:
            subprocess.run(
                ["git", "clone", "--depth", "1", url, str(dest)],
                check=True,
                env=env,
            )
    rev = subprocess.run(
        ["git", "-C", str(dest), "rev-parse", "--short", "HEAD"],
        capture_output=True,
        text=True,
        check=False,
    )
    return rev.stdout.strip()


def _iter_files(root: Path, exts: set[str], max_bytes: int = 512_000):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d.lower() not in SKIP_DIR_NAMES and not d.startswith(".")]
        for fn in filenames:
            low = fn.lower()
            if not any(low.endswith(ext) for ext in exts):
                continue
            p = Path(dirpath) / fn
            try:
                sz = p.stat().st_size
                if sz < 40 or sz > max_bytes:
                    continue
            except OSError:
                continue
            yield p


def _ingest_markdown_tree(
    store: KnowledgeStore,
    *,
    source: str,
    root: Path,
    root_label: str,
    url_base: str = "",
    prefix_rel: str = "",
    category_override: Optional[str] = None,
    extra_tags: Optional[list[str]] = None,
) -> int:
    added = 0
    batch_dirty = 0
    for path in _iter_files(root, {".md", ".markdown"}):
        if path.name.lower() in SKIP_MD_NAMES:
            continue
        rel = str(path.relative_to(root))
        rel_key = f"{prefix_rel}/{rel}" if prefix_rel else rel
        try:
            raw = path.read_text(encoding="utf-8", errors="ignore")
        except OSError:
            continue
        if len(raw.strip()) < 60:
            continue

        c_hash = sha256_text(raw)
        if store.document_unchanged(source, rel_key, c_hash):
            continue

        meta, body = strip_frontmatter(raw)
        stem_title = path.stem.replace("-", " ").replace("_", " ").strip()
        if stem_title.lower() == "readme" and path.parent != root:
            stem_title = path.parent.name.replace("-", " ").replace("_", " ").strip()
        title = str(meta.get("title") or stem_title or rel).strip()[:200]

        parent_parts = [p.replace("-", " ").replace("_", " ") for p in Path(rel).parent.parts if p not in (".", "src", "docs", "document", "cheatsheets")]
        bc_parts = [root_label, *parent_parts[-3:], title]
        root_bc = " > ".join(dict.fromkeys(p for p in bc_parts if p))

        sections = chunk_markdown(raw, doc_title=title, root_breadcrumb=root_bc)
        if not sections:
            continue

        cat = category_override or infer_category_from_path(source, rel_key)
        cves = extract_cves(raw)
        mitre = extract_mitre_ids(raw)
        tags = list(dict.fromkeys([source, cat, *(extra_tags or []), *mitre]))
        url = f"{url_base.rstrip('/')}/{rel}" if url_base else ""

        store.upsert_document(
            source=source,
            category=cat,
            rel_path=rel_key,
            title=title,
            url=url,
            tags=tags,
            cves=cves,
            full_text=body[:120_000],
            sections_with_chunks=sections,
            content_hash=c_hash,
        )
        added += 1
        batch_dirty += 1
        if batch_dirty >= 200:
            store.conn.commit()
            batch_dirty = 0
    if batch_dirty:
        store.conn.commit()
    return added


# ---- Phase 1 Ingesters ----

def ingest_hacktricks(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "hacktricks"
    rev = _git_sync("https://github.com/hacktricks-wiki/hacktricks.git", dest)
    n = _ingest_markdown_tree(
        store,
        source="hacktricks",
        root=dest,
        root_label="HackTricks",
        url_base="https://github.com/hacktricks-wiki/hacktricks/blob/master",
    )
    log(f"hacktricks: indexed {n} new/updated docs (rev {rev})")
    return rev


def ingest_hacktricks_cloud(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "hacktricks-cloud"
    rev = _git_sync("https://github.com/hacktricks-wiki/hacktricks-cloud.git", dest)
    n = _ingest_markdown_tree(
        store,
        source="hacktricks-cloud",
        root=dest,
        root_label="HackTricks Cloud",
        url_base="https://github.com/hacktricks-wiki/hacktricks-cloud/blob/master",
        category_override="cloud",
    )
    log(f"hacktricks-cloud: indexed {n} new/updated docs (rev {rev})")
    return rev


def ingest_payloads(store: KnowledgeStore, src_dir: Path) -> str:
    repos = [
        ("PayloadsAllTheThings", "https://github.com/swisskyrepo/PayloadsAllTheThings.git", "PayloadsAllTheThings", None),
        ("InternalAllTheThings", "https://github.com/swisskyrepo/InternalAllTheThings.git", "InternalAllTheThings", "ad"),
    ]
    revs = []
    total = 0
    for folder, url, label, cat_ov in repos:
        dest = src_dir / folder
        rev = _git_sync(url, dest)
        revs.append(f"{folder}:{rev}")
        total += _ingest_markdown_tree(
            store,
            source="payloads",
            root=dest,
            root_label=label,
            url_base=url.removesuffix(".git") + "/blob/master",
            prefix_rel=folder,
            category_override=cat_ov,
        )
    log(f"payloads: indexed {total} new/updated docs ({', '.join(revs)})")
    return ",".join(revs)


def ingest_owasp(store: KnowledgeStore, src_dir: Path) -> str:
    repos = [
        ("owasp-wstg", "https://github.com/OWASP/wstg.git", "OWASP WSTG", "web"),
        ("owasp-cheatsheets", "https://github.com/OWASP/CheatSheetSeries.git", "OWASP CheatSheets", "web"),
        ("owasp-api", "https://github.com/OWASP/API-Security.git", "OWASP API Security", "api"),
        ("owasp-mastg", "https://github.com/OWASP/owasp-mastg.git", "OWASP MASTG", "mobile"),
    ]
    revs = []
    total = 0
    for folder, url, label, cat_ov in repos:
        dest = src_dir / folder
        rev = _git_sync(url, dest)
        revs.append(f"{folder}:{rev}")
        total += _ingest_markdown_tree(
            store,
            source="owasp",
            root=dest,
            root_label=label,
            url_base=url.removesuffix(".git") + "/blob/master",
            prefix_rel=folder,
            category_override=cat_ov,
            extra_tags=["owasp"],
        )
    log(f"owasp: indexed {total} new/updated docs ({', '.join(revs)})")
    return ",".join(revs)


def ingest_privesc_bins(store: KnowledgeStore, src_dir: Path) -> str:
    revs = []
    total = 0

    # GTFOBins
    gtfo = src_dir / "gtfobins"
    revs.append("gtfobins:" + _git_sync("https://github.com/GTFOBins/GTFOBins.github.io.git", gtfo))
    gtfo_dir = gtfo / "_gtfobins"
    if gtfo_dir.exists():
        for p in sorted(gtfo_dir.glob("*")):
            if not p.is_file():
                continue
            raw = p.read_text(encoding="utf-8", errors="ignore")
            c_hash = sha256_text(raw)
            rel_key = f"gtfobins/{p.name}"
            if store.document_unchanged("privesc-bins", rel_key, c_hash):
                continue
            meta, _ = strip_frontmatter(raw)
            funcs = meta.get("functions") if isinstance(meta, dict) else None
            if not isinstance(funcs, dict):
                continue
            bin_name = p.stem
            lines = [f"# GTFOBins: {bin_name}"]
            for fn_name, items in funcs.items():
                lines.append(f"\n## {fn_name}")
                if isinstance(items, list):
                    for it in items:
                        if isinstance(it, dict):
                            if it.get("description"):
                                lines.append(str(it["description"]))
                            if it.get("code"):
                                lines.append(f"```sh\n{it['code']}\n```")
            body = "\n".join(lines).strip()
            sections = chunk_markdown(body, doc_title=f"GTFOBins: {bin_name}", root_breadcrumb=f"GTFOBins > {bin_name}")
            if sections:
                store.upsert_document(
                    source="privesc-bins",
                    category="privesc",
                    rel_path=rel_key,
                    title=f"GTFOBins: {bin_name}",
                    url=f"https://gtfobins.github.io/gtfobins/{bin_name}/",
                    tags=["gtfobins", "linux", "privesc", bin_name, *list(funcs.keys())],
                    cves=[],
                    full_text=body,
                    sections_with_chunks=sections,
                    content_hash=c_hash,
                )
                total += 1
        store.conn.commit()

    # LOLBAS
    lolbas = src_dir / "lolbas"
    revs.append("lolbas:" + _git_sync("https://github.com/LOLBAS-Project/LOLBAS.git", lolbas))
    yml_dir = lolbas / "yml"
    if yml_dir.exists():
        for p in _iter_files(yml_dir, {".yml", ".yaml"}):
            raw = p.read_text(encoding="utf-8", errors="ignore")
            c_hash = sha256_text(raw)
            rel_key = f"lolbas/{p.relative_to(yml_dir)}"
            if store.document_unchanged("privesc-bins", rel_key, c_hash):
                continue
            try:
                data = yaml.safe_load(raw)
            except Exception:
                continue
            if not isinstance(data, dict) or not data.get("Name"):
                continue
            name = str(data["Name"])
            desc = str(data.get("Description", ""))
            lines = [f"# LOLBAS: {name}", desc]
            mitre_ids = []
            for cmd in (data.get("Commands") or [])[:20]:
                if not isinstance(cmd, dict):
                    continue
                cat_c = str(cmd.get("Category", ""))
                mitre_id = str(cmd.get("MitreID", ""))
                if mitre_id:
                    mitre_ids.append(mitre_id)
                lines.append(f"\n## {cat_c} ({mitre_id})" if mitre_id else f"\n## {cat_c}")
                if cmd.get("Description"):
                    lines.append(str(cmd["Description"]))
                if cmd.get("Command"):
                    lines.append(f"```cmd\n{cmd['Command']}\n```")
            body = "\n".join(lines).strip()
            sections = chunk_markdown(body, doc_title=f"LOLBAS: {name}", root_breadcrumb=f"LOLBAS > {name}")
            if sections:
                store.upsert_document(
                    source="privesc-bins",
                    category="privesc",
                    rel_path=rel_key,
                    title=f"LOLBAS: {name}",
                    url=f"https://lolbas-project.github.io/",
                    tags=list(dict.fromkeys(["lolbas", "windows", "privesc", name, *mitre_ids])),
                    cves=[],
                    full_text=body,
                    sections_with_chunks=sections,
                    content_hash=c_hash,
                )
                total += 1
        store.conn.commit()

    # WADComs
    wadcoms = src_dir / "wadcoms"
    revs.append("wadcoms:" + _git_sync("https://github.com/WADComs/WADComs.github.io.git", wadcoms))
    wad_dir = wadcoms / "_wadcoms"
    if wad_dir.exists():
        for p in sorted(wad_dir.glob("*.md")):
            raw = p.read_text(encoding="utf-8", errors="ignore")
            c_hash = sha256_text(raw)
            rel_key = f"wadcoms/{p.name}"
            if store.document_unchanged("privesc-bins", rel_key, c_hash):
                continue
            meta, _ = strip_frontmatter(raw)
            if not isinstance(meta, dict):
                continue
            desc = str(meta.get("description", ""))
            cmd = str(meta.get("command", ""))
            services = meta.get("services") or []
            attack_types = meta.get("attack_types") or []
            title = f"WADComs: {p.stem}"
            body = (
                f"# {title}\n"
                f"Services: {', '.join(map(str, services))} | Attack types: {', '.join(map(str, attack_types))}\n\n"
                f"{desc}\n\n```sh\n{cmd}\n```"
            ).strip()
            store.upsert_document(
                source="privesc-bins",
                category="ad",
                rel_path=rel_key,
                title=title,
                url="https://wadcoms.github.io/",
                tags=list(dict.fromkeys(["wadcoms", "ad", p.stem, *map(str, services), *map(str, attack_types)])),
                cves=[],
                full_text=body,
                sections_with_chunks=[(title, f"WADComs > {p.stem}", body, _split_section_to_chunks(body))],
                content_hash=c_hash,
            )
            total += 1
        store.conn.commit()

    log(f"privesc-bins: indexed {total} new/updated docs ({', '.join(revs)})")
    return ",".join(revs)


def ingest_mitre_attack(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "mitre-attack"
    rev = _git_sync("https://github.com/mitre-attack/attack-stix-data.git", dest)
    bundles = [
        ("enterprise", dest / "enterprise-attack" / "enterprise-attack.json"),
        ("mobile", dest / "mobile-attack" / "mobile-attack.json"),
        ("ics", dest / "ics-attack" / "ics-attack.json"),
    ]
    total = 0
    for domain, path in bundles:
        if not path.exists():
            continue
        bundle = json.loads(path.read_text(encoding="utf-8"))
        for doc in parse_mitre_stix(bundle, domain):
            c_hash = sha256_text(doc["full_text"])
            if store.document_unchanged("mitre-attack", doc["rel_path"], c_hash):
                continue
            store.upsert_document(
                source="mitre-attack",
                category=doc["category"],
                rel_path=doc["rel_path"],
                title=doc["title"],
                url=doc["url"],
                tags=doc["tags"],
                cves=doc["cves"],
                full_text=doc["full_text"],
                sections_with_chunks=doc["sections"],
                content_hash=c_hash,
            )
            total += 1
        store.conn.commit()
    log(f"mitre-attack: indexed {total} new/updated techniques (rev {rev})")
    return rev


def ingest_atomic_red_team(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "atomic-red-team"
    rev = _git_sync("https://github.com/redcanaryco/atomic-red-team.git", dest, sparse_paths=["atomics"])
    atomics_dir = dest / "atomics"
    total = 0
    if atomics_dir.exists():
        for p in _iter_files(atomics_dir, {".yaml", ".yml"}):
            if not p.stem.startswith("T"):
                continue
            raw = p.read_text(encoding="utf-8", errors="ignore")
            rel = str(p.relative_to(atomics_dir))
            c_hash = sha256_text(raw)
            if store.document_unchanged("atomic-red-team", rel, c_hash):
                continue
            parsed = parse_atomic_yaml(raw, rel)
            if not parsed:
                continue
            store.upsert_document(
                source="atomic-red-team",
                category=parsed["category"],
                rel_path=rel,
                title=parsed["title"],
                url=parsed["url"],
                tags=parsed["tags"],
                cves=parsed["cves"],
                full_text=parsed["full_text"],
                sections_with_chunks=parsed["sections"],
                content_hash=c_hash,
            )
            total += 1
            if total % 150 == 0:
                store.conn.commit()
        store.conn.commit()
    log(f"atomic-red-team: indexed {total} new/updated techniques (rev {rev})")
    return rev


def ingest_nuclei(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "nuclei-templates"
    rev = _git_sync("https://github.com/projectdiscovery/nuclei-templates.git", dest)
    total = 0
    batch = 0
    for p in _iter_files(dest, {".yaml", ".yml"}, max_bytes=120_000):
        rel = str(p.relative_to(dest))
        if rel.startswith(".") or rel.startswith("helpers/"):
            continue
        raw = p.read_text(encoding="utf-8", errors="ignore")
        c_hash = sha256_text(raw)
        if store.document_unchanged("nuclei", rel, c_hash):
            continue
        parsed = parse_nuclei_yaml(raw, rel)
        if not parsed:
            continue
        store.upsert_document(
            source="nuclei",
            category=parsed["category"],
            rel_path=rel,
            title=parsed["title"],
            url=parsed["url"],
            tags=parsed["tags"],
            cves=parsed["cves"],
            full_text=parsed["full_text"],
            sections_with_chunks=parsed["sections"],
            cve_entries=parsed["cve_entries"],
            content_hash=c_hash,
        )
        total += 1
        batch += 1
        if batch >= 400:
            store.conn.commit()
            batch = 0
    if batch:
        store.conn.commit()
    log(f"nuclei: indexed {total} new/updated templates (rev {rev})")
    return rev


def ingest_cisa_kev(store: KnowledgeStore, src_dir: Path) -> str:
    url = "https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json"
    resp = httpx.get(url, timeout=60.0, follow_redirects=True)
    resp.raise_for_status()
    data = resp.json()
    version = str(data.get("catalogVersion", ""))
    vulns = data.get("vulnerabilities") or []
    total = 0
    for v in vulns:
        cve = str(v.get("cveID", "")).strip().upper()
        if not cve:
            continue
        vendor = str(v.get("vendorProject", "")).strip()
        product = str(v.get("product", "")).strip()
        vname = str(v.get("vulnerabilityName", "")).strip()
        desc = str(v.get("shortDescription", "")).strip()
        action = str(v.get("requiredAction", "")).strip()
        ransomware = str(v.get("knownRansomwareCampaignUse", "")).strip()
        cwes = v.get("cwes") or []
        date_added = str(v.get("dateAdded", "")).strip()

        body = (
            f"# CISA KEV {cve}: {vname}\n"
            f"Vendor/Product: {vendor} {product} | Date Added: {date_added} | "
            f"Known Ransomware Use: {ransomware} | CWE: {', '.join(cwes)}\n\n"
            f"{desc}\n\n"
            f"Required Action: {action}"
        ).strip()
        c_hash = sha256_text(body)
        if store.document_unchanged("cisa-kev", cve, c_hash):
            continue
        store.upsert_document(
            source="cisa-kev",
            category="cve",
            rel_path=cve,
            title=f"CISA KEV {cve}: {vname}",
            url=f"https://nvd.nist.gov/vuln/detail/{cve}",
            tags=list(dict.fromkeys(["cisa-kev", "kev", "exploited", vendor, product, *cwes])),
            cves=[cve],
            full_text=body,
            sections_with_chunks=[(f"CISA KEV {cve}", f"CISA KEV > {cve} ({vendor} {product})", body, _split_section_to_chunks(body))],
            cve_entries=[(cve, "critical", True, f"[CISA KEV] {vendor} {product}: {vname}")],
            content_hash=c_hash,
        )
        total += 1
    store.conn.commit()
    log(f"cisa-kev: indexed {total} new/updated KEV entries (catalog {version})")
    return version


# ---- Phase 2 Ingesters ----

def ingest_exploitdb(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "exploitdb"
    rev = _git_sync("https://gitlab.com/exploit-database/exploitdb.git", dest)
    csv_path = dest / "files_exploits.csv"
    if not csv_path.exists():
        raise FileNotFoundError(f"missing {csv_path}")

    total = 0
    batch = 0
    with csv_path.open("r", encoding="utf-8", errors="ignore", newline="") as f:
        reader = csv.DictReader(f)
        for row in reader:
            edb_id = str(row.get("id", "")).strip()
            file_rel = str(row.get("file", "")).strip()
            desc = str(row.get("description", "")).strip()
            date_pub = str(row.get("date_published", "")).strip()
            etype = str(row.get("type", "")).strip()
            platform = str(row.get("platform", "")).strip()
            codes = str(row.get("codes", "")).strip()
            if not edb_id or not desc:
                continue

            # Keep modern/relevant exploits (2015+) or any exploit with a CVE
            year = int(date_pub[:4]) if len(date_pub) >= 4 and date_pub[:4].isdigit() else 0
            cves = extract_cves(codes + " " + desc)
            if year < 2015 and not cves:
                continue

            code_snippet = ""
            exp_file = dest / file_rel
            if exp_file.is_file() and exp_file.stat().st_size <= 32_000:
                try:
                    code_snippet = exp_file.read_text(encoding="utf-8", errors="ignore")[:3500]
                except OSError:
                    code_snippet = ""

            body = (
                f"# Exploit-DB EDB-{edb_id}: {desc}\n"
                f"Platform: {platform} | Type: {etype} | Published: {date_pub}"
                + (f" | CVEs: {', '.join(cves)}" if cves else "")
            )
            if code_snippet.strip():
                body += f"\n\n```\n{code_snippet.strip()}\n```"

            c_hash = sha256_text(body)
            rel_key = f"edb-{edb_id}"
            if store.document_unchanged("exploitdb", rel_key, c_hash):
                continue
            cve_entries = [(c, "", False, f"Exploit-DB EDB-{edb_id} ({platform}/{etype}): {desc}") for c in cves]
            store.upsert_document(
                source="exploitdb",
                category="exploits",
                rel_path=rel_key,
                title=f"EDB-{edb_id}: {desc}",
                url=f"https://www.exploit-db.com/exploits/{edb_id}",
                tags=list(dict.fromkeys(["exploitdb", platform, etype])),
                cves=cves,
                full_text=body,
                sections_with_chunks=[(f"EDB-{edb_id}", f"Exploit-DB > {platform} > EDB-{edb_id} {desc[:80]}", body, _split_section_to_chunks(body))],
                cve_entries=cve_entries or None,
                content_hash=c_hash,
            )
            total += 1
            batch += 1
            if batch >= 400:
                store.conn.commit()
                batch = 0
    if batch:
        store.conn.commit()
    log(f"exploitdb: indexed {total} new/updated exploits (rev {rev})")
    return rev


def ingest_bugbounty_writeups(store: KnowledgeStore, src_dir: Path) -> str:
    repos = [
        ("hackerone-reports", "https://github.com/reddelexc/hackerone-reports.git", "HackerOne Disclosed", "writeups", ["bug-bounty", "writeups"]),
        ("bugbounty-cheatsheet", "https://github.com/EdOverflow/bugbounty-cheatsheet.git", "BugBounty Cheatsheet", "writeups", ["bug-bounty", "cheatsheet"]),
        ("awesome-bugbounty-writeups", "https://github.com/devanshbatham/Awesome-Bugbounty-Writeups.git", "Awesome BugBounty Writeups", "writeups", ["bug-bounty", "writeups"]),
        ("awesome-waf", "https://github.com/0xInfection/Awesome-WAF.git", "Awesome WAF", "web", ["waf", "waf-bypass"]),
        ("ad-exploitation-cheatsheet", "https://github.com/S1ckB0y1337/Active-Directory-Exploitation-Cheat-Sheet.git", "AD Exploitation Cheat Sheet", "ad", ["ad", "active-directory"]),
    ]
    revs = []
    total = 0
    for folder, url, label, cat_ov, tags in repos:
        dest = src_dir / folder
        try:
            rev = _git_sync(url, dest)
            revs.append(f"{folder}:{rev}")
            total += _ingest_markdown_tree(
                store,
                source="bugbounty-writeups",
                root=dest,
                root_label=label,
                url_base=url.removesuffix(".git") + "/blob/master",
                prefix_rel=folder,
                category_override=cat_ov,
                extra_tags=tags,
            )
        except Exception as e:
            log(f"bugbounty-writeups ({folder}) warning: {e}")
    log(f"bugbounty-writeups: indexed {total} new/updated docs ({', '.join(revs)})")
    return ",".join(revs)


def ingest_poc_in_github(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "poc-in-github"
    years = [str(y) for y in range(2018, 2027)]
    rev = _git_sync("https://github.com/nomi-sec/PoC-in-GitHub.git", dest, sparse_paths=years)
    total = 0
    batch = 0
    for y in years:
        ydir = dest / y
        if not ydir.exists():
            continue
        for p in _iter_files(ydir, {".json"}, max_bytes=250_000):
            if not p.name.upper().startswith("CVE-"):
                continue
            rel = str(p.relative_to(dest))
            raw = p.read_text(encoding="utf-8", errors="ignore")
            c_hash = sha256_text(raw)
            if store.document_unchanged("poc-in-github", rel, c_hash):
                continue
            try:
                entries = json.loads(raw)
            except Exception:
                continue
            parsed = parse_poc_in_github_json(p.stem, entries)
            if not parsed:
                continue
            store.upsert_document(
                source="poc-in-github",
                category=parsed["category"],
                rel_path=rel,
                title=parsed["title"],
                url=parsed["url"],
                tags=parsed["tags"],
                cves=parsed["cves"],
                full_text=parsed["full_text"],
                sections_with_chunks=parsed["sections"],
                cve_entries=parsed["cve_entries"],
                content_hash=c_hash,
            )
            total += 1
            batch += 1
            if batch >= 400:
                store.conn.commit()
                batch = 0
    if batch:
        store.conn.commit()
    log(f"poc-in-github: indexed {total} new/updated CVE PoC lists 2018-2026 (rev {rev})")
    return rev


def ingest_sigma_rules(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "sigma"
    rev = _git_sync("https://github.com/SigmaHQ/sigma.git", dest, sparse_paths=["rules"])
    rules_dir = dest / "rules"
    total = 0
    batch = 0
    if rules_dir.exists():
        for p in _iter_files(rules_dir, {".yml", ".yaml"}, max_bytes=80_000):
            rel = str(p.relative_to(rules_dir))
            raw = p.read_text(encoding="utf-8", errors="ignore")
            c_hash = sha256_text(raw)
            if store.document_unchanged("sigma-rules", rel, c_hash):
                continue
            parsed = parse_sigma_yaml(raw, rel)
            if not parsed:
                continue
            store.upsert_document(
                source="sigma-rules",
                category=parsed["category"],
                rel_path=rel,
                title=parsed["title"],
                url=parsed["url"],
                tags=parsed["tags"],
                cves=parsed["cves"],
                full_text=parsed["full_text"],
                sections_with_chunks=parsed["sections"],
                cve_entries=parsed["cve_entries"],
                content_hash=c_hash,
            )
            total += 1
            batch += 1
            if batch >= 400:
                store.conn.commit()
                batch = 0
    if batch:
        store.conn.commit()
    log(f"sigma-rules: indexed {total} new/updated detection rules (rev {rev})")
    return rev


def ingest_cvelist_v5(store: KnowledgeStore, src_dir: Path) -> str:
    dest = src_dir / "cvelistV5"
    years = [f"cves/{y}" for y in range(2020, 2027)]
    rev = _git_sync("https://github.com/CVEProject/cvelistV5.git", dest, sparse_paths=years)
    # Dual-tier indexing: 100% of CVEs go to cve_index + FTS5; only high-signal CVEs
    # (CVSS >= 7.0 / high / critical, or already referenced by CISA KEV / Nuclei /
    # Exploit-DB / PoC-in-GitHub) are queued for dense vector embeddings.
    known_cves = store.known_cve_ids()
    total = 0
    high_signal = 0
    lexical_only = 0
    batch = 0
    for y in years:
        ydir = dest / y
        if not ydir.exists():
            continue
        for p in _iter_files(ydir, {".json"}, max_bytes=200_000):
            if not p.name.startswith("CVE-"):
                continue
            rel = str(p.relative_to(dest))
            raw = p.read_text(encoding="utf-8", errors="ignore")
            c_hash = sha256_text(raw)
            if store.document_unchanged("cvelist-v5", rel, c_hash):
                continue
            try:
                data = json.loads(raw)
            except Exception:
                continue
            parsed = parse_cve_v5_json(data, rel)
            if not parsed:
                continue
            cve_id = parsed["cves"][0]
            should_embed = bool(parsed.get("is_high_signal") or cve_id in known_cves)
            store.upsert_document(
                source="cvelist-v5",
                category="cve",
                rel_path=rel,
                title=parsed["title"],
                url=parsed["url"],
                tags=parsed["tags"],
                cves=parsed["cves"],
                full_text=parsed["full_text"],
                sections_with_chunks=parsed["sections"],
                cve_entries=parsed["cve_entries"],
                content_hash=c_hash,
                embed_chunks=should_embed,
            )
            total += 1
            if should_embed:
                high_signal += 1
            else:
                lexical_only += 1
            batch += 1
            if batch >= 500:
                store.conn.commit()
                batch = 0
    if batch:
        store.conn.commit()
    log(
        f"cvelist-v5: indexed {total} CVEs 2020-2026 "
        f"(vector-queued={high_signal}, lexical-only={lexical_only}, rev {rev})"
    )
    return rev


def ingest_primus_seed(store: KnowledgeStore, src_dir: Path, max_docs: int = 15000) -> str:
    if not os.environ.get("HF_TOKEN") and not (Path.home() / ".cache" / "huggingface" / "token").exists():
        raise RuntimeError(
            "trendmicro-ailab/Primus-Seed is gated on Hugging Face; accept terms and set HF_TOKEN to ingest it."
        )
    from datasets import load_dataset

    ds = load_dataset("trendmicro-ailab/Primus-Seed", split="train", streaming=True)
    total = 0
    batch = 0
    for idx, row in enumerate(ds):
        if idx >= max_docs:
            break
        text = str(row.get("content") or row.get("text") or "").strip()
        if len(text) < 200:
            continue
        src_name = str(row.get("source") or row.get("type") or "primus").strip()
        rel_key = f"primus/{idx:06d}"
        c_hash = sha256_text(text)
        if store.document_unchanged("primus-seed", rel_key, c_hash):
            continue
        first_line = text.splitlines()[0].lstrip("#").strip()[:160] or f"Primus-Seed #{idx}"
        sections = chunk_markdown(text[:30_000], doc_title=first_line, root_breadcrumb=f"Primus-Seed > {src_name} > {first_line}")
        if not sections:
            sections = [(first_line, f"Primus-Seed > {src_name}", text[:12_000], _split_section_to_chunks(text[:12_000]))]
        store.upsert_document(
            source="primus-seed",
            category="threat-intel",
            rel_path=rel_key,
            title=first_line,
            url="https://huggingface.co/datasets/trendmicro-ailab/Primus-Seed",
            tags=list(dict.fromkeys(["primus-seed", src_name, *extract_mitre_ids(text)])),
            cves=extract_cves(text),
            full_text=text[:30_000],
            sections_with_chunks=sections,
            content_hash=c_hash,
        )
        total += 1
        batch += 1
        if batch >= 250:
            store.conn.commit()
            batch = 0
    if batch:
        store.conn.commit()
    log(f"primus-seed: indexed {total} new/updated curated docs")
    return f"stream:{max_docs}"


SOURCES: list[tuple[str, int, Callable[[KnowledgeStore, Path], str]]] = [
    # Phase 1 — Core Tactical Pack
    ("cisa-kev", 1, ingest_cisa_kev),
    ("privesc-bins", 1, ingest_privesc_bins),
    ("mitre-attack", 1, ingest_mitre_attack),
    ("atomic-red-team", 1, ingest_atomic_red_team),
    ("payloads", 1, ingest_payloads),
    ("owasp", 1, ingest_owasp),
    ("hacktricks-cloud", 1, ingest_hacktricks_cloud),
    ("hacktricks", 1, ingest_hacktricks),
    ("nuclei", 1, ingest_nuclei),
    # Phase 2 — Deep Reference Pack (100% open, zero-gate)
    ("bugbounty-writeups", 2, ingest_bugbounty_writeups),
    ("sigma-rules", 2, ingest_sigma_rules),
    ("exploitdb", 2, ingest_exploitdb),
    ("poc-in-github", 2, ingest_poc_in_github),
    ("cvelist-v5", 2, ingest_cvelist_v5),
    # Phase 3 — Optional gated HF corpus (requires HF_TOKEN)
    ("primus-seed", 3, ingest_primus_seed),
]


def run_embedding_pass(store: KnowledgeStore, batch_size: int = 512, checkpoint_every: int = 8192) -> int:
    pending = int(store.conn.execute("SELECT COUNT(*) FROM chunks WHERE embedded = 0").fetchone()[0])
    if pending == 0:
        log("All chunks are already embedded.")
        return 0

    embedder = LocalEmbedder(prefer_mps=True)
    accel = "Apple MPS GPU" if embedder._ensure_mps() else "ONNX CPU"
    log(f"Embedding {pending:,} pending chunks using {embedder.model_name} ({accel}, f16)...")
    vec_index = store.open_writable_vector_index()
    t0 = time.perf_counter()
    done = 0
    since_save = 0

    for batch in store.iter_unembedded_chunks(batch_size=batch_size):
        ids = [cid for cid, _ in batch]
        texts = [txt for _, txt in batch]
        vecs = embedder.embed_documents(texts, batch_size=64)
        vec_index.add(np.asarray(ids, dtype=np.uint64), vecs.astype(np.float16))
        store.mark_chunks_embedded(ids)
        done += len(ids)
        since_save += len(ids)
        if since_save >= checkpoint_every:
            store.save_vector_index_atomic(vec_index)
            elapsed = max(0.1, time.perf_counter() - t0)
            log(f"  embedded {done:,}/{pending:,} ({done/elapsed:.1f} chunks/s) — checkpoint saved")
            since_save = 0

    if since_save > 0 or not store.vec_path.exists():
        store.save_vector_index_atomic(vec_index)

    elapsed = max(0.1, time.perf_counter() - t0)
    log(f"Embedding complete: {done:,} chunks in {elapsed:.1f}s ({done/elapsed:.1f} chunks/s)")
    return done


def main() -> None:
    parser = argparse.ArgumentParser(description="Ingest & index ArtexV2 Cybersecurity Knowledge Base")
    parser.add_argument("--data-dir", default=None, help="Target data directory (default: data/secrag)")
    parser.add_argument("--phase", type=int, choices=[1, 2, 3], default=None, help="Ingest only Phase 1, 2, or 3 sources")
    parser.add_argument("--all", action="store_true", help="Ingest Phase 1 and Phase 2 open sources")
    parser.add_argument("--source", action="append", default=[], help="Ingest specific source(s) by name")
    parser.add_argument("--skip-embed", action="store_true", help="Only populate SQLite + FTS5; skip vector embedding pass")
    parser.add_argument("--embed-only", action="store_true", help="Run vector embedding pass on pending chunks without fetching sources")
    parser.add_argument("--prune-sources", action="store_true", help="Delete cloned git repositories in _sources/ after indexing")
    parser.add_argument("--batch-size", type=int, default=512, help="SQLite fetch batch size (default: 512)")
    parser.add_argument("--stats", action="store_true", help="Print current DB stats and exit")
    args = parser.parse_args()

    data_dir = resolve_data_dir(args.data_dir)
    store = KnowledgeStore(data_dir=data_dir)
    # Prefix with underscore so `go test ./...` at the repo root never walks cloned repos.
    src_dir = data_dir / "_sources"
    src_dir.mkdir(parents=True, exist_ok=True)

    try:
        if args.stats:
            print(json.dumps(store.stats(), indent=2))
            return

        if not args.embed_only:
            selected = []
            want_sources = {s.strip().lower() for s in args.source if s.strip()}
            for name, phase, fn in SOURCES:
                if want_sources:
                    if name in want_sources:
                        selected.append((name, phase, fn))
                elif args.all:
                    if phase in (1, 2):
                        selected.append((name, phase, fn))
                elif args.phase is not None:
                    if phase == args.phase:
                        selected.append((name, phase, fn))
                else:
                    if phase == 1:
                        selected.append((name, phase, fn))

            for name, phase, fn in selected:
                log(f"=== Starting [phase {phase}] {name} ===")
                store.update_ingestion_state(name, phase, "running")
                try:
                    rev = fn(store, src_dir)
                    store.update_ingestion_state(name, phase, "done", revision=rev)
                except Exception as exc:
                    log(f"ERROR in {name}: {exc}")
                    store.update_ingestion_state(name, phase, "error", error=str(exc))

        if not args.skip_embed:
            run_embedding_pass(store, batch_size=args.batch_size)

        if args.prune_sources and src_dir.exists():
            log(f"Pruning cloned source repos at {src_dir}...")
            shutil.rmtree(src_dir, ignore_errors=True)

        log("Final stats: " + json.dumps({
            k: v for k, v in store.stats().items() if k not in ("sources", "categories")
        }))
    finally:
        store.close()


if __name__ == "__main__":
    main()
