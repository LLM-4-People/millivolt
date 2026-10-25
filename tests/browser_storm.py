"""Storm banner and incident dialog checks for the browser fixture."""

from .support import require


async def storm_checks(page, errors, checks):
    """Drive the storm banner, incident dialog and recovery probes."""
    for viewport in ({'width': 1440, 'height': 1000}, {'width': 390, 'height': 844}):
        await page.set_viewport_size(viewport)
        state = await page.evaluate('''async () => {
            const fixture = {enabled:true,banner_enabled:true,storms:Array.from({length:8}, (_, i) => ({
                provider:'neutral-provider-with-a-long-scope-name.example',
                model:i ? 'neutral-model-with-a-long-name/' + i : '',scope:i ? 'model' : 'provider',
                state:i ? 'half_open' : 'open',reason:i ? 'transport' : 'http_503',quota:false,
                error_percent:75,error_requests:9,failures:15,samples:20,window_ms:60000,queued:4,
                active_models:3,affected_models:2,affected_model_percent:200/3,
                retry_at:'2026-09-05T12:00:00Z',recovery_successes:i ? 1 : 0,recovery_required:2,
            }))};
            window.browserStormFixture=fixture;
            applyStormState(fixture);
            const banner=document.querySelector('#storm-banner');
            banner.focus();
            await new Promise(requestAnimationFrame);
            banner.scrollTop=30;
            const row=banner.firstElementChild;
            applyStormState(fixture);
            const rect=banner.getBoundingClientRect();
            const state={visible:!banner.hidden,rows:banner.querySelectorAll('.storm-row').length,
                height:rect.height,bounded:rect.height<=Math.min(180,innerHeight*.25)+1,
                scrollable:banner.scrollHeight>banner.clientHeight,
                retained:banner.firstElementChild===row && banner.scrollTop===30,
                focused:document.activeElement===banner,
                horizontal:banner.scrollWidth>banner.clientWidth+1 || rect.right>innerWidth+1,
                recovery:banner.textContent.includes('Checking recovery'),
                attempt_rate:banner.textContent.includes('75% failed attempts'),
                affected_requests:banner.textContent.includes('9 requests affected')};
            return state;
        }''')
        require(state['visible'] and state['rows'] == 8 and state['height'] > 0 and state['bounded'], state)
        require(state['scrollable'] and state['retained'] and state['focused'] and not state['horizontal'], state)
        require(state['recovery'] and state['attempt_rate'] and state['affected_requests'], state)
        trigger = page.locator('#storm-banner button').first
        await trigger.focus()
        await page.keyboard.press('Enter')
        await page.wait_for_function('!document.querySelector("#storm-dialog").hidden')
        await page.wait_for_function('document.activeElement?.dataset.operator === "storm-close"')
        details = await page.evaluate('''() => {
            const dialog=document.querySelector('#storm-dialog');
            const facts=Object.fromEntries([...dialog.querySelectorAll('dt')].map(node=>[node.textContent,node.nextElementSibling.textContent]));
            const rect=dialog.querySelector('.storm-dialog-panel').getBoundingClientRect();
            return {facts,modal:dialog.getAttribute('role')==='dialog' && document.querySelector('main').inert,
                bounded:rect.left>=0 && rect.right<=innerWidth && rect.top>=0 && rect.bottom<=innerHeight,
                horizontal:dialog.scrollWidth>dialog.clientWidth+1};
        }''')
        require(details['modal'] and details['bounded'] and not details['horizontal'], details)
        require(details['facts']['Requests affected'] == '9' and details['facts']['Failed upstream attempts'] == '15', details)
        require(details['facts']['Sampled upstream attempts'] == '20' and details['facts']['Queued requests'] == '4', details)
        require(details['facts']['Affected active models'] == '2 / 3 (66.7%)' and details['facts']['Next retry'], details)
        await page.keyboard.press('Tab')
        require(await page.evaluate('document.activeElement?.dataset.operator === "storm-close"'), 'incident modal lost its focus trap')
        updated = await page.evaluate('''() => {
            const close=document.activeElement;
            browserStormFixture.storms[0]={...browserStormFixture.storms[0],queued:7,error_requests:10,state:'half_open',recovery_successes:1};
            applyStormState(browserStormFixture);
            const facts=Object.fromEntries([...document.querySelectorAll('#storm-dialog dt')].map(node=>[node.textContent,node.nextElementSibling.textContent]));
            return document.activeElement===close && facts['Queued requests']==='7' && facts['Requests affected']==='10' && facts['Successful recovery probes']==='1 / 2';
        }''')
        require(updated, 'incident modal did not refresh or displaced keyboard focus')
        await page.keyboard.press('Escape')
        require(await page.evaluate('document.querySelector("#storm-dialog").hidden && document.activeElement===document.querySelector("#storm-banner button")'), 'incident Escape did not restore trigger focus')
        await trigger.click()
        await page.wait_for_function('document.activeElement?.dataset.operator === "storm-close"')
        await page.evaluate('applyStormState({...browserStormFixture,storms:[]})')
        require(await page.locator('#storm-dialog').is_visible(), 'incident resolved by unexpectedly closing focused dialog')
        require('no longer active' in await page.locator('#storm-dialog').inner_text(), 'incident retained stale recovery counts')
        await page.locator('#storm-dialog [data-operator="storm-close"]').click()
        require(await page.evaluate('''() => {
            const el = document.activeElement;
            return document.querySelector('#storm-banner').hidden && el &&
              el.getClientRects().length > 0 && (el.id === 'btn-nav' || el.id === 'btn-settings');
        }'''), 'resolved incident did not restore visible focus')
        await page.evaluate('delete window.browserStormFixture')
        state['details'] = details
        state['keyboard_and_live_update'] = True
        checks.append({'width': viewport['width'], **state})
