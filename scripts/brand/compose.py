#!/usr/bin/env python3
"""Render the promotional plates in Chromium at exact pixel sizes."""
from __future__ import annotations

import json
import sys
from pathlib import Path

from playwright.sync_api import sync_playwright

from scripts.brand import facts

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
SHOT = ROOT / "docs/images/overview/dashboard.png"
MARK = ROOT / "internal/web/static/favicon.svg"
FONT_DIR = ROOT / "site-src/assets/fonts"
FIELD_DIR = HERE / "fields"

THEMES = {
    "dark": {
        "bg": "#141210",
        "text": "#f2eee7",
        "muted": "#a39b8b",
        "faint": "#8a8274",
        "caption": "#a39b8b",
        "hair": "rgba(232,228,222,.14)",
        "hair2": "rgba(232,228,222,.30)",
        "accent": "#7aa0ff",
        "accent2": "#b18cff",
        "field": (FIELD_DIR / "cover-dark.jpg").as_uri(),
        "pane": "#141210",
        "pane_border": "rgba(232,228,222,.22)",
        "shadow": "0 34px 76px rgba(0,0,0,.62), 0 2px 0 rgba(255,255,255,.04) inset",
        "mark_shadow": "0 6px 18px rgba(0,0,0,.35)",
        "wash": "linear-gradient(90deg, rgba(12,11,9,.88) 0%, rgba(12,11,9,.80) 30%, rgba(12,11,9,.38) 54%, rgba(12,11,9,.02) 74%, rgba(12,11,9,.18) 100%)",
        "vignette": "radial-gradient(120% 120% at 78% 46%, transparent 35%, rgba(0,0,0,.55) 100%)",
    },
    "light": {
        "bg": "#f1ece1",
        "text": "#171411",
        "muted": "#5f594e",
        "faint": "#5c554a",
        "caption": "#4e4840",
        "hair": "rgba(23,20,17,.16)",
        "hair2": "rgba(23,20,17,.34)",
        "accent": "#4864d8",
        "accent2": "#7a53d1",
        "field": (FIELD_DIR / "cover-light.jpg").as_uri(),
        "pane": "#141210",
        "pane_border": "rgba(23,20,17,.45)",
        "shadow": "0 16px 30px rgba(52,42,28,.18), 0 2px 6px rgba(52,42,28,.14), 0 1px 0 rgba(255,255,255,.55) inset",
        "mark_shadow": "0 3px 10px rgba(52,42,28,.20)",
        "wash": "linear-gradient(90deg, rgba(244,240,231,.90) 0%, rgba(244,240,231,.84) 32%, rgba(244,240,231,.42) 56%, rgba(244,240,231,.02) 76%, rgba(244,240,231,.55) 100%)",
        "vignette": "radial-gradient(120% 120% at 78% 46%, transparent 40%, rgba(88,74,54,.18) 100%)",
    },
}

CSS = """
@font-face { font-family: "Instrument Sans"; font-weight: 400 700; src: url(__FONT__/instrument-sans.woff2) format("woff2"); }
@font-face { font-family: "Instrument Sans"; font-style: italic; font-weight: 400 700; src: url(__FONT__/instrument-sans-italic.woff2) format("woff2"); }
@font-face { font-family: "Instrument Serif"; font-weight: 400; src: url(__FONT__/instrument-serif.woff2) format("woff2"); }
@font-face { font-family: JBM; font-weight: 400 700; src: url(__FONT__/jetbrains-mono.woff2) format("woff2"); }
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body { width: 100%; height: 100%; }
body { background: #000; }
.plate {
  position: relative; overflow: hidden; background: var(--bg); color: var(--text);
  font-family: "Instrument Sans", system-ui, sans-serif; -webkit-font-smoothing: antialiased;
}
.bg { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; }
.wash { position: absolute; inset: 0; background: var(--wash); }
.vignette { position: absolute; inset: 0; background: var(--vignette); }
.frame { position: absolute; inset: 18px; border: 1px solid var(--hair); border-radius: 10px; }
.frame::before, .frame::after {
  content: ""; position: absolute; width: 7px; height: 7px; border: 1px solid var(--hair2);
}
.frame::before { left: -1px; top: -1px; border-right: 0; border-bottom: 0; border-radius: 10px 0 0 0; }
.frame::after { right: -1px; bottom: -1px; border-left: 0; border-top: 0; border-radius: 0 0 10px 0; }

.brand { position: absolute; display: flex; align-items: center; gap: 14px; }
.mark { border-radius: 23%; box-shadow: var(--mark_shadow); }
.wordmark { font-family: JBM; font-weight: 500; letter-spacing: -.055em; }
.divider { width: 1px; align-self: center; background: var(--hair2); }
.kicker { font-family: JBM; text-transform: uppercase; color: var(--muted); }

.copy { position: absolute; }
.eyebrow { display: flex; align-items: center; gap: 9px; font-family: JBM; text-transform: uppercase; color: var(--muted); }
.eyebrow i { display: block; background: var(--accent); border-radius: 1px; }
h1 { font-family: "Instrument Serif", Georgia, serif; font-weight: 400; letter-spacing: -.02em; line-height: .98; text-wrap: balance; }
.lead { font-weight: 400; color: var(--muted); text-wrap: pretty; }
.lead b { font-weight: 500; color: var(--text); }
.lead.balance { text-wrap: balance; }

.screen { position: absolute; overflow: hidden; background: var(--pane);
  border: 1px solid var(--pane_border); box-shadow: var(--shadow); }
.screen img { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; object-position: top left; display: block; }
.screen .fade { position: absolute; left: 0; right: 0; bottom: 0; height: 76px;
  background: linear-gradient(to bottom, rgba(20,18,16,0), #141210); }
.screen::after { content: ""; position: absolute; inset: 0; pointer-events: none; border-radius: inherit;
  box-shadow: inset 0 1px 0 rgba(255,255,255,.10); }

.plate-crop { position: absolute; overflow: hidden; background: var(--pane);
  border: 1px solid var(--pane_border); box-shadow: var(--shadow); }
.plate-crop .shot { position: absolute; background-image: url(__SHOT__); background-repeat: no-repeat; }
.rail-label { position: absolute; font-family: JBM; text-transform: uppercase; color: var(--faint); letter-spacing: .16em; }

.credits { position: absolute; display: flex; align-items: baseline; justify-content: space-between;
  font-family: JBM; text-transform: uppercase; letter-spacing: .13em; }
.credits .left { display: flex; align-items: center; gap: 10px; color: var(--faint); }
.credits .tick { width: 2px; background: var(--accent); }
.credits .note { display: flex; align-items: center; gap: 7px; color: var(--caption); text-align: right; }
.credits .dot { width: 5px; height: 5px; border-radius: 50%; background: var(--accent); box-shadow: 0 0 8px var(--accent); }
"""


def _vars(t: dict) -> str:
    return "; ".join(f"--{k.replace('_', '-')}: {v}" for k, v in t.items())


def cover(theme: str, width: int, height: int) -> str:
    t = THEMES[theme]
    # One frame spec: 64 px copy margin, a 24 px gap between the pane and the
    # frame on every edge the pane does not bleed through.
    style = f"""
    .plate {{ width: {width}px; height: {height}px; {_vars(t)} }}
    .brand {{ left: 64px; top: 48px; height: 44px; }}
    .mark {{ width: 44px; height: 44px; }}
    .wordmark {{ font-size: 27px; line-height: 1; }}
    .divider {{ height: 30px; }}
    .kicker {{ font-size: 11px; letter-spacing: .19em; }}
    .copy {{ left: 64px; top: 150px; width: 648px; }}
    .eyebrow {{ font-size: 11px; letter-spacing: .18em; }}
    .eyebrow i {{ width: 22px; height: 2px; }}
    h1 {{ font-size: 64px; margin-top: 22px; }}
    .lead {{ font-size: 17px; line-height: 1.6; margin-top: 26px; width: 648px; }}
    .screen {{ left: 736px; top: 40px; width: 822px; height: 548px; border-radius: 16px; }}
    .credits {{ left: 64px; right: 24px; bottom: 22px; font-size: 11px; }}
    .credits .tick {{ height: 12px; }}
    .credits .note {{ font-size: 10.5px; }}
    """
    body = f"""
    <div class="plate">
      <img class="bg" src="{t['field']}">
      <div class="wash"></div>
      <div class="vignette"></div>
      <div class="frame"></div>
      <div class="brand">
        <img class="mark" src="{MARK}">
        <span class="wordmark">millivolt</span>
        <span class="divider"></span>
        <span class="kicker">model relay / dashboard</span>
      </div>
      <div class="copy">
        <p class="eyebrow"><i></i>{facts.EYEBROW}</p>
        <h1>{facts.HEADLINE_HTML}</h1>
        <p class="lead">OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages clients share <b>one self-hosted proxy</b>. Translate between them, route to any configured upstream including the Cursor agent bridge, and account for every request.</p>
      </div>
      <div class="screen"><img src="{SHOT}"><div class="fade"></div></div>
      <div class="credits">
        <span class="left"><span class="tick"></span>{facts.FOOTER}</span>
        <span class="note"><span class="dot"></span>{facts.SAMPLE_NOTE}</span>
      </div>
    </div>
    """
    return f"<style>{CSS}{style}</style>{body}"


def og_card() -> str:
    """Headline, one sentence, and a clean full-width KPI strip."""
    t = THEMES["dark"]
    # The strip is the capture's KPI band: y 60..200 scaled to 1088 wide
    # (0.7556), so the band ends in the gap above the explorer cards.
    style = f"""
    .plate {{ width: 1200px; height: 630px; {_vars(t)} }}
    .brand {{ left: 56px; top: 38px; height: 46px; }}
    .mark {{ width: 46px; height: 46px; }}
    .wordmark {{ font-size: 29px; line-height: 1; }}
    .divider {{ height: 31px; }}
    .kicker {{ font-size: 11px; letter-spacing: .19em; }}
    .copy {{ left: 56px; top: 132px; width: 1040px; }}
    .eyebrow {{ font-size: 11px; letter-spacing: .18em; }}
    .eyebrow i {{ width: 22px; height: 2px; }}
    h1 {{ font-size: 47px; margin-top: 18px; }}
    .lead {{ font-size: 17px; line-height: 1.55; margin-top: 18px; width: 690px; }}
    .screen {{ left: 56px; top: 384px; width: 1088px; height: 116px; border-radius: 12px; }}
    .screen img {{ width: 1088px; top: -45px; }}
    .credits {{ left: 56px; right: 56px; bottom: 30px; font-size: 11px; }}
    .credits .tick {{ height: 12px; }}
    .credits .note {{ font-size: 10.5px; }}
    """
    body = f"""
    <div class="plate">
      <img class="bg" src="{t['field']}">
      <div class="wash"></div>
      <div class="vignette"></div>
      <div class="frame"></div>
      <div class="brand">
        <img class="mark" src="{MARK}">
        <span class="wordmark">millivolt</span>
        <span class="divider"></span>
        <span class="kicker">model relay / dashboard</span>
      </div>
      <div class="copy">
        <p class="eyebrow"><i></i>{facts.EYEBROW}</p>
        <h1>{facts.HEADLINE_HTML}</h1>
        <p class="lead balance">A self-hosted, cross-protocol inference proxy with a live dashboard, SQLite history, and per-request accounting.</p>
      </div>
      <div class="screen"><img src="{SHOT}"></div>
      <div class="credits">
        <span class="left"><span class="tick"></span>{facts.FOOTER}</span>
        <span class="note"><span class="dot"></span>{facts.SAMPLE_NOTE}</span>
      </div>
    </div>
    """
    return f"<style>{CSS}{style}</style>{body}"


VARIANTS = {
    "cover-dark": ("dark", 1600, 640),
    "cover-light": ("light", 1600, 640),
}


def render(name: str, out: Path, html: str, width: int, height: int) -> None:
    html = html.replace("__FONT__", FONT_DIR.as_uri()).replace("__SHOT__", SHOT.as_uri())
    page_path = HERE / f"{name}.html"
    page_path.write_text(f"<!doctype html><meta charset=utf-8>{html}", encoding="utf-8")
    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": width, "height": height},
                                device_scale_factor=1)
        page.goto(page_path.as_uri(), wait_until="networkidle")
        page.wait_for_timeout(300)
        page.locator(".plate").screenshot(path=str(out))
        browser.close()
    page_path.unlink(missing_ok=True)
    print(json.dumps({"out": str(out), "width": width, "height": height}))


