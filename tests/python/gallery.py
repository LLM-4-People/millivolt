#!/usr/bin/env python3
"""Pin the capture gallery to one registry, one manifest and one page.

The registry is the schema: every plate it declares must have a committed
capture, a line in the generated page, a working plate upgrade and a route.
The manifest is the evidence: the bytes, dimensions and dashboard digest of
the run. The page is generated, so a hand edit is drift, not content.
"""
from __future__ import annotations

import unittest
from html import escape

from scripts import site_plates
from scripts.gallery import GALLERY, manifest
from scripts.gallery import render
from tests.support import ROOT


class CaptureGalleryTests(unittest.TestCase):
    def test_manifest_matches_the_registry_and_current_ui(self):
        errors = manifest.verify(GALLERY)
        self.assertEqual(errors, [], "\n".join(errors))

    def test_gallery_page_is_generated_from_the_registry(self):
        self.assertEqual(render.check(), [])

    def test_every_plate_is_published_and_captioned(self):
        page = (ROOT / "docs/gallery.md").read_text(encoding="utf-8")
        for plate in GALLERY.plates:
            self.assertEqual(page.count(plate.path), 1,
                             f"{plate.path} must appear exactly once in the gallery")
            self.assertIn(escape(plate.alt, quote=True), page)
            self.assertIn(escape(plate.title), page)
            self.assertIn(escape(plate.note), page)
        self.assertEqual(page.count("<figure>"), len(GALLERY.plates))
        self.assertEqual(page.count("<figcaption>"), len(GALLERY.plates))

    def test_plate_numbers_follow_registry_order(self):
        numbers = [GALLERY.number(plate) for plate in GALLERY.plates]
        self.assertEqual(numbers, list(range(1, len(GALLERY.plates) + 1)))

    def test_every_route_is_named(self):
        for plate in GALLERY.plates:
            self.assertTrue(site_plates.route(plate).strip(), plate.id)

    def test_plain_markdown_image_becomes_a_plate(self):
        plate = GALLERY.plates[0]
        content = (
            '<p><img src="../assets/images/'
            + plate.path.removeprefix("images/")
            + '" alt="capture" width="1920" height="1200" loading="lazy" decoding="async"></p>')
        rendered = site_plates.enhance(content)
        self.assertIn('class="doc-plate"', rendered)
        self.assertIn('class="plate-hd"', rendered)
        self.assertIn("split · black / clear", rendered)
        self.assertIn('class="plate-link"', rendered)
        self.assertIn(escape(plate.note), rendered)
        self.assertEqual(rendered.count("<figure"), 1)
        self.assertNotIn("<p><img", rendered)

    def test_generated_figure_keeps_one_plate_and_drops_the_redundant_caption(self):
        plate = GALLERY.plates[0]
        content = (
            "<figure>\n<img src=\"images/"
            + plate.path.removeprefix("images/")
            + '" alt="capture">\n'
            "<figcaption><strong>01 · " + escape(plate.title) + "</strong> "
            + escape(plate.note) + "</figcaption>\n</figure>")
        rendered = site_plates.enhance(content)
        self.assertEqual(rendered.count("<figure"), 1)
        self.assertEqual(rendered.count("<figcaption"), 1)
        self.assertNotIn("<strong>01", rendered)
        self.assertIn(escape(plate.note), rendered)

    def test_unknown_art_is_left_alone(self):
        original = '<p><img src="assets/images/diagrams/reverse-proxy.svg" alt="topology" width="1200" height="420"></p>'
        self.assertEqual(site_plates.enhance(original), original)


if __name__ == "__main__":
    unittest.main()
