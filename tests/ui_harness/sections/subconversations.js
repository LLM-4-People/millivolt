// sub-conversations editor: the settings dashboard surface
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function subconversations(H) {
  const {cfgDoc, check, d, w} = H;
  const state = H.state;

  // ---- sub-conversations editor: the settings dashboard surface ----
  // The W52 spec's settings-editor section: the editor renders the
  // server doc's seeded entries under the conversation category the
  // schema owns, edits collect round-trip to the strict POST shape
  // (client, params, strip), add/remove rows work through the shared
  // delegated wiring, live per-card validation mirrors
  // config.validateSubConversations with the server's message wording,
  // and Apply blocks on the first invalid card before the POST. The
  // client datalist seeds from the same known-set union the ro scope
  // datalists read, and the help-text search surfaces the row like its
  // siblings with the rail badge counting the hit.
  {
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields.push(
      { key: 'conversation_idle_gap', category: 'conversation', label: 'Idle gap', help: 'A gap longer than this starts a new conversation for the same client+key.', kind: 'duration', hot_reload: true },
      { key: 'sub_conversations', category: 'conversation', label: 'Sub-conversation tracking', help: 'Per-client tracking of a request body field that carries a sub-conversation identity, exemplified by opencode promptCacheKey.', kind: 'sub_conversations', hot_reload: true },
    );
    doc.categories.push({ id: 'conversation', label: 'Conversations', help: 'How the request log groups turns into conversations.' });
    doc.values.conversation_idle_gap = '15m';
    doc.values.sub_conversations = [
      { client: 'opencode', params: ['promptCacheKey', 'sessionKey'], strip: true },
      { client: 'zed-cli', params: ['threadId'], strip: false },
    ];
    doc.revision = 'sc-r1';
    // The known sets are unioned across the operator states at datalist
    // render time; pin them to the stubbed wire values before the render
    // (the ro section's discipline).
    w.eval(`(() => {
      pauseState = { ...pauseState, [KNOWN_CLIENTS_KEY]: ['c'] };
      debugState = { ...debugState, [KNOWN_CLIENTS_KEY]: ['c'] };
      throttleState = { ...throttleState, [KNOWN_CLIENTS_KEY]: ['c'] };
    })()`);
    w.__scDoc = doc;
    w.eval("settingsCat = 'conversation'; settingsDoc = window.__scDoc; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const scWrap = d.querySelector('#settings-fields [data-kind="sub_conversations"]');
    check('the settings sheet renders the sub-conversations editor under the conversation category',
      !!scWrap && scWrap.closest('.st-row').dataset.cat === 'conversation' &&
      Number(d.querySelector('[data-st-cat="conversation"] .rail-n').textContent) === 2);
    const cards = () => [...scWrap.querySelectorAll('.sc-card')];
    check('seeded entries render as cards with client, ordered params and the strip toggle',
      cards().length === 2 &&
      cards()[0].querySelector('.sc-client').value === 'opencode' &&
      [...cards()[0].querySelectorAll('.sc-param')].map(p => p.value).join(',') === 'promptCacheKey,sessionKey' &&
      cards()[0].querySelector('.sc-strip').checked === true &&
      cards()[1].querySelector('.sc-client').value === 'zed-cli' &&
      !cards()[1].querySelector('.sc-strip').checked);
    check('the strip hint names the pre-relay client body scope',
      scWrap.querySelector('.mr-hint').textContent.includes('strip removes every configured field present at the top level of the client request body before relay or translation'));
    check('watermark examples ride the empty fields',
      cards()[0].querySelector('.sc-client').placeholder.includes('e.g. opencode') &&
      cards()[0].querySelector('.prov-add .sc-param-in').placeholder.includes('e.g. promptCacheKey') &&
      [...cards()[0].querySelectorAll('.sc-param')].every(p => p.placeholder.includes('e.g. promptCacheKey')));
    check('the entry count line renders against the server cap',
      scWrap.querySelector('.mr-count').textContent === '2 / 16 entries');
    check('the client datalist seeds from the same known sets the ro scope datalists read',
      cards()[0].querySelector('.sc-client').getAttribute('list') === 'sc-dl-client' &&
      [...scWrap.querySelectorAll('#sc-dl-client option')].map(o => o.value).join(',') === 'c');
    check('the strip label states the forward and strip semantics',
      cards()[0].querySelector('.st-check').textContent.includes('strip') &&
      cards()[0].querySelector('.st-check').textContent.includes('forwards it unchanged'));
    check('collect round-trips the entries losslessly in the strict wire shape',
      JSON.stringify(w.collectSettingsValues().sub_conversations) === JSON.stringify([
        { client: 'opencode', params: ['promptCacheKey', 'sessionKey'], strip: true },
        { client: 'zed-cli', params: ['threadId'], strip: false },
      ]));
    check('the clean sub-conversations draft reports no unsaved changes',
      d.getElementById('settings-count').textContent === '' && !w.settingsIsDirty());
    // The help-text search: the same label + key + help haystack the
    // siblings ride surfaces the row, and the category rail badge counts
    // the hit (the GAP-5 filter pattern).
    {
      const q = d.getElementById('settings-q');
      q.value = 'promptCacheKey';
      w.filterSettings();
      const visible = [...d.querySelectorAll('#settings-fields .st-row')].filter(r => !r.hidden).map(r => r.dataset.key);
      check('the help-text search surfaces the sub-conversations row',
        JSON.stringify(visible) === JSON.stringify(['sub_conversations']) &&
        Number(d.querySelector('[data-st-cat="conversation"] .rail-n').textContent) === 1 &&
        d.querySelector('[data-st-cat="conversation"]').classList.contains('has-hit'));
      q.value = '';
      w.filterSettings();
    }
    // add/remove param rows through the real controls; the add row trims
    // and enforces the grammar and the cap (deny by default: a refusal
    // flashes the add input and adds nothing).
    const zed = () => cards()[1];
    const zedAddIn = () => zed().querySelector('.prov-add .sc-param-in');
    zedAddIn().value = 'cacheKey';
    zed().querySelector('[data-sc-param-add]').click();
    check('the param add button appends a row, clears the add input and marks the sheet dirty',
      [...zed().querySelectorAll('.sc-param')].map(p => p.value).join(',') === 'threadId,cacheKey' &&
      zedAddIn().value === '' && w.settingsIsDirty() &&
      d.getElementById('settings-count').textContent === '1 unsaved');
    zedAddIn().value = 'fallbackKey';
    zedAddIn().dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    check('Enter in the param add input appends through the same wiring',
      [...zed().querySelectorAll('.sc-param')].length === 3);
    zedAddIn().value = 'threadId';
    zed().querySelector('[data-sc-param-add]').click();
    check('a duplicate param name flashes the add input instead of adding a row',
      [...zed().querySelectorAll('.sc-param')].length === 3 &&
      zedAddIn().classList.contains('prov-bad'));
    zedAddIn().value = 'fourthKey';
    zedAddIn().dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    check('the fourth param row still fits the per-entry cap',
      [...zed().querySelectorAll('.sc-param')].length === 4);
    zedAddIn().value = 'fifthKey';
    zed().querySelector('[data-sc-param-add]').click();
    check('the param add gate refuses beyond the server cap of 4',
      [...zed().querySelectorAll('.sc-param')].length === 4 &&
      [...zed().querySelectorAll('.sc-param')].map(p => p.value).join(',') === 'threadId,cacheKey,fallbackKey,fourthKey' &&
      zedAddIn().classList.contains('prov-bad'));
    // the live grammar mirror of config.checkSubConversationParam,
    // judged per keystroke on the row itself with the server's message
    // vocabulary. The pinned control byte is \x01: a single-line input's
    // spec value-sanitization strips \r\n at the DOM value layer before
    // validation could see it, but other control bytes survive a paste.
    const gin = () => [...zed().querySelectorAll('.sc-param')].pop();
    const type = v => { gin().value = v; gin().dispatchEvent(new w.Event('input', { bubbles: true })); };
    type('bad\x01byte');
    check('a control byte in a param name reddens the row with the JSON key segment message',
      zed().dataset.scState === 'error' &&
      zed().querySelector('.sc-err').textContent === `params: 'bad\x01byte' is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)` &&
      gin().classList.contains('prov-bad'));
    type('ba"d');
    check('a quote in a param name reddens with the same server vocabulary',
      zed().querySelector('.sc-err').textContent === `params: 'ba"d' is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`);
    type('ba\\ck');
    check('a backslash in a param name reddens with the same server vocabulary',
      zed().querySelector('.sc-err').textContent === `params: 'ba\\ck' is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`);
    type('a'.repeat(65));
    check('a 65-byte param name reddens with the byte-bound message',
      zed().querySelector('.sc-err').textContent === 'params: must be 1..64 bytes, got 65');
    type('');
    check('an emptied param row names the empty rule with the server wording',
      zed().querySelector('.sc-err').textContent === 'params: must not be empty or only whitespace');
    type('fourthKey');
    check('repairing the row clears the entry error',
      zed().dataset.scState !== 'error' && zed().querySelector('.sc-err').textContent === '');
    // the duplicate-param live check: the same name twice in one entry.
    const dupIn = [...zed().querySelectorAll('.sc-param')][1];
    dupIn.value = 'threadId';
    dupIn.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('a duplicate param within the entry names the duplicate with the server wording',
      zed().dataset.scState === 'error' &&
      zed().querySelector('.sc-err').textContent === "params: duplicate 'threadId'" &&
      dupIn.classList.contains('prov-bad'));
    dupIn.value = 'cacheKey';
    dupIn.dispatchEvent(new w.Event('input', { bubbles: true }));
    // removing every param row surfaces the no-param rule.
    [...zed().querySelectorAll('[data-sc-p-rm]')].forEach(btn => btn.click());
    check('removing every param row names the no-param rule and reddens the add row',
      zed().dataset.scState === 'error' &&
      zed().querySelector('.sc-err').textContent === 'no param set - give the entry a request body field to track, or remove the entry' &&
      zedAddIn().classList.contains('prov-bad'));
    zedAddIn().value = 'threadId';
    zedAddIn().dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    check('re-adding the param clears the no-param error',
      zed().dataset.scState !== 'error' &&
      [...zed().querySelectorAll('.sc-param')].map(p => p.value).join(',') === 'threadId');
    // the client rules: required, exact, one entry per client.
    const zedClient = () => zed().querySelector('.sc-client');
    zedClient().value = 'opencode';
    zedClient().dispatchEvent(new w.Event('input', { bubbles: true }));
    check('a duplicate client across cards names the earlier entry with the server wording',
      zed().dataset.scState === 'error' &&
      zed().querySelector('.sc-err').textContent === "duplicate client 'opencode' with entry 1; merge the entries or change one client");
    // The middle-duplicate ordinal (the first-occurrence registration): a
    // duplicate client whose card ALSO carries another error must not
    // re-register the client, or a later duplicate cites the wrong entry.
    gin().value = 'ba"d';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    scWrap.querySelector('[data-sc-add]').click();
    const scDupMid = cards()[2];
    scDupMid.querySelector('.sc-client').value = 'opencode';
    scDupMid.querySelector('.sc-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    scDupMid.querySelector('.prov-add .sc-param-in').value = 'k';
    scDupMid.querySelector('[data-sc-param-add]').click();
    check('a duplicate client carrying another error does not shift the cited ordinal',
      cards().length === 3 &&
      scDupMid.querySelector('.sc-err').textContent === "duplicate client 'opencode' with entry 1; merge the entries or change one client");
    scDupMid.querySelector('[data-sc-rm]').click();
    gin().value = 'threadId';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    zedClient().value = '';
    zedClient().dispatchEvent(new w.Event('input', { bubbles: true }));
    check('an emptied client names the required-client rule with the server wording',
      zed().querySelector('.sc-err').textContent === 'client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry');
    zedClient().value = 'zed-cli';
    zedClient().dispatchEvent(new w.Event('input', { bubbles: true }));
    check('the fully repaired draft is clean again',
      scWrap.querySelectorAll('.sc-card[data-sc-state="error"]').length === 0 && !w.settingsIsDirty());
    // the blocked-Apply drive: an invalid draft must stop the POST, name
    // the fix on the status line and focus the first offending row (the
    // ro save-gate precedent).
    gin().value = 'ba"d';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    const failuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid sub-conversations draft blocks the save and focuses the offending row',
      state.failures.length === failuresBefore &&
      d.getElementById('settings-count').textContent === 'sub-conversations - fix the highlighted entry first' &&
      d.activeElement === gin() && gin().classList.contains('prov-bad'));
    gin().value = 'threadId';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    // The hidden-offender drive (the W53 bug, the live-verified sc case):
    // with the sheet showing the providers category the sub-conversations
    // block is hidden, so the gate's focus() and scrollIntoView() were
    // silent no-ops on it - only the status line moved. The shared reveal
    // must select the offender's category through the rail machinery before
    // focusing the offending param.
    gin().value = 'ba"d';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    w.eval("settingsCat = 'providers'; showSettingsCat()");
    const scBlockRow = scWrap.closest('.st-row');
    check('switching the sheet to the providers category hides the sub-conversations block',
      scBlockRow.hidden && !d.querySelector('[data-st-cat="conversation"]').classList.contains('active'));
    const scHiddenFailures = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden sub-conversations offender selects its category and focuses it',
      state.failures.length === scHiddenFailures &&
        d.getElementById('settings-count').textContent === 'sub-conversations - fix the highlighted entry first' &&
        w.eval('settingsCat') === 'conversation' &&
        d.querySelector('[data-st-cat="conversation"]').classList.contains('active') &&
        d.querySelector('[data-st-cat="conversation"]').getAttribute('aria-current') === 'page' &&
        !scBlockRow.hidden &&
        d.activeElement === gin() && gin().classList.contains('prov-bad'));
    gin().value = 'threadId';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    // the entry add/remove lifecycle: a fresh card asks for its client,
    // then its param; a middle removal renumbers the survivors' visible
    // ordinals and their aria-labels (the one ordinal owner's restamp).
    scWrap.querySelector('[data-sc-add]').click();
    const fresh = cards()[2];
    check('a fresh entry card asks for its client and counts against the cap line',
      cards().length === 3 && fresh.dataset.scState === 'error' &&
      fresh.querySelector('.sc-err').textContent === 'client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry' &&
      scWrap.querySelector('.mr-count').textContent === '3 / 16 entries' &&
      d.activeElement === fresh.querySelector('.sc-client') &&
      fresh.querySelector('.sc-client').getAttribute('aria-label') === 'entry 3 client');
    fresh.querySelector('.sc-client').value = 'new-cli';
    fresh.querySelector('.sc-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    check('a named client with no param names the missing param',
      fresh.dataset.scState === 'error' &&
      fresh.querySelector('.sc-err').textContent === 'no param set - give the entry a request body field to track, or remove the entry');
    fresh.querySelector('.prov-add .sc-param-in').value = 'convKey';
    fresh.querySelector('[data-sc-param-add]').click();
    check('the completed entry joins the draft valid',
      fresh.dataset.scState !== 'error' && cards().length === 3);
    zed().querySelector('[data-sc-rm]').click();
    const midSurvivors = cards();
    check('a middle-card removal renumbers the survivors\' visible ordinals',
      midSurvivors.length === 2 &&
      [...scWrap.querySelectorAll('.sc-num')].map(n => n.textContent).join(',') === 'entry 1,entry 2');
    check('the renumbered survivors\' client labels carry the same ordinals',
      midSurvivors[0].querySelector('.sc-client').getAttribute('aria-label') === 'entry 1 client' &&
      midSurvivors[1].querySelector('.sc-client').getAttribute('aria-label') === 'entry 2 client');
    midSurvivors[1].querySelector('[data-sc-rm]').click();
    check('the tail removal leaves the seeded first entry alone',
      cards().length === 1 && cards()[0].querySelector('.sc-client').value === 'opencode');
    // rebuild the removed entry behind the survivor through the real
    // controls; the strip toggle rides the rebuild and must collect.
    scWrap.querySelector('[data-sc-add]').click();
    const rebuilt = cards()[1];
    rebuilt.querySelector('.sc-client').value = 'zed-cli';
    rebuilt.querySelector('.sc-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    rebuilt.querySelector('.prov-add .sc-param-in').value = 'threadId';
    rebuilt.querySelector('[data-sc-param-add]').click();
    rebuilt.querySelector('.sc-strip').click();
    check('the strip toggle collects into the entry and the rebuilt draft is the real edit',
      JSON.stringify(w.collectSettingsValues().sub_conversations) === JSON.stringify([
        { client: 'opencode', params: ['promptCacheKey', 'sessionKey'], strip: true },
        { client: 'zed-cli', params: ['threadId'], strip: true },
      ]) && w.settingsIsDirty());
    // the save path: the POST carries the strict shape with the loaded
    // revision (the settings save-flow stub discipline).
    const originalScFetch = w.fetch;
    const scPosts = [];
    w.fetch = (url, init) => {
      if (!String(url).includes('/admin/config')) return originalScFetch(url, init);
      return new Promise(resolve => {
        if (init?.method === 'POST') scPosts.push({ resolve, body: JSON.parse(init.body) });
        else resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(doc)) });
      });
    };
    const scSave = w.applySettings();
    check('the sub-conversations save posts the strict lossless shape with the loaded revision',
      scPosts.length === 1 && scPosts[0].body.revision === 'sc-r1' &&
      JSON.stringify(scPosts[0].body.values.sub_conversations) === JSON.stringify([
        { client: 'opencode', params: ['promptCacheKey', 'sessionKey'], strip: true },
        { client: 'zed-cli', params: ['threadId'], strip: true },
      ]));
    const savedSc = JSON.parse(JSON.stringify(scPosts[0].body.values.sub_conversations));
    scPosts.shift().resolve({ ok: true, statusText: 'OK', json: async () => ({ saved: true, revision: 'sc-r2', values: { ...doc.values, sub_conversations: savedSc }, effective: {}, restart_required: [] }) });
    await scSave;
    check('the accepted save adopts the new revision and a clean draft',
      w.eval('settingsDoc.revision') === 'sc-r2' && !w.settingsIsDirty() &&
      /saved/.test(d.getElementById('settings-count').textContent));
    w.fetch = originalScFetch;
    // the entry cap: 16 seeded entries saturate the count line and the
    // add gate refuses a 17th (mirrors config.SubConversationsMax).
    {
      const capDoc = JSON.parse(JSON.stringify(cfgDoc));
      capDoc.fields.push({ key: 'sub_conversations', category: 'conversation', label: 'Sub-conversation tracking', help: 'Per-client tracking of a request body field that carries a sub-conversation identity.', kind: 'sub_conversations', hot_reload: true });
      capDoc.categories.push({ id: 'conversation', label: 'Conversations', help: 'How the request log groups turns into conversations.' });
      capDoc.values.sub_conversations = Array.from({ length: 16 }, (_, i) => ({ client: 'cap-' + i, params: ['k'], strip: false }));
      w.__scCapDoc = capDoc;
      w.eval("settingsCat = 'conversation'; settingsDoc = window.__scCapDoc; fillSettingsForm(settingsDoc)");
      const capWrap = d.querySelector('#settings-fields [data-kind="sub_conversations"]');
      const capAdd = capWrap.querySelector('[data-sc-add]');
      check('sixteen seeded entries saturate the count line and disable the add button',
        capWrap.querySelectorAll('.sc-card').length === 16 &&
        capWrap.querySelector('.mr-count').textContent === '16 / 16 entries' && capAdd.disabled);
      capAdd.click();
      w.addScCard(capWrap);
      check('the entry add gate refuses beyond the server cap of 16',
        capWrap.querySelectorAll('.sc-card').length === 16);
      delete w.__scCapDoc;
    }
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
    delete w.__scDoc;
  }
};
