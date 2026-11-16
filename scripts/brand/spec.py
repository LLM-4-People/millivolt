#!/usr/bin/env python3
"""Render the README specification strips from one art field and the facts."""
from __future__ import annotations

import base64
from pathlib import Path

from scripts.brand import facts

FIELDS = Path(__file__).resolve().parent / "fields"
DESKTOP = {
    "field": FIELDS / "spec-desktop.jpg",
    "width": 1600,
    "height": 192,
    "number_y": 105,
    "label_y": 130,
    "number_size": 56,
    "label_size": 19,
    "letter_spacing": 2.1,
    "columns": (82, 489, 840, 1237),
}
MOBILE = {
    "field": FIELDS / "spec-mobile.jpg",
    "width": 800,
    "height": 447,
    "number_size": 44,
    "label_size": 16,
    "letter_spacing": 1.6,
    "positions": ((96, 150, 174), (439, 150, 174), (96, 318, 342), (439, 318, 342)),
}


def _data_uri(path: Path) -> str:
    encoded = base64.b64encode(path.read_bytes()).decode("ascii")
    return "data:image/jpeg;base64," + encoded


def _frame(width: int, height: int) -> str:
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
        f'viewBox="0 0 {width} {height}" role="img" aria-labelledby="title desc">\n'
        '  <title id="title">millivolt at a glance</title>\n'
        f'  <desc id="desc">{facts.SPEC_DESC}</desc>\n'
        "  <defs>\n"
        f'    <clipPath id="frame"><rect x="0.5" y="0.5" width="{width - 1}" '
        f'height="{height - 1}" rx="16"/></clipPath>\n'
        "  </defs>\n"
        '  <g clip-path="url(#frame)">\n'
    )


def _close(width: int, height: int) -> str:
    return (
        f'    <rect x="0.5" y="0.5" width="{width - 1}" height="{height - 1}" rx="16" '
        'fill="none" stroke="#3a342b"/>\n'
        "  </g>\n"
        "</svg>\n"
    )


def _text_block(rows, positions, number_size: int, label_size: int, letter: float) -> str:
    numbers = []
    labels = []
    for row, (x, number_y, label_y) in zip(rows, positions):
        numbers.append(f'        <text x="{x}" y="{number_y}">{row["value"]}</text>')
        labels.append(f'        <text x="{x}" y="{label_y}">{row["label"]}</text>')
    return (
        '    <g font-family="ui-monospace, SFMono-Regular, Menlo, Consolas, monospace">\n'
        f'      <g fill="#f2eee7" font-size="{number_size}" font-weight="500">\n'
        + "\n".join(numbers)
        + "\n      </g>\n"
        f'      <g fill="#b8b0a0" font-size="{label_size}" letter-spacing="{letter}">\n'
        + "\n".join(labels)
        + "\n      </g>\n"
        "    </g>\n"
    )


def render_desktop() -> str:
    layout = DESKTOP
    positions = tuple((x, layout["number_y"], layout["label_y"]) for x in layout["columns"])
    header = _frame(layout["width"], layout["height"])
    image = (
        f'    <image x="0" y="0" width="{layout["width"]}" height="{layout["height"]}" '
        f'preserveAspectRatio="none" href="{_data_uri(layout["field"])}"/>\n'
    )
    return header + image + _text_block(
        facts.SPEC_ROWS, positions, layout["number_size"], layout["label_size"],
        layout["letter_spacing"]) + _close(layout["width"], layout["height"])


def render_mobile() -> str:
    layout = MOBILE
    header = _frame(layout["width"], layout["height"])
    image = (
        f'    <image x="0" y="0" width="{layout["width"]}" height="{layout["height"]}" '
        f'preserveAspectRatio="none" href="{_data_uri(layout["field"])}"/>\n'
    )
    return header + image + _text_block(
        facts.SPEC_ROWS, layout["positions"], layout["number_size"],
        layout["label_size"], layout["letter_spacing"]) + _close(
            layout["width"], layout["height"])


def outputs() -> dict[str, str]:
    return {
        "docs/images/brand/readme-spec.svg": render_desktop(),
        "docs/images/brand/readme-spec-mobile.svg": render_mobile(),
    }
