#!/usr/bin/env python3
"""Build the static millivolt site from the canonical Markdown sources.

The site is deliberately a small, dependency-light static build. Markdown stays
in its existing owners; this script renders it, rewrites repository links for a
browser, copies the canonical brand assets, derives the site token stylesheet
from the dashboard palette, and generates the navigation, search index and SEO
files.
"""

from __future__ import annotations

import argparse
import html
import json
import os
import re
import shutil
import struct
import sys
import textwrap
from dataclasses import dataclass
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urljoin, urlsplit, urlunsplit

import markdown
import nh3
from markdown.extensions.toc import TocExtension

ROOT = Path(__file__).resolve().parent.parent
SITE_SRC = ROOT / "site-src"
TEMPLATES = SITE_SRC / "templates"
# The published site's canonical origin. The Pages workflow passes the live base
# URL from `actions/configure-pages`, so a preview or a fork builds with its own
# origin; the repository default is the one the site is published at.
CUSTOM_DOMAIN = "millivolt.llm4people.dev"
DEFAULT_BASE_URL = f"https://{CUSTOM_DOMAIN}/"
GITHUB_REPOSITORY = "https://github.com/LLM-4-People/millivolt"
GITHUB_BRANCH = "main"
GITHUB_BLOB = f"{GITHUB_REPOSITORY}/blob/{GITHUB_BRANCH}/"
GITHUB_TREE = f"{GITHUB_REPOSITORY}/tree/{GITHUB_BRANCH}/"
GITHUB_EDIT = f"{GITHUB_REPOSITORY}/edit/{GITHUB_BRANCH}/"

sys.path.insert(0, str(ROOT))
from scripts.perf_docs import DataError, landing_metrics  # noqa: E402
from tests.repository_check import heading_slug  # noqa: E402
from tests.support import root_custom_properties  # noqa: E402

DOCS = ROOT / "docs"
DOC_IMAGES = DOCS / "images"
DASHBOARD_CSS = ROOT / "internal/web/static/css/dashboard.css"
BRAND_SOURCE = ROOT / "internal/web/static"
# The dashboard's brand files are canonical. The site publishes them under
# the names its template already uses.
BRAND_ASSETS = {
    "mark.svg": BRAND_SOURCE / "favicon.svg",
    "favicon.ico": BRAND_SOURCE / "favicon.ico",
    "apple-touch-icon.png": BRAND_SOURCE / "apple-touch-icon.png",
}
# README-only artwork stays in the repository for GitHub's renderer; no
# generated site page references it.
SITE_IMAGE_EXCLUDES = frozenset({
    "brand/readme-cover-dark.png",
    "brand/readme-cover-light.png",
    "brand/readme-spec.svg",
    "brand/readme-spec-mobile.svg",
})

ROOT_PAGE_MAP = {
    "README.md": "index.html",
    "CONTRIBUTING.md": "contributing.html",
    "SECURITY.md": "security.html",
    "LICENSE": "license.html",
    "SUPPORT.md": "support.html",
    "THIRD_PARTY_NOTICES.md": "third-party.html",
    "docs/README.md": "docs/index.html",
}

NAV_GROUPS = (
    (
        "Start",
        (
            ("Documentation", "docs/index.html", "docs/README.md"),
            ("Install and first run", "docs/install.html", "docs/install.md"),
            ("Dashboard tour", "docs/dashboard.html", "docs/dashboard.md"),
            ("Protocol and clients", "docs/protocol.html", "docs/protocol.md"),
        ),
    ),
    (
        "Integrate",
        (
            ("Adapters", "docs/adapters.html", "docs/adapters.md"),
            ("MCP server", "docs/mcp.html", "docs/mcp.md"),
            ("Operator CLI", "docs/cli.html", "docs/cli.md"),
        ),
    ),
    (
        "Operate",
        (
            ("Operations", "docs/operations.html", "docs/operations.md"),
            ("Configuration", "docs/configuration.html", "docs/configuration.md"),
            ("Settings reference", "docs/settings.html", "docs/settings.md"),
            ("Containers and data", "docs/containers.html", "docs/containers.md"),
            ("Routes and debugging", "docs/routes.html", "docs/routes.md"),
            ("Reverse proxy", "docs/reverse-proxy.html", "docs/reverse-proxy.md"),
            ("Performance", "docs/performance.html", "docs/performance.md"),
            ("Security", "security.html", "SECURITY.md"),
        ),
    ),
    (
        "Extend",
        (
            ("Architecture", "docs/architecture.html", "docs/architecture.md"),
            ("Contributing", "contributing.html", "CONTRIBUTING.md"),
        ),
    ),
    (
        "Project",
        (
            ("Support", "support.html", "SUPPORT.md"),
            ("License", "license.html", "LICENSE"),
            ("Third-party notices", "third-party.html", "THIRD_PARTY_NOTICES.md"),
        ),
    ),
)

# README anchors are referenced from the docs. The landing page owns the
# corresponding visible sections so those links keep working on the site.
README_ANCHORS = {
    "quickstart",
    "docker-compose",
    "connect-a-client",
    "what-you-get",
    "dashboard-overview",
    "performance-and-footprint",
}

ATTRIBUTE = re.compile(r'\b(href|src)="([^"]*)"')
EXTERNAL = re.compile(r"^([a-zA-Z][a-zA-Z0-9+.-]*:|//)")
HEADING = re.compile(r"^#\s+(.+?)\s*$", re.MULTILINE)
PARAGRAPH = re.compile(r"<p>(.*?)</p>", re.DOTALL)
TAG = re.compile(r"<[^>]+>")
SPACE = re.compile(r"\s+")
MARKDOWN_SUFFIXES = (".md", ".markdown")
IMAGE_SUFFIXES = (".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico")


@dataclass(frozen=True)
class Page:
    source: Path | None
    output: str
    title: str
    description: str
    kind: str


class TextExtractor(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.parts: list[str] = []
        self.skip = 0

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if tag in {"script", "style", "nav", "footer"}:
            self.skip += 1

    def handle_endtag(self, tag: str) -> None:
        if tag in {"script", "style", "nav", "footer"} and self.skip:
            self.skip -= 1

    def handle_data(self, data: str) -> None:
        if not self.skip:
            self.parts.append(data)


def plain_text(fragment: str) -> str:
    parser = TextExtractor()
    parser.feed(fragment)
    return SPACE.sub(" ", html.unescape(" ".join(parser.parts))).strip()


def strip_html(fragment: str) -> str:
    return SPACE.sub(" ", html.unescape(TAG.sub("", fragment))).strip()


# Content allowlist for one rendered document. The article body is the one
# place author Markdown becomes HTML, and python-markdown passes raw HTML
# through unsanitized, so the builder declares what a document may contain and
# drops everything else. The set is measured from the current artifact: these
# are the tags the pipeline and the Markdown extensions actually emit.
CONTENT_TAGS = frozenset({
    "a", "blockquote", "br", "code", "dd", "dl", "dt", "em", "figcaption",
    "figure", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "img", "kbd", "li",
    "ol", "p", "picture", "pre", "source", "strong", "sub", "sup", "table",
    "tbody", "td", "th", "thead", "tr", "ul",
})
# Attributes the pipeline owns or the extensions add. `style` and `class` are
# value-restricted below rather than dropped: table alignment and the fenced
# code language marker are both produced by Markdown itself.
CONTENT_ATTRIBUTES = {
    "a": {"class", "href", "title"},
    "code": {"class"},
    "h1": {"id"}, "h2": {"id"}, "h3": {"id"},
    "h4": {"id"}, "h5": {"id"}, "h6": {"id"},
    "img": {"alt", "decoding", "height", "loading", "src", "width"},
    "source": {"media", "srcset", "type"},
    "td": {"style"},
    "th": {"style"},
}
# The only classes the builder emits. The TOC permalink anchor and the fenced
# code language marker are the two producers.
CONTENT_CLASSES = frozenset({"headerlink"}) | {
    f"language-{name}" for name in (
        "bash", "console", "css", "csv", "diff", "dockerfile", "go", "html",
        "http", "ini", "javascript", "json", "jsonc", "makefile", "markdown",
        "nginx", "powershell", "promql", "python", "sh", "sql", "text",
        "toml", "xml", "yaml",
    )
}
# The one inline style the pipeline needs: table column alignment, which
# Markdown emits for its `| ---: |` syntax.
CONTENT_TEXT_ALIGN = re.compile(r"\s*text-align\s*:\s*(?:left|right|center)\s*;?\s*")
# Schemes a document link or image may use. Relative paths, fragments and
# protocol-relative URLs carry no scheme and are admitted by the sanitizer;
# `data:` is not, because a document has no reason to inline one and the
# pipeline's own image handling rejects it.
CONTENT_URL_SCHEMES = frozenset({"http", "https", "mailto", "tel"})


def _content_attribute_filter(tag: str, attribute: str, value: str) -> str | None:
    """Return the attribute value, or None to drop it.

    This is the value-level half of the content allowlist: the tag and
    attribute names are already restricted by CONTENT_TAGS and
    CONTENT_ATTRIBUTES, and this stops a permitted attribute from carrying
    something the builder never emits.
    """
    if attribute == "class":
        parts = value.split()
        return value if parts and set(parts) <= CONTENT_CLASSES else None
    if attribute == "style":
        return value.strip() if CONTENT_TEXT_ALIGN.fullmatch(value) else None
    if attribute == "srcset":
        # ammonia scheme-checks URL attributes like `src` and `href`, not
        # `srcset`: it is a comma-separated candidate list. Check every
        # candidate here, or a `javascript:`/`data:` candidate would ride a
        # permitted attribute past the scheme allowlist.
        candidates = [part.strip() for part in value.split(",") if part.strip()]
        if not candidates:
            return None
        for candidate in candidates:
            url = candidate.split()[0]
            if not _content_url_allowed(url):
                return None
        return value
    return value


def _content_url_allowed(url: str) -> bool:
    """Report whether a document URL may be emitted.

    Relative paths, fragments and protocol-relative URLs are admitted (the
    sanitizer's own URL handling and the build's rewrite stage bound them);
    anything carrying a scheme must be in CONTENT_URL_SCHEMES.
    """
    if not url or url.startswith(("#", "//")):
        return True
    scheme, separator, _rest = url.partition(":")
    if not separator:
        return True
    if not scheme:
        return False
    return scheme.lower() in CONTENT_URL_SCHEMES


def sanitize_content(content: str) -> str:
    """Sanitize one rendered document at the single render choke point.

    Every doc page flows from `md.convert`, so this is the lowest common path
    and no page can bypass it. python-markdown passes raw HTML, `attr_list`
    attribute blocks and `javascript:`-style link targets through unchanged;
    this admits only the declared element, attribute and URL-scheme set. The
    pipeline's own post-processing (link rewriting, image dimensions) runs
    after this and only ever adds build-owned values.
    """
    return nh3.clean(
        content,
        tags=CONTENT_TAGS,
        attributes=CONTENT_ATTRIBUTES,
        attribute_filter=_content_attribute_filter,
        url_schemes=CONTENT_URL_SCHEMES,
        link_rel=None,
    )


def summarize(text: str, width: int = 158) -> str:
    """Trim a description at a word boundary instead of cutting mid-token."""
    compact = SPACE.sub(" ", text).strip()
    if not compact:
        return ""
    return textwrap.shorten(compact, width=width, placeholder="...")


def attr_escape(value: str) -> str:
    return html.escape(value, quote=True)


def read_version() -> str:
    return (ROOT / "VERSION").read_text(encoding="utf-8").strip()


def page_relative(target: str, current_output: str) -> str:
    """Return a site-root target as a URL relative to the current page."""
    path, separator, fragment = target.partition("#")
    if not path:
        return "#" + fragment
    start = os.path.dirname(current_output)
    relative = os.path.relpath(path, start or ".")
    if relative == ".":
        relative = "./"
    return relative + (separator + fragment if separator else "")


def link_attrs(target: str, current_output: str, *, external_new_tab: bool = True) -> str:
    url = target
    extra = ""
    if target.startswith("http://") or target.startswith("https://"):
        if external_new_tab:
            extra = ' target="_blank" rel="noopener noreferrer"'
    elif target.endswith("#quickstart") or "#" in target and target.startswith("index.html"):
        url = page_relative(target, current_output)
    elif not target.startswith("#") and not target.startswith("mailto:") and not target.startswith("tel:"):
        url = page_relative(target, current_output)
    return f'href="{attr_escape(url)}"{extra}'


def output_is_active(target: str, current_output: str) -> bool:
    path = target.split("#", 1)[0]
    return bool(path) and path == current_output


def nav_links(current_output: str) -> str:
    blocks: list[str] = []
    for label, entries in NAV_GROUPS:
        links: list[str] = []
        for text, target, _source in entries:
            active = output_is_active(target, current_output)
            aria = ' aria-current="page"' if active else ""
            css = ' class="active"' if active else ""
            links.append(
                f'<a {link_attrs(target, current_output)}{css}{aria}>{html.escape(text)}</a>'
            )
        blocks.append(
            '<div class="nav-group">'
            f'<p class="nav-label">{html.escape(label)}</p>'
            + "".join(links)
            + "</div>"
        )
    return "".join(blocks)


def nav_group_of(output: str) -> str:
    """Return the nav group a page belongs to, for its breadcrumb."""
    for label, entries in NAV_GROUPS:
        for _text, target, _source in entries:
            if target.split("#", 1)[0] == output:
                return label
    return "Docs"


def flatten_nav() -> list[tuple[str, str, str]]:
    pages: list[tuple[str, str, str]] = []
    for _label, entries in NAV_GROUPS:
        for text, target, source in entries:
            if source and target.split("#", 1)[0].endswith(".html"):
                pages.append((text, target.split("#", 1)[0], source))
    return pages


def previous_next(current_output: str) -> tuple[tuple[str, str] | None, tuple[str, str] | None]:
    pages = flatten_nav()
    for index, (text, target, _source) in enumerate(pages):
        if target == current_output:
            previous = (pages[index - 1][0], pages[index - 1][1]) if index else None
            following = (pages[index + 1][0], pages[index + 1][1]) if index + 1 < len(pages) else None
            return previous, following
    return None, None


def image_size(path: Path) -> tuple[int, int] | None:
    """Read PNG or SVG dimensions without requiring an image library."""
    try:
        if path.suffix.lower() == ".png":
            data = path.read_bytes()[:24]
            if data[:8] != b"\x89PNG\r\n\x1a\n":
                return None
            width, height = struct.unpack(">II", data[16:24])
            return width, height
        if path.suffix.lower() == ".svg":
            head = path.read_text(encoding="utf-8", errors="replace")[:1000]
            width = re.search(r'\bwidth="(\d+)"', head)
            height = re.search(r'\bheight="(\d+)"', head)
            if width and height:
                return int(width.group(1)), int(height.group(1))
    except (OSError, ValueError, struct.error):
        return None
    return None


def site_page_for(source: Path) -> str | None:
    key = source.relative_to(ROOT).as_posix()
    if key in ROOT_PAGE_MAP:
        return ROOT_PAGE_MAP[key]
    if key.startswith("docs/") and key.endswith(".md"):
        stem = Path(key).stem
        if stem == "README":
            return "docs/index.html"
        return f"docs/{stem}.html"
    return None


def external_repo_url(target: Path, fragment: str = "") -> str:
    relative = target.relative_to(ROOT).as_posix()
    base = GITHUB_TREE if target.is_dir() else GITHUB_BLOB
    url = base + relative
    return url + ("#" + fragment if fragment else "")


def rewrite_url(url: str, source: Path, output: str) -> str:
    """Resolve one Markdown URL against its source file."""
    if not url or url.startswith("#") or url.startswith("data:"):
        return url
    split = urlsplit(url)
    if split.scheme or split.netloc:
        return url
    raw_path = split.path
    if not raw_path:
        return url
    target = (source.parent / raw_path).resolve()
    try:
        target.relative_to(ROOT)
    except ValueError as exc:
        raise BuildError(f"{source}: link escapes the repository: {url}") from exc
    fragment = split.fragment

    if target.is_dir():
        index = target / "README.md"
        if index.exists():
            target = index
        else:
            return external_repo_url(target, fragment)

    if target.suffix.lower() in MARKDOWN_SUFFIXES or target.name == "LICENSE":
        site_target = site_page_for(target)
        if site_target:
            relative = page_relative(site_target, output)
            if fragment:
                relative += "#" + fragment
            return relative
        if target.exists():
            return external_repo_url(target, fragment)
        raise BuildError(f"{source}: missing document target: {url}")

    if target.suffix.lower() in IMAGE_SUFFIXES:
        try:
            relative_image = target.relative_to(DOC_IMAGES)
        except ValueError:
            relative_image = None
        if relative_image is not None:
            return page_relative("assets/images/" + relative_image.as_posix(), output) + (
                "#" + fragment if fragment else ""
            )
        if target.exists():
            return external_repo_url(target, fragment)
        raise BuildError(f"{source}: missing image target: {url}")

    if target.exists():
        return external_repo_url(target, fragment)
    raise BuildError(f"{source}: missing local target: {url}")


def rewrite_content_urls(content: str, source: Path, output: str) -> str:
    def replace(match: re.Match[str]) -> str:
        attr, value = match.group(1), match.group(2)
        resolved = rewrite_url(html.unescape(value), source, output)
        return f'{attr}="{attr_escape(resolved)}"'

    content = ATTRIBUTE.sub(replace, content)
    content = re.sub(
        r'<a href="(https?://[^"]+)"',
        r'<a href="\1" target="_blank" rel="noopener noreferrer"',
        content,
    )
    return content


def enhance_images_with_root(content: str, output: str, site_root: Path) -> str:
    def replace(match: re.Match[str]) -> str:
        tag = match.group(0)
        src_match = re.search(r'\bsrc="([^"]+)"', tag)
        if not src_match:
            return tag
        src = html.unescape(src_match.group(1))
        if src.startswith(("http://", "https://", "data:")):
            return tag
        target = (site_root / Path(output).parent / src).resolve()
        additions = []
        size = image_size(target)
        if size:
            additions.append(f'width="{size[0]}" height="{size[1]}"')
        if "loading=" not in tag:
            additions.append('loading="lazy"')
        if "decoding=" not in tag:
            additions.append('decoding="async"')
        if additions:
            tag = tag[:-1].rstrip() + " " + " ".join(additions) + ">"
        return tag

    return re.sub(r"<img\b[^>]*>", replace, content)


def markdown_engine() -> markdown.Markdown:
    """The one Markdown configuration for every rendered document."""
    return markdown.Markdown(
        extensions=[
            "extra",
            "sane_lists",
            TocExtension(
                slugify=lambda value, separator: heading_slug(value),
                separator="-",
                toc_depth="2-3",
                permalink=True,
                permalink_class="headerlink",
                permalink_title="Link to this section",
            ),
        ],
        output_format="html5",
    )


# GitHub alert syntax: a blockquote whose first paragraph opens with
# `[!KIND]`. The set is closed because each kind maps to one label and one
# icon; an unknown marker stays a plain blockquote rather than inventing a
# callout nobody styled.
CALLOUT_KINDS = {
    "NOTE": ("Note", '<circle cx="7" cy="7" r="5.4"/><path d="M7 6.5v4"/><circle cx="7" cy="4.3" r=".7" fill="currentColor" stroke="none"/>'),
    "TIP": ("Tip", '<path d="M4.6 10.4a4 4 0 1 1 4.8 0v1.4H4.6z"/><path d="M6 13.4h2"/>'),
    "IMPORTANT": ("Important", '<path d="M7 1.9 13 12H1z"/><path d="M7 5.6v3.3"/><circle cx="7" cy="10.7" r=".6" fill="currentColor" stroke="none"/>'),
    "WARNING": ("Warning", '<path d="M7 1.9 13 12H1z"/><path d="M7 5.6v3.3"/><circle cx="7" cy="10.7" r=".6" fill="currentColor" stroke="none"/>'),
    "CAUTION": ("Caution", '<path d="M7 1.9 13 12H1z"/><path d="M7 5.6v3.3"/><circle cx="7" cy="10.7" r=".6" fill="currentColor" stroke="none"/>'),
}
# Raw-Markdown form of the same alerts, used when a page's description is
# derived before rendering. The blockquote's own continuation lines go with the
# marker, so a warning never becomes the page summary.
CALLOUT_SOURCE = re.compile(
    r"^>\s*\[!(?:NOTE|TIP|IMPORTANT|WARNING|CAUTION)\][^\n]*(?:\n>.*)*\n*", re.M
)
CALLOUT_BLOCK = re.compile(
    r"<blockquote>\s*<p>\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*(.*?)</p>(.*?)</blockquote>",
    re.S,
)


def render_callouts(content: str) -> str:
    """Turn GitHub alert blockquotes into the docs callout vocabulary.

    Runs after sanitizing and link rewriting, so the blockquote and its inner
    content are already final and this only wraps them in build-owned markup.
    """

    def replace(match: re.Match[str]) -> str:
        kind = match.group(1)
        label, icon = CALLOUT_KINDS[kind]
        first, rest = match.group(2), match.group(3)
        return (
            f'<aside class="callout callout-{kind.lower()}" role="note">'
            f'<p class="callout-label"><svg class="callout-icon" viewBox="0 0 14 14" '
            f'aria-hidden="true">{icon}</svg>{label}</p>'
            f"<p>{first}</p>{rest}</aside>"
        )

    return CALLOUT_BLOCK.sub(replace, content)


def render_fragment(
    md: markdown.Markdown, text: str, source: Path, output: str, site_root: Path
) -> str:
    """Render one document through the whole content pipeline.

    This is the single owner of the order: convert, sanitize, rewrite the
    repository URLs a browser needs, then add build-owned image dimensions.
    The sanitizer runs first because everything after it only adds values this
    builder itself produces; running it later would discard those additions and
    let a rewrite turn an admitted URL into something the allowlist never saw.
    """
    content = md.convert(text)
    content = sanitize_content(content)
    content = rewrite_content_urls(content, source, output)
    content = enhance_images_with_root(content, output, site_root)
    content = render_callouts(content)
    return content


def render_markdown(source: Path, output: str, site_root: Path) -> tuple[str, str, str, list[dict]]:
    text = source.read_text(encoding="utf-8")
    if source.name == "LICENSE":
        title = "MIT License"
        content = '<h1 id="mit-license">MIT License</h1><pre class="license-text">' + html.escape(text) + "</pre>"
        return title, "MIT license terms for millivolt.", content, []
    heading = HEADING.search(text)
    title = strip_html(heading.group(1)) if heading else source.stem.replace("-", " ").title()
    md = markdown_engine()
    content = render_fragment(md, text, source, output, site_root)
    # A page lede describes the page, not the warning that leads it. Callouts
    # are build-owned asides and are skipped so a page whose first blocks are
    # `[!WARNING]` still gets a real description for search and social cards.
    described = re.sub(r'<aside class="callout.*?</aside>', "", content, flags=re.S)
    first_paragraph = PARAGRAPH.search(described)
    description = plain_text(first_paragraph.group(1)) if first_paragraph else title
    description = summarize(description)
    return title, description, content, md.toc_tokens


# Search indexes a page as its lede plus one document per h2. The split is on the
# rendered heading, which is where the sanitizer has already closed the markup,
# so a section's text is exactly what a reader sees between two headings.
SEARCH_SECTION = re.compile(r'<h([23])\b[^>]*\bid="([^"]+)"[^>]*>', re.S)
SEARCH_TEXT_MAX = 1200


def search_sections(content: str) -> list[dict]:
    """Split one rendered document into its sections for the search index.

    A section starts at an h2 or an h3 and ends at the next heading, so its text
    is what a reader sees under that heading and no text is indexed twice. An h3
    carries its h2 as context, which is what the result list shows.
    """
    matches = list(SEARCH_SECTION.finditer(content))
    sections: list[dict] = []
    parent = ""
    for index, match in enumerate(matches):
        level, anchor = match.group(1), match.group(2)
        end = matches[index + 1].start() if index + 1 < len(matches) else len(content)
        chunk = content[match.end():end]
        heading_html, _, body = chunk.partition("</h" + level + ">")
        heading = strip_html(heading_html).rstrip("#\u00b6 ").strip()
        if level == "2":
            parent = heading
        sections.append({
            "heading": heading,
            "id": anchor,
            "text": plain_text(body)[:SEARCH_TEXT_MAX],
            "context": "" if level == "2" else parent,
        })
    return sections


def toc_html(tokens: list[dict]) -> str:
    """Render a nested table of contents from markdown toc tokens."""

    def render(nodes: list[dict]) -> str:
        result: list[str] = []
        for node in nodes:
            level = int(node.get("level", 0))
            if level not in (2, 3):
                continue
            nested = render(node.get("children", []))
            # strip_html() unescapes character references, so its result is
            # display text and never markup. Escape it again: a heading like
            # "Looks safe &lt;svg onload=...&gt;" is inert in the article but
            # would otherwise become a live element in the table of contents.
            label = html.escape(strip_html(node["name"]))
            result.append(
                f'<li class="toc-level-{level}"><a href="#{attr_escape(node["id"])}">'
                f'{label}</a>{nested}</li>'
            )
        if not result:
            return ""
        return '<ul class="toc-list">' + "".join(result) + "</ul>"

    return render(tokens)


def page_for_source(source: Path, *, kind: str) -> Page:
    output = site_page_for(source)
    if not output:
        raise BuildError(f"no site output registered for {source}")
    if source.name == "LICENSE":
        title, description = "MIT License", "MIT license terms for millivolt."
    else:
        text = source.read_text(encoding="utf-8")
        heading = HEADING.search(text)
        title = strip_html(heading.group(1)) if heading else source.stem.replace("-", " ").title()
        described = CALLOUT_SOURCE.sub("", text)
        paragraph = PARAGRAPH.search(markdown.markdown(described, extensions=["extra"]))
        description = plain_text(paragraph.group(1)) if paragraph else title
        description = summarize(description)
    return Page(source=source, output=output, title=title, description=description, kind=kind)


def replace_tokens(template: str, values: dict[str, str]) -> str:
    rendered = template
    for key, value in values.items():
        rendered = rendered.replace("{{" + key + "}}", value)
    return rendered


def base_values(
    *,
    title: str,
    description: str,
    output: str,
    base_url: str,
    version: str,
    body: str,
    body_class: str,
    theme_color: str,
    head_extra: str = "",
    active: str = "docs",
    root_override: str | None = None,
    include_canonical: bool = True,
) -> dict[str, str]:
    canonical_path = "" if output == "index.html" else output
    canonical = urljoin(base_url, canonical_path)
    og_image = urljoin(base_url, "assets/og.png")
    if root_override is None:
        root = page_relative("index.html", output)
        root = root[: -len("index.html")] if root.endswith("index.html") else root
        # For docs/foo.html root is "../"; for root pages and index.html it is "".
        if root in ("", "./"):
            root = ""
        elif not root.endswith("/"):
            root += "/"
    else:
        root = root_override
        if root and not root.endswith("/"):
            root += "/"
    canonical_tag = ""
    if include_canonical:
        canonical_tag = f'<link rel="canonical" href="{attr_escape(canonical)}">'
    og_url = base_url if output == "404.html" else canonical
    return {
        "title": html.escape(title),
        "description": attr_escape(description),
        "canonical": attr_escape(canonical),
        "canonical_tag": canonical_tag,
        "og_url": attr_escape(og_url),
        "og_image": attr_escape(og_image),
        "og_image_alt": attr_escape("millivolt: three client protocols in, any configured upstream out, every request recorded"),
        "site_name": "millivolt",
        "root": root,
        "body": body,
        "body_class": body_class,
        "theme_color": attr_escape(theme_color),
        "head_extra": head_extra,
        "version": html.escape(version),
        "nav_overview": ' aria-current="page"' if active == "overview" else "",
        "nav_docs": ' aria-current="page"' if active == "docs" else "",
    }


def render_base(values: dict[str, str]) -> str:
    template = (TEMPLATES / "base.html").read_text(encoding="utf-8")
    return replace_tokens(template, values)


def performance_headline() -> dict[str, str]:
    """The landing's four headline values, from the measured result bundle.

    perf/results.json is the only source of published performance numbers;
    docs/performance.md is rendered from it by scripts/perf_docs.py. Reading the
    bundle here keeps the landing from becoming a second parser of the page.
    """
    try:
        return landing_metrics(ROOT)
    except DataError as exc:
        raise BuildError(f"cannot read the performance bundle: {exc}") from exc


def render_landing(*, version: str, base_url: str, theme_color: str) -> str:
    body = (TEMPLATES / "landing.html").read_text(encoding="utf-8")
    perf = performance_headline()
    schema = {
        "@context": "https://schema.org",
        "@graph": [
            {
                "@type": "WebSite",
                "name": "millivolt",
                "url": base_url,
            },
            {
                "@type": "SoftwareApplication",
                "name": "millivolt",
                "applicationCategory": "DeveloperApplication",
                "operatingSystem": "Linux",
                "description": "Self-hosted OpenAI-compatible inference proxy with a live dashboard, SQLite history, and operator controls.",
                "license": "https://opensource.org/licenses/MIT",
                "codeRepository": GITHUB_REPOSITORY,
                "downloadUrl": GITHUB_REPOSITORY + "/releases/latest",
                "softwareVersion": version,
                "offers": {
                    "@type": "Offer",
                    "price": "0",
                    "priceCurrency": "USD",
                },
            },
        ],
    }
    head_extra = '<script type="application/ld+json">' + json.dumps(schema, ensure_ascii=False) + "</script>"
    values = base_values(
        title="millivolt: self-hosted OpenAI-compatible LLM proxy",
        description="See every AI request. Keep your own routing with a self-hosted OpenAI-compatible proxy, live dashboard, and SQLite history.",
        output="index.html",
        base_url=base_url,
        version=version,
        body=body,
        body_class="landing",
        theme_color=theme_color,
        head_extra=head_extra,
        active="overview",
    )
    values.update(perf)
    return render_base(values)


def render_doc(page: Page, *, version: str, base_url: str, theme_color: str, site_root: Path) -> tuple[str, dict]:
    assert page.source is not None
    title, description, content, tokens = render_markdown(page.source, page.output, site_root)
    title = page.title or title
    description = page.description or description
    previous, following = previous_next(page.output)
    previous_html = ""
    following_html = ""
    if previous:
        previous_html = (
            '<a class="page-nav-link previous" href="%s"><span>Previous</span><strong>%s</strong></a>'
            % (attr_escape(page_relative(previous[1], page.output)), html.escape(previous[0]))
        )
    if following:
        following_html = (
            '<a class="page-nav-link next" href="%s"><span>Next</span><strong>%s</strong></a>'
            % (attr_escape(page_relative(following[1], page.output)), html.escape(following[0]))
        )
    source_key = page.source.relative_to(ROOT).as_posix()
    edit_url = GITHUB_EDIT + source_key
    source_url = GITHUB_BLOB + source_key
    group = nav_group_of(page.output)
    crumbs = "" if page.output == "docs/index.html" else (
        '<nav class="crumbs" aria-label="Breadcrumb">'
        f'<a href="{attr_escape(page_relative("docs/index.html", page.output))}">Docs</a>'
        '<span class="crumb-sep" aria-hidden="true">/</span>'
        f'<span class="crumb-group">{html.escape(group)}</span>'
        "</nav>"
    )
    doc_body = (
        '<div class="wrap docs-shell">'
        '<details class="docs-nav" open>'
        '<summary class="docs-nav-summary">'
        '<span class="docs-nav-summary-text">'
        f'<span class="docs-nav-group">{html.escape(group)}</span>'
        f'<span class="docs-nav-page">{html.escape(title)}</span>'
        "</span>"
        '<svg class="docs-nav-chevron" viewBox="0 0 16 16" aria-hidden="true">'
        '<path d="m4 6 4 4 4-4"/></svg>'
        "</summary>"
        '<button class="docs-nav-collapse" type="button" aria-expanded="true" '
        'aria-controls="docs-nav-list">Collapse menu</button>'
        f'<nav class="docs-nav-inner" id="docs-nav-list" aria-label="Documentation">'
        f"{nav_links(page.output)}</nav>"
        "</details>"
        '<details class="toc" open>'
        '<summary class="toc-summary">On this page'
        '<svg class="docs-nav-chevron" viewBox="0 0 16 16" aria-hidden="true">'
        '<path d="m4 6 4 4 4-4"/></svg></summary>'
        '<div class="toc-sticky"><p class="toc-title">On this page</p>'
        + toc_html(tokens)
        + "</div></details>"
        '<main id="main" class="doc-main">'
        '<div class="read-progress" aria-hidden="true"></div>'
        '<button class="docs-nav-reveal" type="button" aria-expanded="false" '
        'aria-controls="docs-nav-list">Show menu</button>'
        + crumbs
        + '<article class="doc-content">'
        + content
        + "</article>"
        '<footer class="doc-footer">'
        '<div class="doc-links"><a href="'
        + attr_escape(source_url)
        + '" target="_blank" rel="noopener noreferrer">View source</a><a href="'
        + attr_escape(edit_url)
        + '" target="_blank" rel="noopener noreferrer">Edit this page</a></div>'
        '<nav class="page-nav" aria-label="Documentation pages">'
        + previous_html
        + following_html
        + "</nav></footer>"
        "</main>"
        "</div>"
    )
    values = base_values(
        title=f"{title} - millivolt",
        description=description,
        output=page.output,
        base_url=base_url,
        version=version,
        body=doc_body,
        body_class="docs",
        theme_color=theme_color,
        active="docs",
    )
    return render_base(values), {
        "title": title,
        "url": page.output,
        "description": description,
        "headings": [
            {"text": strip_html(node["name"]), "id": node["id"]}
            for node in tokens
            if int(node.get("level", 0)) in (2, 3)
        ],
        # The lede is what the page says before its first section; the sections
        # themselves carry the reference text, so a long page is indexed whole
        # instead of being cut at one fixed character count.
        "text": plain_text(SEARCH_SECTION.split(content)[0])[:SEARCH_TEXT_MAX],
        "sections": search_sections(content),
    }


def render_not_found(*, version: str, base_url: str, theme_color: str) -> str:
    # A custom 404 served from a nested path must not resolve assets relative
    # to that path. Use the site's absolute base path and keep the page out of
    # search results with no canonical URL.
    site_root = urlsplit(base_url).path or "/"
    if not site_root.endswith("/"):
        site_root += "/"
    body = (
        '<main id="main" class="wrap not-found">'
        '<p class="eyebrow">404</p>'
        "<h1>Page not found.</h1>"
        "<p>The page may have moved, or the link may be stale. Start from the "
        f'<a href="{attr_escape(site_root)}index.html">overview</a> or the '
        f'<a href="{attr_escape(site_root)}docs/index.html">documentation</a>.</p>'
        f'<a class="btn btn-primary" href="{attr_escape(site_root)}index.html#quickstart">Run it locally</a>'
        "</main>"
    )
    values = base_values(
        title="Page not found - millivolt",
        description="The requested millivolt documentation page was not found.",
        output="404.html",
        base_url=base_url,
        version=version,
        body=body,
        body_class="docs",
        theme_color=theme_color,
        active="",
        root_override=site_root,
        include_canonical=False,
        head_extra='<meta name="robots" content="noindex,follow">',
    )
    return render_base(values)


def write_sitemap(site_root: Path, base_url: str, outputs: list[str]) -> None:
    urls = []
    for output in outputs:
        if output == "404.html":
            continue
        path = "" if output == "index.html" else output
        urls.append(f"  <url><loc>{html.escape(urljoin(base_url, path))}</loc></url>")
    (site_root / "sitemap.xml").write_text(
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n'
        + "\n".join(urls)
        + "\n</urlset>\n",
        encoding="utf-8",
    )
    (site_root / "robots.txt").write_text(
        "User-agent: *\nAllow: /\nSitemap: "
        + urljoin(base_url, "sitemap.xml")
        + "\n",
        encoding="utf-8",
    )


def canonical_tokens() -> dict[str, str]:
    """Read the dashboard's :root block, the only palette/motif owner."""
    if not DASHBOARD_CSS.is_file():
        raise BuildError(f"missing canonical palette: {DASHBOARD_CSS.relative_to(ROOT)}")
    return root_custom_properties(DASHBOARD_CSS.read_text(encoding="utf-8"))


def write_tokens_css(site_root: Path, tokens: dict[str, str]) -> None:
    lines = [
        "/* Generated by scripts/build_site.py from the dashboard :root palette.",
        "   Do not edit: internal/web/static/css/dashboard.css is the owner. */",
        ":root {",
    ]
    lines.extend(f"  {name}: {value};" for name, value in tokens.items())
    lines.append("}")
    (site_root / "assets" / "tokens.css").write_text("\n".join(lines) + "\n", encoding="utf-8")


def copy_assets(site_root: Path, tokens: dict[str, str]) -> None:
    assets = site_root / "assets"
    assets.mkdir(parents=True, exist_ok=True)
    if (SITE_SRC / "assets").is_dir():
        shutil.copytree(SITE_SRC / "assets", assets, dirs_exist_ok=True)
    for name, source in BRAND_ASSETS.items():
        if not source.is_file():
            raise BuildError(f"missing canonical brand asset: {source.relative_to(ROOT)}")
        shutil.copyfile(source, assets / name)
    if DOC_IMAGES.is_dir():
        for source in sorted(DOC_IMAGES.rglob("*")):
            if not source.is_file():
                continue
            relative = source.relative_to(DOC_IMAGES).as_posix()
            if relative in SITE_IMAGE_EXCLUDES:
                continue
            target = assets / "images" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, target)
    write_tokens_css(site_root, tokens)
    (site_root / ".nojekyll").write_text("", encoding="utf-8")


def readme_image_errors() -> list[str]:
    """README images must not pin height. GitHub's markdown CSS applies
    max-width but not height:auto, so a fixed height stays at the intrinsic
    height while the width shrinks, squashing the artwork. The width hint alone
    scales correctly."""
    readme = (ROOT / "README.md").read_text(encoding="utf-8")
    errors = []
    images = re.findall(r"<img\b[^>]*>", readme)
    if not any("readme-cover-dark.png" in tag for tag in images):
        errors.append("README is missing the cover fallback image")
    for tag in images:
        if not re.search(r"\bheight=", tag):
            continue
        src = re.search(r'\bsrc="([^"]*)"', tag)
        errors.append(f"README image declares height: {src.group(1) if src else tag}")
    return errors


def build_site(output: Path, *, base_url: str = DEFAULT_BASE_URL) -> list[str]:
    output = output.resolve()
    base_url = base_url.rstrip("/") + "/"
    image_errors = readme_image_errors()
    if image_errors:
        raise BuildError("; ".join(image_errors))
    version = read_version()
    tokens = canonical_tokens()
    if "--rail" not in tokens:
        raise BuildError("canonical palette is missing --rail for the theme color")
    theme_color = tokens["--rail"]
    if output.exists():
        shutil.rmtree(output)
    output.mkdir(parents=True)
    copy_assets(output, tokens)

    pages: list[Page] = []
    # Docs pages, in nav order, plus any unlisted docs file.
    seen_sources: set[Path] = set()
    for _label, entries in NAV_GROUPS:
        for _text, _target, source_key in entries:
            if not source_key:
                continue
            source = ROOT / source_key
            if source in seen_sources:
                continue
            seen_sources.add(source)
            kind = "project" if source_key in {
                "CONTRIBUTING.md",
                "SECURITY.md",
                "LICENSE",
                "SUPPORT.md",
                "THIRD_PARTY_NOTICES.md",
            } else "doc"
            pages.append(page_for_source(source, kind=kind))
    for source in sorted(DOCS.glob("*.md")):
        if source in seen_sources:
            continue
        if source.name == "README.md" and (ROOT / "docs/README.md") not in seen_sources:
            pages.append(page_for_source(source, kind="doc"))
        elif source.name != "README.md":
            pages.append(page_for_source(source, kind="doc"))

    (output / "index.html").write_text(
        render_landing(version=version, base_url=base_url, theme_color=theme_color),
        encoding="utf-8",
    )
    search_index: list[dict] = []
    for page in pages:
        body, search = render_doc(
            page,
            version=version,
            base_url=base_url,
            theme_color=theme_color,
            site_root=output,
        )
        target = output / page.output
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(body, encoding="utf-8")
        search_index.append(search)
    (output / "404.html").write_text(
        render_not_found(version=version, base_url=base_url, theme_color=theme_color),
        encoding="utf-8",
    )

    (output / "search.json").write_text(
        json.dumps(search_index, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    write_sitemap(
        output,
        base_url,
        ["index.html"] + [page.output for page in pages] + ["404.html"],
    )
    return ["index.html"] + [page.output for page in pages] + ["404.html"]


class BuildError(RuntimeError):
    pass


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default="site", help="generated site directory")
    parser.add_argument(
        "--base-url",
        default=os.environ.get("SITE_URL", DEFAULT_BASE_URL),
        help="public site URL used for canonical and social metadata",
    )
    args = parser.parse_args(argv)
    # The domain has one owner outside this repository: the Pages setting. Warn
    # when a build for the live site disagrees with the committed default, which
    # is what a half-finished domain migration looks like from here.
    live = os.environ.get("SITE_URL")
    if live and urlsplit(live).hostname != urlsplit(DEFAULT_BASE_URL).hostname:
        print(
            f"build_site: building for {urlsplit(live).hostname}, but the repository "
            f"default is {urlsplit(DEFAULT_BASE_URL).hostname}; set the Pages custom "
            "domain to match, or the published metadata will name the wrong origin",
            file=sys.stderr,
        )
    try:
        outputs = build_site(Path(args.output), base_url=args.base_url)
    except BuildError as exc:
        print(f"build_site: {exc}", file=sys.stderr)
        return 1
    print(f"built {len(outputs)} pages in {Path(args.output).resolve()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
