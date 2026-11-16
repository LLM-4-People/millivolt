"""Read and verify the capture manifest that stamps one gallery run.

The manifest is the bridge between a browser run and every later reader. It
records the pixels each plate produced, the digest of the dashboard source that
produced them, and the one aggregate count the run verified. A UI change
invalidates the digest, so a stale gallery fails the test suite instead of
quietly describing an older dashboard.
"""
from __future__ import annotations

import hashlib
import json
from datetime import datetime, timezone
from pathlib import Path

from .model import IMAGE_ROOT, ROOT, Gallery

CAPTURE = IMAGE_ROOT / "capture.json"
STATIC_ROOT = ROOT / "internal/web/static"
SCHEMA = 1
GENERATED_BY = "python3 -B -O -m tests.explorer_check --docs-history docs/images"


def ui_digest() -> str:
    """Hash the dashboard sources that decide what a capture shows.

    Every embedded file counts: markup changes the layout, CSS changes the
    case, scripts change the states, and the self-hosted faces change the
    glyphs. The digest is over paths and bytes, so a rename is a change too.
    """
    digest = hashlib.sha256()
    for path in sorted(STATIC_ROOT.rglob("*")):
        if not path.is_file():
            continue
        relative = path.relative_to(STATIC_ROOT).as_posix()
        digest.update(relative.encode("utf-8") + b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def path_of(relative: str) -> Path:
    """A registry path is relative to docs/, as Markdown writes it."""
    return ROOT / "docs" / relative


def write(staging: Path, gallery: Gallery, records: int) -> Path:
    from scripts.site_media import image_size

    entries = []
    for plate in gallery.plates:
        source = staging / plate.path.removeprefix("images/")
        width_height = image_size(source)
        if width_height is None:
            raise RuntimeError(f"captured plate has unreadable dimensions: {plate.path}")
        width, height = width_height
        payload = source.read_bytes()
        entries.append({
            "id": plate.id,
            "path": plate.path,
            "width": width,
            "height": height,
            "bytes": len(payload),
            "sha256": hashlib.sha256(payload).hexdigest(),
        })
    document = {
        "schema": SCHEMA,
        "generator": GENERATED_BY,
        "captured_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat(),
        "ui_sha256": ui_digest(),
        "source_records": records,
        "plates": entries,
    }
    target = staging / "capture.json"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
    return target


def read() -> dict:
    if not CAPTURE.is_file():
        raise FileNotFoundError(
            f"missing {CAPTURE.relative_to(ROOT)}; run the documented capture")
    return json.loads(CAPTURE.read_text(encoding="utf-8"))


def verify(gallery: Gallery) -> list[str]:
    """Every committed plate must match the manifest and the current UI."""
    from scripts.site_media import image_size

    try:
        document = read()
    except (OSError, ValueError) as error:
        return [str(error)]
    errors: list[str] = []
    if document.get("schema") != SCHEMA:
        errors.append(f"capture.json schema is {document.get('schema')!r}, want {SCHEMA}")
    registered = {plate.path: plate for plate in gallery.plates}
    recorded = {}
    for entry in document.get("plates") or []:
        path = entry.get("path")
        if not isinstance(path, str) or path in recorded:
            errors.append(f"capture.json has a duplicate or invalid plate path: {path!r}")
            continue
        recorded[path] = entry
        target = path_of(path)
        if not target.is_file():
            errors.append(f"{path}: capture is missing")
            continue
        if target.stat().st_size != entry.get("bytes"):
            errors.append(f"{path}: bytes differ from capture.json; rerun the capture")
        if hashlib.sha256(target.read_bytes()).hexdigest() != entry.get("sha256"):
            errors.append(f"{path}: sha256 differs from capture.json; rerun the capture")
        size = image_size(target)
        if size != (entry.get("width"), entry.get("height")):
            errors.append(f"{path}: dimensions {size} differ from capture.json")
    for path in registered:
        if path not in recorded:
            errors.append(f"{path}: registered plate has no capture record")
    for path in recorded:
        if path not in registered:
            errors.append(f"{path}: captured plate is not in the registry; "
                          "add it or rerun after removing the file")
    if document.get("ui_sha256") != ui_digest():
        errors.append("the dashboard source changed since the gallery capture; "
                      "rerun the documented capture so the plates describe the shipped UI")
    if not isinstance(document.get("source_records"), int) or document["source_records"] <= 0:
        errors.append("capture.json has no verified source record count")
    return errors
