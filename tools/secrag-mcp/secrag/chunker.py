"""Structure-aware Markdown & cybersecurity record chunkers."""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any, Optional

import yaml

from .storage import extract_cves, extract_mitre_ids

HEADING_RE = re.compile(r"^(#{1,6})\s+(.*)$")
FENCE_RE = re.compile(r"^\s*(?:(?:[-*+]|\d+[.)])\s+|>\s*)*(`{3,}|~{3,})(.*)$")
FRONTMATTER_RE = re.compile(r"^---[ \t]*\r?\n(.*?)\r?\n(?:---|\.\.\.)[ \t]*(?:\r?\n|$)", re.DOTALL)


def _parse_fence_line(line: str) -> Optional[tuple[str, int, str]]:
    """Parse a CommonMark-style fenced code delimiter line (including inside lists/blockquotes).

    Returns (marker_char, fence_length, stripped_info_string) or None.
    """
    fm = FENCE_RE.match(line)
    if not fm:
        return None
    seq = fm.group(1)
    return seq[0], len(seq), fm.group(2).strip()

TARGET_CHUNK_CHARS = 1500
MAX_CHUNK_CHARS = 2200
MAX_SECTION_CHARS = 14000


def strip_frontmatter(text: str) -> tuple[dict[str, Any], str]:
    m = FRONTMATTER_RE.match(text)
    if not m:
        return {}, text
    body = text[m.end():]
    try:
        meta = yaml.safe_load(m.group(1))
        if isinstance(meta, dict):
            return meta, body
    except Exception:
        pass
    return {}, body


def _split_section_to_chunks(content: str, target: int = TARGET_CHUNK_CHARS, hard_max: int = MAX_CHUNK_CHARS) -> list[str]:
    """Split markdown section into code-block-preserving chunks."""
    text = content.strip()
    if not text:
        return []
    if len(text) <= hard_max:
        return [text]

    # Tokenize into logical blocks: paragraphs and atomic fenced code blocks.
    blocks: list[str] = []
    cur: list[str] = []
    in_fence = False
    fence_char = ""
    fence_len = 0

    for line in text.splitlines():
        fence = _parse_fence_line(line)
        if fence:
            marker, flen, info = fence
            if not in_fence:
                if marker != "`" or "`" not in info:
                    if cur:
                        blocks.append("\n".join(cur).strip())
                        cur = []
                    in_fence = True
                    fence_char = marker
                    fence_len = flen
                    cur.append(line)
                    continue
            elif marker == fence_char and flen >= fence_len and info == "":
                cur.append(line)
                blocks.append("\n".join(cur).strip())
                cur = []
                in_fence = False
                fence_char = ""
                fence_len = 0
                continue

        if in_fence:
            cur.append(line)
        elif not line.strip():
            if cur:
                blocks.append("\n".join(cur).strip())
                cur = []
        else:
            cur.append(line)

    if cur:
        blocks.append("\n".join(cur).strip())

    chunks: list[str] = []
    buf: list[str] = []
    buf_len = 0

    for blk in blocks:
        if not blk:
            continue
        if len(blk) > hard_max:
            if buf:
                chunks.append("\n\n".join(buf))
                buf = []
                buf_len = 0
            # Split oversized single block cleanly on line boundaries.
            sub_lines: list[str] = []
            sub_len = 0
            for ln in blk.splitlines():
                if sub_len + len(ln) + 1 > hard_max and sub_lines:
                    chunks.append("\n".join(sub_lines))
                    sub_lines = [ln]
                    sub_len = len(ln)
                else:
                    sub_lines.append(ln)
                    sub_len += len(ln) + 1
            if sub_lines:
                chunks.append("\n".join(sub_lines))
            continue

        if buf and buf_len + len(blk) + 2 > target:
            chunks.append("\n\n".join(buf))
            # Keep last short paragraph as light overlap if non-code and small
            if len(buf[-1]) <= 220 and not buf[-1].startswith("```"):
                buf = [buf[-1], blk]
                buf_len = len(buf[0]) + len(blk) + 2
            else:
                buf = [blk]
                buf_len = len(blk)
        else:
            buf.append(blk)
            buf_len += len(blk) + (2 if len(buf) > 1 else 0)

    if buf:
        chunks.append("\n\n".join(buf))
    return chunks


def chunk_markdown(
    text: str,
    *,
    doc_title: str,
    root_breadcrumb: str = "",
    min_section_chars: int = 25,
) -> list[tuple[str, str, str, list[str]]]:
    """Parse Markdown into heading-aware sections and code-safe chunks.

    Returns list of (heading, breadcrumb, section_content, [chunk_texts]).
    """
    _, body = strip_frontmatter(text)
    body = body.strip()
    if not body:
        return []

    sections_raw: list[tuple[str, str, list[str]]] = []
    heading_stack: list[tuple[int, str]] = []
    cur_heading = doc_title
    cur_breadcrumb = root_breadcrumb or doc_title
    cur_lines: list[str] = []

    in_fence = False
    fence_char = ""
    fence_len = 0

    for line in body.splitlines():
        fence = _parse_fence_line(line)
        if fence:
            marker, flen, info = fence
            if not in_fence:
                if marker != "`" or "`" not in info:
                    in_fence = True
                    fence_char = marker
                    fence_len = flen
                    cur_lines.append(line)
                    continue
            elif marker == fence_char and flen >= fence_len and info == "":
                in_fence = False
                fence_char = ""
                fence_len = 0
                cur_lines.append(line)
                continue

        if not in_fence:
            hm = HEADING_RE.match(line)
            if hm:
                level = len(hm.group(1))
                title = hm.group(2).strip().strip("#").strip()
                if title:
                    if cur_lines and "\n".join(cur_lines).strip():
                        sections_raw.append((cur_heading, cur_breadcrumb, cur_lines))
                    while heading_stack and heading_stack[-1][0] >= level:
                        heading_stack.pop()
                    heading_stack.append((level, title))
                    parts = [root_breadcrumb] if root_breadcrumb else []
                    parts.extend(h for _, h in heading_stack)
                    # Avoid duplicating doc_title if already in root_breadcrumb
                    dedup_parts: list[str] = []
                    for p in parts:
                        if not dedup_parts or dedup_parts[-1].lower() != p.lower():
                            dedup_parts.append(p)
                    cur_heading = title
                    cur_breadcrumb = " > ".join(dedup_parts)
                    cur_lines = [line]
                    continue

        cur_lines.append(line)

    if cur_lines and "\n".join(cur_lines).strip():
        sections_raw.append((cur_heading, cur_breadcrumb, cur_lines))

    out: list[tuple[str, str, str, list[str]]] = []
    for heading, breadcrumb, lines in sections_raw:
        sec_text = "\n".join(lines).strip()
        if len(sec_text) < min_section_chars:
            continue
        chunks = _split_section_to_chunks(sec_text)
        if chunks:
            out.append((heading, breadcrumb, sec_text[:MAX_SECTION_CHARS], chunks))
    return out


_NORM_SEP_RE = re.compile(r"[\s_\-]+")


def _tok_re(*tokens: str) -> re.Pattern[str]:
    alt = "|".join(tokens)
    return re.compile(rf"(?<![a-z0-9])(?:{alt})(?![a-z0-9])")


_CATEGORY_RULES: list[tuple[str, re.Pattern[str]]] = [
    (
        "cloud",
        _tok_re(
            "cloud", "aws", "azure", "gcp", "kubernetes", "k8s", "docker",
            "terraform", "ci-cd", "cicd", "helm", "eks", "aks", "gke",
            "entra", "cognito", "github-actions", "imds", "iam", "s3",
        ),
    ),
    (
        "ad",
        _tok_re(
            "active-directory", "kerberos", "ldap(?!-injection)", "ntlm",
            "bloodhound", "adcs", "windows-active", "domain-controller",
            "dcsync", "kerberoast", "asreproast", "rbcd", "gpo", "delegation",
        ),
    ),
    (
        "privesc",
        _tok_re(
            "privesc", "privilege-escalation", "linux-hardening",
            "windows-local", "suid", "uac", "sudo",
        ),
    ),
    (
        "mobile",
        _tok_re("mobile", "android", "ios", "apk", "ipa", "mastg", "frida", "webview"),
    ),
    (
        "api",
        _tok_re(
            "api", "graphql", "rest", "jwt", "json-web-token", "oauth",
            "oidc", "grpc", "websocket", "cswsh", "swagger", "openapi",
            "bola", "bfla",
        ),
    ),
    (
        "web",
        _tok_re(
            "pentesting-web", "xss", "cross-site-scripting", "sqli",
            "sql-injection", "nosql-injection", "ssrf", "xxe",
            "xml-external-entity", "ssti", "template-injection", "csrf",
            "request-forgery", "idor", "direct-object-references",
            "file-inclusion", "lfi", "rfi", "command-injection",
            "request-smuggling", "http-smuggling", "desync", "upload-insecure-files",
            "file-upload", "prototype-pollution", "open-redirect", "cors",
            "crlf", "directory-traversal", "path-traversal",
            "deserialization", "waf", "wstg", "cheatsheet", "web",
            "ldap-injection", "xpath-injection", "ssi-injection",
            "xslt-injection", "csv-injection", "clickjacking",
            "cache-poisoning", "cache-deception", "host-header", "race-condition",
            "toctou", "account-takeover", "saml", "sso", "type-juggling",
            "mass-assignment", "business-logic", "tabnabbing", "dom-clobbering",
            "postmessage", "css-injection", "latex-injection", "prompt-injection",
            "llm", "agentic", "csti", "verb-tampering", "dependency-confusion",
        ),
    ),
    (
        "binary",
        _tok_re(
            "binary", "pwn", "reversing", "reverse-engineering", "rop",
            "heap", "shellcode", "malware", "buffer-overflow",
        ),
    ),
    (
        "network",
        _tok_re(
            "network", "pentesting-network", "smb", "ssh", "ftp", "smtp",
            "snmp", "dns", "pivoting", "tunnel", "tunneling",
        ),
    ),
]


def infer_category_from_path(source: str, rel_path: str) -> str:
    p = rel_path.lower().replace("\\", "/")
    if source in ("hacktricks-cloud", "hacking-the-cloud"):
        return "cloud"
    if source in ("cisa-kev", "cvelist-v5", "trickest-cve", "ghsa-advisories"):
        return "cve"
    if source in ("hackerone-disclosed", "bugbounty-writeups"):
        return "writeups"
    if source in ("vulhub-pocs", "metasploit-docs", "exploitdb", "poc-in-github"):
        return "exploits"
    if source in ("portswigger-labs",):
        return "web"
    if source in ("semgrep-rules", "sigma-rules"):
        return "detection"
    if source in ("nuclei",):
        return "cve" if "/cves/" in p else "web"
    if source in ("mitre-attack", "atomic-red-team"):
        return "mitre"
    if source in ("gtfobins", "lolbas"):
        return "privesc"
    if source in ("wadcoms",):
        return "ad"
    if source in ("bugbounty-writeups",):
        return "writeups"
    if source in ("exploitdb",):
        return "exploits"
    if source in ("primus-seed",):
        return "threat-intel"
    if source in ("sigma-rules",):
        return "detection"
    if source in ("poc-in-github",):
        return "exploits"

    norm = _NORM_SEP_RE.sub("-", p)
    for category, pattern in _CATEGORY_RULES:
        if pattern.search(norm):
            return category
    return "general"


def parse_nuclei_yaml(raw_yaml: str, rel_path: str) -> Optional[dict[str, Any]]:
    try:
        data = yaml.safe_load(raw_yaml)
    except Exception:
        return None
    if not isinstance(data, dict) or not data.get("id"):
        return None
    info = data.get("info")
    if not isinstance(info, dict):
        return None

    tid = str(data.get("id", "")).strip()
    name = str(info.get("name", tid)).strip()
    severity = str(info.get("severity", "")).strip().lower()
    desc = str(info.get("description", "")).strip()
    remediation = str(info.get("remediation", "")).strip()

    tags_raw = info.get("tags", "")
    if isinstance(tags_raw, list):
        tags = [str(t).strip() for t in tags_raw if t]
    else:
        tags = [t.strip() for t in str(tags_raw).split(",") if t.strip()]

    refs = info.get("reference") or []
    if isinstance(refs, str):
        refs = [refs]
    elif not isinstance(refs, list):
        refs = []
    refs = [str(r).strip() for r in refs if r][:6]

    classification = info.get("classification") or {}
    cves = extract_cves(tid + " " + name + " " + desc + " " + str(classification))
    cwe = ""
    cvss = ""
    if isinstance(classification, dict):
        cwe = str(classification.get("cwe-id", "") or "")
        cvss = str(classification.get("cvss-score", "") or "")

    # Extract compact HTTP paths / matchers for practical pentest insights
    paths: list[str] = []
    for key in ("http", "requests"):
        reqs = data.get(key)
        if isinstance(reqs, list):
            for req in reqs[:3]:
                if isinstance(req, dict):
                    for p in (req.get("path") or [])[:4]:
                        paths.append(str(p))
                    for raw_req in (req.get("raw") or [])[:1]:
                        first_line = str(raw_req).strip().splitlines()[0] if str(raw_req).strip() else ""
                        if first_line:
                            paths.append(first_line[:160])

    lines = [
        f"# Nuclei Template: {name} ({tid})",
        f"Severity: {severity or 'unknown'}" + (f" | CVSS: {cvss}" if cvss else "") + (f" | CWE: {cwe}" if cwe else ""),
        f"Tags: {', '.join(tags)}" if tags else "",
        f"CVEs: {', '.join(cves)}" if cves else "",
        "",
        desc,
    ]
    if paths:
        lines.append("\n### Detection Paths / Request Signature")
        lines.append("```text\n" + "\n".join(paths[:6]) + "\n```")
    if remediation:
        lines.append(f"\n### Remediation\n{remediation}")
    if refs:
        lines.append("\n### References\n" + "\n".join(f"- {r}" for r in refs))

    body = "\n".join(l for l in lines if l is not None).strip()
    cat = "cve" if cves or "/cves/" in rel_path.lower() else infer_category_from_path("nuclei", rel_path)
    cve_entries = [(c, severity, False, f"Nuclei [{severity}]: {name}") for c in cves]

    return {
        "title": f"{tid}: {name}",
        "category": cat,
        "tags": list(dict.fromkeys(["nuclei", severity, *tags])),
        "cves": cves,
        "url": refs[0] if refs else "",
        "full_text": body,
        "sections": [(name, f"Nuclei > {cat} > {tid}", body, _split_section_to_chunks(body))],
        "cve_entries": cve_entries,
    }


def parse_mitre_stix(bundle: dict[str, Any], domain: str) -> list[dict[str, Any]]:
    objs = bundle.get("objects") or []
    docs: list[dict[str, Any]] = []
    for obj in objs:
        if not isinstance(obj, dict) or obj.get("type") != "attack-pattern":
            continue
        if obj.get("revoked") or obj.get("x_mitre_deprecated"):
            continue
        name = str(obj.get("name", "")).strip()
        desc = str(obj.get("description", "")).strip()
        detection = str(obj.get("x_mitre_detection", "")).strip()
        platforms = [str(p) for p in (obj.get("x_mitre_platforms") or [])]
        tactics = [
            str(kp.get("phase_name", ""))
            for kp in (obj.get("kill_chain_phases") or [])
            if isinstance(kp, dict)
        ]
        ext_id = ""
        url = ""
        for ref in obj.get("external_references") or []:
            if isinstance(ref, dict) and ref.get("source_name") == "mitre-attack":
                ext_id = str(ref.get("external_id", "")).strip()
                url = str(ref.get("url", "")).strip()
                break
        if not ext_id or not name:
            continue

        parts = [
            f"# MITRE ATT&CK {ext_id}: {name}",
            f"Domain: {domain} | Tactics: {', '.join(tactics)} | Platforms: {', '.join(platforms)}",
            "",
            desc,
        ]
        if detection:
            parts.append(f"\n## Detection\n{detection}")
        full_text = "\n".join(parts).strip()
        breadcrumb = f"MITRE ATT&CK > {domain} > {ext_id} {name}"
        sections = chunk_markdown(full_text, doc_title=f"{ext_id}: {name}", root_breadcrumb=f"MITRE ATT&CK > {domain}")
        if not sections:
            sections = [(name, breadcrumb, full_text, _split_section_to_chunks(full_text))]

        docs.append(
            {
                "rel_path": f"{domain}/{ext_id}.md",
                "title": f"{ext_id}: {name}",
                "category": "mitre",
                "tags": list(dict.fromkeys(["mitre-attack", domain, ext_id, *tactics, *platforms])),
                "cves": extract_cves(full_text),
                "url": url,
                "full_text": full_text,
                "sections": sections,
            }
        )
    return docs


def parse_atomic_yaml(raw_yaml: str, rel_path: str) -> Optional[dict[str, Any]]:
    try:
        data = yaml.safe_load(raw_yaml)
    except Exception:
        return None
    if not isinstance(data, dict) or not data.get("attack_technique"):
        return None
    tid = str(data.get("attack_technique", "")).strip()
    display = str(data.get("display_name", tid)).strip()
    tests = data.get("atomic_tests") or []
    if not isinstance(tests, list) or not tests:
        return None

    sections: list[tuple[str, str, str, list[str]]] = []
    all_parts = [f"# Atomic Red Team {tid}: {display}"]
    platforms_all: set[str] = set()

    for t in tests[:25]:
        if not isinstance(t, dict):
            continue
        tname = str(t.get("name", "Atomic Test")).strip()
        tdesc = str(t.get("description", "")).strip()
        plats = [str(p) for p in (t.get("supported_platforms") or [])]
        platforms_all.update(plats)
        exec_block = t.get("executor") or {}
        cmd = ""
        exec_name = ""
        if isinstance(exec_block, dict):
            exec_name = str(exec_block.get("name", ""))
            cmd = str(exec_block.get("command", "") or "").strip()

        sec_lines = [
            f"## {tname}",
            f"Technique: {tid} ({display}) | Platforms: {', '.join(plats)} | Executor: {exec_name}",
            tdesc,
        ]
        if cmd:
            sec_lines.append(f"```{exec_name or 'sh'}\n{cmd[:1800]}\n```")
        sec_text = "\n\n".join(x for x in sec_lines if x).strip()
        all_parts.append(sec_text)
        bc = f"Atomic Red Team > {tid} {display} > {tname}"
        sections.append((tname, bc, sec_text, _split_section_to_chunks(sec_text)))

    if not sections:
        return None
    full_text = "\n\n".join(all_parts)
    return {
        "title": f"Atomic {tid}: {display}",
        "category": "mitre",
        "tags": list(dict.fromkeys(["atomic-red-team", tid, *sorted(platforms_all)])),
        "cves": extract_cves(full_text),
        "url": f"https://github.com/redcanaryco/atomic-red-team/blob/master/atomics/{tid}/{tid}.yaml",
        "full_text": full_text,
        "sections": sections,
    }


def parse_cve_v5_json(data: dict[str, Any], rel_path: str) -> Optional[dict[str, Any]]:
    meta = data.get("cveMetadata")
    if not isinstance(meta, dict):
        return None
    if str(meta.get("state", "")).upper() != "PUBLISHED":
        return None
    cve_id = str(meta.get("cveId", "")).strip().upper()
    if not cve_id.startswith("CVE-"):
        return None

    containers = data.get("containers") or {}
    cna = containers.get("cna") or {}
    if not isinstance(cna, dict):
        return None
    adp_list = [a for a in (containers.get("adp") or []) if isinstance(a, dict)]

    title = str(cna.get("title", "") or "").strip()
    descs = [
        str(d.get("value", "")).strip()
        for d in (cna.get("descriptions") or [])
        if isinstance(d, dict) and str(d.get("lang", "en")).lower().startswith("en") and d.get("value")
    ]
    desc = descs[0] if descs else ""
    if not desc or len(desc) < 25:
        return None

    affected_strs: list[str] = []
    for aff in (cna.get("affected") or [])[:6]:
        if not isinstance(aff, dict):
            continue
        vendor = str(aff.get("vendor", "") or "").strip()
        product = str(aff.get("product", "") or "").strip()
        if vendor or product:
            affected_strs.append(f"{vendor} {product}".strip())

    def _extract_cwes(problem_types: Any) -> list[str]:
        found: list[str] = []
        for pt in problem_types or []:
            if not isinstance(pt, dict):
                continue
            for pd in pt.get("descriptions") or []:
                if isinstance(pd, dict):
                    cwe_id = str(pd.get("cweId", "") or "").strip()
                    cwe_desc = str(pd.get("description", "") or "").strip()
                    if cwe_id:
                        entry = f"{cwe_id} ({cwe_desc})" if cwe_desc and cwe_desc != cwe_id else cwe_id
                        if entry not in found:
                            found.append(entry)
        return found

    cwes = _extract_cwes(cna.get("problemTypes"))
    if not cwes:
        for adp in adp_list:
            for entry in _extract_cwes(adp.get("problemTypes")):
                if entry not in cwes:
                    cwes.append(entry)

    def _extract_cvss(metrics: Any) -> tuple[str, str]:
        for metric in metrics or []:
            if not isinstance(metric, dict):
                continue
            for k in ("cvssV4_0", "cvssV3_1", "cvssV3_0", "cvssV2_0"):
                mv = metric.get(k)
                if isinstance(mv, dict):
                    score = str(mv.get("baseScore", "") or "").strip()
                    sev = str(mv.get("baseSeverity", "") or "").strip().lower()
                    if score or sev:
                        return score, sev
        return "", ""

    cvss_score, severity = _extract_cvss(cna.get("metrics"))
    if not cvss_score or not severity:
        for adp in adp_list:
            a_score, a_sev = _extract_cvss(adp.get("metrics"))
            if not cvss_score and a_score:
                cvss_score = a_score
            if not severity and a_sev:
                severity = a_sev
            if cvss_score and severity:
                break

    refs = [
        str(r.get("url", "")).strip()
        for r in (cna.get("references") or [])[:5]
        if isinstance(r, dict) and r.get("url")
    ]

    heading = f"{cve_id}: {title}" if title else cve_id
    lines = [
        f"# {heading}",
        f"Severity: {severity or 'unknown'}" + (f" (CVSS {cvss_score})" if cvss_score else ""),
        f"Affected: {', '.join(affected_strs)}" if affected_strs else "",
        f"CWE: {', '.join(cwes[:3])}" if cwes else "",
        "",
        desc,
    ]
    if refs:
        lines.append("\nReferences:\n" + "\n".join(f"- {u}" for u in refs))
    body = "\n".join(l for l in lines if l is not None).strip()
    summary = (title or desc)[:260]

    cvss_float = 0.0
    try:
        cvss_float = float(cvss_score) if cvss_score else 0.0
    except ValueError:
        cvss_float = 0.0
    is_high_signal = cvss_float >= 7.0 or severity in ("high", "critical")

    return {
        "title": heading[:240],
        "category": "cve",
        "tags": list(dict.fromkeys(["cve", severity, *affected_strs[:3], *extract_mitre_ids(body)])),
        "cves": [cve_id],
        "url": f"https://nvd.nist.gov/vuln/detail/{cve_id}",
        "full_text": body,
        "sections": [(heading, f"CVEListV5 > {cve_id}", body, _split_section_to_chunks(body))],
        "cve_entries": [(cve_id, severity, False, summary)],
        "cvss_float": cvss_float,
        "is_high_signal": is_high_signal,
    }


def parse_sigma_yaml(raw_yaml: str, rel_path: str) -> Optional[dict[str, Any]]:
    """Parse a SigmaHQ detection rule YAML into a detection/evasion reference doc."""
    try:
        data = yaml.safe_load(raw_yaml)
    except Exception:
        return None
    if not isinstance(data, dict) or not data.get("title"):
        return None

    level = str(data.get("level", "") or "").strip().lower()
    if level not in ("medium", "high", "critical"):
        return None

    title = str(data.get("title", "")).strip()
    rule_id = str(data.get("id", "") or "").strip()
    status = str(data.get("status", "") or "").strip()
    desc = str(data.get("description", "") or "").strip()

    logsource = data.get("logsource") or {}
    ls_parts = []
    if isinstance(logsource, dict):
        for k in ("product", "category", "service"):
            if logsource.get(k):
                ls_parts.append(f"{k}={logsource[k]}")

    raw_tags = data.get("tags") or []
    tags: list[str] = ["sigma", level]
    mitre_ids: list[str] = []
    for t in raw_tags:
        ts = str(t).strip()
        if not ts:
            continue
        low = ts.lower()
        if low.startswith("attack.t"):
            mid = ts.split(".", 1)[1].upper()
            mitre_ids.append(mid)
            tags.append(mid)
        elif low.startswith("attack."):
            tags.append(low.split(".", 1)[1])
        else:
            tags.append(ts)

    detection = data.get("detection") or {}
    det_yaml = ""
    if isinstance(detection, dict):
        try:
            det_yaml = yaml.safe_dump(detection, sort_keys=False, width=100)[:1400].strip()
        except Exception:
            det_yaml = ""

    fps = [str(f).strip() for f in (data.get("falsepositives") or []) if f][:4]
    refs = [str(r).strip() for r in (data.get("references") or []) if r][:4]

    lines = [
        f"# Sigma Detection Rule: {title}",
        f"Level: {level} | Status: {status or 'unknown'} | Logsource: {', '.join(ls_parts) or 'generic'}"
        + (f" | MITRE: {', '.join(mitre_ids)}" if mitre_ids else ""),
        "",
        desc,
    ]
    if det_yaml:
        lines.append(f"\n### Detection Logic (SIEM/EDR Indicators)\n```yaml\n{det_yaml}\n```")
    if fps:
        lines.append("\n### False Positives\n" + "\n".join(f"- {fp}" for fp in fps))
    if refs:
        lines.append("\n### References\n" + "\n".join(f"- {r}" for r in refs))

    body = "\n".join(lines).strip()
    cves = extract_cves(body)
    bc_prod = logsource.get("product") if isinstance(logsource, dict) else None
    bc = f"SigmaHQ > {bc_prod or 'generic'} > {title}"

    return {
        "title": f"Sigma [{level}]: {title}"[:220],
        "category": "detection",
        "tags": list(dict.fromkeys([*tags, *(ls_parts[:2])])),
        "cves": cves,
        "url": refs[0] if refs else f"https://github.com/SigmaHQ/sigma/blob/master/rules/{rel_path}",
        "full_text": body,
        "sections": [(title, bc, body, _split_section_to_chunks(body))],
        "cve_entries": [(c, level, False, f"Sigma [{level}]: {title}") for c in cves] or None,
    }


def parse_poc_in_github_json(cve_id: str, entries: list[Any]) -> Optional[dict[str, Any]]:
    """Parse one CVE's PoC-in-GitHub JSON array into a ranked PoC index document."""
    if not isinstance(entries, list) or not entries:
        return None
    norm_cve = cve_id.strip().upper()
    if not norm_cve.startswith("CVE-"):
        return None

    valid = [e for e in entries if isinstance(e, dict) and e.get("html_url")]
    if not valid:
        return None
    valid.sort(key=lambda e: int(e.get("stargazers_count") or 0), reverse=True)
    top = valid[:8]

    lines = [
        f"# GitHub PoCs for {norm_cve} ({len(valid)} repo(s))",
        "",
    ]
    top_desc = ""
    for repo in top:
        name = str(repo.get("full_name") or repo.get("name") or "").strip()
        url = str(repo.get("html_url") or "").strip()
        stars = int(repo.get("stargazers_count") or 0)
        desc = str(repo.get("description") or "").strip()
        updated = str(repo.get("updated_at") or "")[:10]
        if not top_desc and desc:
            top_desc = desc[:180]
        lines.append(f"- **{name}** ({stars}★, updated {updated}): {url}")
        if desc:
            lines.append(f"  {desc[:240]}")

    body = "\n".join(lines).strip()
    summary = f"{len(valid)} GitHub PoC(s) (top {int(top[0].get('stargazers_count') or 0)}★): {top_desc or top[0].get('html_url')}"
    return {
        "title": f"GitHub PoCs: {norm_cve}",
        "category": "exploits",
        "tags": ["poc-in-github", "poc", "exploit", norm_cve],
        "cves": [norm_cve],
        "url": str(top[0].get("html_url") or ""),
        "full_text": body,
        "sections": [(f"GitHub PoCs {norm_cve}", f"PoC-in-GitHub > {norm_cve}", body, _split_section_to_chunks(body))],
        "cve_entries": [(norm_cve, "", False, summary[:300])],
    }


_H1_META_RE = re.compile(r"^\|\s*\*\*([^*]+)\*\*\s*\|\s*(.*?)\s*\|$", re.MULTILINE)
_MD_LINK_TEXT_RE = re.compile(r"\[([^\]]+)\]\([^)]+\)")
_SHIELDS_BADGE_RE = re.compile(r"^\s*!\[[^\]]*\]\(https://img\.shields\.io/[^)]+\)\s*$", re.MULTILINE)


def _strip_md_links(s: str) -> str:
    return _MD_LINK_TEXT_RE.sub(r"\1", s).strip()


def parse_hackerone_report_md(raw_md: str, rel_path: str) -> Optional[dict[str, Any]]:
    """Parse one disclosed HackerOne Markdown report from ajaysenr/HackerOne-Disclosed-Reports."""
    if not raw_md or len(raw_md.strip()) < 160:
        return None
    lines = raw_md.splitlines()
    title = ""
    for ln in lines[:10]:
        if ln.startswith("# "):
            title = ln[2:].strip()
            break
    if not title:
        title = f"HackerOne Report {Path(rel_path).stem}"

    meta: dict[str, str] = {}
    for k, v in _H1_META_RE.findall(raw_md[:2500]):
        meta[k.strip().lower()] = _strip_md_links(v)

    report_id = meta.get("report id", Path(rel_path).stem).strip()
    program = meta.get("program", "").strip()
    severity_raw = meta.get("severity", "").strip()
    severity_low = severity_raw.split("—")[0].split("-")[0].strip().lower()
    if severity_low not in ("critical", "high", "medium", "low", "none"):
        severity_low = ""
    weakness = meta.get("weakness", "").strip()
    bounty = meta.get("bounty", "").strip()
    resolution = meta.get("resolution", "").strip().lower()
    if resolution in ("spam", "not applicable", "not_applicable"):
        return None

    # Strip empty boilerplate summary lines to maximize token density
    cleaned = raw_md.replace("> *No official summary provided by the program.*", "").strip()
    if len(cleaned) < 220:
        return None

    cves = extract_cves(cleaned)
    root_bc = " > ".join(
        p for p in ["HackerOne Disclosed", weakness or program or "Report", title[:90]] if p
    )
    sections = chunk_markdown(cleaned[:45_000], doc_title=title[:200], root_breadcrumb=root_bc)
    if not sections:
        return None

    has_bounty = bool(bounty and "$" in bounty and not bounty.strip().startswith("$0"))
    is_high_signal = (
        severity_low in ("critical", "high")
        or has_bounty
        or len(cleaned) >= 1200
    )
    tags = list(
        dict.fromkeys(
            t
            for t in ["hackerone", "bug-bounty", "writeups", severity_low, weakness, program, *extract_mitre_ids(cleaned)]
            if t and t != "—"
        )
    )
    cve_entries = [
        (c, severity_low, False, f"HackerOne #{report_id} ({program or 'disclosed'}, {bounty or severity_low}): {title[:180]}")
        for c in cves[:8]
    ] or None
    return {
        "title": f"H1 #{report_id}: {title}"[:240],
        "category": "writeups",
        "tags": tags[:12],
        "cves": cves,
        "url": f"https://hackerone.com/reports/{report_id}" if report_id.isdigit() else "",
        "full_text": cleaned[:45_000],
        "sections": sections,
        "cve_entries": cve_entries,
        "is_high_signal": is_high_signal,
    }


def parse_trickest_cve_md(raw_md: str, rel_path: str) -> Optional[dict[str, Any]]:
    """Parse one Trickest CVE Markdown file, keeping only CVEs with PoC or HackerOne references."""
    if not raw_md:
        return None
    # Only index Trickest entries that add concrete PoC / HackerOne signal beyond bare CVEListV5.
    low = raw_md.lower()
    has_poc = ("#### github" in low and "github.com/" in low) or "hackerone.com/reports/" in low or "exploit-db.com/" in low
    if not has_poc:
        return None

    cleaned = _SHIELDS_BADGE_RE.sub("", raw_md)
    cleaned = re.sub(r"\n{3,}", "\n\n", cleaned).strip()
    cves = extract_cves(Path(rel_path).stem + "\n" + cleaned[:300])
    if not cves:
        return None
    cve_id = cves[0]

    # Cap noisy mega-lists of 200+ duplicate GitHub forks
    poc_lines: list[str] = []
    github_count = 0
    for ln in cleaned.splitlines():
        if "https://github.com/" in ln and ln.lstrip().startswith("-"):
            github_count += 1
            if github_count > 12:
                continue
        poc_lines.append(ln)
    body = "\n".join(poc_lines).strip()[:16_000]
    if len(body) < 80:
        return None

    title = f"Trickest PoC: {cve_id}"
    bc = f"Trickest CVE > {cve_id}"
    return {
        "title": title,
        "category": "cve",
        "tags": ["trickest-cve", "poc", "exploit", cve_id],
        "cves": [cve_id],
        "url": f"https://github.com/trickest/cve/blob/main/{rel_path}",
        "full_text": body,
        "sections": [(title, bc, body, _split_section_to_chunks(body))],
        "cve_entries": [(cve_id, "", False, f"Trickest curated PoC/references ({github_count} GitHub PoC(s)) for {cve_id}")],
    }


def parse_ghsa_json(data: dict[str, Any], rel_path: str) -> Optional[dict[str, Any]]:
    """Parse one GitHub Security Advisory (OSV JSON) from github/advisory-database."""
    if not isinstance(data, dict):
        return None
    ghsa_id = str(data.get("id") or "").strip().upper()
    if not ghsa_id.startswith("GHSA-"):
        return None
    if data.get("withdrawn"):
        return None

    summary = str(data.get("summary") or "").strip()
    details = str(data.get("details") or "").strip()
    if not summary and len(details) < 40:
        return None

    aliases = [str(a).strip().upper() for a in (data.get("aliases") or []) if isinstance(a, str)]
    cves = [a for a in aliases if a.startswith("CVE-")]
    if not cves:
        cves = extract_cves(summary + "\n" + details)

    db_spec = data.get("database_specific") or {}
    severity = str(db_spec.get("severity") or "").strip().lower()
    if severity == "moderate":
        severity = "medium"
    cwes = [str(c).strip() for c in (db_spec.get("cwe_ids") or []) if isinstance(c, str)]

    pkgs: list[str] = []
    ecosystems: list[str] = []
    affected_lines: list[str] = []
    for aff in (data.get("affected") or [])[:8]:
        if not isinstance(aff, dict):
            continue
        pkg = aff.get("package") or {}
        eco = str(pkg.get("ecosystem") or "").strip()
        name = str(pkg.get("name") or "").strip()
        if not name:
            continue
        if eco and eco not in ecosystems:
            ecosystems.append(eco)
        if name not in pkgs:
            pkgs.append(name)
        ver_parts: list[str] = []
        for rng in (aff.get("ranges") or [])[:4]:
            if not isinstance(rng, dict):
                continue
            for ev in rng.get("events") or []:
                if isinstance(ev, dict):
                    if ev.get("introduced"):
                        ver_parts.append(f">={ev['introduced']}")
                    if ev.get("fixed"):
                        ver_parts.append(f"fixed:{ev['fixed']}")
                    if ev.get("last_affected"):
                        ver_parts.append(f"<={ev['last_affected']}")
        affected_lines.append(f"- `{eco}:{name}` ({', '.join(ver_parts[:6])})" if ver_parts else f"- `{eco}:{name}`")

    refs = [
        str(r.get("url") or "").strip()
        for r in (data.get("references") or [])[:6]
        if isinstance(r, dict) and r.get("url")
    ]
    heading = f"{ghsa_id}" + (f" ({cves[0]})" if cves else "") + (f": {summary}" if summary else "")
    body_lines = [
        f"# {heading}",
        f"Severity: {severity or 'unknown'}" + (f" | CWE: {', '.join(cwes[:4])}" if cwes else ""),
    ]
    if affected_lines:
        body_lines.append("\n### Affected Packages\n" + "\n".join(affected_lines))
    if details:
        body_lines.append("\n### Details\n" + details[:6000])
    if refs:
        body_lines.append("\n### References\n" + "\n".join(f"- {u}" for u in refs))
    body = "\n".join(body_lines).strip()

    eco_label = ecosystems[0] if ecosystems else "ecosystem"
    bc = f"GHSA > {eco_label} > {pkgs[0] if pkgs else ghsa_id} ({ghsa_id})"
    is_high_signal = severity in ("high", "critical")
    cve_entries = [
        (c, severity, False, f"[{ghsa_id} / {eco_label} {', '.join(pkgs[:2])}] {summary[:200]}")
        for c in cves[:5]
    ] or None

    return {
        "title": heading[:240],
        "category": "cve",
        "tags": list(dict.fromkeys(["ghsa", severity, *ecosystems[:3], *pkgs[:3], *cwes[:3]])),
        "cves": cves,
        "url": f"https://github.com/advisories/{ghsa_id}",
        "full_text": body,
        "sections": [(heading[:140], bc, body, _split_section_to_chunks(body))],
        "cve_entries": cve_entries,
        "is_high_signal": is_high_signal,
    }


def parse_semgrep_yaml(raw_yaml: str, rel_path: str) -> Optional[dict[str, Any]]:
    """Parse security-category Semgrep rules into whitebox source-code auditing patterns."""
    if not raw_yaml or "security" not in raw_yaml.lower():
        return None
    try:
        data = yaml.safe_load(raw_yaml)
    except Exception:
        return None
    if not isinstance(data, dict) or not isinstance(data.get("rules"), list):
        return None

    sec_rules: list[dict[str, Any]] = []
    for r in data["rules"]:
        if not isinstance(r, dict):
            continue
        meta = r.get("metadata") or {}
        if not isinstance(meta, dict):
            continue
        cat = str(meta.get("category") or "").strip().lower()
        if cat != "security" and "/security/" not in rel_path.lower():
            continue
        sec_rules.append(r)
    if not sec_rules:
        return None

    lang = Path(rel_path).parts[0] if Path(rel_path).parts else "generic"
    sections: list[tuple[str, str, str, list[str]]] = []
    all_parts: list[str] = []
    all_tags: list[str] = ["semgrep", "whitebox", "source-review", lang]
    all_cves: list[str] = []

    for r in sec_rules[:10]:
        rid = str(r.get("id") or Path(rel_path).stem).strip()
        msg = str(r.get("message") or "").strip()
        sev = str(r.get("severity") or "").strip().lower()
        meta = r.get("metadata") or {}
        cwes = meta.get("cwe") or []
        if isinstance(cwes, str):
            cwes = [cwes]
        owasp = meta.get("owasp") or []
        if isinstance(owasp, str):
            owasp = [owasp]
        vclasses = meta.get("vulnerability_class") or []
        if isinstance(vclasses, str):
            vclasses = [vclasses]
        refs = [str(u) for u in (meta.get("references") or [])[:4] if isinstance(u, str)]

        # Extract concise code-sink pattern representation
        pat_raw = r.get("pattern") or r.get("patterns") or r.get("pattern-either") or r.get("pattern-sinks") or ""
        if isinstance(pat_raw, str):
            pat_str = pat_raw.strip()[:1000]
        else:
            try:
                pat_str = yaml.safe_dump(pat_raw, sort_keys=False)[:1000].strip()
            except Exception:
                pat_str = ""

        sec_lines = [
            f"## {rid} ({lang}, {sev or 'warning'})",
            f"CWE: {', '.join(str(x) for x in cwes[:3])}" if cwes else "",
            f"Class: {', '.join(str(x) for x in vclasses[:3])} | OWASP: {', '.join(str(x) for x in owasp[:2])}"
            if (vclasses or owasp)
            else "",
            msg,
        ]
        if pat_str:
            sec_lines.append(f"```yaml\n{pat_str}\n```")
        if refs:
            sec_lines.append("References:\n" + "\n".join(f"- {u}" for u in refs))
        sec_text = "\n\n".join(x for x in sec_lines if x).strip()
        if len(sec_text) < 60:
            continue
        all_parts.append(sec_text)
        bc = f"Semgrep Whitebox > {lang} > {rid}"
        sections.append((rid, bc, sec_text, _split_section_to_chunks(sec_text)))
        for x in [*vclasses[:2], *cwes[:2]]:
            all_tags.append(str(x))
        all_cves.extend(extract_cves(sec_text))

    if not sections:
        return None
    title = f"Semgrep [{lang}]: {sections[0][0]}"[:220]
    full_text = "\n\n".join(all_parts)[:30_000]
    return {
        "title": title,
        "category": "detection",
        "tags": list(dict.fromkeys(all_tags))[:12],
        "cves": list(dict.fromkeys(all_cves)),
        "url": f"https://github.com/semgrep/semgrep-rules/blob/develop/{rel_path}",
        "full_text": full_text,
        "sections": sections,
        "cve_entries": None,
    }
