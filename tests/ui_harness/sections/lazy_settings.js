// lazy Settings module
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function lazySettings(H) {
  const {STATIC, TEST_DASHBOARD_VERSION, assembleHTML, check, dashboardDOM, dom, fs, loaded, pageOptions, path, settleUntil, sleep} = H;
  const state = H.state;

  // ---- lazy Settings module ----
  // Production keeps settings.js out of the first-paint shell; the trigger
  // injects exactly one versioned request and the sheet opens only after that
  // module has initialized. These checks assemble without settings.js so the
  // real chrome.js trigger/loader path runs, then simulate the script element's
  // load/error events (jsdom does not fetch external scripts).
  {
    const shell = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
    check('production index.html has no settings.js script reference',
      !/<script\b[^>]*\bsrc=["'][^"']*\/dash\/js\/settings\.js/.test(shell));

    const lazyShell = async () => {
      const appended = [];
      const errors = [];
      const dom = dashboardDOM(assembleHTML(undefined, { includeSettings: false }), {
        ...pageOptions,
        beforeParse(win) {
          pageOptions.beforeParse(win);
          win.console.error = (...args) => errors.push(args.join(' '));
          const append = win.HTMLHeadElement.prototype.appendChild;
          win.HTMLHeadElement.prototype.appendChild = function (node) {
            if (node && node.tagName === 'SCRIPT' &&
                String(node.getAttribute('src') || '').includes('/dash/js/settings.js')) {
              appended.push(node);
              return node;
            }
            return append.call(this, node);
          };
        },
      });
      await loaded(dom);
      return { dom, sw: dom.window, sd: dom.window.document, appended, errors };
    };

    const ok = await lazyShell();
    try {
      check('first paint issues no settings.js request', ok.appended.length === 0);
      check('the settings sheet starts closed before the module loads',
        ok.sd.getElementById('settings-sheet').hidden);
      // Escape and the header close path must be safe before the module exists.
      ok.sd.dispatchEvent(new ok.sw.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      ok.sw.closeHeaderMenus(undefined, true);
      ok.sw.closeSettings(true);
      check('close and Escape are safe no-ops before settings.js loads',
        ok.sd.getElementById('settings-sheet').hidden);

      ok.sw.toggleSettings({ stopPropagation() {} });
      ok.sw.toggleSettings({ stopPropagation() {} });
      ok.sw.toggleSettings({ stopPropagation() {} });
      const script = ok.appended[0];
      check('repeated and concurrent settings opens issue exactly one versioned request',
        ok.appended.length === 1 && script &&
        script.getAttribute('src') === '/dash/js/settings.js?v=' + TEST_DASHBOARD_VERSION);

      let opened = 0;
      ok.sw.settingsModule = { open() { opened++; }, close() {} };
      check('the sheet stays closed while the settings module is loading',
        ok.sd.getElementById('settings-sheet').hidden);
      script.onload();
      await settleUntil(() => opened === 1);
      check('the loaded settings module opens the sheet exactly once', opened === 1);
    } finally { ok.dom.window.close(); }

    // A close while the module is still downloading must cancel the pending
    // open; the loader resolving later must not pop the sheet back up.
    const cancelled = await lazyShell();
    try {
      let opened = 0;
      cancelled.sw.toggleSettings({ stopPropagation() {} });
      cancelled.sw.closeSettings(true);
      cancelled.sw.settingsModule = { open() { opened++; }, close() {} };
      cancelled.appended[0].onload();
      await sleep(0); // negative proof: a close during load must cancel the pending open
      check('closing during the settings load cancels the pending open', opened === 0);
      cancelled.sw.toggleSettings({ stopPropagation() {} });
      check('a later settings open uses the loaded module exactly once', opened === 1);
    } finally { cancelled.dom.window.close(); }

    // The chart metrics picker renders at first paint and shares its chevron
    // with the settings provider cards. It must not depend on settings.js.
    {
      const noSettings = dashboardDOM(assembleHTML(undefined, { includeSettings: false }), pageOptions);
      try {
        await loaded(noSettings);
        const sd = noSettings.window.document;
        noSettings.window.eval("chartView.preset = 'overview'");
        noSettings.window.summaryPickerSync();
        const btn = sd.getElementById('chart-metrics-btn');
        check('the chart metrics picker renders before settings.js loads',
          !!btn && btn.innerHTML.includes('<svg') &&
          sd.getElementById('chart-metrics-count').textContent === '5/5');
      } finally { noSettings.window.close(); }
    }

    const failed = await lazyShell();
    try {
      failed.sw.toggleSettings({ stopPropagation() {} });
      check('a settings load attempt starts with the sheet closed',
        failed.appended.length === 1 && failed.sd.getElementById('settings-sheet').hidden);
      failed.appended[0].onerror();
      await settleUntil(() => failed.sd.getElementById('settings-count').textContent === 'could not load config');
      check('a settings load failure stays closed and surfaces the shared status',
        failed.sd.getElementById('settings-sheet').hidden &&
        failed.sd.getElementById('settings-count').textContent === 'could not load config');
      check('a settings load failure reports a console error',
        failed.errors.some(line => line.includes('settings module failed to load')));
      failed.sw.toggleSettings({ stopPropagation() {} });
      await sleep(0); // negative proof: a failed load is not retried on the next open
      check('a failed settings load is not retried on the next open',
        failed.appended.length === 1);
    } finally { failed.dom.window.close(); }
  }
};
