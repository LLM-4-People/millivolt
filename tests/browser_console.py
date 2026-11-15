"""Operator console rail axis invariants checked in the real browser fixture."""

from .support import require


async def console_rail_checks(page, results):
    """The announced tablist axis must be the one the rail paints.

    The strip is one scrolling row on a wide viewport and a three-column grid
    below 560px. Announcing "horizontal" in both lost the grid's two-row
    reading order to assistive technology, because Up/Down visibly cross rows
    there while the announcement promised Left/Right only.
    """
    await page.click('#btn-controls')
    await page.wait_for_selector('#oc-rail .oc-rail-item')
    await page.set_viewport_size({'width': 1440, 'height': 900})
    await page.evaluate('new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
    wide = await page.evaluate("""() => {
        const rail = document.getElementById('oc-rail');
        return {
            display: getComputedStyle(rail).display,
            orientation: rail.getAttribute('aria-orientation'),
        };
    }""")
    require(wide['display'] != 'grid' and wide['orientation'] == 'horizontal', wide)

    await page.set_viewport_size({'width': 390, 'height': 844})
    await page.evaluate('new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))')
    narrow = await page.evaluate("""() => {
        const rail = document.getElementById('oc-rail');
        const tabs = [...rail.querySelectorAll('.oc-rail-item')];
        return {
            display: getComputedStyle(rail).display,
            columns: getComputedStyle(rail).gridTemplateColumns.split(' ').length,
            orientation: rail.getAttribute('aria-orientation'),
            tabs: tabs.length,
        };
    }""")
    require(narrow['display'] == 'grid' and narrow['columns'] == 3, narrow)
    require(narrow['orientation'] == 'vertical', narrow)

    # Home and End jump through the painted reading order: first and last cell.
    await page.evaluate("""() => {
        const tabs = [...document.querySelectorAll('#oc-rail .oc-rail-item')];
        tabs[1].focus();
    }""")
    await page.keyboard.press('End')
    end_section = await page.evaluate("""() => document.activeElement.dataset.ocSection""")
    await page.keyboard.press('Home')
    home_section = await page.evaluate("""() => document.activeElement.dataset.ocSection""")
    first = await page.evaluate("""() => document.querySelector('#oc-rail .oc-rail-item').dataset.ocSection""")
    last = await page.evaluate("""() => [...document.querySelectorAll('#oc-rail .oc-rail-item')].at(-1).dataset.ocSection""")
    require(home_section == first and end_section == last, {'home': home_section, 'end': end_section})

    await page.set_viewport_size({'width': 1440, 'height': 1000})
    await page.evaluate('window.operatorConsole && operatorConsole.close(true)')
    results.append({'console_rail': {'wide': wide, 'narrow': narrow,
                                    'home': home_section, 'end': end_section}})
