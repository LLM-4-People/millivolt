"""One owner for static image dimensions and README-only brand plates."""
from __future__ import annotations

import json
import re
import struct
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BRAND_MANIFEST = ROOT / "scripts/brand/manifest.json"
# The suffixes a documentation page may reference as artwork.
IMAGE_SUFFIXES = (".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico")


def image_size(path: Path) -> tuple[int, int] | None:
    """Read PNG, WebP or SVG dimensions without requiring an image library."""
    try:
        if path.suffix.lower() == ".png":
            data = path.read_bytes()[:24]
            if data[:8] != b"\x89PNG\r\n\x1a\n":
                return None
            width, height = struct.unpack(">II", data[16:24])
            return width, height
        if path.suffix.lower() == ".webp":
            data = path.read_bytes()[:40]
            if data[:4] != b"RIFF" or data[8:12] != b"WEBP":
                return None
            chunk = data[12:16]
            if chunk == b"VP8X":
                width = 1 + int.from_bytes(data[24:27], "little")
                height = 1 + int.from_bytes(data[27:30], "little")
                return width, height
            if chunk == b"VP8 ":
                start = data.find(b"\x9d\x01\x2a", 20)
                if start < 0 or start + 7 > len(data):
                    return None
                width = int.from_bytes(data[start + 3:start + 5], "little") & 0x3fff
                height = int.from_bytes(data[start + 5:start + 7], "little") & 0x3fff
                return width, height
            if chunk == b"VP8L":
                if len(data) < 25 or data[20] != 0x2f:
                    return None
                bits = int.from_bytes(data[21:25], "little")
                return 1 + (bits & 0x3fff), 1 + ((bits >> 14) & 0x3fff)
            return None
        if path.suffix.lower() == ".svg":
            head = path.read_text(encoding="utf-8", errors="replace")[:1000]
            width = re.search(r'\bwidth="(\d+)"', head)
            height = re.search(r'\bheight="(\d+)"', head)
            if width and height:
                return int(width.group(1)), int(height.group(1))
    except (OSError, ValueError, struct.error):
        return None
    return None

def readme_only_images() -> frozenset[str]:
    payload = json.loads(BRAND_MANIFEST.read_text(encoding="utf-8"))
    return frozenset(
        Path(entry["path"]).relative_to("docs/images").as_posix()
        for entry in payload["assets"]
        if entry["builder"] in ("cover", "spec")
    )

SITE_IMAGE_EXCLUDES = readme_only_images()
