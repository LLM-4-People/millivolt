"""Compose the two-surface capture plate.

Every published capture is one state photographed on the two primary surfaces:
Instrument Black and Instrument Clear. The plate keeps both, split by the
diagonal from the bottom-left corner to the top-right corner, so a reader sees
one composition and both cases at once. The dark surface owns the upper-left
half, the clear surface the lower-right half, and the seam is drawn at a
sub-pixel-sampled scale so the cut stays crisp when the plate is scaled down.

The two inputs must describe the same layout: the capture applies the mode
without reloading, so state, scroll and data are byte-identical except colour.
"""
from __future__ import annotations

from pathlib import Path

from PIL import Image, ImageDraw

DARK_MODE = "black"
CLEAR_MODE = "clear"
SEAM = (240, 233, 220, 235)
SEAM_SHADOW = (0, 0, 0, 60)
SEAM_WIDTH = 2
SUPERSAMPLE = 2


def split(dark_path: Path, clear_path: Path, output: Path) -> None:
    dark = Image.open(dark_path).convert("RGB")
    clear = Image.open(clear_path).convert("RGB")
    if dark.size != clear.size:
        raise RuntimeError(
            f"split capture sizes differ: {dark.size} and {clear.size}")
    width, height = dark.size
    scale = SUPERSAMPLE

    # The clear surface owns the triangle below/right of the diagonal; the
    # dark surface owns the triangle above/left. Equal areas, one cut.
    mask = Image.new("L", (width * scale, height * scale), 0)
    ImageDraw.Draw(mask).polygon(
        [(width * scale, 0), (width * scale, height * scale), (0, height * scale)],
        fill=255)
    mask = mask.resize((width, height), Image.LANCZOS)
    plate = Image.composite(clear, dark, mask).convert("RGBA")

    overlay = Image.new("RGBA", (width * scale, height * scale), (0, 0, 0, 0))
    pen = ImageDraw.Draw(overlay)
    start = (0, height * scale - 1)
    end = (width * scale - 1, 0)
    pen.line([(start[0], start[1] + scale), (end[0], end[1] + scale)],
             fill=SEAM_SHADOW, width=(SEAM_WIDTH + 1) * scale)
    pen.line([start, end], fill=SEAM, width=SEAM_WIDTH * scale)
    plate.alpha_composite(overlay.resize((width, height), Image.LANCZOS))
    output.parent.mkdir(parents=True, exist_ok=True)
    plate.convert("RGB").save(output, format="PNG", optimize=True)
