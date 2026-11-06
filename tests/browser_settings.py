"""Settings sheet invariants checked in the real browser fixture."""

from .support import require


async def settings_checks(page, results):
    """Every rendered settings control carries an id or a name.

    Chrome's autofill audit flags form fields that have neither. Composite
    editors build their rows dynamically, so the invariant is asserted on the
    final rendered DOM rather than in a template string.
    """
    await page.click('#btn-settings')
    await page.wait_for_function(
        "window.settingsModule && "
        "document.querySelectorAll('#settings-fields .st-row').length > 0")
    probe = await page.evaluate("""() => {
        const controls = [...document.querySelectorAll(
            '#settings-fields input, #settings-fields select, #settings-fields textarea')];
        return {
            total: controls.length,
            unnamed: controls.filter(el => !el.id && !el.name)
                .map(el => el.tagName + '.' + el.className),
        };
    }""")
    require(probe['total'] > 20 and not probe['unnamed'], probe)
    # The probe never edits, so a forced close cannot discard operator work.
    await page.evaluate('settingsModule.close(true)')
    results.append({'settings_controls': probe})
    return probe
