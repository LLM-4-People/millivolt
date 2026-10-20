#!/usr/bin/env python3
"""Validate the generated static site without a browser.

The docs build is small but easy to break at the boundaries: a repository link
can escape the site, a heading anchor can drift, or a generated image can lose
its dimensions. This module builds into a temporary directory and checks the
observable contract.
"""

from __future__ import annotations

import json
import re
import struct
import tempfile
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit

from scripts.build_site import DEFAULT_BASE_URL, README_ANCHORS, build_site
from tests.repository_check import markdown_anchors


class PageParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.h1 = 0
        self.ids: set[str] = set()
        self.links: list[tuple[str, str]] = []
        self.images: list[dict[str, str]] = []
        self.scripts: list[str] = []
        self.styles: list[str] = []
        self.meta: dict[str, str] = {}
        self.title = ""
        self._in_title = False

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        values = {key: value or "" for key, value in attrs}
        if "id" in values:
            self.ids.add(values["id"])
        if tag == "h1":
            self.h1 += 1
        if tag == "a" and "href" in values:
            self.links.append(("href", values["href"]))
        if tag == "img":
            self.images.append(values)
        if tag == "script" and values.get("src"):
            self.scripts.append(values["src"])
        if tag == "link" and values.get("rel") == "stylesheet":
            self.styles.append(values.get("href", ""))
        if tag == "meta":
            key = values.get("name") or values.get("property")
            if key:
                self.meta[key] = values.get("content", "")
        if tag == "title":
            self._in_title = True

    def handle_endtag(self, tag: str) -> None:
        if tag == "title":
            self._in_title = False

    def handle_data(self, data: str) -> None:
        if self._in_title:
            self.title += data


def png_size(path: Path) -> tuple[int, int] | None:
    try:
        data = path.read_bytes()[:24]
    except OSError:
        return None
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        return None
    width, height = struct.unpack(">II", data[16:24])
    return width, height


def local_target(site: Path, page: Path, url: str) -> tuple[Path | None, str]:
    split = urlsplit(url)
    if split.scheme or split.netloc or url.startswith(("#", "mailto:", "tel:", "data:")):
        return None, split.fragment
    path = unquote(split.path)
    if not path:
        return None, split.fragment
    target = (site / page.parent / path).resolve()
    if target.is_dir():
        target = target / "index.html"
    return target, split.fragment


def check_site(site: Path) -> list[str]:
    errors: list[str] = []
    pages = sorted(site.rglob("*.html"))
    parsed: dict[Path, PageParser] = {}
    for page in pages:
        parser = PageParser()
        parser.feed(page.read_text(encoding="utf-8"))
        parsed[page] = parser
    for page in pages:
        parser = parsed[page]
        rel = page.relative_to(site).as_posix()
        if parser.h1 != 1:
            errors.append(f"{rel}: expected exactly one h1, found {parser.h1}")
        if not parser.title.strip():
            errors.append(f"{rel}: empty title")
        if "millivolt" not in parser.title:
            errors.append(f"{rel}: title does not name millivolt")
        for key in ("description", "viewport", "og:title", "og:description", "og:image", "twitter:card"):
            if not parser.meta.get(key):
                errors.append(f"{rel}: missing meta {key}")
        if parser.meta.get("og:image") and not parser.meta["og:image"].startswith(DEFAULT_BASE_URL):
            errors.append(f"{rel}: og:image is not absolute and on the default base")
        if 'class="skip-link"' not in page.read_text(encoding="utf-8"):
            errors.append(f"{rel}: missing skip link")
        if 'id="main"' not in page.read_text(encoding="utf-8"):
            errors.append(f"{rel}: missing main landmark")
        if re.search(r"<a(?:href|class|aria|target|rel|id)=", page.read_text(encoding="utf-8")):
            errors.append(f"{rel}: malformed anchor tag")
        for image in parser.images:
            src = image.get("src", "")
            if not src:
                errors.append(f"{rel}: image without src")
                continue
            if src.startswith(("http://", "https://", "data:")):
                continue
            target, _fragment = local_target(site, page, src)
            if not target or not target.exists():
                errors.append(f"{rel}: missing image {src}")
                continue
            if "alt" not in image:
                errors.append(f"{rel}: image missing alt text: {src}")
            if not image.get("width") or not image.get("height"):
                errors.append(f"{rel}: image missing dimensions: {src}")
        for _attr, value in parser.links:
            if not value.startswith(("http://", "https://", "mailto:", "tel:", "#")) and value.endswith(
                ("README.md", "CONTRIBUTING.md", "SECURITY.md")
            ):
                errors.append(f"{rel}: repository markdown link was not rewritten: {value}")
            if value.startswith("#") and value[1:] and value[1:] not in parser.ids:
                errors.append(f"{rel}: missing same-page anchor {value}")
            target, fragment = local_target(site, page, value)
            if target is None:
                continue
            if not target.exists():
                errors.append(f"{rel}: missing link target {value}")
                continue
            if fragment and target.suffix == ".html":
                ids = parsed[target].ids if target in parsed else set()
                if fragment not in ids:
                    errors.append(f"{rel}: missing anchor #{fragment} in {target.relative_to(site)}")
        for src in parser.scripts:
            if src.startswith(("http://", "https://")):
                errors.append(f"{rel}: external script {src}")
                continue
            target, _fragment = local_target(site, page, src)
            if not target or not target.exists():
                errors.append(f"{rel}: missing script {src}")
        for href in parser.styles:
            if href.startswith(("http://", "https://")):
                errors.append(f"{rel}: external stylesheet {href}")
                continue
            target, _fragment = local_target(site, page, href)
            if not target or not target.exists():
                errors.append(f"{rel}: missing stylesheet {href}")
        text = page.read_text(encoding="utf-8")
        if "{{title}}" in text or "{{root}}" in text or "{{body}}" in text:
            errors.append(f"{rel}: unresolved base token")
        if "fonts.googleapis" in text or "fonts.gstatic" in text:
            errors.append(f"{rel}: external font request")

    landing = parsed.get(site / "index.html")
    if not landing:
        errors.append("missing index.html")
    else:
        missing = sorted(README_ANCHORS - landing.ids)
        if missing:
            errors.append("landing is missing README anchors: " + ", ".join(missing))
        if "See every AI request" not in (site / "index.html").read_text(encoding="utf-8"):
            errors.append("landing is missing the product headline")
    readme_anchors = markdown_anchors((Path(__file__).resolve().parents[2] / "README.md").read_text(encoding="utf-8"))
    missing_readme = sorted(README_ANCHORS - readme_anchors)
    if missing_readme:
        errors.append("README is missing anchors used by the site: " + ", ".join(missing_readme))

    for name in ("docs/index.html", "docs/dashboard.html", "docs/operations.html", "contributing.html", "security.html", "404.html", "search.json", "sitemap.xml", "robots.txt", "assets/site.css", "assets/site.js", "assets/og.png"):
        if not (site / name).exists():
            errors.append(f"missing generated file: {name}")

    search_path = site / "search.json"
    if search_path.exists():
        try:
            entries = json.loads(search_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError as exc:
            errors.append(f"search.json is not valid JSON: {exc}")
            entries = []
        if not isinstance(entries, list) or len(entries) < 8:
            errors.append("search.json does not contain the docs pages")
        for entry in entries:
            url = entry.get("url", "")
            if not url or not (site / url).exists():
                errors.append(f"search entry has missing URL: {url!r}")
            if not entry.get("title"):
                errors.append(f"search entry has no title: {url!r}")
            if not entry.get("description"):
                errors.append(f"search entry has no description: {url!r}")

    og = site / "assets/og.png"
    if og.exists() and png_size(og) != (1200, 630):
        errors.append(f"og.png dimensions are {png_size(og)}, expected 1200x630")

    sitemap = (site / "sitemap.xml").read_text(encoding="utf-8") if (site / "sitemap.xml").exists() else ""
    for page in pages:
        if page.name == "404.html":
            continue
        rel = "" if page == site / "index.html" else page.relative_to(site).as_posix()
        if rel and rel not in sitemap:
            errors.append(f"sitemap missing {rel}")
    if "<loc>" not in sitemap:
        errors.append("sitemap has no URLs")
    return errors


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="millivolt-site-") as temp:
        site = Path(temp) / "site"
        try:
            outputs = build_site(site)
        except Exception as exc:  # noqa: BLE001 - the build error is the result
            print(f"site: build failed: {exc}")
            return 1
        if len(outputs) < 12:
            print(f"site: expected at least 12 outputs, got {len(outputs)}")
            return 1
        errors = check_site(site)
    if errors:
        for error in errors:
            print("site: " + error)
        return 1
    print("site: clean")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
