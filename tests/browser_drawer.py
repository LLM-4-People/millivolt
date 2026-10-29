"""Real-browser inspector layout, long-line, copy and mobile checks."""

from .support import require


INJECT = """() => {
    if (typeof _dashTickTimer !== 'undefined' && _dashTickTimer) clearInterval(_dashTickTimer);
    streamLive = false;
    const base = Date.now() - 3210;
    const longPath = '/v1/chat/completions/team/' + 'very-long-segment/'.repeat(12) + 'stream';
    const longLine = 'single-long-line:' + 'x'.repeat(2200);
    const rec = {
      id: 'drawer-browser-fixture', provider: 'provider',
      client: 'neutral-client/1.0', model: 'family/model-2026-10-29-preview',
      key_hash: '0123456789abcdef', conversation_id: 'cv-drawer',
      status_code: 502, start: new Date(base).toISOString(), end: new Date(base + 3210).toISOString(),
      stream: true, method: 'POST', path: longPath,
      client_ip: '127.0.0.1', client_lang: 'go/1.25',
      client_meta: {pkg_ver: '1.0.0', runtime: 'go', runtime_ver: '1.25.3', os: 'linux', arch: 'amd64', format: 'openai', timeout_ms: 120000},
      user_agent: 'neutral-client/1.0 (linux; amd64) ' + 'long-agent-suffix/'.repeat(8),
      ttft_ms: 640, duration_ms: 3210, gen_tps: 42.7, chunks: 812,
      first_token_at: new Date(base + 640).toISOString(),
      last_token_at: new Date(base + 3100).toISOString(),
      finish_reason: 'stop', request_bytes: 2048, response_bytes: 4096, upstream_gzip: true,
      usage: {input_tokens: 1200, output_tokens: 480, total_tokens: 1680,
        reasoning_tokens: 96, cache_read_tokens: 640},
      answer_tokens: 384, cost: 0.0042, tool_calls: 2, tool_names: ['read_file', 'bash'],
      req_max_tokens: 4096, req_temperature: 0.7, req_top_p: 0.95,
      req_thinking: true, req_tools_count: 2, req_tool_choice: 'auto',
      turns_user: 4, turns_assistant: 3, turns_tool: 2, last_turn_role: 'user',
      chars_system: 1820, chars_user: 9420, chars_assistant: 2110,
      error_type: 'upstream_unreachable', error_code: 'dial_timeout',
      error_msg: 'dial tcp 10.0.0.8:443: connect: connection refused after 30s while dialing the configured upstream provider endpoint',
      prompt_preview: ['prompt head', longLine,
        ...Array.from({length: 20}, (_, i) => 'line ' + (i + 1) + ': captured prompt body')].join('\\n'),
      response_preview: '{"answer":"A compact response preview"}',
      response_headers: {'content-type': ['application/json; charset=utf-8'],
        'x-request-id': ['req_abcdef0123456789']},
      retries: 1,
      attempts: [{status_code: 502, error_type: 'bad_gateway', error_code: 'overloaded',
        error_msg: 'upstream overloaded while processing the request',
        provider_request_id: 'req_attempt_01', retry_after_ms: 1260,
        at: new Date(base + 100).toISOString()}],
    };
    window.__drawerFixture = rec;
    lastData = {...(lastData || {}), records: [...(lastData?.records || []), rec],
      counters: {in_flight: 0, total_requests: 1, total_errors: 1}};
    openDrawer(rec.id);
    return {id: rec.id, path: longPath, prompt: rec.prompt_preview};
  }"""


async def drawer_checks(page, results):
    """Drive the real inspector at desktop and mobile widths."""
    await page.set_viewport_size({'width': 1440, 'height': 1000})
    fixture = await page.evaluate(INJECT)
    await page.wait_for_function("drawerId === 'drawer-browser-fixture' && document.getElementById('drawer').classList.contains('open')")

    desktop = await page.evaluate("""() => {
        const drawer = document.getElementById('drawer');
        const body = document.getElementById('drawer-body');
        const rect = drawer.getBoundingClientRect();
        const path = document.querySelector('.d-path');
        const pathRect = path.getBoundingClientRect();
        const copy = document.querySelector('#d-sec-request .detail-kv .copy-btn');
        return {
          width: rect.width, bodyScroll: body.scrollWidth, bodyClient: body.clientWidth,
          pathHeight: pathRect.height, pathLine: parseFloat(getComputedStyle(path).lineHeight),
          copyOpacity: copy ? getComputedStyle(copy).opacity : null,
          sections: document.querySelectorAll('#drawer-body .detail-section').length,
          tabs: [...document.querySelectorAll('[data-group-tab]')].map(t => t.dataset.groupTab),
          activeGroup: document.querySelector('[data-group-tab][aria-selected="true"]')?.dataset.groupTab,
          hero: !!document.querySelector('.d-hero'),
          alert: document.querySelector('.d-alert')?.textContent || '',
        };
      }""")
    require(desktop['width'] <= 640.5 and desktop['hero'], desktop)
    require(desktop['tabs'] == ['request', 'content', 'usage', 'diagnostics'] and
            desktop['activeGroup'] == 'request', desktop)
    require(desktop['bodyScroll'] <= desktop['bodyClient'] + 1, desktop)
    require(desktop['pathHeight'] > desktop['pathLine'] * 1.5, desktop)
    require(desktop['copyOpacity'] == '0', desktop)
    require('request failed' in desktop['alert'] and 'dial_timeout' in desktop['alert'], desktop)

    # Park the row in the middle of the scroll region before hovering: the
    # sticky jump strip otherwise covers the first row after scrollIntoView.
    await page.evaluate("""() => {
        document.querySelector('#d-sec-request .detail-kv .copy-btn')
          .scrollIntoView({block: 'center'});
      }""")
    await page.wait_for_timeout(120)
    await page.hover('#d-sec-request .detail-kv .copy-btn')
    # The shared opacity transition is 120ms; read the settled hover value.
    await page.wait_for_timeout(180)
    hovered = await page.evaluate("""() => getComputedStyle(
        document.querySelector('#d-sec-request .detail-kv .copy-btn')).opacity""")
    require(float(hovered) > 0, hovered)

    # The one copy path: stub the clipboard write, click the hero path action
    # and the section copy, and verify the payloads and the polite toast.
    copied = await page.evaluate("""async () => {
        const original = copyText;
        window.__copied = null;
        copyText = async text => { window.__copied = String(text); return true; };
        document.querySelector('[data-copy-label="path"]').click();
        await new Promise(r => setTimeout(r, 20));
        const path = window.__copied;
        document.querySelector('#d-sec-request .detail-sec-actions .copy-btn').click();
        await new Promise(r => setTimeout(r, 20));
        const section = window.__copied;
        const toast = document.getElementById('toast').textContent;
        copyText = original;
        return {path, section, toast};
      }""")
    require(copied['path'] == fixture['path'], copied)
    require('id: drawer-browser-fixture' in copied['section'], copied)
    require(copied['toast'] == 'copied Request', copied)

    # The prompt lives under Content; switch the classified strip before
    # measuring it.
    await page.evaluate("""() => document.querySelector('[data-group-tab="content"]').click()""")
    # Expand the prompt and switch off wrapping: the blocked long line must
    # become horizontally scrollable rather than widen the drawer.
    expanded = await page.evaluate("""() => {
        const code = document.querySelector('#d-sec-prompt .detail-code');
        const pre = code.querySelector('.preview-box');
        const collapsedHeight = pre.getBoundingClientRect().height;
        code.querySelector('[data-code-toggle]').click();
        const height = pre.getBoundingClientRect().height;
        code.querySelector('[data-code-wrap]').click();
        const nowrap = code.classList.contains('nowrap');
        return {collapsedHeight, height, nowrap,
          scrollWidth: pre.scrollWidth, clientWidth: pre.clientWidth,
          bodyScroll: document.getElementById('drawer-body').scrollWidth,
          bodyClient: document.getElementById('drawer-body').clientWidth};
      }""")
    require(expanded['collapsedHeight'] <= 170 and expanded['height'] > 170 and expanded['nowrap'], expanded)
    require(expanded['scrollWidth'] > expanded['clientWidth'], expanded)
    require(expanded['bodyScroll'] <= expanded['bodyClient'] + 1, expanded)

    # Mobile: the inspector owns the viewport, rows stack and the close/copy
    # targets stay reachable. Return to Request for the row-geometry check.
    await page.evaluate("""() => document.querySelector('[data-group-tab="request"]').click()""")
    await page.set_viewport_size({'width': 390, 'height': 844})
    await page.wait_for_timeout(120)
    mobile = await page.evaluate("""() => {
        const drawer = document.getElementById('drawer').getBoundingClientRect();
        const body = document.getElementById('drawer-body');
        const row = document.querySelector('#drawer-body .detail-kv');
        const key = row.querySelector('.k').getBoundingClientRect();
        const value = row.querySelector('.v').getBoundingClientRect();
        const copy = row.querySelector('.copy-btn');
        const copyRect = copy ? copy.getBoundingClientRect() : null;
        const closeBtn = document.querySelector('#drawer [data-focus-key="close"]');
        const close = closeBtn.getBoundingClientRect();
        return {width: drawer.width, viewport: innerWidth, height: drawer.height,
          keyTop: key.top, keyHeight: key.height, valueTop: value.top,
          copy: copyRect ? {w: copyRect.width, h: copyRect.height} : null,
          close: {w: close.width, h: close.height, label: closeBtn.getAttribute('aria-label')},
          tabs: [...document.querySelectorAll('[data-group-tab]')].map(t => t.dataset.groupTab),
          activeGroup: document.querySelector('[data-group-tab][aria-selected="true"]')?.dataset.groupTab,
          bodyScroll: body.scrollWidth, bodyClient: body.clientWidth};
      }""")
    require(abs(mobile['width'] - mobile['viewport']) <= 1, mobile)
    require(mobile['tabs'] == ['request', 'content', 'usage', 'diagnostics'] and
            mobile['activeGroup'] == 'request', mobile)
    require(mobile['valueTop'] >= mobile['keyTop'] + mobile['keyHeight'] - 1, mobile)
    require(mobile['copy'] and mobile['copy']['w'] >= 32 and mobile['copy']['h'] >= 32, mobile)
    require(mobile['close']['w'] >= 38 and mobile['close']['h'] >= 38 and
            mobile['close']['label'].startswith('close request detail'), mobile)
    require(mobile['bodyScroll'] <= mobile['bodyClient'] + 1, mobile)

    # Locking a scrolling document removes its scrollbar; the drawer reserves
    # that gutter so the page behind it does not jump sideways.
    await page.set_viewport_size({'width': 1280, 'height': 585})
    await page.wait_for_timeout(120)
    shift = await page.evaluate("""async () => {
        closeDrawer();
        const left = () => document.querySelector('header').getBoundingClientRect().left;
        const before = left();
        openDrawer(window.__drawerFixture.id);
        await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
        const after = left();
        // Headless Chromium uses overlay scrollbars (zero gutter), so verify
        // the reservation rule directly as well as the no-shift outcome.
        document.documentElement.style.setProperty('--drawer-gutter', '15px');
        const reserved = getComputedStyle(document.body).paddingRight;
        closeDrawer();
        return {before, after, reserved};
      }""")
    require(abs(shift['after'] - shift['before']) <= 1, shift)
    require(shift['reserved'] == '15px', shift)

    await page.evaluate("""() => {
        closeDrawer();
        if (window.__drawerFixture) {
          lastData = {...lastData,
            records: (lastData.records || []).filter(r => r.id !== window.__drawerFixture.id)};
          delete window.__drawerFixture;
        }
        armDashboardTicks();
      }""")
    await page.set_viewport_size({'width': 1440, 'height': 1000})
    results.append({'drawer_desktop': desktop, 'drawer_copy': copied,
                    'drawer_expanded': expanded, 'drawer_mobile': mobile,
                    'drawer_scroll_lock': shift})
