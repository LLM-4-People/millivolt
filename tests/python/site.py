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
import tempfile
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit

from scripts.build_site import DEFAULT_BASE_URL, README_ANCHORS, build_site, image_size
from tests.repository_check import markdown_anchors
from tests.support import (PALETTE_OWNER, ROOT, SITE_STYLESHEET, root_custom_properties,
                           site_palette_errors, token_drift_errors)

BRAND_COPIES = {
    "assets/mark.svg": "internal/web/static/favicon.svg",
    "assets/favicon.ico": "internal/web/static/favicon.ico",
    "assets/apple-touch-icon.png": "internal/web/static/apple-touch-icon.png",
}
STALE_SOURCE_ASSETS = (
    "site-src/assets/mark.svg",
    "site-src/assets/favicon.ico",
    "site-src/assets/apple-touch-icon.png",
    "site-src/assets/social-card.png",
)
# Runtime-only assets may live in the published tree without a markup or
# stylesheet reference. The list stays empty unless a genuine case appears.
RUNTIME_ASSET_ALLOWLIST = frozenset()


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


def asset_reference_errors(site: Path, texts: list[str]) -> list[str]:
    """Every published URL asset needs a consumer. Generated HTML, templates,
    JS and CSS all count; an allowlist entry must name a genuinely runtime-only
    asset, never an old duplicate.
    """
    corpus = "\n".join(texts)
    errors = []
    for asset in sorted((site / "assets").rglob("*")):
        if not asset.is_file():
            continue
        rel = asset.relative_to(site).as_posix()
        if rel in RUNTIME_ASSET_ALLOWLIST:
            continue
        if rel not in corpus:
            errors.append(f"published asset {rel} is not referenced by generated HTML, "
                          "a template, JavaScript, or CSS")
    return errors


def entity_color_errors() -> list[str]:
    """The landing's entity swatches consume the dashboard's entity registry.
    The site cannot import the JS registry at build time, so pin the used
    values to entities.js instead of hand-copying a second palette."""
    registry = (ROOT / "internal/web/static/js/entities.js").read_text(encoding="utf-8")
    canonical = {value.lower() for value in re.findall(r"color:\s*'(#[0-9a-fA-F]{6})'", registry)}
    landing = (ROOT / "site-src/templates/landing.html").read_text(encoding="utf-8")
    used = {value.lower() for value in re.findall(r"--entity:\s*(#[0-9a-fA-F]{6})", landing)}
    if not canonical:
        return ["entities.js has no entity color registry"]
    return [f"landing.html entity color {value} is not in entities.js"
            for value in sorted(used - canonical)]


def check_site(site: Path) -> list[str]:
    errors: list[str] = []
    pages = sorted(site.rglob("*.html"))
    page_texts = {page: page.read_text(encoding="utf-8") for page in pages}
    parsed: dict[Path, PageParser] = {}
    for page in pages:
        parser = PageParser()
        parser.feed(page_texts[page])
        parsed[page] = parser
    canonical_css = (ROOT / PALETTE_OWNER).read_text(encoding="utf-8")
    canonical_tokens = root_custom_properties(canonical_css)
    theme_color = canonical_tokens.get("--rail", "")
    if not theme_color:
        errors.append(f"{PALETTE_OWNER}: missing --rail for the site theme color")
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
        if theme_color and parser.meta.get("theme-color") != theme_color:
            errors.append(f"{rel}: theme-color {parser.meta.get('theme-color')!r} does not match "
                          f"the canonical --rail {theme_color!r}")
        if 'class="skip-link"' not in page_texts[page]:
            errors.append(f"{rel}: missing skip link")
        if 'id="main"' not in page_texts[page]:
            errors.append(f"{rel}: missing main landmark")
        if re.search(r"<a(?:href|class|aria|target|rel|id)=", page_texts[page]):
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
                continue
            try:
                declared = (int(image["width"]), int(image["height"]))
            except ValueError:
                errors.append(f"{rel}: image has non-integer dimensions: {src}")
                continue
            intrinsic = image_size(target)
            if intrinsic is None:
                continue
            if target.suffix.lower() == ".svg":
                # SVG is scalable, so the declared layout size may differ from
                # the intrinsic width/height. The aspect ratio may not drift.
                if declared[0] * intrinsic[1] != declared[1] * intrinsic[0]:
                    errors.append(f"{rel}: image dimensions {declared[0]}x{declared[1]} do not "
                                  f"preserve the {intrinsic[0]}x{intrinsic[1]} aspect ratio of {src}")
            elif declared != intrinsic:
                errors.append(f"{rel}: image dimensions {declared[0]}x{declared[1]} do not match "
                              f"the {intrinsic[0]}x{intrinsic[1]} intrinsic size of {src}")
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
        styles = [Path(urlsplit(href).path).name for href in parser.styles]
        if ("tokens.css" in styles and "site.css" in styles
                and styles.index("tokens.css") > styles.index("site.css")):
            errors.append(f"{rel}: assets/tokens.css must load before assets/site.css")
        text = page_texts[page]
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
        if "See every AI request" not in page_texts.get(site / "index.html", ""):
            errors.append("landing is missing the product headline")
    readme_anchors = markdown_anchors((ROOT / "README.md").read_text(encoding="utf-8"))
    missing_readme = sorted(README_ANCHORS - readme_anchors)
    if missing_readme:
        errors.append("README is missing anchors used by the site: " + ", ".join(missing_readme))
    errors.extend(entity_color_errors())

    for name in ("docs/index.html", "docs/dashboard.html", "docs/operations.html",
                 "contributing.html", "security.html", "404.html", "search.json",
                 "sitemap.xml", "robots.txt", "assets/tokens.css", "assets/site.css",
                 "assets/site.js", "assets/og.png"):
        if not (site / name).exists():
            errors.append(f"missing generated file: {name}")

    tokens_path = site / "assets/tokens.css"
    if tokens_path.exists():
        errors.extend(token_drift_errors(
            canonical_css, tokens_path.read_text(encoding="utf-8"), "assets/tokens.css"))

    site_css_path = ROOT / SITE_STYLESHEET
    if not site_css_path.exists():
        errors.append(f"missing site stylesheet: {SITE_STYLESHEET}")
    else:
        errors.extend(site_palette_errors([
            (PALETTE_OWNER, canonical_css),
            (SITE_STYLESHEET, site_css_path.read_text(encoding="utf-8")),
        ]))

    base_template = (ROOT / "site-src/templates/base.html").read_text(encoding="utf-8")
    literal = re.search(r"#[0-9a-fA-F]{3,8}", base_template)
    if literal:
        errors.append(f"base.html carries the literal palette value {literal.group(0)}; "
                      "use a build token")

    for built_name, source_name in BRAND_COPIES.items():
        built = site / built_name
        source = ROOT / source_name
        if not source.is_file():
            errors.append(f"missing canonical brand asset: {source_name}")
        elif not built.is_file():
            errors.append(f"missing generated brand asset: {built_name}")
        elif built.read_bytes() != source.read_bytes():
            errors.append(f"generated {built_name} is not byte-identical to {source_name}")

    for name in STALE_SOURCE_ASSETS:
        if (ROOT / name).exists():
            errors.append(f"duplicate source asset still present: {name}")
    if (site / "assets/social-card.png").exists():
        errors.append("published social-card.png has no consumer")

    reference_texts = list(page_texts.values())
    for pattern in ("*.js", "*.css"):
        for path in sorted((site / "assets").rglob(pattern)):
            reference_texts.append(path.read_text(encoding="utf-8"))
    for template in sorted((ROOT / "site-src/templates").glob("*.html")):
        reference_texts.append(template.read_text(encoding="utf-8"))
    errors.extend(asset_reference_errors(site, reference_texts))

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
    if og.exists():
        og_size = image_size(og)
        if og_size != (1200, 630):
            errors.append(f"og.png dimensions are {og_size}, expected 1200x630")

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
