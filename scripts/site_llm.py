#!/usr/bin/env python3
"""Machine-readable docs outputs from the one site page model.

`build_site.py` owns page discovery, titles, descriptions and rendered section
text. This module turns that model into the artifacts an LLM or an MCP server
reads, so navigation, search, `llms.txt`, Markdown copies and the embedded MCP
index cannot drift apart. It writes only under the site output; the checked-in
MCP copy is produced by `build_site.py --write-manifest`.
"""
from __future__ import annotations

import hashlib
import json
from pathlib import Path

MANIFEST_SCHEMA = 1
GENERATED_BY = "scripts/build_site.py"


def markdown_name(output: str) -> str:
    """The Markdown twin of a rendered page, beside its HTML output."""
    return output[:-5] + ".md" if output.endswith(".html") else output + ".md"


def _group_for(output: str, nav_groups) -> str:
    for label, entries in nav_groups:
        for _text, target, _source in entries:
            if target.split("#", 1)[0] == output:
                return label
    return "Project"


def page_records(pages, search_index, nav_groups, *, root: Path, base_url: str, version: str) -> list[dict]:
    """Join the build's page and search models into one ordered record list.

    The source Markdown is the prose owner; title, description, group and
    sections come from the same values the HTML page and search index use.
    """
    search_by_url = {entry["url"]: entry for entry in search_index}
    records: list[dict] = []
    for order, page in enumerate(pages):
        source = page.source
        if source is None:
            continue
        raw = source.read_text(encoding="utf-8")
        search = search_by_url.get(page.output, {})
        url = base_url.rstrip("/") + "/" + page.output
        markdown_url = base_url.rstrip("/") + "/" + markdown_name(page.output)
        records.append({
            "path": source.relative_to(root).as_posix(),
            "slug": Path(page.output).with_suffix("").as_posix(),
            "output": page.output,
            "markdown": markdown_name(page.output),
            "url": url,
            "markdown_url": markdown_url,
            "title": page.title,
            "description": page.description,
            "group": _group_for(page.output, nav_groups),
            "order": order,
            "bytes": len(raw.encode("utf-8")),
            "sha256": hashlib.sha256(raw.encode("utf-8")).hexdigest(),
            "sections": search.get("sections", []),
            "markdown_text": raw,
        })
    return records


def manifest(records: list[dict], *, base_url: str, version: str) -> dict:
    return {
        "schema": MANIFEST_SCHEMA,
        "generated_by": GENERATED_BY,
        "base_url": base_url.rstrip("/") + "/",
        "version": version,
        "pages": records,
    }


def _site_path(record: dict) -> str:
    return record["output"]


def write_outputs(site_root: Path, records: list[dict], *, base_url: str, version: str) -> None:
    """Write per-page Markdown, the JSON index, llms.txt and llms-full.txt."""
    for record in records:
        target = site_root / record["markdown"]
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(record["markdown_text"], encoding="utf-8")

    payload = manifest(records, base_url=base_url, version=version)
    (site_root / "docs.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    summary = (
        "Self-hosted, cross-protocol inference proxy with a live dashboard, "
        "SQLite history, and an operator plane. One Go binary; no hosted dependency."
    )
    lines = ["# millivolt", "", f"> {summary}", ""]
    seen_group = None
    for record in records:
        group = record["group"]
        if group != seen_group:
            lines.extend(["", f"## {group}", ""])
            seen_group = group
        description = record["description"].strip()
        lines.append(f"- [{record['title']}]({record['markdown_url']}): {description}")
    lines.extend(["", "## Machine-readable", "",
                  f"- [All documentation in one file]({base_url.rstrip('/')}/llms-full.txt)",
                  f"- [JSON page and section index]({base_url.rstrip('/')}/docs.json)"])
    (site_root / "llms.txt").write_text("\n".join(lines) + "\n", encoding="utf-8")

    full = [f"# millivolt documentation", "", f"> {summary}", ""]
    for record in records:
        full.extend([f"<!-- {record['path']} -->", "", record["markdown_text"].rstrip(), "",
                     "---", ""])
    (site_root / "llms-full.txt").write_text("\n".join(full), encoding="utf-8")


def write_checked_manifest(site_root: Path, target: Path) -> None:
    """Copy the generated JSON index into the Go embed. No second writer."""
    source = site_root / "docs.json"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(source.read_text(encoding="utf-8"), encoding="utf-8")
