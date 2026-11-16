"""Upgrade a captured image into an inspection plate at build time.

One transform serves both shapes a page can carry: a plain Markdown image
(wrapped in a paragraph) and the generated gallery's semantic figure. The
registry supplies the number, station, route, split legend and magnified
details; the page keeps its own prose and alt text. Nothing here is copied into
the Markdown, so a plate cannot describe one surface while the capture shows
another.
"""
from __future__ import annotations

import html
import re

from scripts.gallery import GALLERY, Plate

IMAGE = re.compile(r"<img\b[^>]*>")
PARAGRAPH_IMAGE = re.compile(r"<p>\s*(<img\b[^>]*>)\s*</p>", re.S)
FIGURE = re.compile(r"<figure>\s*(<img\b[^>]*>)\s*<figcaption>.*?</figcaption>\s*</figure>", re.S)
ATTRIBUTE = re.compile(r'\b(src|alt|width|height)="([^"]*)"')
ASSET_PREFIX = "assets/images/"
def _attributes(tag: str) -> dict[str, str]:
    return {name: html.unescape(value) for name, value in ATTRIBUTE.findall(tag)}


def _registry_path(src: str) -> str | None:
    if src.startswith(("http://", "https://", "//", "data:")):
        return None
    clean = src.split("?", 1)[0].split("#", 1)[0]
    at = clean.find(ASSET_PREFIX)
    if at >= 0:
        return "images/" + clean[at + len(ASSET_PREFIX):]
    if clean.startswith("images/"):
        return clean
    return None


def route(plate: Plate) -> str:
    """A short, mechanical statement of where the captured state lives."""
    setup = plate.setup
    dim = setup.get("dim")
    if plate.kind in ("page", "element"):
        return f"#/by/{dim}"
    if plate.kind == "chart":
        return f"chart · {setup['preset']}" + (f" · p{setup['pct']}" if setup.get("pct") else "")
    if plate.kind == "chart-menu":
        return "chart · window picker"
    if plate.kind == "scoped":
        return "#/ by provider · filtered"
    if plate.kind == "attempts":
        return "request log · retries"
    if plate.kind == "drawer":
        return "drawer · " + str(setup.get("group") or "request")
    if plate.kind == "console":
        return "console · " + str(setup["section"])
    if plate.kind == "settings":
        return "settings · " + str(setup["category"])
    if plate.kind == "theme":
        return "header · theme tray"
    return plate.kind


def _plate(plate: Plate, tag: str, src: str) -> str:
    attrs = _attributes(tag)
    width = int(attrs.get("width") or 0)
    height = int(attrs.get("height") or 0)
    number = GALLERY.number(plate)
    big = " plate-wide" if plate.wide else (" plate-narrow" if 0 < width < 900 else "")
    scroll = ' class="plate-scroll" tabindex="0" role="region" aria-label="Scrollable capture"' if plate.wide else ""
    image = tag
    link = (f'<a class="plate-link" href="{html.escape(src, quote=True)}" target="_blank" '
            f'rel="noopener noreferrer" aria-label="Open {html.escape(plate.title)} at full size">'
            f"{image}</a>")
    body = f"<div{scroll}>{link}</div>" if plate.wide else link
    warning = " Scroll the plate horizontally to see every column." if plate.wide else ""
    return (
        f'<figure class="doc-plate{big}" id="plate-{html.escape(plate.id)}">'
        '<div class="plate-hd">'
        f'<span class="plate-no">{number:02d}</span>'
        f'<span class="plate-title">{html.escape(plate.title)}</span>'
        f'<span class="plate-meta">{html.escape(plate.station)} · {html.escape(route(plate))}</span>'
        '<span class="plate-split">split · black / clear</span>'
        f'<a class="plate-open" href="{html.escape(src, quote=True)}" target="_blank" '
        'rel="noopener noreferrer">full size</a>'
        "</div>"
        f'<div class="plate-body">{body}</div>'
        '<figcaption class="plate-ft">'
        f'<p class="plate-note">{html.escape(plate.note)}{warning}'
        '<span class="sr-only"> The plate is split diagonally: the upper-left half is '
        "Instrument Black, the lower-right half is Instrument Clear.</span></p>"
        "</figcaption>"
        "</figure>"
    )


def enhance(content: str) -> str:
    """Replace every registered capture with its plate, leaving other art alone."""
    def figure(match: re.Match[str]) -> str:
        tag = match.group(1)
        attrs = _attributes(tag)
        src = attrs.get("src", "")
        plate = GALLERY.by_path(_registry_path(src) or "")
        return _plate(plate, tag, src) if plate else match.group(0)

    def paragraph(match: re.Match[str]) -> str:
        tag = match.group(1)
        attrs = _attributes(tag)
        src = attrs.get("src", "")
        plate = GALLERY.by_path(_registry_path(src) or "")
        return _plate(plate, tag, src) if plate else match.group(0)

    content = FIGURE.sub(figure, content)
    return PARAGRAPH_IMAGE.sub(paragraph, content)
