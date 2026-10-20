#!/usr/bin/env python3
"""Build the static millivolt site from the canonical Markdown sources.

The site is deliberately a small, dependency-light static build. Markdown stays
in its existing owners; this script only renders it, rewrites repository links
for a browser, and generates the navigation, search index and SEO files.
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
from dataclasses import dataclass
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urljoin, urlsplit, urlunsplit

import markdown
from markdown.extensions.toc import TocExtension

ROOT = Path(__file__).resolve().parent.parent
SITE_SRC = ROOT / "site-src"
TEMPLATES = SITE_SRC / "templates"
DEFAULT_BASE_URL = "https://llm-4-people.github.io/millivolt/"
GITHUB_REPOSITORY = "https://github.com/LLM-4-People/millivolt"
GITHUB_BRANCH = "main"
GITHUB_BLOB = f"{GITHUB_REPOSITORY}/blob/{GITHUB_BRANCH}/"
GITHUB_TREE = f"{GITHUB_REPOSITORY}/tree/{GITHUB_BRANCH}/"
GITHUB_EDIT = f"{GITHUB_REPOSITORY}/edit/{GITHUB_BRANCH}/"

sys.path.insert(0, str(ROOT))
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
# README-only covers stay in the repository for GitHub's renderer; no
# generated site page references them.
SITE_IMAGE_EXCLUDES = frozenset({
    "brand/readme-cover-dark.png",
    "brand/readme-cover-light.png",
})

ROOT_PAGE_MAP = {
    "README.md": "index.html",
    "CONTRIBUTING.md": "contributing.html",
    "SECURITY.md": "security.html",
    "LICENSE": "license.html",
    "THIRD_PARTY_NOTICES.md": "third-party.html",
    "docs/README.md": "docs/index.html",
}

NAV_GROUPS = (
    (
        "Get started",
        (
            ("Documentation", "docs/index.html", "docs/README.md"),
            ("Quickstart", "index.html#quickstart", None),
            ("Dashboard", "docs/dashboard.html", "docs/dashboard.md"),
            ("Protocol", "docs/protocol.html", "docs/protocol.md"),
        ),
    ),
    (
        "Guides",
        (
            ("Adapters", "docs/adapters.html", "docs/adapters.md"),
            ("Reverse proxy", "docs/reverse-proxy.html", "docs/reverse-proxy.md"),
            ("MCP server", "docs/mcp.html", "docs/mcp.md"),
        ),
    ),
    (
        "Reference",
        (
            ("Operations", "docs/operations.html", "docs/operations.md"),
            ("Architecture", "docs/architecture.html", "docs/architecture.md"),
        ),
    ),
    (
        "Project",
        (
            ("Contributing", "contributing.html", "CONTRIBUTING.md"),
            ("Security", "security.html", "SECURITY.md"),
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


def render_markdown(source: Path, output: str, site_root: Path) -> tuple[str, str, str, list[dict]]:
    text = source.read_text(encoding="utf-8")
    if source.name == "LICENSE":
        title = "MIT License"
        content = '<h1 id="mit-license">MIT License</h1><pre class="license-text">' + html.escape(text) + "</pre>"
        return title, "MIT license terms for millivolt.", content, []
    heading = HEADING.search(text)
    title = strip_html(heading.group(1)) if heading else source.stem.replace("-", " ").title()
    md = markdown.Markdown(
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
    content = md.convert(text)
    content = rewrite_content_urls(content, source, output)
    content = enhance_images_with_root(content, output, site_root)
    first_paragraph = PARAGRAPH.search(content)
    description = plain_text(first_paragraph.group(1)) if first_paragraph else title
    if len(description) > 180:
        description = description[:177].rstrip() + "..."
    return title, description, content, md.toc_tokens


def toc_html(tokens: list[dict]) -> str:
    """Render a nested table of contents from markdown toc tokens."""

    def render(nodes: list[dict]) -> str:
        result: list[str] = []
        for node in nodes:
            level = int(node.get("level", 0))
            if level not in (2, 3):
                continue
            nested = render(node.get("children", []))
            result.append(
                f'<li class="toc-level-{level}"><a href="#{attr_escape(node["id"])}">'
                f'{strip_html(node["name"])}</a>{nested}</li>'
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
        paragraph = PARAGRAPH.search(markdown.markdown(text, extensions=["extra"]))
        description = plain_text(paragraph.group(1)) if paragraph else title
        if len(description) > 180:
            description = description[:177].rstrip() + "..."
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
) -> dict[str, str]:
    canonical_path = "" if output == "index.html" else output
    canonical = urljoin(base_url, canonical_path)
    og_image = urljoin(base_url, "assets/og.png")
    root = page_relative("index.html", output)
    root = root[: -len("index.html")] if root.endswith("index.html") else root
    # For docs/foo.html root is "../"; for root pages and index.html it is "".
    if root in ("", "./"):
        root = ""
    elif not root.endswith("/"):
        root += "/"
    return {
        "title": html.escape(title),
        "description": attr_escape(description),
        "canonical": attr_escape(canonical),
        "og_image": attr_escape(og_image),
        "og_image_alt": attr_escape("millivolt: see every AI request, keep your own routing"),
        "site_name": "millivolt",
        "root": root,
        "body": body,
        "body_class": body_class,
        "theme_color": attr_escape(theme_color),
        "head_extra": head_extra,
        "version": html.escape(version),
        "nav_overview": ' aria-current="page"' if active == "overview" else "",
        "nav_docs": ' aria-current="page"' if active == "docs" else "",
        "nav_dashboard": ' aria-current="page"' if active == "dashboard" else "",
    }


def render_base(values: dict[str, str]) -> str:
    template = (TEMPLATES / "base.html").read_text(encoding="utf-8")
    return replace_tokens(template, values)


def render_landing(*, version: str, base_url: str, theme_color: str) -> str:
    body = (TEMPLATES / "landing.html").read_text(encoding="utf-8")
    schema = {
        "@context": "https://schema.org",
        "@type": "SoftwareApplication",
        "name": "millivolt",
        "applicationCategory": "DeveloperApplication",
        "operatingSystem": "Linux",
        "description": "Self-hosted OpenAI-compatible inference proxy with a live dashboard, SQLite history, and operator controls.",
        "license": "https://opensource.org/licenses/MIT",
        "codeRepository": GITHUB_REPOSITORY,
    }
    head_extra = '<script type="application/ld+json">' + json.dumps(schema, ensure_ascii=False) + "</script>"
    values = base_values(
        title="millivolt - precision instrument on the wire",
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
    doc_body = (
        '<div class="wrap docs-shell">'
        '<details class="docs-nav" open>'
        '<summary class="docs-nav-summary">Documentation menu</summary>'
        f'<nav class="docs-nav-inner" aria-label="Documentation">{nav_links(page.output)}</nav>'
        "</details>"
        '<main id="main" class="doc-main">'
        '<div class="doc-meta"><span class="mono-label">'
        + ("project" if page.kind == "project" else "documentation")
        + '</span><span class="doc-links"><a href="'
        + attr_escape(source_url)
        + '" target="_blank" rel="noopener noreferrer">view source</a><a href="'
        + attr_escape(edit_url)
        + '" target="_blank" rel="noopener noreferrer">edit this page</a></span></div>'
        '<article class="doc-content">'
        + content
        + "</article>"
        '<nav class="page-nav" aria-label="Documentation pages">'
        + previous_html
        + following_html
        + "</nav>"
        "</main>"
        "<aside class=\"toc\" aria-label=\"On this page\">"
        '<div class="toc-sticky"><span class="mono-label">On this page</span>'
        + toc_html(tokens)
        + "</div></aside>"
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
        active="dashboard" if page.output == "docs/dashboard.html" else "docs",
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
        "text": plain_text(content)[:5000],
    }


def render_not_found(*, version: str, base_url: str, theme_color: str) -> str:
    body = (
        '<main id="main" class="wrap not-found">'
        '<p class="eyebrow">404</p>'
        "<h1>Page not found.</h1>"
        "<p>The page may have moved, or the link may be stale. Start from the "
        '<a href="index.html">overview</a> or the <a href="docs/index.html">documentation</a>.</p>'
        '<a class="btn btn-primary" href="index.html#quickstart">Run it locally</a>'
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


def build_site(output: Path, *, base_url: str = DEFAULT_BASE_URL) -> list[str]:
    output = output.resolve()
    base_url = base_url.rstrip("/") + "/"
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
    try:
        outputs = build_site(Path(args.output), base_url=args.base_url)
    except BuildError as exc:
        print(f"build_site: {exc}", file=sys.stderr)
        return 1
    print(f"built {len(outputs)} pages in {Path(args.output).resolve()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
