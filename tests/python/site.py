#!/usr/bin/env python3
"""Validate the generated static site without a browser.

The docs build is small but easy to break at the boundaries: a repository link
can escape the site, a heading anchor can drift, or a generated image can lose
its dimensions. This module builds into a temporary directory and checks the
observable contract.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import tempfile
from html import unescape as html_unescape
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit

from scripts.build_site import (DEFAULT_BASE_URL, README_ANCHORS, build_site, image_size,
                                markdown_engine, performance_headline, render_fragment,
                                site_fonts, toc_html)
from scripts.perf_docs import check as performance_drift_errors
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
RUNTIME_ASSET_ALLOWLIST = frozenset({
    # The pre-hash names. A document cached before the content-hashed deploy
    # still asks for these; they carry the current bytes and disappear only
    # after every client cache has turned over. No page links them.
    "assets/tokens.css", "assets/site.css", "assets/site.js",
})
# The shell files at content-hashed URLs: each generation is served for a
# document cached by an earlier deployment, so none of them has an in-repo
# consumer. The current generation is asserted separately.
SHELL_GENERATION = re.compile(r"assets/(?:tokens|site)\.[0-9a-f]{10}\.(?:css|js)$")


class PageParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.h1 = 0
        self.ids: set[str] = set()
        self.heading_ids: list[str] = []
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
            if tag in ("h2", "h3") and values["id"] != "search-title":
                # search-title is the dialog's own heading in page chrome, not
                # article content, so the index rightly does not carry it.
                self.heading_ids.append(values["id"])
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


def local_target(site: Path, page: Path, url: str, *, base_path: str = "/") -> tuple[Path | None, str]:
    split = urlsplit(url)
    if split.scheme or split.netloc or url.startswith(("#", "mailto:", "tel:", "data:")):
        return None, split.fragment
    path = unquote(split.path)
    if not path:
        return None, split.fragment
    absolute = path.startswith("/")
    if base_path != "/" and path.startswith(base_path):
        path = path[len(base_path):]
    if path.startswith("/"):
        path = path.lstrip("/")
        absolute = True
    base = site if absolute else site / page.parent
    target = (base / path).resolve()
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
        if rel in RUNTIME_ASSET_ALLOWLIST or SHELL_GENERATION.fullmatch(rel):
            continue
        # A stylesheet inside assets/ names a sibling by its path under
        # assets/ (for example url("fonts/instrument-sans.woff2")), while
        # HTML names the published path. Either spelling is a real consumer.
        local = rel.removeprefix("assets/")
        if rel not in corpus and local not in corpus:
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


BRAND_IMAGE_CONTRACTS = {
    "docs/images/brand/readme-cover-dark.png": (1600, 640),
    "docs/images/brand/readme-cover-light.png": (1600, 640),
    "docs/images/brand/readme-spec.svg": (1600, 144),
    "docs/images/brand/readme-spec-mobile.svg": (800, 384),
    "site-src/assets/og.png": (1200, 630),
    ".github/social-preview.png": (1280, 640),
}


def brand_image_errors() -> list[str]:
    """Pin the dimensions of artwork that GitHub renders or a maintainer
    uploads. The PWA icon pixel contracts live in the Go web tests."""
    errors = []
    for name, expected in BRAND_IMAGE_CONTRACTS.items():
        actual = image_size(ROOT / name)
        if actual != expected:
            errors.append(f"{name}: dimensions are {actual}, expected {expected[0]}x{expected[1]}")
    return errors


PERFORMANCE_SECTIONS = (
    "artifact-and-idle-footprint",
    "method",
    "fast-response-throughput",
    "sustained-capacity",
    "streaming-concurrency",
    "streaming-throughput",
    "durable-writer-and-overload",
    "storage-and-retained-projection",
    "query-and-metadata-paths",
    "implementation-benchmarks",
    "reproduce",
    "limitations",
)


def performance_page_errors(site: Path, page_texts: dict[Path, str]) -> list[str]:
    """The measured page is the single numeric source, and the landing cards
    are generated from it. Pin the report structure and the generated headline
    so a docs edit cannot silently drop a table, a method section or a value."""
    errors = []
    page = site / "docs/performance.html"
    if not page.is_file():
        return ["missing generated performance page: docs/performance.html"]
    text = page_texts.get(page, "")
    for section in PERFORMANCE_SECTIONS:
        if f'id="{section}"' not in text:
            errors.append(f"performance page is missing section #{section}")
    if text.count("<table>") < 12:
        errors.append(f"performance page has {text.count('<table>')} tables, want at least 12")
    normalized = re.sub(r"\s+", " ", text)
    for phrase in ("not deployment guarantees", "generated from the result bundle", "Reproduce"):
        if phrase not in normalized:
            errors.append(f"performance page is missing {phrase!r}")
    # The page is generated, not edited: a bundle or page change that is not
    # rendered together fails here instead of publishing a stale number.
    errors.extend(performance_drift_errors(ROOT))
    landing = page_texts.get(site / "index.html", "")
    try:
        headline = performance_headline()
    except Exception as exc:
        errors.append(f"performance headline could not be read: {exc}")
    else:
        for key, value in headline.items():
            if value not in landing:
                errors.append(f"landing is missing generated {key} value {value!r}")
    for path, body in page_texts.items():
        if "{{perf_" in body:
            errors.append(f"{path.relative_to(site)}: unresolved performance token")
    readme = (ROOT / "README.md").read_text(encoding="utf-8")
    docs_index = (ROOT / "docs/README.md").read_text(encoding="utf-8")
    if "docs/performance.md" not in readme:
        errors.append("README does not link to the canonical performance page")
    if "performance.md" not in docs_index:
        errors.append("docs/README.md does not link to the canonical performance page")
    operations = (ROOT / "docs/operations.md").read_text(encoding="utf-8")
    if "### Measured example" in operations:
        errors.append("operations.md still carries a second measured example")
    return errors


def referenced_shell(site: Path, stem: str, extension: str) -> str:
    """The content-hashed name every page must reference for this asset."""
    text = (site / "index.html").read_text(encoding="utf-8")
    match = re.search(rf"assets/({stem}\.[0-9a-f]{{10}}\.{extension})", text)
    return match.group(1) if match else ""


def shell_asset_errors(site: Path) -> list[str]:
    """The three shell files load at content-hashed URLs.

    A cached document and a new deployment must never pair a page with an
    incompatible stylesheet or script: the URL names the bytes. The name is
    verified against the file, the stable spelling must not appear in any page,
    and the generations the build retained from history must be on disk so a
    document still holding the previous hash keeps its styling.
    """
    import scripts.build_site as build_site

    errors: list[str] = []
    for stem, extension in (("tokens", "css"), ("site", "css"), ("site", "js")):
        name = referenced_shell(site, stem, extension)
        if not name:
            errors.append(f"no page references a content-hashed {stem}.{extension}")
            continue
        path = site / "assets" / name
        if not path.is_file():
            errors.append(f"missing referenced asset: assets/{name}")
            continue
        digest = hashlib.sha256(path.read_bytes()).hexdigest()[:10]
        if not name.startswith(f"{stem}.{digest}."):
            errors.append(f"assets/{name} does not carry its content hash")
    for page in sorted(site.rglob("*.html")):
        text = page.read_text(encoding="utf-8")
        for stable in ("assets/tokens.css", "assets/site.css", "assets/site.js"):
            if stable in text:
                errors.append(f"{page.relative_to(site)}: references the unhashed {stable}")
    if (ROOT / ".git").exists():
        retained = build_site.retained_shell_assets()
        for key, generations in retained.items():
            stem, extension, _stable = build_site.SHELL_ASSETS[key]
            for content in generations:
                name = build_site.asset_filename(stem, extension, content)
                if not (site / "assets" / name).exists():
                    errors.append(f"retained generation assets/{name} was not published")
    return errors


def css_declarations(block: str) -> dict[str, str]:
    declarations: dict[str, str] = {}
    for piece in block.split(";"):
        key, separator, value = piece.partition(":")
        if separator:
            declarations[key.strip()] = value.strip()
    return declarations


def docs_shell_layout_errors(site: Path) -> list[str]:
    """Docs pages spend available width on the article, not on empty gutters.

    Reference tables wrap cells when the article inherits the marketing
    measure. The docs frame is wider than the marketing shell, every docs
    header and footer shares it, and the article has no cap of its own. Prose
    keeps its 70ch measure, so the extra width goes to tables and code.
    """
    name = referenced_shell(site, "site", "css")
    if not name:
        return ["no page references the site stylesheet"]
    css = (site / "assets" / name).read_text(encoding="utf-8")
    errors: list[str] = []
    open_rule = re.findall(r"\.doc-main\s*\{([^}]*)\}", css)
    if not open_rule:
        errors.append("site stylesheet has no docs article rule")
    for block in open_rule:
        article = css_declarations(block)
        if "max-width" in article:
            errors.append(f"the docs article caps its width at {article['max-width']}")
    shell_rule = re.search(r"(?m)^\.docs-shell\s*\{([^}]*)\}", css)
    if not shell_rule:
        errors.append("site stylesheet has no docs shell rule")
    else:
        shell = css_declarations(shell_rule.group(1))
        if shell.get("width") != "var(--docs-width)":
            errors.append("the docs shell does not use the shared docs frame width")
    wrap_rule = re.search(r"body\.docs \.wrap\s*\{([^}]*)\}", css)
    if not wrap_rule:
        errors.append("the docs header and footer do not share the docs frame")
    elif css_declarations(wrap_rule.group(1)).get("width") != "var(--docs-width)":
        errors.append("the docs header and footer use a different width than the docs shell")
    if not re.search(r"--docs-width:\s*min\(var\(--docs-max\),\s*calc\(100% - 48px\)\)", css):
        errors.append("the docs frame width is not derived from --docs-max with the standard gutter")
    return errors


def not_found_base_errors() -> list[str]:
    """A nested 404 must use the absolute path of whatever base it deploys on."""
    errors: list[str] = []
    for base_url, prefix in (
        ("https://example.com/", "/"),
        ("https://example.com/prefix/", "/prefix/"),
    ):
        with tempfile.TemporaryDirectory(prefix="millivolt-404-") as temp:
            site = Path(temp) / "site"
            build_site(site, base_url=base_url)
            text = (site / "404.html").read_text(encoding="utf-8")
            site_css = referenced_shell(site, "site", "css")
            for needle in (
                f'href="{prefix}assets/{site_css}"',
                f'href="{prefix}index.html"',
                'name="robots" content="noindex,follow"',
                f'property="og:url" content="{base_url}"',
            ):
                if needle not in text:
                    errors.append(f"404.html at {base_url} is missing {needle}")
            if 'rel="canonical"' in text:
                errors.append(f"404.html at {base_url} must not declare a canonical URL")
    return errors


def content_safety_errors() -> list[str]:
    """Pin the docs content allowlist at the one render choke point.

    python-markdown passes raw HTML, `attr_list` attribute blocks and arbitrary
    link schemes through unchanged, so a docs contribution could otherwise ship
    script to the published origin. Every case renders through
    `render_fragment`, which is the real pipeline stage, so the test fails when
    the sanitizer is dropped from the pipeline and not only when the sanitizer
    itself is weakened.
    """
    errors = []
    payloads = (
        ("inline script", "<p>x</p><script>alert(1)</script>", ("<script",)),
        ("img onerror", '<img src="https://example.test/a.png" alt="a" onerror="alert(1)">',
         ("onerror",)),
        ("svg onload", '<svg onload="alert(1)"></svg>', ("<svg", "onload")),
        ("iframe srcdoc", '<iframe srcdoc="<script>alert(1)</script>"></iframe>',
         ("<iframe", "<script")),
        ("base href", '<base href="https://attacker.example/">', ("<base",)),
        ("meta refresh", '<meta http-equiv="refresh" content="0;url=https://x">',
         ("<meta", "http-equiv")),
        ("form action", '<form action="https://attacker.example/steal"></form>', ("<form",)),
        ("object data", '<object data="https://attacker.example/x"></object>', ("<object",)),
        ("style block", "<style>body{display:none}</style>", ("<style",)),
        ("attr_list onclick", '<p onclick="alert(1)">t</p>', ("onclick",)),
        ("attr_list style", '<p style="position:fixed;inset:0">t</p>', ("position:fixed",)),
        ("javascript link", '<a href="javascript:alert(1)">x</a>', ("javascript:",)),
        ("case-folded javascript", '<a href="JaVaScRiPt:alert(1)">x</a>', ("javascript:",)),
        ("data link", '<a href="data:text/html;base64,AAA">x</a>', ("data:text/html",)),
        ("details ontoggle", '<details open ontoggle="alert(1)">d</details>', ("ontoggle",)),
        # ammonia scheme-checks URL attributes, not the comma-separated
        # srcset candidate list, so the value filter owns that check.
        ("srcset javascript", '<picture><source srcset="javascript:alert(1)">'
         '<img src="https://example.test/a.png" alt="a"></picture>', ("javascript:",)),
        ("srcset data", '<picture><source srcset="data:image/svg+xml;base64,AAA">'
         '<img src="https://example.test/a.png" alt="a"></picture>', ("data:image",)),
        # The builder owns target/rel: an author-supplied pair must not survive
        # to be re-emitted before the builder's rewrite (which only matched an
        # anchor whose href came first).
        ("author target and rel", '<a target="_blank" href="https://example.test/">x</a>'
         '<a rel="opener" href="https://example.test/">y</a>', ('rel="opener"',)),
    )
    with tempfile.TemporaryDirectory(prefix="millivolt-content-") as temp:
        site = Path(temp) / "site"
        site.mkdir()
        source = Path(temp) / "fixture.md"
        source.write_text("# fixture\n", encoding="utf-8")

        def render(text: str) -> str:
            return render_fragment(markdown_engine(), text, source, "docs/fixture.html", site)

        for name, payload, banned in payloads:
            output = render(payload)
            lowered = output.lower()
            for needle in banned:
                if needle in lowered:
                    errors.append(f"site: pipeline let {name} through: {output!r}")
        # Positive cases: every construct the docs and landing actually use must
        # survive, so the allowlist cannot be tightened into silent breakage.
        kept = (
            ('<table><thead><tr><th style="text-align: right;">n</th></tr></thead></table>',
             '<th style="text-align: right;">'),
            ("<p><kbd>01</kbd><sub>pre-1.0</sub></p>", "<kbd>01</kbd>"),
            ("~~~sh\ngo run .\n~~~", 'class="language-sh"'),
            ("## A heading\n\ntext\n", 'class="headerlink"'),
            ('<a href="https://example.test/x">d</a>', 'href="https://example.test/x"'),
            ("[q](#quickstart)", 'href="#quickstart"'),
            ('<a href="mailto:a@b.c">m</a>', 'href="mailto:a@b.c"'),
            ('<picture><source media="(min-width: 761px)" srcset="https://example.test/a.svg 1x, '
             'https://example.test/b.svg 2x"><img src="https://example.test/b.png" alt="x" width="800">'
             '</picture>', 'srcset="https://example.test/a.svg 1x, https://example.test/b.svg 2x"'),
        )
        for payload, expected in kept:
            if expected not in render(payload):
                errors.append(f"site: pipeline dropped a legitimate construct: {payload!r}")
    return errors


def performance_headline_escape_errors() -> list[str]:
    """The landing headline values are the one bundle-derived sink outside the
    Markdown sanitizer.

    The landing page is assembled from `site-src/templates/landing.html`, so
    `sanitize_content` never sees it. The values come from
    `perf/results.json`; a hand-edited or hostile bundle must reach the page as
    literal text, never as markup. The date and revision are format-checked in
    the reader, and every value is escaped at the sink.
    """
    import scripts.build_site as build_site

    errors: list[str] = []
    payload = '<img src="https://e.test/x.png" onerror="window.__pwned=1">PAWNED'
    original = build_site.performance_headline
    build_site.performance_headline = lambda: {
        key: payload for key in ("perf_image", "perf_idle", "perf_loaded", "perf_rps",
                                 "perf_sustained", "perf_date", "perf_revision",
                                 "perf_revision_full", "perf_platform")
    }
    try:
        page = build_site.render_landing(
            version="0.0.0", base_url="https://example.test/", theme_color="#000000",
            assets={"asset_tokens": "tokens.0000000000.css",
                    "asset_site_css": "site.0000000000.css",
                    "asset_site_js": "site.0000000000.js"})
    finally:
        build_site.performance_headline = original
    if '<img src="https://e.test/x.png"' in page or 'onerror="window.__pwned=1"' in page:
        errors.append("site: a performance value reaches the landing template as markup")
    if "PAWNED" not in page:
        errors.append("site: an escaped performance value lost its literal text")
    return errors


def toc_escape_errors() -> list[str]:
    """The table of contents must escape heading text a second time.

    A heading written as `Looks safe &lt;svg onload=...&gt;` renders as inert
    text in the article, because Markdown escapes it. The TOC builder strips
    markup and unescapes character references, so re-emitting that value raw
    turned the escaped text into a live element in the sidebar.
    """
    name = 'Looks safe &lt;svg onload="alert(document.domain)"&gt;'
    rendered = toc_html([{"level": 2, "id": "looks-safe", "name": name, "children": []}])
    errors = []
    if "<svg" in rendered:
        errors.append(f"site: table of contents emitted live markup: {rendered}")
    if "&lt;svg" not in rendered:
        errors.append(f"site: table of contents did not preserve the heading text: {rendered}")
    return errors


def docs_image_errors() -> list[str]:
    """Every published docs image needs a Markdown consumer.

    The built-site pass only sees images its pages already reference, so an
    orphan under docs/images survives it forever. The gallery is published from
    a capture run, and a recaptured folder that loses its last reference is
    exactly the drift this catches.
    """
    referenced: set[str] = set()
    for path in ROOT.rglob("*.md"):
        if not {"node_modules", ".venv", ".git", "agents"}.isdisjoint(
                path.relative_to(ROOT).parts):
            continue
        for match in re.finditer(r"images/[A-Za-z0-9_./-]+", path.read_text(encoding="utf-8")):
            referenced.add(match.group(0))
    return [f"docs/{rel.relative_to(ROOT / 'docs').as_posix()}: no Markdown consumer"
            for rel in ROOT.joinpath("docs/images").rglob("*")
            if rel.is_file()
            and rel.relative_to(ROOT / 'docs').as_posix() not in referenced]


def check_site(site: Path, *, base_url: str = DEFAULT_BASE_URL) -> list[str]:
    base_path = urlsplit(base_url).path or "/"
    errors: list[str] = docs_image_errors()
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
        if parser.meta.get("og:image") and not parser.meta["og:image"].startswith(base_url):
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
            target, _fragment = local_target(site, page, src, base_path=base_path)
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
            target, fragment = local_target(site, page, value, base_path=base_path)
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
            target, _fragment = local_target(site, page, src, base_path=base_path)
            if not target or not target.exists():
                errors.append(f"{rel}: missing script {src}")
        for href in parser.styles:
            if href.startswith(("http://", "https://")):
                errors.append(f"{rel}: external stylesheet {href}")
                continue
            target, _fragment = local_target(site, page, href, base_path=base_path)
            if not target or not target.exists():
                errors.append(f"{rel}: missing stylesheet {href}")
        styles = [Path(urlsplit(href).path).name for href in parser.styles]
        tokens_index = next((index for index, name in enumerate(styles)
                             if name.startswith("tokens.")), None)
        site_index = next((index for index, name in enumerate(styles)
                           if name.startswith("site.")), None)
        if tokens_index is not None and site_index is not None and tokens_index > site_index:
            errors.append(f"{rel}: the token stylesheet must load before the site stylesheet")
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
    errors.extend(brand_image_errors())

    not_found = page_texts.get(site / "404.html", "")
    if 'name="robots" content="noindex,follow"' not in not_found:
        errors.append("404.html must be noindex,follow")
    if 'rel="canonical"' in not_found:
        errors.append("404.html must not declare a canonical URL")
    if f'href="{base_path}assets/{referenced_shell(site, "site", "css")}"' not in not_found:
        errors.append("404.html must load site assets from the absolute site root")

    for name in ("docs/index.html", "docs/dashboard.html", "docs/operations.html",
                 "contributing.html", "security.html", "404.html", "search.json",
                 "sitemap.xml", "robots.txt", "assets/og.png"):
        if not (site / name).exists():
            errors.append(f"missing generated file: {name}")
    for stem, extension in (("tokens", "css"), ("site", "css"), ("site", "js")):
        if not referenced_shell(site, stem, extension):
            errors.append(f"no page references a content-hashed {stem}.{extension}")
    # One published origin: the metadata a crawler reads names the base URL, so
    # a build cannot half-migrate to a custom domain or leak a fork's origin
    # into canonical, social or sitemap output. Ordinary outbound links to the
    # repository are expected and stay untouched.
    for page in pages:
        rel = page.relative_to(site).as_posix()
        for key in ("og:url", "og:image", "twitter:image"):
            value = parsed[page].meta.get(key, "")
            if value and not value.startswith(base_url):
                errors.append(f"{rel}: {key} {value!r} is not on the published base {base_url}")
        if page == site / "index.html":
            for key in ("og:image:alt", "twitter:image:alt"):
                value = parsed[page].meta.get(key, "")
                if "not a benchmark" not in value:
                    errors.append(f"index.html: {key} must label the dashboard values as a sample")
        canonical = re.search(r'<link rel="canonical" href="([^"]+)"',
                              page_texts[page])
        if canonical and not html_unescape(canonical.group(1)).startswith(base_url):
            errors.append(f"{rel}: canonical {canonical.group(1)!r} is not on the published base")
    published_host = urlsplit(base_url).hostname or ""
    sitemap = (site / "sitemap.xml").read_text(encoding="utf-8")
    for loc in re.findall(r"<loc>([^<]+)</loc>", sitemap):
        if not loc.startswith(base_url):
            errors.append(f"sitemap.xml: {loc!r} is not on the published base")
            break
    robots = (site / "robots.txt").read_text(encoding="utf-8")
    for found in re.findall(r"^Sitemap: https?://([A-Za-z0-9.-]+)", robots, re.M):
        if found != published_host:
            errors.append(f"robots.txt: sitemap names {found} instead of the published host")

    tokens_name = referenced_shell(site, "tokens", "css")
    tokens_path = site / "assets" / tokens_name
    if tokens_path.exists():
        errors.extend(token_drift_errors(
            canonical_css, tokens_path.read_text(encoding="utf-8"), f"assets/{tokens_name}"))
    else:
        errors.append(f"missing referenced token stylesheet: assets/{tokens_name}")

    errors.extend(shell_asset_errors(site))
    errors.extend(docs_shell_layout_errors(site))

    site_css_path = ROOT / SITE_STYLESHEET
    if not site_css_path.exists():
        errors.append(f"missing site stylesheet: {SITE_STYLESHEET}")
    else:
        errors.extend(site_palette_errors([
            (PALETTE_OWNER, canonical_css),
            (SITE_STYLESHEET, site_css_path.read_text(encoding="utf-8")),
        ]))

    # The reading shell is a contract, not a coat of paint: every docs page
    # carries a breadcrumb (except the docs home), a page footer with at least
    # one neighbour, and an open contents rail that the script collapses only
    # when it becomes an inline panel.
    for page in pages:
        rel = page.relative_to(site).as_posix()
        if not rel.startswith("docs/") or rel == "docs/index.html":
            continue
        text = page_texts[page]
        if '<nav class="crumbs"' not in text:
            errors.append(f"{rel}: docs page has no breadcrumb")
        if 'class="page-nav"' not in text or "page-nav-link" not in text:
            errors.append(f"{rel}: docs page has no previous/next footer")
        if '<details class="toc" open>' not in text:
            errors.append(f"{rel}: contents rail is not open in the markup")
        if 'mono-label">documentation' in text:
            errors.append(f"{rel}: the retired eyebrow still precedes the title")
        if '<details class="docs-nav" open>' not in text:
            errors.append(f"{rel}: docs menu is not a labelled disclosure")
    operations = page_texts.get(site / "docs/operations.html", "")
    if 'class="callout callout-warning"' not in operations:
        errors.append("docs/operations.html: the exposure warning is not a callout")

    # Search indexes sections, not whole pages: a long reference page must be
    # reachable at every heading, and a section link must land on a real anchor.
    try:
        index = json.loads((site / "search.json").read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        errors.append(f"search.json is unreadable: {exc}")
        index = []
    for entry in index:
        page = site / entry["url"]
        if not page.exists():
            errors.append(f"search.json names a missing page: {entry['url']}")
            continue
        ids = parsed[page].ids if page in parsed else set()
        for section in entry.get("sections", []):
            if section["id"] not in ids:
                errors.append(f"search.json: {entry['url']} has no anchor #{section['id']}")
                break
        indexed = [section["id"] for section in entry.get("sections", [])]
        if indexed != parsed[page].heading_ids:
            errors.append(f"search.json: {entry['url']} indexes {len(indexed)} of "
                          f"{len(parsed[page].heading_ids)} headings, or in a different order")

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

    for relative in site_fonts():
        built = site / "assets" / relative
        source = ROOT / "internal/web/static" / relative
        if not source.is_file():
            errors.append(f"missing canonical font: internal/web/static/{relative}")
        elif not built.is_file():
            errors.append(f"missing published font: assets/{relative}")
        elif built.read_bytes()[:4] != b"wOF2":
            errors.append(f"published font assets/{relative} is not a WOFF2 file")
        elif built.read_bytes() != source.read_bytes():
            errors.append(f"published font assets/{relative} is not byte-identical to its source")
    if not site_fonts():
        errors.append("site.css requests no self-hosted font")
    tokens_css = (site / "assets" / referenced_shell(site, "tokens", "css")).read_text(encoding="utf-8")
    for family in ('"Instrument Sans"', '"JetBrains Mono"'):
        if family not in tokens_css:
            errors.append(f"tokens.css does not carry the {family} family from dashboard.css")
    site_css = (site / "assets" / referenced_shell(site, "site", "css")).read_text(encoding="utf-8")
    for external in ("fonts.googleapis", "fonts.gstatic", "url(http"):
        if external in site_css:
            errors.append(f"site.css makes an external font or asset request: {external}")
    if "var(--display)" not in site_css:
        errors.append("site.css does not use the display face for titles")

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

    sitemap = (site / "sitemap.xml").read_text(encoding="utf-8") if (site / "sitemap.xml").exists() else ""
    for page in pages:
        if page.name == "404.html":
            continue
        rel = "" if page == site / "index.html" else page.relative_to(site).as_posix()
        if rel and rel not in sitemap:
            errors.append(f"sitemap missing {rel}")
    if "<loc>" not in sitemap:
        errors.append("sitemap has no URLs")
    errors.extend(performance_page_errors(site, page_texts))
    errors.extend(performance_headline_escape_errors())
    errors.extend(not_found_base_errors())
    errors.extend(content_safety_errors())
    errors.extend(toc_escape_errors())
    return errors


def main() -> int:
    # The workflow builds for the live Pages base, so the checker validates the
    # same origin it publishes: a subpath deployment and a custom domain fail
    # on different things.
    base_url = os.environ.get("SITE_URL", DEFAULT_BASE_URL)
    with tempfile.TemporaryDirectory(prefix="millivolt-site-") as temp:
        site = Path(temp) / "site"
        try:
            outputs = build_site(site, base_url=base_url)
        except Exception as exc:  # noqa: BLE001 - the build error is the result
            print(f"site: build failed: {exc}")
            return 1
        if len(outputs) < 12:
            print(f"site: expected at least 12 outputs, got {len(outputs)}")
            return 1
        errors = check_site(site, base_url=base_url)
    if errors:
        for error in errors:
            print("site: " + error)
        return 1
    print("site: clean")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
