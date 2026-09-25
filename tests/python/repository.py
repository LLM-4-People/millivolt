#!/usr/bin/env python3
"""Deterministic offline regressions for the small publication checker."""

import os
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

from .. import repository_check as check
from ..support import RETIRED_TOKENS, _template_comment_spans


class RepositoryChecks(unittest.TestCase):
    def test_test_source_layout(self):
        for name in ("internal/unit_test.go", "scripts/browser_check.py", "ui_check.js", "other.spec.js"):
            with self.subTest(name=name):
                self.assertEqual(check.test_layout_errors([name]),
                                 ["test source must live under tests/: " + name])
        self.assertEqual(check.test_layout_errors([
            "tests/_go/internal/unit.go", "tests/python/go.py", "tests/browser_check.py", "tests/ui_check.js",
            "scripts/check.sh", "scripts/backup_db.py", "scripts/licenses/main.go", "cmd/stress/main.go",
        ]), [])
        for name in ("tests/_go/internal/unit_test.go", "tests/python/go_test.py", "tests/unit_test.js"):
            with self.subTest(name=name):
                self.assertEqual(check.test_layout_errors([name]),
                                 ["physical test filename must omit _test suffix: " + name])

    def test_markdown_targets_and_scope(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / ".gitignore").write_text("node_modules/\n", encoding="utf-8")
            (root / "docs").mkdir()
            (root / "node_modules").mkdir()
            (root / "docs" / "existing file.md").write_text("# Existing\n", encoding="utf-8")
            (root / "node_modules" / "README.md").write_text("[ignored](absent)", encoding="utf-8")
            fence = chr(96) * 3
            (root / "README.md").write_text(
                "[good](docs/existing%20file.md#heading)\n"
                "[external](https://example.invalid/no-network)\n"
                "[mail](mailto:operator@example.invalid)\n"
                "[local anchor](#heading)\n"
                + fence + "\n[example](absent-example)\n" + fence + "\n"
                "[reference]: <docs/existing file.md>\n", encoding="utf-8",
            )
            self.assertEqual(check.markdown_errors(root), [])
            (root / "docs" / "bad.md").write_text("[broken](missing.md)\n", encoding="utf-8")
            self.assertEqual(check.markdown_errors(root),
                             ["docs/bad.md:1: missing local link missing.md"])

    def test_markdown_escape_is_not_portable(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "repo"
            root.mkdir()
            (root / ".gitignore").write_text("", encoding="utf-8")
            (Path(temp) / "outside.md").write_text("outside", encoding="utf-8")
            (root / "README.md").write_text("[outside](../outside.md)", encoding="utf-8")
            self.assertEqual(len(check.markdown_errors(root)), 1)

    def test_private_markdown_is_skipped_and_cannot_satisfy_public_links(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / ".gitignore").write_text("private.md\n", encoding="utf-8")
            (root / "private.md").write_text("[not public](missing.md)\n" + chr(0x2014), encoding="utf-8")
            (root / "README.md").write_text("[public link](private.md)\n", encoding="utf-8")
            self.assertEqual(check.markdown_errors(root),
                             ["README.md:1: missing local link private.md"])
            (root / "README.md").write_text("Public text\n", encoding="utf-8")
            self.assertEqual(check.markdown_errors(root), [])
            _, public = check.source_inventory(root)
            self.assertNotIn("private.md", public)
            self.assertEqual(check.typography_errors(root, public), [])

    def test_git_environment_drops_routing_and_config(self):
        with patch.dict(os.environ, {"GIT_DIR": "/never-use", "GIT_WORK_TREE": "/never-use",
                                     "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.excludesFile",
                                     "GIT_CONFIG_VALUE_0": "/never-use", "GIT_TEMPLATE_DIR": "/never-use"}):
            env = check.git_environment()
        self.assertEqual({key for key in env if key.startswith("GIT_")},
                         {"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL"})
        self.assertEqual(env["GIT_CONFIG_GLOBAL"], os.devnull)

    @unittest.skipUnless(shutil.which("git"), "git is required for publication ignore checks")
    def test_real_git_detects_missing_ignore_and_hidden_source(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            patterns = "".join("/" + name + "\n" for name in check.IGNORED_PATHS)
            (root / ".gitignore").write_text(patterns, encoding="utf-8")
            with patch.dict(os.environ, {"GIT_DIR": str(root / "must-not-exist"),
                                         "GIT_WORK_TREE": str(root / "must-not-exist")}):
                self.assertEqual(check.publication_errors(root), [])
            self.assertFalse((root / "must-not-exist").exists())
            self.assertFalse((root / ".git").exists())
            # An unanchored binary name must not hide cmd/proxy source.
            (root / ".gitignore").write_text(patterns.replace("/proxy.yaml\n", "") + "proxy\n",
                                            encoding="utf-8")
            errors = check.publication_errors(root)
            self.assertIn("sensitive/runtime path is not ignored: proxy.yaml", errors)
            self.assertIn("public source is ignored: cmd/proxy/main.go", errors)

    def test_typography_checks_text_without_following_symlinks(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            dash = chr(0x2014)
            (root / "source.js").write_text("// fine\n// wrong " + dash + " punctuation\n", encoding="utf-8")
            (root / "binary").write_bytes(b"\0" + dash.encode("utf-8"))
            (root / "invalid-utf8").write_bytes(b"\xff" + dash.encode("utf-8"))
            (root / "link").symlink_to(root / "source.js")
            self.assertEqual(check.typography_errors(root, ["source.js", "binary", "invalid-utf8", "link"]),
                             ["source.js:2: use sentence punctuation or '-' instead of an em dash"])

    @unittest.skipUnless(shutil.which("git"), "git is required for publication ignore checks")
    def test_publication_typography_excludes_private_and_dependency_files(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            patterns = "".join("/" + name + "\n" for name in check.IGNORED_PATHS)
            (root / ".gitignore").write_text(patterns + "node_modules/\n", encoding="utf-8")
            (root / "node_modules").mkdir()
            for name in ["proxy.yaml", "node_modules/private.js", "public.js"]:
                (root / name).write_text(chr(0x2014), encoding="utf-8")
            with patch.dict(os.environ, {"GIT_DIR": str(root / "must-not-exist"),
                                         "GIT_WORK_TREE": str(root / "must-not-exist")}):
                self.assertEqual(check.publication_errors(root),
                                 ["public.js:1: use sentence punctuation or '-' instead of an em dash"])
            self.assertFalse((root / "must-not-exist").exists())

    def test_dead_css_class_detector(self):
        live = [("internal/web/static/css/dash.css", ".tile { color: red }\n.tile.off { opacity: .5 }\n"),
                ("internal/web/static/js/app.js", 'render(`<span class="tile${hidden ? " off" : ""}">x</span>`);\n'),
                ("internal/web/static/index.html", '<span class="tile"></span>\n')]
        self.assertEqual(check.dead_css_class_errors(live), [])
        dead = live + [("internal/web/static/css/dash.css", ".ghost { color: blue }\n")]
        self.assertEqual(check.dead_css_class_errors(dead),
                         ["internal/web/static/css/dash.css: stylesheet class .ghost has no emitter"])

    def test_duplicate_js_function_detector(self):
        twin = 'function one(box) {\n  return [...document.querySelectorAll("#" + box)].length;\n}\n'
        text = (twin.replace('one', 'alpha') + twin.replace('one', 'beta')
                + 'function gamma(box) {\n  return [...document.querySelectorAll("#" + box)].map(c => c);\n}\n')
        pairs = [("internal/web/static/js/app.js", text)]
        self.assertEqual(check.duplicate_js_function_errors(pairs), [
            "internal/web/static/js/app.js:4: function beta duplicates alpha "
            "(internal/web/static/js/app.js:1) - extract the shared helper"])
        self.assertEqual(check.duplicate_js_function_errors(
            [("internal/web/static/js/app.js",
              'function alpha(x) { return x + 1; }\nfunction beta(x) { return x + 1; }\n')]), [])

    def test_removed_vocabulary_detector(self):
        clean = [("internal/web/static/js/app.js", "render('spark');\n")]
        self.assertEqual(check.removed_vocabulary_errors(clean), [])
        stale = [("internal/web/static/js/app.js", "render(); // the old spark-row layout\n")]
        self.assertEqual(check.removed_vocabulary_errors(stale), [
            "internal/web/static/js/app.js: retired token 'spark-row' "
            "(the per-half spark rows became one multi-line sparkline per tile)"])
        # Retired vocabulary flags in every spelling the audit found: case
        # folded, -, _, / and space separators, camel humps, squished
        # forms, and plurals.
        gap_variants = (
            "the Spark-Row layout", "the SPARK_ROW layout", "the spark rows layout",
            "the TileFmt layout", "the TILE_FMT layout",
            "the InOutRatio layout", "the in_out_ratio layout",
            "the ChartStackPaths layout", "the Ten tiles layout", "the TEN-TILE layout",
            "the sparkRow layout", "the tenTiles layout", "the in/out ratio layout",
        )
        for variant in gap_variants:
            with self.subTest(variant=variant):
                errors = check.removed_vocabulary_errors(
                    [("internal/web/static/js/app.js", "render(); // " + variant + "\n")])
                self.assertTrue(errors, "retired variant must flag: " + variant)
        # The exact current spellings still flag.
        for token, reason in RETIRED_TOKENS:
            with self.subTest(token=token):
                errors = check.removed_vocabulary_errors(
                    [("internal/web/static/js/app.js", "render(); // the " + token + " layout\n")])
                self.assertTrue(errors, "retired token must flag: " + token)
        # Near-miss prose must not flag.
        negatives = (
            "XTEN TILESX", "sparkline rows", "tile formats",
            "the tile format helper lives on", "tilefmtx",
            "input/output ratio commentary", "attentiveness",
            "stacked paths of the chart", "the tennis court",
            "in outage ratio",
        )
        for negative in negatives:
            with self.subTest(negative=negative):
                self.assertEqual(check.removed_vocabulary_errors(
                    [("internal/web/static/js/app.js", "render(); // " + negative + "\n")]), [])

    def test_js_template_comment_detector(self):
        pairs = [("internal/web/static/js/app.js",
                  'const a = `text\n // stray comment in the body\nmore`;\n'
                  'const b = `ok ${x.map(v => `item ${v}`).join("")}`;\n'
                  'const url = `https://example.invalid/x`;\n')]
        self.assertEqual(check.js_template_comment_errors(pairs),
                         ["internal/web/static/js/app.js:2: JS comment inside a template-literal body"])
        self.assertEqual(check.js_template_comment_errors(
            [("internal/web/static/js/app.js", 'const ok = `plain body\nwith https://example.invalid link`;\n')]), [])
        # Quotes, backticks and braces inside regex literals must not
        # desynchronize the lexer; comments inside ${...} expressions are
        # skipped, never flagged. Each row is a real violation class the
        # corpus audit found. The division row is a control: a naive
        # always-regex lexer would swallow the template after it.
        flagged = "internal/web/static/js/app.js:{}: JS comment inside a template-literal body"
        rows = (
            # a " inside a regex before a template (the backup download block)
            ('const m = /say="([^"]+)"/.exec(s);\nconst t = `ok ${m}\n// stray\n`;\n',
             [flagged.format(3)]),
            # a ' and a ` inside a regex (the provider header token shape)
            ("const RE = /^[!#'`|~]+$/;\nconst t = `ok\n// stray\n`;\n",
             [flagged.format(3)]),
            # a lone ` in a regex: the old lexer also turned the real code
            # comment after it into a phantom template body (false positive)
            ('const re = /`/;\n// innocent code comment\nconst t = `ok\n// stray\n`;\n',
             [flagged.format(4)]),
            # quotes and braces inside a regex inside an expression
            ('const t = `x${/"}/.test(s)}y\n// stray\n`;\n',
             [flagged.format(2)]),
            # division context: the slash after an identifier is not a regex
            ('const mib = n / (1024 * 1024);\nconst t = `ok\n// stray\n`;\n',
             [flagged.format(3)]),
            # postfix ++ before a division: the + must not classify the
            # following / as a regex, whose phantom scan swallowed the
            # template's opening backtick and hid the comment
            ('const t = `x${a++ / 2}y\n// stray\n`;\n',
             [flagged.format(2)]),
            # same postfix shape in statement position, template on the
            # same source line
            ('const x = a++ / 2; const t = `ok\n// stray\n`;\n',
             [flagged.format(2)]),
            # the string skipper's newline bail bounds the worst desync: a
            # statement-position regex after ')' misreads as division and
            # its quote opens a phantom string, but the bail stops the
            # swallow at the line end so the next template still tracks
            ('if (c) /["]/.test(s);\nconst t = `ok\n// stray\n`;\n',
             [flagged.format(3)]),
            # a character class carrying {, } and " inside an expression
            ('const t = `x${/[{}"]/.source.length}y\n// stray\n`;\n',
             [flagged.format(2)]),
            # a character class carrying /, " and `: none of them close it
            ('const t = `x${/[/"`]/.source.length}y\n// stray\n`;\n',
             [flagged.format(2)]),
            # an apostrophe inside a block comment inside an expression
            ("const t = `x${s /* it's */}y\n// stray\n`;\n",
             [flagged.format(2)]),
            # an apostrophe inside a line comment inside an expression
            ("const t = `x${s // it's\n}y\n// stray\n`;\n",
             [flagged.format(3)]),
            # a } inside a block comment must not phantom-close the
            # expression and swallow the expression's own comment line
            ('const t = `a${s /* } */\n// note\n+ 1}b\n`;\n', []),
        )
        for text, expected in rows:
            with self.subTest(text=text):
                self.assertEqual(check.js_template_comment_errors(
                    [("internal/web/static/js/app.js", text)]), expected)
        # A body line starting with /* renders as text, exactly like //.
        self.assertEqual(check.js_template_comment_errors(
            [("internal/web/static/js/app.js", 'const t = `x\n/* rendered\n`;\n')]),
            [flagged.format(2)])
        # Known accepted false-positive class: a rendered URL at a body line
        # start still flags - suspicious either way.
        self.assertEqual(check.js_template_comment_errors(
            [("internal/web/static/js/app.js", 'const t = `x\n//cdn.example/x\n`;\n')]),
            [flagged.format(2)])

    def test_template_spans_scan_the_real_shadowed_regions(self):
        """Corpus canaries: the regions the regex-blind lexer lost to quote
        desync must be scanned. fillSettingsLive is the first region the
        chrome.js backup-download regex shadows; renderDrawer and
        fillDrawerDebug sit behind the escapeHtml regex in log.js."""
        chrome = (check.ROOT / "internal/web/static/js/chrome.js").read_text(encoding="utf-8")
        spans = _template_comment_spans(chrome)
        needle = '<div class="st-live-row"><span class="k">listen</span>'
        start = chrome.index(needle)
        self.assertTrue(any(s <= start and start + len(needle) <= e for s, e in spans),
                        "fillSettingsLive template body is not scanned")
        log = (check.ROOT / "internal/web/static/js/log.js").read_text(encoding="utf-8")
        spans = _template_comment_spans(log)
        expr = log.index("clientLabel(r.client) || '?'")
        sep = log.index(" · ", expr)
        self.assertTrue(any(s <= sep and sep + 3 <= e for s, e in spans),
                        "renderDrawer template body is not scanned")
        for needle in ("<h4>Request body", "<h4>Response body"):
            start = log.index(needle)
            self.assertTrue(any(s <= start and start + len(needle) <= e for s, e in spans),
                            "fillDrawerDebug template body is not scanned: " + needle)

    def test_spark_height_mirror_detector(self):
        js = 'const CHART_SPARK_W = 72, CHART_SPARK_H = 14;\n'
        css = '.chart-total .spark { display: block; height: 14px; }\n'
        pairs = [("internal/web/static/js/chart.js", js),
                 ("internal/web/static/css/dashboard.css", css)]
        self.assertEqual(check.spark_height_mirror_errors(pairs), [])
        drifted = [("internal/web/static/js/chart.js", js),
                   ("internal/web/static/css/dashboard.css",
                    '.chart-total .spark { display: block; height: 13px; }\n')]
        self.assertEqual(check.spark_height_mirror_errors(drifted), [
            "internal/web/static/js/chart.js: CHART_SPARK_H=14 no longer mirrors "
            "the .chart-total .spark height=13px in internal/web/static/css/dashboard.css"])
        # A vanished anchor is drift too, never a silent pass.
        self.assertEqual(check.spark_height_mirror_errors(
            [("internal/web/static/js/chart.js", 'const x = 1;\n'),
             ("internal/web/static/css/dashboard.css", css)]),
            ["internal/web/static/js/chart.js: CHART_SPARK_H declaration not found"])
        self.assertEqual(check.spark_height_mirror_errors(
            [("internal/web/static/js/chart.js", js),
             ("internal/web/static/css/dashboard.css", '.x { height: 14px; }\n')]),
            ["internal/web/static/css/dashboard.css: .chart-total .spark height not found"])

    def test_palette_mirror_detector(self):
        good = [
            ("internal/web/static/css/dashboard.css",
             ':root { --bg: #1b1826; --accent: #5b8cff; }\n'),
            ("internal/web/static/sw.js",
             '"background:#1b1826;\'><p style="color:#5b8cff">offline</p></body></html>"'),
            ("internal/web/static/manifest.webmanifest", '{"theme_color": "#1b1826"}'),
            ("internal/web/static/index.html", '<meta name="theme-color" content="#1b1826">'),
            ("internal/web/static/favicon.svg", '<stop stop-color="#5B8CFF"/>'),
        ]
        self.assertEqual(check.palette_mirror_errors(good), [])
        off_palette = good[:-1] + [("internal/web/static/favicon.svg",
                                    '<stop stop-color="#badbee"/>')]
        self.assertEqual(check.palette_mirror_errors(off_palette), [
            "internal/web/static/favicon.svg: hex #badbee is not a :root custom-property "
            "value in internal/web/static/css/dashboard.css"])
        # The palette owner is the :root block only: a legitimate non-:root
        # hex in dashboard.css must not satisfy a mirror.
        non_root = [
            ("internal/web/static/css/dashboard.css",
             ':root { --bg: #1b1826; }\n.mr-err { color: #ff7d72; }\n'),
            ("internal/web/static/index.html", '<meta name="theme-color" content="#ff7d72">'),
        ]
        self.assertEqual(check.palette_mirror_errors(non_root), [
            "internal/web/static/index.html: hex #ff7d72 is not a :root custom-property "
            "value in internal/web/static/css/dashboard.css"])
        self.assertEqual(check.palette_mirror_errors(good[1:]),
                         ["internal/web/static/css/dashboard.css: "
                          "the :root custom-property palette is missing"])

    def test_gzip_corpus_assembly_reaches_root_and_deploy(self):
        """The perimeter's corpus half, pinned: gzip_corpus must feed the
        choke-point detector every tracked .go file outside tests, including
        a root-level file (version.go) and a deploy/ file (the synthetic
        pair the unit rows already use). A revert of the assembly to a
        narrower directory-prefix filter reddens here even though every
        detector unit row stays green - the unit rows feed synthetic pairs
        directly and never see the assembly."""
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            names = [
                "version.go", "cmd/proxy/main.go", "deploy/sidecar/main.go",
                "tests/_go/internal/web/web.go", "binary.go", "invalid.go", "notes.txt",
            ]
            for name in names[:4]:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("package x\n", encoding="utf-8")
            (root / "binary.go").write_bytes(b"\0package x\n")
            (root / "invalid.go").write_bytes(b"\xffpackage x\n")
            (root / "notes.txt").write_text("not go\n", encoding="utf-8")
            self.assertEqual([name for name, _ in check.gzip_corpus(root, names)],
                             ["version.go", "cmd/proxy/main.go", "deploy/sidecar/main.go"])

    def test_check_assembles_detector_corpus_through_gzip_corpus(self):
        """The perimeter's wiring half, pinned: check() must assemble the
        choke-point detector's corpus by CALLING gzip_corpus. The corpus
        unit row pins the filter's reach; this row pins the call site, so
        a check() that inlines a narrower prefix-filtered loop while
        gzip_corpus stays defined reddens here even though every detector
        unit row stays green. The match is textual inside check()'s
        region: a real call satisfies it, and so would call-shaped comment
        text; the argument/result residual (a narrowed argument list or a
        discarded result bypasses it) is recorded in agents/accepts.md."""
        source = (check.ROOT / "tests" / "repository_check.py").read_text(encoding="utf-8")
        start = source.index("def check(")
        end = source.index("\ndef ", start)
        self.assertRegex(source[start:end], r"\bgzip_corpus\s*\(",
                         "check() no longer assembles the detector corpus through gzip_corpus")

    def test_artifact_gzip_choke_point_detector(self):
        good = [
            ("cmd/proxy/log.go", '\tzw := proxy.NewArtifactGzipWriter(ew)\n'),
            ("internal/proxy/debug.go", '\tzw := NewArtifactGzipWriter(&compressed)\n'),
        ]
        self.assertEqual(check.artifact_gzip_choke_point_errors(good), [])
        # The capture gate hand-wraps instead of crossing the choke point: the
        # wrap is gone AND the direct construction is banned - one row proves
        # both new failure modes.
        hand_wrapped = good[:1] + [
            ("internal/proxy/debug.go", '\tzw := gzip.NewWriterLevel(&compressed, 2)\n')]
        self.assertEqual(check.artifact_gzip_choke_point_errors(hand_wrapped), [
            "internal/proxy/debug.go: the NewArtifactGzipWriter artifact choke point is gone - "
            "every artifact download must cross the one shared gzip artifact writer",
            "internal/proxy/debug.go: direct compress/gzip writer construction - "
            "NewArtifactGzipWriter is the only saved-artifact construction site"])
        # A duplicated per-branch wrap is drift, never a silent pass.
        duplicated = good[:1] + [
            ("internal/proxy/debug.go",
             '\tzw := NewArtifactGzipWriter(&compressed)\n'
             '\tzw2 := NewArtifactGzipWriter(zw)\n')]
        self.assertEqual(check.artifact_gzip_choke_point_errors(duplicated), [
            "internal/proxy/debug.go: 2 NewArtifactGzipWriter wraps - "
            "the artifact compression gate is one choke point, never a per-branch writer"])
        # The export gate answers the same rules with its own file named.
        self.assertEqual(check.artifact_gzip_choke_point_errors([
            ("cmd/proxy/log.go", '\tzw := gzip.NewWriter(ew)\n'),
            good[1]]), [
            "cmd/proxy/log.go: the NewArtifactGzipWriter artifact choke point is gone - "
            "every artifact download must cross the one shared gzip artifact writer",
            "cmd/proxy/log.go: direct compress/gzip writer construction - "
            "NewArtifactGzipWriter is the only saved-artifact construction site"])
        # Either gate leaving the corpus fails too, never a silent pass.
        self.assertEqual(check.artifact_gzip_choke_point_errors(good[1:]),
                         ["cmd/proxy/log.go: artifact source is not in the checked corpus"])
        self.assertEqual(check.artifact_gzip_choke_point_errors(good[:1]),
                         ["internal/proxy/debug.go: artifact source is not in the checked corpus"])
        self.assertEqual(check.artifact_gzip_choke_point_errors([]), [
            "cmd/proxy/log.go: artifact source is not in the checked corpus",
            "internal/proxy/debug.go: artifact source is not in the checked corpus"])
        # The whole-tree construction ban: a hand-rolled writer in any new
        # source file outside tests is caught with its line, while the two
        # codec owners and test sources are the only clean constructions.
        hand_rolled_tree = good + [
            ("internal/backup/export.go",
             "func save(w io.Writer) {\n"
             "\tzw := gzip.NewWriter(w)\n"
             "\tdefer zw.Close()\n"
             "}\n")]
        self.assertEqual(check.artifact_gzip_choke_point_errors(hand_rolled_tree), [
            "internal/backup/export.go:2: direct compress/gzip writer construction - "
            "the only codec owners are internal/proxy/gzip.go (saved artifacts) "
            "and internal/web/gzip.go (the dashboard's transit and precomputed representations)"])
        # The perimeter reaches every tracked .go file outside tests: a
        # construction in a root-level or deploy/ Go file is caught with the
        # same owner-label wording.
        for name in ("version.go", "deploy/sidecar/main.go"):
            self.assertEqual(check.artifact_gzip_choke_point_errors(good + [
                (name, "func save(w io.Writer) {\n\tzw := gzip.NewWriterLevel(w, 2)\n}\n")]), [
                name + ":2: direct compress/gzip writer construction - "
                "the only codec owners are internal/proxy/gzip.go (saved artifacts) "
                "and internal/web/gzip.go (the dashboard's transit and precomputed representations)"])
        clean_constructions = good + [
            ("internal/proxy/gzip.go", "\tzw, err := gzip.NewWriterLevel(w, artifactGzipLevel)\n"),
            ("internal/web/gzip.go", "\twriter, err := gzip.NewWriterLevel(nil, dashboardGzipLevel)\n"),
            ("tests/_go/internal/web/web.go", "\twriter, err := gzip.NewWriterLevel(&encoded, level.value)\n"),
        ]
        self.assertEqual(check.artifact_gzip_choke_point_errors(clean_constructions), [])
        # The real tree: both gates cross the choke point exactly once, with
        # no direct compress/gzip construction of their own, and the two
        # codec owners are the only direct construction sites - the moved
        # precompute call site stays clean.
        self.assertEqual(check.artifact_gzip_choke_point_errors([
            ("cmd/proxy/log.go",
             (check.ROOT / "cmd/proxy/log.go").read_text(encoding="utf-8")),
            ("internal/proxy/debug.go",
             (check.ROOT / "internal/proxy/debug.go").read_text(encoding="utf-8")),
            ("internal/proxy/gzip.go",
             (check.ROOT / "internal/proxy/gzip.go").read_text(encoding="utf-8")),
            ("internal/web/gzip.go",
             (check.ROOT / "internal/web/gzip.go").read_text(encoding="utf-8")),
            ("internal/web/web.go",
             (check.ROOT / "internal/web/web.go").read_text(encoding="utf-8"))]), [])


if __name__ == "__main__":
    unittest.main()
