#!/usr/bin/env python3
"""Build the brand plates from one facts file, or verify the committed bytes.

The specification strips, the README covers, the site Open Graph card and the
GitHub social preview are generated assets. Their dimensions and hashes live in
manifest.json; `--check` verifies every committed asset against that manifest
and recomputes the two SVG strips from source. The PNG renders need Chromium
and Playwright, so they are a documented local build, not a CI step.

    python3 -m scripts.brand.build            # regenerate every plate
    python3 -m scripts.brand.build --only spec,social
    python3 -m scripts.brand.build --check    # verify committed assets
"""
from __future__ import annotations

import argparse
import hashlib
import json
import tempfile
from pathlib import Path

from scripts.brand import facts, spec
from scripts.site_media import image_size

ROOT = Path(__file__).resolve().parents[2]
MANIFEST = Path(__file__).resolve().parent / "manifest.json"
TEMPLATE = Path(__file__).resolve().parent / "templates" / "social.html"
SOCIAL = ROOT / ".github/social-preview.png"
COVERS = {
    "dark": ROOT / "docs/images/brand/readme-cover-dark.png",
    "light": ROOT / "docs/images/brand/readme-cover-light.png",
}
OG = ROOT / "site-src/assets/og.png"

ASSETS = (
    (COVERS["dark"], 1600, 640, "cover"),
    (COVERS["light"], 1600, 640, "cover"),
    ("docs/images/brand/readme-spec.svg", 1600, 192, "spec"),
    ("docs/images/brand/readme-spec-mobile.svg", 800, 447, "spec"),
    (OG, 1200, 630, "og"),
    (SOCIAL, 1280, 640, "social"),
)


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def build_spec() -> None:
    for name, content in spec.outputs().items():
        (ROOT / name).write_text(content, encoding="utf-8")


def build_social() -> None:
    from playwright.sync_api import sync_playwright

    template = TEMPLATE.read_text(encoding="utf-8")
    values = {
        "font_sans": (ROOT / "site-src/assets/fonts/instrument-sans.woff2").as_uri(),
        "font_serif": (ROOT / "site-src/assets/fonts/instrument-serif.woff2").as_uri(),
        "font_mono": (ROOT / "site-src/assets/fonts/jetbrains-mono.woff2").as_uri(),
        "mark": (ROOT / "internal/web/static/favicon.svg").as_uri(),
        "hero": (ROOT / "site-src/assets/relay-field.webp").as_uri(),
        "dashboard": (ROOT / "docs/images/overview/dashboard.png").as_uri(),
        "headline": facts.HEADLINE_HTML,
        "eyebrow": facts.EYEBROW.upper(),
        "lede": facts.SOCIAL_LEDE,
        "footer": facts.FOOTER,
    }
    values.update(facts.SAMPLE)
    for key, value in values.items():
        template = template.replace("{{" + key + "}}", value)
    if "{{" in template:
        raise SystemExit("social template has unresolved placeholders")
    with tempfile.TemporaryDirectory(prefix="millivolt-brand-") as temp:
        page_path = Path(temp) / "social.html"
        page_path.write_text(template, encoding="utf-8")
        with sync_playwright() as p:
            browser = p.chromium.launch()
            page = browser.new_page(viewport={"width": 1280, "height": 640}, device_scale_factor=1)
            page.goto(page_path.as_uri(), wait_until="networkidle")
            page.wait_for_timeout(350)
            page.locator(".plate").screenshot(path=str(SOCIAL))
            browser.close()


def build_covers() -> None:
    from scripts.brand import compose

    for theme, target in COVERS.items():
        compose.render(
            f"cover-{theme}", target, compose.cover(theme, 1600, 640), 1600, 640)


def build_og() -> None:
    from scripts.brand import compose

    compose.render("og", OG, compose.og_card(), 1200, 630)


def write_manifest() -> None:
    entries = []
    for target, width, height, builder in ASSETS:
        path = ROOT / target if not isinstance(target, Path) else target
        entries.append({
            "path": path.relative_to(ROOT).as_posix(),
            "width": width,
            "height": height,
            "builder": builder,
            "bytes": path.stat().st_size,
            "sha256": _sha256(path),
        })
    payload = {
        "schema": 1,
        "generator": "python3 -m scripts.brand.build",
        "assets": entries,
    }
    MANIFEST.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")


def check() -> int:
    payload = json.loads(MANIFEST.read_text(encoding="utf-8"))
    if payload.get("schema") != 1:
        print("brand: manifest schema is not 1")
        return 1
    errors = []
    for entry in payload.get("assets", []):
        path = ROOT / entry["path"]
        if not path.is_file():
            errors.append(f"missing asset: {entry['path']}")
            continue
        if path.stat().st_size != entry["bytes"]:
            errors.append(f"{entry['path']}: byte count is {path.stat().st_size}, want {entry['bytes']}")
        if _sha256(path) != entry["sha256"]:
            errors.append(f"{entry['path']}: sha256 does not match the manifest")
        size = image_size(path)
        if size != (entry["width"], entry["height"]):
            errors.append(f"{entry['path']}: dimensions are {size}, want {entry['width']}x{entry['height']}")
    for name, content in spec.outputs().items():
        path = ROOT / name
        if not path.is_file() or path.read_text(encoding="utf-8") != content:
            errors.append(f"{name}: not reproducible from scripts/brand/spec.py")
    if errors:
        for error in errors:
            print("brand: " + error)
        return 1
    print("brand: clean")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--only", default="spec,social,cover,og",
                        help="comma-separated builders: spec, social, cover, og")
    parser.add_argument("--check", action="store_true", help="verify the committed assets")
    args = parser.parse_args()
    if args.check:
        return check()
    only = {part.strip() for part in args.only.split(",") if part.strip()}
    if "spec" in only:
        build_spec()
    if "social" in only:
        build_social()
    if "cover" in only:
        build_covers()
    if "og" in only:
        build_og()
    write_manifest()
    print("brand: wrote " + ", ".join(sorted(only)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
