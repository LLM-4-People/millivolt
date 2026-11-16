"""Capture every plate in scripts.gallery from a read-only dashboard session.

This module owns the scene strategies, not the plate list. The registry says
which view to take and with which declared setup; this module turns one scene
kind into browser steps, stages the pixels, and writes the capture manifest.
Adding a view means adding a Plate, never editing a second capture list.
"""
from __future__ import annotations

from pathlib import Path
from urllib.parse import quote

from playwright.async_api import Page

from scripts.gallery import GALLERY, Plate, compose, manifest
from scripts.gallery.model import VIEWPORTS
from .browser_check import require
from .support import pick_chart_control

NAV_TIMEOUT = 30000
READY_TIMEOUT = 40000
CAPTURE_SETTLE = 250


def navigation(base: str, filters, dim: str) -> str:
    pairs = [*filters, ("by", dim)]
    return base + "/#/" + "/".join(
        quote(str(part), safe="") for pair in pairs for part in pair)


def docs_directory(value) -> Path:
    output = Path(value).absolute()
    require(output.resolve() == output and (not output.exists() or output.is_dir()),
            "documentation output must be a direct directory, without symlinked ancestors")
    return output


async def capture_image(page: Page, destination: Path, selector: str | None = None,
                        *, clip: dict | None = None, full_page: bool = True) -> None:
    docs_directory(destination.parent)
    require(not destination.is_symlink(), "refusing a symlinked documentation image")
    destination.parent.mkdir(parents=True, exist_ok=True)
    # A click that opened this surface can leave a hover card open; the shutter
    # must not publish a tooltip as part of the view.
    await page.mouse.move(0, 0)
    await page.evaluate("""async () => {
        await document.fonts.ready;
        await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    }""")
    options = {"path": str(destination), "type": "png", "scale": "css",
               "animations": "disabled"}
    if clip is not None:
        await page.screenshot(clip=clip, **options)
    elif selector is not None:
        await page.locator(selector).screenshot(**options)
    else:
        await page.screenshot(full_page=full_page, **options)


async def wait_ready(page: Page, dim: str, count: int, *, chart: bool = True,
                     plot: bool = False, tiles: bool = False) -> None:
    """Wait for one scope's aggregates to agree before the shutter.

    Data agreement, a rendered plot and the summary tiles are separate
    questions: the saved chart preset can restore the tiles-only overview on a
    fresh navigation, so a data wait must not require a plot canvas.
    """
    ready = ["explorerAgg?.dim === dim", "explorerAgg.scope?.matches === n",
             "kpiAgg?.requests === n", "lastData", "!_renderQueued", "!_renderDirty"]
    if chart:
        ready.append("chartAgg?.buckets.reduce((sum,b) => sum+b.req,0) === n")
    if plot:
        ready.append("!!_up")
    if tiles:
        ready.append("document.querySelectorAll('#chart-totals .chart-total').length >= 5")
    await page.wait_for_function(
        "([dim,n]) => " + " && ".join(ready), arg=[dim, count], timeout=READY_TIMEOUT)
    require(await page.evaluate("""() => kpiInFlight() === 0 &&
        !lastData.in_flight_records?.length && storageState?.dropped === 0"""),
        'history capture has pending work or storage loss')


async def wait_scoped(page: Page, total: int) -> None:
    """Wait for a filtered scope, never the unfiltered total.

    Before the click lands, the scope still matches every record and the
    chart sum equals it; that state satisfies a naive "some scope" wait and
    the capture then misses the chip. The total is what distinguishes the
    two.
    """
    await page.wait_for_function("""total => {
        const n = explorerAgg?.scope?.matches;
        return typeof n === 'number' && n > 0 && n < total &&
            document.querySelector('#xp-scope .chip-x[data-chip-dim]') &&
            chartAgg?.buckets.reduce((sum,b) => sum+b.req,0) === n &&
            lastData && !_renderQueued && !_renderDirty;
    }""", arg=total, timeout=READY_TIMEOUT)
    require(await page.evaluate("() => kpiInFlight() === 0 && storageState?.dropped === 0"),
            'scoped history capture has pending work or storage loss')


async def set_viewport(page: Page, name: str) -> None:
    viewport = VIEWPORTS[name]
    await page.set_viewport_size({"width": viewport.width, "height": viewport.height})


async def apply_mode(page: Page, mode: str) -> None:
    """Recolour the live page through the app's one theme mutation path.

    A reload would discard a drawer, a filter or an open tray, so the two
    halves of a plate must come from one document. applyState is the shared
    owner of that mutation for both surfaces.
    """
    await page.evaluate("""mode => {
        if (typeof applyState !== 'function') {
            throw new Error('theme state module is not loaded');
        }
        applyState('instrument', mode, false);
    }""", mode)
    await page.evaluate("""async () => {
        await document.fonts.ready;
        await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    }""")
    await page.wait_for_function("""() =>
        (typeof _renderDirty === 'undefined' || (!_renderDirty && !_renderQueued))""",
        timeout=READY_TIMEOUT)
    await page.wait_for_timeout(CAPTURE_SETTLE)


async def _goto(page: Page, base: str, dim: str) -> None:
    await page.goto(navigation(base, (), dim), wait_until="domcontentloaded")


async def _window(page: Page, value: str | None) -> None:
    if value:
        await pick_chart_control(page, "#chart-window", value)


async def scene_page(page: Page, base: str, plate: Plate, count: int) -> None:
    setup = plate.setup
    await set_viewport(page, str(setup["viewport"]))
    await _goto(page, base, str(setup["dim"]))
    await wait_ready(page, str(setup["dim"]), count)
    await _window(page, setup.get("window"))  # type: ignore[arg-type]
    if setup.get("window"):
        await wait_ready(page, str(setup["dim"]), count)
    return  # captured by run()


async def scene_element(page: Page, base: str, plate: Plate, count: int) -> None:
    setup = plate.setup
    await set_viewport(page, str(setup["viewport"]))
    await _goto(page, base, str(setup["dim"]))
    await wait_ready(page, str(setup["dim"]), count)
    await _window(page, setup.get("window"))  # type: ignore[arg-type]
    if setup.get("window"):
        await wait_ready(page, str(setup["dim"]), count)
    fit = setup.get("require_fit")
    if fit:
        require(await page.locator(str(fit)).evaluate(
            "el => el.scrollWidth <= el.clientWidth + 2"),
            f"{plate.id}: capture would clip a column")


async def scene_scoped(page: Page, base: str, plate: Plate, count: int) -> None:
    await set_viewport(page, str(plate.setup["viewport"]))
    await _goto(page, base, "provider")
    await wait_ready(page, "provider", count)
    cards = page.locator("#xp-gallery .xp-node[data-xp-key]")
    require(await cards.count() >= 2, "scoped explorer needs two values")
    await cards.first.click()
    await wait_scoped(page, count)
    require(await page.locator("#xp-scope .chip-x[data-chip-dim]").count() == 1,
            "scoped capture has no removable filter chip")


async def scene_attempts(page: Page, base: str, plate: Plate, count: int) -> None:
    await set_viewport(page, str(plate.setup["viewport"]))
    await _goto(page, base, "model")
    await wait_ready(page, "model", count)
    request_id = await page.evaluate("""() => {
        const rows = logVisible().filter(r => !r.live && !r.debug && r.attempts?.length);
        return rows[0]?.id;
    }""")
    require(request_id, "history has no visible request with absorbed attempts")
    toggle = page.locator(f'.retry-toggle[data-retry-for="{request_id}"]')
    require(await toggle.count() == 1, "retry badge is missing for the chosen row")
    await toggle.click()
    await page.wait_for_function(
        """id => document.querySelectorAll(`tr.retry-sub[data-retry-of="${CSS.escape(id)}"]:not([hidden])`).length > 0""",
        arg=request_id)
    await page.locator("#tbl-requests").evaluate("el => { el.scrollTop = 0; }")


async def scene_drawer(page: Page, base: str, plate: Plate, count: int) -> None:
    setup = plate.setup
    await set_viewport(page, str(setup["viewport"]))
    await _goto(page, base, "model")
    await wait_ready(page, "model", count)
    group = setup.get("group")

    def candidate_js(group):
        return """group => {
            const rows = logVisible().filter(r => !r.live && !r.debug &&
                r.usage?.output_tokens > 0 && r.ttft_ms > 0);
            return (group === 'diagnostics'
                ? rows.find(r => r.attempts?.length)
                : rows[0])?.id;
        }"""

    # The newest rows may carry no usage or timing (aborted and retried
    # turns); page the archive until one shows both, rather than pinning a
    # fixture position.
    request_id = await page.evaluate(candidate_js(group), group)
    for _ in range(30):
        if request_id:
            break
        await page.evaluate("""() => {
            const table = document.getElementById('tbl-requests');
            table.scrollTop = table.scrollHeight;
            table.dispatchEvent(new Event('scroll'));
        }""")
        await page.wait_for_timeout(300)
        request_id = await page.evaluate(candidate_js(group), group)
    require(request_id, "history has no visible finalized request for the drawer")
    await page.locator(
        f'#tbl-requests tr.exp-row:not(.retry-sub) [data-open-req="{request_id}"]').click()
    await page.wait_for_function(
        "id => drawerId === id && document.querySelector('#drawer').classList.contains('open')",
        arg=request_id)
    require(await page.locator("#drawer-debug").count() == 0,
            "documentation drawer contains debug data")
    await wait_ready(page, "model", count)
    if group:
        await page.locator(f'#drawer-body [data-group-tab="{group}"]').click()
        await page.locator("#drawer-body").evaluate("""(body, group) => {
            const headings = [...body.querySelectorAll('h4')];
            const wanted = group === 'diagnostics'
                ? ['Retry attempts', 'Queue', 'Rate limit']
                : ['Tokens'];
            const hit = headings.find(h => wanted.some(prefix => h.textContent.startsWith(prefix)));
            if (!hit) throw new Error('request detail has no ' + group + ' section');
            hit.closest('.detail-section').scrollIntoView({block: 'start'});
        }""", group)


async def scene_console(page: Page, base: str, plate: Plate, count: int) -> None:
    setup = plate.setup
    await set_viewport(page, str(setup["viewport"]))
    await _goto(page, base, "model")
    await wait_ready(page, "model", count)
    section = str(setup["section"])
    await page.locator("#btn-controls").click()
    await page.locator("#operator-console").wait_for(state="visible")
    await page.locator(f'.oc-rail-item[data-oc-section="{section}"]').click()
    await page.wait_for_function("""kind => {
        const pane = document.getElementById('oc-pane');
        return pane && pane.dataset.section === kind;
    }""", arg=section)
    if setup.get("composer"):
        await page.locator('[data-oc-new="limit"]').click()
        await page.locator(".oc-compose").wait_for(state="visible")
    else:
        await page.wait_for_function("() => !document.querySelector('.oc-compose')")
    if section == "export":
        require(await page.evaluate("() => ocDrafts.export && ocDrafts.export.mode === 'all'"),
                "documentation export section must start on all history")
        await page.wait_for_function(
            "() => ocDrafts.export && !ocDrafts.export.counting", timeout=15000)
    if section == "system":
        await page.wait_for_function(
            "() => typeof restartServerState.available === 'boolean'")


async def scene_settings(page: Page, base: str, plate: Plate, count: int) -> None:
    setup = plate.setup
    await set_viewport(page, str(setup["viewport"]))
    await _goto(page, base, "model")
    await wait_ready(page, "model", count)
    category = str(setup["category"])
    await page.locator("#btn-settings").click()
    await page.locator(f'#settings-rail [data-st-cat="{category}"]').click()
    await page.wait_for_function("""category => settingsDoc && settingsCat === category &&
        document.querySelector('#btn-settings-apply').disabled""", arg=category)


async def scene_chart(page: Page, base: str, plate: Plate, count: int) -> None:
    setup = plate.setup
    await set_viewport(page, str(setup["viewport"]))
    await _goto(page, base, "model")
    await page.wait_for_function("() => document.querySelector('#chart-preset') && chartAgg")
    await pick_chart_control(page, "#chart-preset", str(setup["preset"]))
    if setup.get("pct"):
        await pick_chart_control(page, "#chart-pct", str(setup["pct"]))
    await _window(page, setup.get("window"))  # type: ignore[arg-type]
    overview = setup["preset"] == "overview"
    await wait_ready(page, "model", count, chart=not overview, plot=not overview,
                     tiles=overview)


async def scene_chart_menu(page: Page, base: str, plate: Plate, count: int) -> None:
    setup = plate.setup
    await set_viewport(page, str(setup["viewport"]))
    await _goto(page, base, "model")
    await wait_ready(page, "model", count)
    await pick_chart_control(page, "#chart-preset", "traffic")
    await _window(page, "all")
    await wait_ready(page, "model", count, plot=True)
    await page.locator("#chart-window").click()
    await page.locator("#chart-window-panel").wait_for(state="visible")


async def _clip_union(page: Page, selectors: tuple[str, ...], pad: int = 12) -> dict:
    boxes = []
    for selector in selectors:
        box = await page.locator(selector).bounding_box()
        if box:
            boxes.append(box)
    require(boxes, "menu capture has no visible boxes")
    left = max(0, min(box["x"] for box in boxes) - pad)
    top = max(0, min(box["y"] for box in boxes) - pad)
    right = max(box["x"] + box["width"] for box in boxes) + pad
    bottom = max(box["y"] + box["height"] for box in boxes) + pad
    viewport = page.viewport_size
    return {"x": left, "y": top, "width": min(right, viewport["width"]) - left,
            "height": min(bottom, viewport["height"]) - top}


async def scene_theme(page: Page, base: str, plate: Plate, count: int) -> None:
    setup = plate.setup
    await set_viewport(page, str(setup["viewport"]))
    await _goto(page, base, "model")
    await wait_ready(page, "model", count)
    await page.locator("#btn-theme").click()
    await page.locator("#theme-menu").wait_for(state="visible")


SCENES = {
    "page": scene_page,
    "element": scene_element,
    "scoped": scene_scoped,
    "attempts": scene_attempts,
    "drawer": scene_drawer,
    "console": scene_console,
    "settings": scene_settings,
    "chart": scene_chart,
    "chart-menu": scene_chart_menu,
    "theme": scene_theme,
}

SELECTORS = {
    "page": None,
    "element": None,
    "scoped": None,
    "attempts": ".grid-pair > .card:last-child",
    "drawer": "#drawer",
    "console": "#operator-console",
    "settings": "#settings-sheet",
    "chart": ".traffic-card",
    "chart-menu": None,
    "theme": "#theme-menu",
}


async def _capture(page: Page, base: str, plate: Plate, count: int, staging: Path) -> None:
    handler = SCENES[plate.kind]
    await handler(page, base, plate, count)
    destination = staging / plate.path.removeprefix("images/")
    work = staging / ".split"
    work.mkdir(parents=True, exist_ok=True)
    dark = work / (plate.id + "-black.png")
    clear = work / (plate.id + "-clear.png")

    async def shoot(target: Path) -> None:
        if plate.kind == "page":
            full_page = bool(plate.setup.get("full_page", True))
            if not full_page:
                await page.evaluate("() => window.scrollTo(0, 0)")
                await page.wait_for_timeout(CAPTURE_SETTLE)
            await capture_image(page, target, full_page=full_page)
        elif plate.kind == "chart-menu":
            clip = await _clip_union(page, (".traffic-card", "#chart-window-panel"))
            await capture_image(page, target, clip=clip)
        else:
            selector = plate.setup.get("selector") or SELECTORS[plate.kind]
            await capture_image(page, target, selector)

    await apply_mode(page, compose.DARK_MODE)
    await shoot(dark)
    await apply_mode(page, compose.CLEAR_MODE)
    await shoot(clear)
    await apply_mode(page, compose.DARK_MODE)
    compose.split(dark, clear, destination)
    dark.unlink(missing_ok=True)
    clear.unlink(missing_ok=True)
    work.rmdir()
    # Leave no tray, sheet or drawer open for the next scene.
    await page.keyboard.press("Escape")
    await page.wait_for_timeout(CAPTURE_SETTLE)


async def run(page: Page, base: str, staging: Path, count: int,
              errors: list[str] | None = None) -> None:
    """Capture every registered plate into staging, then stamp the manifest."""
    for plate in GALLERY.plates:
        await _capture(page, base, plate, count, staging)
        if errors:
            require(not errors, errors)
    manifest.write(staging, GALLERY, count)


def publish(staging: Path, output: Path) -> int:
    """Validate the complete staged gallery before replacing any public file."""
    output = docs_directory(output)
    files = tuple(sorted(path for path in staging.rglob("*") if path.is_file()))
    require(files, "documentation capture produced no files")
    for source in files:
        require(not source.is_symlink(), "invalid staged capture")
        destination = output / source.relative_to(staging)
        docs_directory(destination.parent)
        require(not destination.is_symlink() and (not destination.exists() or destination.is_file()),
                "invalid documentation capture destination")
    for source in files:
        destination = output / source.relative_to(staging)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(source.read_bytes())
    return len([path for path in files if path.suffix == ".png"])
