#!/usr/bin/env python3
"""Reject wall-of-text prose in the documentation people read.

A paragraph that carries a dozen facts in one block is hard to scan even when
every fact is correct. The docs style is one idea per short paragraph, a list
or table for three or more parallel items, and roughly 110 words or fewer in a
prose paragraph. This check is mechanical so a later edit cannot quietly
reintroduce a blob.

Generated pages are excluded: docs/settings.md restates the schema's help text,
so its row length is owned by internal/config, not by this file. Tables, lists,
blockquotes, code and headings are structural and are not counted.
"""
from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
MAX_WORDS = 110
# Generated from a schema or a capture; editing the page would be wrong.
GENERATED = {"docs/settings.md"}
SOURCES = (
    "README.md",
    "CONTRIBUTING.md",
    "SECURITY.md",
    "SUPPORT.md",
)
DOCS = "docs"


def prose_paragraphs(text: str) -> list[tuple[int, str]]:
    """Yield (line, paragraph) for prose blocks, skipping structure."""
    lines = text.splitlines()
    paragraphs: list[tuple[int, str]] = []
    block: list[str] = []
    start = 0
    fence = False
    front_matter = False
    in_comment = False
    list_indent: int | None = None

    def flush() -> None:
        if block:
            paragraphs.append((start, "\n".join(block)))
            block.clear()

    for number, raw in enumerate(lines, 1):
        line = raw.rstrip()
        stripped = line.strip()
        if number == 1 and stripped == "---":
            front_matter = True
            continue
        if front_matter:
            if stripped == "---":
                front_matter = False
            continue
        if stripped.startswith("```") or stripped.startswith("~~~"):
            flush()
            fence = not fence
            continue
        if fence:
            continue
        if in_comment:
            if "-->" in stripped:
                in_comment = False
            continue
        if stripped.startswith("<!--"):
            flush()
            if "-->" not in stripped:
                in_comment = True
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        marker = re.match(r"^(?:[-*+]|\d+\.)\s", stripped)
        if list_indent is not None:
            # A list item's continuation lines are list content even though they
            # read like prose. The list ends at a nonblank line that is neither
            # indented past the marker nor another marker.
            if not stripped or marker or indent > list_indent:
                flush()
                if marker:
                    list_indent = indent
                continue
            list_indent = None
        if marker:
            flush()
            list_indent = indent
            continue
        structural = (
            not stripped
            or stripped.startswith(("#", "|", ">", "<"))
            or raw.startswith(("    ", "\t"))
        )
        if structural:
            flush()
            continue
        if not block:
            start = number
        block.append(stripped)
    flush()
    return paragraphs


def source_files() -> list[Path]:
    files = [ROOT / name for name in SOURCES]
    files.extend(sorted((ROOT / DOCS).rglob("*.md")))
    return [path for path in files if path.is_file()]


def main() -> int:
    errors: list[str] = []
    for path in source_files():
        relative = path.relative_to(ROOT).as_posix()
        if relative in GENERATED:
            continue
        for line, paragraph in prose_paragraphs(path.read_text(encoding="utf-8")):
            words = len(paragraph.split())
            if words <= MAX_WORDS:
                continue
            opening = " ".join(paragraph.split())[:64]
            errors.append(
                f"{relative}:{line}: {words}-word paragraph (limit {MAX_WORDS}): {opening}...")
    if errors:
        for error in errors:
            print(error)
        print(f"docs readability: {len(errors)} paragraph(s) over {MAX_WORDS} words; "
              "split the idea, or use a list or table")
        return 1
    print("docs readability: clean")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
