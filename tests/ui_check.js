// jsdom functional test for the dashboard's incremental live-update pipeline.
// Most checks drive the page through its external interfaces: the stubbed
// fetch() and the EventSource listeners the page registers itself. Focused
// helper sections call production render/merge helpers directly to pin
// optimizer contracts that the external paths cannot isolate; they supplement,
// not replace, the end-to-end coverage.
//
// The shared page, fixtures and stub state live in tests/ui_harness/; the
// ordered section modules under tests/ui_harness/sections/ run in registry
// order. This file stays the single entry point (`npm test`).
const harness = require('./ui_harness/harness');
const sections = require('./ui_harness/sections');
const {
  dom, w, d, check, sleep, loaded, installDiagnostics, checkEntityIdentity, reportFailures,
  state
} = harness;

async function main() {
  // Deferred asset scripts load through the interceptor after parsing. Wait
  // for the load event before inspecting styles or driving the dashboard.
  await loaded(dom);
  installDiagnostics();
  checkEntityIdentity();
  // Shared CSS must preserve sentence case, including protocol acronyms and
  // user-authored names. Inspect every formerly forced-uppercase owner.
  {
    // jsdom does not apply viewport media queries; inspect those declarations
    // as well so the shared mobile picker cannot force uppercase unnoticed.
    const declarations = [];
    const visitRules = rules => {
      for (const rule of rules) {
        if (rule.style) declarations.push(rule.style.getPropertyValue('text-transform'));
        if (rule.cssRules) visitRules(rule.cssRules);
      }
    };
    for (const sheet of d.styleSheets) visitRules(sheet.cssRules);
    check('dashboard labels never force uppercase, including mobile media rules',
      !declarations.some(value => value === 'uppercase' || value === 'capitalize'));
    const host = d.createElement('section');
    host.innerHTML = '<div class="pause-known-hd"></div><div class="lim-src"></div>' +
      '<div class="clear-opts"><label></label></div><div class="kpi"><h2></h2></div>' +
      '<span class="xp-dim-by"></span><div class="prov-lb"></div><div class="mr-what"></div>' +
      '<div class="mr-ex-hd"></div><div class="card"><h2></h2></div><div class="ct-h"></div>' +
      '<table><thead><tr><th></th></tr></thead></table><span class="day-chip"></span>' +
      '<div class="detail-section"><h4></h4></div>';
    d.body.appendChild(host);
    try {
      for (const selector of ['.pause-known-hd', '.lim-src', '.clear-opts label', '.kpi h2',
        '.xp-dim-by', '.prov-lb', '.mr-what', '.mr-ex-hd', '.card h2', '.ct-h', 'th',
        '.day-chip', '.detail-section h4']) {
        const label = host.querySelector(selector);
        label.textContent = 'Latency TTFT for Model.MixedCase';
        check(`${selector} preserves sentence case and exact labels`,
          w.getComputedStyle(label).textTransform === 'none' &&
          label.textContent === 'Latency TTFT for Model.MixedCase');
      }
    } finally { host.remove(); }
  }
  await sleep(200); // initial fetch + renderAll

  for (const run of sections) await run(harness);

  reportFailures(false);
  process.exit(state.failures.length ? 1 : 0);
}
// A crashed run must not swallow what it already knows: without this
// handler the process dies on the raw exception and the accumulated
// failures, the diagnostic dump and the check history never print.
main().catch(err => {
  console.error(err);
  if (state.diagDump) console.log(state.diagDump('crash: ' + (err && err.message)));
  reportFailures(true);
  process.exit(1);
});
