// Request-inspector redesign: the hero summary, the error banner, section
// cards and collapse state, the long-text code surface, the sticky section
// jump strip, and the one clipboard/toast path. It runs last so its synthetic
// record never changes another section's window.
module.exports = async function drawerRedesign(H) {
  const {check, d, fire, mkRec, settleUntil, w} = H;
  const base = 1700007000000;
  const longPath = '/v1/chat/completions/' + 'segment-'.repeat(30);
  const longAgent = 'agent/' + 'x'.repeat(220);
  const promptText = Array.from({length: 80}, (_, i) => 'line ' + i + ' of the captured prompt').join('\n');
  const rec = {
    ...mkRec('redesign', 502, base),
    provider: 'provider.example', client: 'client-long/1.2.3',
    model: 'family/model-2026-10-29', path: longPath, method: 'POST',
    duration_ms: 3210, ttft_ms: 640, request_bytes: 2048, response_bytes: 4096,
    req_max_tokens: 4096, req_seed: 1234567890, req_tools_count: 1234,
    upstream_gzip: true, key_hash: 'khash12345678', client_ip: '127.0.0.1',
    user_agent: longAgent,
    error_type: 'upstream_unreachable', error_code: 'dial_timeout',
    error_msg: 'dial tcp 10.0.0.8:443: connect: connection refused',
    prompt_preview: promptText, response_preview: '{"answer":"ok"}',
    response_headers: {'content-type': ['application/json'], 'x-request-id': ['req_abcdef']},
    retries: 1,
    attempts: [{status_code: 502, error_type: 'bad_gateway', error_code: 'overloaded',
      error_msg: 'upstream overloaded', provider_request_id: 'req_att', retry_after_ms: 1260,
      at: new Date(base + 100).toISOString()}],
  };
  fire('end', {record: rec, in_flight: 0});
  await settleUntil(() => !!w.eval("recordById('redesign')"));
  w.openDrawer('redesign');
  const body = d.getElementById('drawer-body');
  const section = title => [...body.querySelectorAll('.detail-section')]
    .find(s => s.querySelector('h4')?.textContent === title);
  const row = key => [...body.querySelectorAll('.detail-kv')]
    .find(el => el.querySelector('.k')?.textContent === key);
  const copyByLabel = label => [...body.querySelectorAll('button[data-copy]')]
    .find(btn => btn.dataset.copyLabel === label);

  check('the copy toast stays outside the modal inert boundary',
    !d.getElementById('toast').hasAttribute('inert'));
  check('the redesigned drawer opens with the hero summary',
    d.getElementById('drawer').classList.contains('open') &&
    !!body.querySelector('.d-hero') &&
    body.querySelector('.d-hero-top').textContent.includes('502') &&
    body.querySelector('.d-hero-top').textContent.includes('POST') &&
    body.querySelector('.d-hero-top').textContent.includes('/v1/chat/completions/') &&
    body.querySelector('.d-hero-ident').textContent.includes('family/model-2026-10-29') &&
    body.querySelector('.d-stats').textContent.includes('3.21s') &&
    body.querySelector('.d-stats').textContent.includes('640ms') &&
    body.querySelector('.d-stats').textContent.includes('retries'));

  check('the error is a top banner with type, code, status and message',
    !!body.querySelector('.d-alert') &&
    body.querySelector('.d-alert').textContent.includes('request failed') &&
    body.querySelector('.d-alert').textContent.includes('502') &&
    body.querySelector('.d-alert').textContent.includes('upstream_unreachable') &&
    body.querySelector('.d-alert').textContent.includes('code dial_timeout') &&
    body.querySelector('.d-alert-msg').textContent.includes('connection refused'));

  const cleanDetail = w.formatDetail({...mkRec('clean-200'), status_code: 200});
  const limitedDetail = w.formatDetail({...mkRec('limited-429'), status_code: 429});
  const inBandDetail = w.formatDetail({...mkRec('inband-200'), status_code: 200,
    error_type: 'upstream_error', error_msg: 'stream reset'});
  check('a clean completion and a rate limit alone render no error banner',
    !cleanDetail.includes('class="d-alert"') && !limitedDetail.includes('class="d-alert"'));
  check('an in-band upstream error still banners on a 200 without an error pill',
    inBandDetail.includes('class="d-alert"') &&
    inBandDetail.includes('upstream error') &&
    inBandDetail.includes('stream reset') &&
    !inBandDetail.includes('<span class="pill err">200</span>'));

  const hostile = w.formatDetail({
    id: 'hostile', status_code: 200, method: 'POST',
    path: '/x/<img src=x onerror=alert(1)>',
    client: '<script>alert(1)</script>', provider: '<img src=x onerror=alert(2)>',
    model: '<script>alert(3)</script>',
    error_type: '<img src=x onerror=alert(4)>', error_msg: '<script>alert(5)</script>',
    response_headers: {'x-evil': ['<img src=x onerror=alert(6)>']},
    prompt_preview: '<script>alert(7)</script>',
  });
  const hostileRoot = d.createElement('div');
  hostileRoot.innerHTML = hostile;
  check('hostile record fields render as text, never markup',
    !hostileRoot.querySelector('img,script') &&
    hostileRoot.textContent.includes('<script>alert(1)</script>') &&
    hostileRoot.textContent.includes('<img src=x onerror=alert(6)>'));

  check('every captured section is a card and the wire sizes are human readable',
    body.querySelectorAll('.detail-section').length >= 9 &&
    row('request bytes')?.querySelector('.v').textContent === '2.05 KB' &&
    row('response bytes')?.querySelector('.v').textContent === '4.1 KB' &&
    row('user-agent')?.querySelector('.v').textContent === longAgent &&
    row('key hash')?.querySelector('.v').textContent.includes('khash'));

  check('section cards keep exact captured counts and their exact copy payloads',
    row('max_tokens')?.querySelector('.v').textContent === '4,096' &&
    row('max_tokens')?.querySelector('.copy-btn').dataset.copy === '4096' &&
    row('seed')?.querySelector('.v').textContent === '1,234,567,890' &&
    row('tools defined')?.querySelector('.v').textContent === '1,234');

  check('the long prompt is a collapsed code surface with wrap and expand controls',
    !!section('Prompt')?.querySelector('.detail-code.collapsed') &&
    !!section('Prompt')?.querySelector('[data-code-wrap]') &&
    !!section('Prompt')?.querySelector('[data-code-toggle]') &&
    section('Prompt').querySelector('.preview-box').textContent === promptText);

  const codeToggle = section('Prompt').querySelector('[data-code-toggle]');
  codeToggle.click();
  check('expanding a long body reveals the whole capture',
    !section('Prompt').querySelector('.detail-code').classList.contains('collapsed') &&
    codeToggle.textContent === 'collapse' && codeToggle.getAttribute('aria-expanded') === 'true');
  const wrap = section('Prompt').querySelector('[data-code-wrap]');
  wrap.click();
  check('the wrap control trades soft wrapping for horizontal scrolling',
    section('Prompt').querySelector('.detail-code').classList.contains('nowrap') &&
    wrap.getAttribute('aria-pressed') === 'false');

  w.renderDrawer();
  const repaintedCode = section('Prompt').querySelector('.detail-code');
  check('expanded and unwrapped code choices survive a live repaint',
    !repaintedCode.classList.contains('collapsed') &&
    repaintedCode.classList.contains('nowrap') &&
    repaintedCode.querySelector('[data-code-toggle]').getAttribute('aria-expanded') === 'true' &&
    repaintedCode.querySelector('[data-code-wrap]').getAttribute('aria-pressed') === 'false');

  const params = section('Parameters');
  const paramsToggle = params.querySelector('[data-section-toggle]');
  paramsToggle.click();
  check('a section card collapses without removing its captured rows',
    params.classList.contains('collapsed') && paramsToggle.getAttribute('aria-expanded') === 'false' &&
    row('model')?.textContent.includes('family/model-2026-10-29'));
  w.renderDrawer();
  check('collapsed sections survive a live repaint',
    section('Parameters')?.classList.contains('collapsed'));
  section('Parameters').querySelector('[data-section-toggle]').click();
  check('expanding restores the section body',
    !section('Parameters').classList.contains('collapsed'));

  const nav = body.querySelector('.d-jump');
  const sectionIds = [...body.querySelectorAll('.detail-section[data-title]')].map(s => s.id);
  check('the sticky jump strip mirrors the rendered sections',
    !!nav && nav.querySelectorAll('.d-jump-btn').length === sectionIds.length &&
    [...nav.querySelectorAll('.d-jump-btn')].every(btn => sectionIds.includes(btn.dataset.jump)));
  const jumpTarget = body.querySelector('.detail-section[data-title="Request"]');
  jumpTarget.querySelector('[data-section-toggle]').click();
  nav.querySelector(`[data-jump="${jumpTarget.id}"]`).click();
  check('a jump chip expands its collapsed target section',
    d.getElementById('drawer').classList.contains('open') &&
    !jumpTarget.classList.contains('collapsed') &&
    jumpTarget.querySelector('[data-section-toggle]').getAttribute('aria-expanded') === 'true');

  // The one clipboard path: rows, sections, the error block and the whole
  // record all copy through copyText and report the polite toast.
  let copied = '';
  let clipboardCalls = 0;
  try {
    Object.defineProperty(w.navigator, 'clipboard', {
      configurable: true,
      value: {writeText: async text => { copied = String(text); clipboardCalls++; }},
    });
  } catch (e) {}
  const pathCopy = copyByLabel('path');
  if (pathCopy) pathCopy.click();
  await settleUntil(() => copied === longPath);
  await settleUntil(() => d.getElementById('toast').textContent === 'copied path');
  check('the hero path copies its exact value and reports the polite toast',
    copied === longPath && d.getElementById('toast').textContent === 'copied path' &&
    d.getElementById('toast').classList.contains('show'));

  const requestCopy = section('Request')?.querySelector('.detail-sec-actions .copy-btn');
  if (requestCopy) requestCopy.click();
  await settleUntil(() => copied.includes('request bytes: 2048'));
  check('a section copy carries every row with its exact value',
    copied.includes('id: redesign') && copied.includes('request bytes: 2048') &&
    copied.includes('response bytes: 4096') && copied.includes('started: ' + rec.start));

  const headerCopy = section('Response headers')?.querySelector('.detail-kv .copy-btn');
  if (headerCopy) headerCopy.click();
  await settleUntil(() => copied === 'application/json');
  check('a response header value copies on its own', copied === 'application/json');

  const retryCopy = section('Retry attempts (absorbed, transparent to client)')
    ?.querySelector('.detail-sec-actions .copy-btn');
  if (retryCopy) retryCopy.click();
  await settleUntil(() => copied.includes('req req_att'));
  check('the absorbed-attempt section copies its timeline as text',
    copied.includes('#1') && copied.includes('502') && copied.includes('retry-after 1.26s') &&
    copied.includes('req req_att'));

  const errorCopy = body.querySelector('.d-alert .copy-btn');
  if (errorCopy) errorCopy.click();
  await settleUntil(() => copied.includes('type: upstream_unreachable'));
  check('the error block copies its type, code and message together',
    copied.includes('code: dial_timeout') && copied.includes('message: dial tcp'));

  w.drawerCopyRecord();
  await settleUntil(() => { try { return JSON.parse(copied).id === 'redesign'; } catch (e) { return false; } });
  check('copy request JSON carries the live record', JSON.parse(copied).id === 'redesign');

  const focusHost = body.querySelector('[data-focus-key="hero-path"]');
  if (focusHost) focusHost.focus();
  w.renderDrawer();
  check('a live repaint restores focus to the same copy control',
    !!d.activeElement && d.activeElement.dataset.focusKey === 'hero-path');

  try {
    Object.defineProperty(w.navigator, 'clipboard', {
      configurable: true,
      value: {writeText: async () => { throw new Error('denied'); }},
    });
  } catch (e) {}
  body.querySelector('[data-focus-key="hero-path"]')?.click();
  await settleUntil(() => d.getElementById('toast').textContent === 'copy failed');
  check('a rejected copy reports the failure instead of claiming success',
    d.getElementById('toast').textContent === 'copy failed' &&
    d.getElementById('toast').classList.contains('err'));

  const callsBefore = clipboardCalls;
  check('the clipboard stub was exercised by the earlier copies', callsBefore >= 4);
  w.closeDrawer();
  check('closing the inspector clears the page scroll lock and persisted card state',
    !d.documentElement.classList.contains('drawer-open') &&
    !section('Parameters') &&
    body.innerHTML === '' &&
    w.eval('detailCollapsed.size') === 0 &&
    w.eval('detailCodeState.size') === 0);
};
