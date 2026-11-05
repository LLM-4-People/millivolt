// test 13: the shared menu foundation (js/menu.js). Exercises the exported
// lifecycle on fresh elements: open/close state, dismissal, anchor/sheet
// placement, roving focus and the choiceMenu builder. Every stub is restored
// and every created node removed before the section returns.
module.exports = async function menuFoundation(H) {
  const {check, d, fs, path, settleUntil, w} = H;
  const STATIC = H.STATIC;
  const created = [];
  const restores = [];

  // createButton/createPanel build throwaway surfaces tracked for cleanup.
  const createButton = (id, marked = true) => {
    const button = d.createElement('button');
    button.type = 'button';
    button.id = id;
    button.textContent = id;
    if (marked) button.setAttribute('aria-expanded', 'false');
    d.body.appendChild(button);
    created.push(button);
    return button;
  };
  const createPanel = (html = '') => {
    const panel = d.createElement('div');
    panel.className = 'mn';
    panel.hidden = true;
    panel.innerHTML = html;
    d.body.appendChild(panel);
    created.push(panel);
    return panel;
  };
  // stubValue replaces one property and returns the restore callback.
  const stubValue = (object, key, value) => {
    const own = Object.prototype.hasOwnProperty.call(object, key);
    const saved = object[key];
    Object.defineProperty(object, key, {configurable: true, writable: true, value});
    return () => {
      if (own) Object.defineProperty(object, key, {configurable: true, writable: true, value: saved});
      else delete object[key];
    };
  };
  // stubRect pins one element's measured box.
  const stubRect = (element, rect) => {
    Object.defineProperty(element, 'getBoundingClientRect', {configurable: true, value: () => rect});
    return () => { delete element.getBoundingClientRect; };
  };

  for (const name of ['menuPlacement', 'menuOpen', 'menuClose', 'menuToggle', 'menuIsOpen',
    'menuCloseAll', 'menuRoving', 'choiceMenu']) {
    check('menu foundation exports ' + name, typeof w[name] === 'function');
  }
  check('the wide fixture viewport opens anchored', w.menuPlacement() === 'anchor');

  // ---- toggle, aria-expanded, one open menu at a time ----
  {
    const triggerA = createButton('menu-a');
    const panelA = createPanel();
    const triggerB = createButton('menu-b');
    const panelB = createPanel();
    let closesA = 0, closesB = 0;
    check('a menu starts closed', !w.menuIsOpen(panelA) && panelA.hidden);
    w.menuToggle(triggerA, panelA, {onClose: () => { closesA++; }});
    check('menuToggle opens with aria-expanded and mn-on synced',
      w.menuIsOpen(panelA) && !panelA.hidden && panelA.classList.contains('mn-on') &&
      panelA.dataset.placement === 'anchor' && triggerA.getAttribute('aria-expanded') === 'true');
    w.menuToggle(triggerA, panelA, {});
    check('a second toggle closes and runs onClose exactly once',
      !w.menuIsOpen(panelA) && panelA.hidden && !panelA.classList.contains('mn-on') &&
      triggerA.getAttribute('aria-expanded') === 'false' && closesA === 1);
    w.menuOpen(triggerA, panelA, {onClose: () => { closesA++; }});
    w.menuOpen(triggerB, panelB, {onClose: () => { closesB++; }});
    check('opening a second menu closes the first one',
      panelA.hidden && !panelB.hidden && triggerA.getAttribute('aria-expanded') === 'false' &&
      triggerB.getAttribute('aria-expanded') === 'true' && closesA === 2 && closesB === 0);
    w.menuClose(panelB);
    check('menuClose closes the named panel and clears its expanded state',
      panelB.hidden && triggerB.getAttribute('aria-expanded') === 'false' && closesB === 1);
    const bare = createButton('menu-bare', false);
    w.menuOpen(bare, panelB, {});
    check('menuOpen never invents aria-expanded on an unmarked trigger', !bare.hasAttribute('aria-expanded'));
    w.menuClose(panelB);
  }

  // ---- dismissal: Escape, outside pointerdown, focus restoration ----
  {
    const trigger = createButton('menu-focus');
    const panel = createPanel(
      '<button type="button" class="mn-item" role="menuitem">one</button>' +
      '<button type="button" class="mn-item" role="menuitem">two</button>');
    const first = panel.querySelector('.mn-item');
    const escape = () => d.dispatchEvent(new w.KeyboardEvent('keydown', {key: 'Escape', bubbles: true, cancelable: true}));
    w.menuOpen(trigger, panel, {initialFocus: 'item'});
    check('item focus lands on the first enabled row', d.activeElement === first);
    escape();
    check('Escape closes and returns focus to the trigger when focus was inside',
      panel.hidden && d.activeElement === trigger);
    w.menuOpen(trigger, panel, {initialFocus: 'panel'});
    check('panel focus lazily adds tabindex=-1 and receives the panel',
      panel.getAttribute('tabindex') === '-1' && d.activeElement === panel);
    escape();
    check('Escape from the panel also returns focus to the trigger', panel.hidden && d.activeElement === trigger);
    const outside = createButton('menu-outside');
    outside.focus();
    w.menuOpen(trigger, panel, {initialFocus: 'none'});
    check('initialFocus none leaves focus where it was', d.activeElement === outside);
    trigger.focus();
    escape();
    check('Escape with the trigger focused keeps focus on the trigger', panel.hidden && d.activeElement === trigger);
    w.menuOpen(trigger, panel, {initialFocus: 'none'});
    outside.focus();
    escape();
    check('Escape with focus outside closes without stealing focus', panel.hidden && d.activeElement === outside);
    w.menuOpen(trigger, panel, {initialFocus: 'none'});
    outside.dispatchEvent(new w.Event('pointerdown', {bubbles: true, composed: true}));
    check('outside pointerdown closes without moving focus', panel.hidden && d.activeElement === outside);
    w.menuOpen(trigger, panel, {initialFocus: 'none'});
    trigger.dispatchEvent(new w.Event('pointerdown', {bubbles: true, composed: true}));
    check('pointerdown on the trigger keeps the menu open', !panel.hidden && d.activeElement === outside);
    // menuClose is the one focus owner: closing while focus is inside the
    // panel hands it back to the trigger (the row-activation path a pick
    // uses), never to the page body. A real browser blurs a focused
    // descendant when its ancestor becomes hidden; jsdom does not, so the
    // hide is instrumented to reproduce that order. Focus must be captured
    // before the panel hides, not after.
    const hiddenDesc = Object.getOwnPropertyDescriptor(w.HTMLElement.prototype, 'hidden');
    Object.defineProperty(panel, 'hidden', {
      configurable: true,
      get() { return hiddenDesc.get.call(panel); },
      set(v) {
        // Blur first: jsdom refuses to blur an element after its ancestor is
        // hidden, while a browser blurs it as part of that hide.
        if (v && panel.contains(d.activeElement)) d.activeElement.blur();
        hiddenDesc.set.call(panel, v);
      },
    });
    w.menuOpen(trigger, panel, {initialFocus: 'item'});
    w.menuClose(panel);
    check('closing while focus is inside returns it to the trigger',
      panel.hidden && d.activeElement === trigger);
    delete panel.hidden;
    w.menuClose(panel);
  }

  // ---- anchor placement, clamping and the sheet flip ----
  {
    const trigger = createButton('menu-place');
    const panel = createPanel();
    const triggerRect = {left: 100, right: 140, top: 100, bottom: 132, width: 40, height: 32};
    const panelRect = {left: 0, top: 0, right: 340, bottom: 100, width: 340, height: 100};
    restores.push(stubRect(trigger, triggerRect));
    restores.push(stubRect(panel, panelRect));
    restores.push(stubValue(w, 'innerWidth', 1024));
    restores.push(stubValue(w, 'innerHeight', 600));

    w.menuOpen(trigger, panel, {});
    check('anchor open sits 4px under the trigger with the room below clamped',
      panel.dataset.placement === 'anchor' && panel.style.top === '136px' &&
      panel.style.maxHeight === '456px' && panel.style.left === '8px');
    triggerRect.top = 500;
    triggerRect.bottom = 532;
    panelRect.height = 200;
    w.menuOpen(trigger, panel, {});
    check('anchor open flips above when the space below cannot hold the panel',
      panel.style.top === '296px' && panel.style.maxHeight === '488px');
    triggerRect.left = 980;
    triggerRect.right = 1024;
    triggerRect.top = 100;
    triggerRect.bottom = 132;
    panelRect.height = 100;
    w.menuOpen(trigger, panel, {});
    check('anchor end alignment clamps to 8px from the right edge', panel.style.left === '676px');
    triggerRect.left = 700;
    triggerRect.right = 740;
    w.menuOpen(trigger, panel, {align: 'start'});
    check('anchor start alignment clamps to 8px from the right edge', panel.style.left === '676px');
    triggerRect.left = 0;
    triggerRect.right = 40;
    w.menuOpen(trigger, panel, {align: 'start'});
    check('anchor start alignment clamps to 8px from the left edge', panel.style.left === '8px');
    w.menuClose(panel);

    let narrow = false;
    const media = {
      get matches() { return narrow; },
      addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {},
    };
    restores.push(stubValue(w, 'matchMedia', () => media));
    triggerRect.left = 100;
    triggerRect.right = 140;
    w.menuOpen(trigger, panel, {});
    check('a wide viewport opens anchored', panel.dataset.placement === 'anchor');
    narrow = true;
    w.dispatchEvent(new w.Event('resize'));
    check('a resize onto the narrow breakpoint flips the open panel to a sheet',
      await settleUntil(() => panel.dataset.placement === 'sheet'));
    const scrim = d.querySelector('.mn-scrim');
    check('sheet mode clears the anchor geometry and shows the scrim',
      panel.style.left === '' && panel.style.top === '' && panel.style.maxHeight === '' &&
      !!scrim && !scrim.hidden && scrim.getAttribute('aria-hidden') === 'true' && scrim.tabIndex === -1);
    w.menuOpen(trigger, panel, {});
    check('a sheet open is bottom-anchored by CSS, not inline coordinates',
      panel.dataset.placement === 'sheet' && panel.style.left === '' && panel.style.top === '');
    narrow = false;
    w.dispatchEvent(new w.Event('resize'));
    check('a resize back to the wide viewport restores the anchor placement',
      await settleUntil(() => panel.dataset.placement === 'anchor' && panel.style.top === '136px'));
    check('the scrim is hidden again outside sheet mode', !!scrim && scrim.hidden);
    w.menuClose(panel);
  }

  // ---- roving focus ----
  {
    const trigger = createButton('menu-roving');
    const panel = createPanel();
    const clicks = [];
    const rows = ['r0', 'r1', 'r2', 'r3', 'r4', 'r5'].map(id => {
      const row = d.createElement('button');
      row.type = 'button';
      row.className = 'mn-item';
      row.setAttribute('role', 'menuitem');
      row.textContent = id;
      row.addEventListener('click', () => clicks.push(id));
      panel.appendChild(row);
      return row;
    });
    const [r0, r1, r2, r3, r4, r5] = rows;
    r1.disabled = true;
    r2.setAttribute('aria-disabled', 'true');
    r3.hidden = true;
    const key = name => d.activeElement.dispatchEvent(
      new w.KeyboardEvent('keydown', {key: name, bubbles: true, cancelable: true}));
    w.menuOpen(trigger, panel, {roving: true});
    check('roving open focuses the first enabled row with one tab stop',
      d.activeElement === r0 && r0.tabIndex === 0 && r4.tabIndex === -1 && r5.tabIndex === -1);
    key('ArrowDown');
    check('ArrowDown skips disabled, hidden and aria-disabled rows', d.activeElement === r4);
    key('ArrowDown');
    check('ArrowDown advances to the next enabled row', d.activeElement === r5);
    key('ArrowDown');
    check('ArrowDown wraps to the start', d.activeElement === r0);
    key('ArrowUp');
    check('ArrowUp wraps to the end', d.activeElement === r5);
    key('Home');
    check('Home jumps to the first enabled row', d.activeElement === r0);
    key('End');
    check('End jumps to the last enabled row', d.activeElement === r5);
    key('ArrowDown');
    key('Enter');
    check('Enter activates the focused row', clicks.join() === 'r0');
    key(' ');
    check('Space activates the focused row', clicks.join() === 'r0,r0');
    d.activeElement.dispatchEvent(new w.KeyboardEvent('keydown',
      {key: 'ArrowDown', altKey: true, bubbles: true, cancelable: true}));
    check('modified arrows stay with the browser', d.activeElement === r0);
    check('exactly one roving row owns the tab stop',
      [r0, r4, r5].filter(row => row.tabIndex === 0).length === 1);
    w.menuClose(panel);
  }

  // ---- choiceMenu ----
  {
    const trigger = createButton('menu-choice', false);
    const label = d.createElement('span');
    label.textContent = 'alpha';
    trigger.appendChild(label);
    const picked = [];
    const items = [
      {id: 'a', label: 'alpha'},
      {id: 'b', label: 'beta', group: 'shared'},
      {id: 'c', label: 'gamma', group: 'shared'},
      {id: 'd', label: 'delta', disabled: true},
    ];
    const choice = w.choiceMenu(trigger, {items, value: 'a', label, title: 'Pick one', onPick: id => picked.push(id)});
    check('choiceMenu syncs aria-expanded and never invents aria-haspopup',
      trigger.getAttribute('aria-expanded') === 'false' && !trigger.hasAttribute('aria-haspopup'));
    check('choiceMenu builds its panel lazily on first open', !d.querySelector('.mn-menu'));
    trigger.click();
    const menu = d.querySelector('.mn-menu');
    check('first open builds the mn menu panel and the expanded state',
      !!menu && menu.getAttribute('role') === 'menu' && menu.classList.contains('mn') &&
      menu.classList.contains('mn-menu') && !menu.hidden && trigger.getAttribute('aria-expanded') === 'true');
    check('the panel head carries the title',
      !!menu.querySelector('.mn-title') && menu.querySelector('.mn-title').textContent === 'Pick one');
    const rowA = menu.querySelector('[data-mn-id="a"]');
    const rowB = menu.querySelector('[data-mn-id="b"]');
    const rowC = menu.querySelector('[data-mn-id="c"]');
    const rowD = menu.querySelector('[data-mn-id="d"]');
    check('rows are menuitemradio buttons with a check slot',
      [rowA, rowB, rowC, rowD].every(row => row && row.tagName === 'BUTTON' &&
        row.getAttribute('role') === 'menuitemradio' && !!row.querySelector('.mn-check')));
    check('groups render as one section with its heading',
      menu.querySelectorAll('.mn-sec').length === 1 &&
      menu.querySelector('.mn-sec-h').textContent === 'shared' &&
      menu.querySelector('.mn-sec').contains(rowB) && menu.querySelector('.mn-sec').contains(rowC));
    check('the stored value renders checked with its glyph, the rest unchecked',
      rowA.getAttribute('aria-checked') === 'true' && rowA.querySelector('.mn-check').textContent === '✓' &&
      rowB.getAttribute('aria-checked') === 'false' && rowB.querySelector('.mn-check').textContent === '');
    check('disabled items render disabled', rowD.disabled === true);
    check('the selected row owns the keyboard entry', d.activeElement === rowA);
    const key = name => d.activeElement.dispatchEvent(
      new w.KeyboardEvent('keydown', {key: name, bubbles: true, cancelable: true}));
    key('ArrowDown');
    check('choice roving lands on the next enabled row', d.activeElement === rowB);
    key('ArrowDown');
    check('choice roving reaches the last enabled row', d.activeElement === rowC);
    key('ArrowDown');
    check('choice roving wraps past the disabled item', d.activeElement === rowA);
    rowC.dispatchEvent(new w.MouseEvent('click', {bubbles: true, cancelable: true}));
    check('clicking a row picks it and calls onPick with the id', picked.join() === 'c');
    check('picking updates aria-checked, the check glyph and the label element',
      rowC.getAttribute('aria-checked') === 'true' && rowC.querySelector('.mn-check').textContent === '✓' &&
      rowA.getAttribute('aria-checked') === 'false' && label.textContent === 'gamma');
    check('picking closes the menu and clears aria-expanded',
      menu.hidden && trigger.getAttribute('aria-expanded') === 'false');
    trigger.click();
    check('reopening focuses the selected row', d.activeElement === rowC);
    rowB.focus();
    key('Enter');
    check('Enter picks like a click', picked.join() === 'c,b' && label.textContent === 'beta');
    trigger.click();
    const beforeDisabled = picked.length;
    rowD.dispatchEvent(new w.MouseEvent('click', {bubbles: true, cancelable: true}));
    check('disabled items are not selectable',
      picked.length === beforeDisabled && rowD.getAttribute('aria-checked') === 'false' && !menu.hidden);
    w.menuClose(menu);

    const keepTrigger = createButton('menu-keep');
    const keepPicked = [];
    const keep = w.choiceMenu(keepTrigger,
      {items: [{id: 'x', label: 'x'}, {id: 'y', label: 'y'}], value: 'x', keepOpen: true, onPick: id => keepPicked.push(id)});
    keepTrigger.click();
    const keepMenu = [...d.querySelectorAll('.mn-menu')].find(node => node !== menu);
    const rowY = keepMenu.querySelector('[data-mn-id="y"]');
    rowY.dispatchEvent(new w.MouseEvent('click', {bubbles: true, cancelable: true}));
    check('keepOpen keeps the menu open after a pick',
      keepPicked.join() === 'y' && !keepMenu.hidden && rowY.getAttribute('aria-checked') === 'true');
    keep.close();

    trigger.click();
    const firstRow = menu.querySelector('.mn-item');
    choice.sync({items: items.map(item => ({...item}))});
    check('sync with the same item set keeps the built rows',
      menu.querySelector('.mn-item') === firstRow && menu.querySelectorAll('.mn-item').length === 4);
    choice.sync({items: items.map(item => ({...item})).concat([{id: 'e', label: 'epsilon'}])});
    check('sync with a changed item set rebuilds the rows',
      menu.querySelector('.mn-item') !== firstRow && menu.querySelectorAll('.mn-item').length === 5 &&
      !!menu.querySelector('[data-mn-id="e"]'));
    choice.sync({value: 'e'});
    check('sync updates the selection and the label element in place',
      menu.querySelector('[data-mn-id="e"]').getAttribute('aria-checked') === 'true' &&
      menu.querySelector('[data-mn-id="b"]').getAttribute('aria-checked') === 'false' &&
      label.textContent === 'epsilon');
    w.menuClose(menu);
  }

  // ---- source contract and listener hygiene ----
  {
    const source = fs.readFileSync(path.join(STATIC, 'js', 'menu.js'), 'utf8');
    const css = fs.readFileSync(path.join(STATIC, 'css', 'dashboard.css'), 'utf8');
    const styled = new Set([...css.matchAll(/\.(mn[a-z-]*)\b/g)].map(match => match[1]));
    const required = ['mn', 'mn-on', 'mn-hd', 'mn-title', 'mn-sub', 'mn-body', 'mn-sec', 'mn-sec-h',
      'mn-item', 'mn-check', 'mn-chips', 'mn-chip', 'mn-note', 'mn-ft', 'mn-actions', 'mn-rail',
      'mn-rail-fill', 'mn-scrim'];
    check('menu.js declares every shared .mn-* class literally',
      required.every(name => source.includes('.' + name) && styled.has(name)));
    w.menuCloseAll();
    check('menuCloseAll with nothing open is a no-op',
      w.menuIsOpen(d.createElement('div')) === false && d.querySelectorAll('.mn.mn-on').length === 0);

    const trigger = createButton('menu-spy');
    const panel = createPanel();
    const docAdded = [], docRemoved = [], winAdded = [], winRemoved = [];
    const wrap = (object, added, removed) => {
      const hadAdd = Object.prototype.hasOwnProperty.call(object, 'addEventListener');
      const hadRemove = Object.prototype.hasOwnProperty.call(object, 'removeEventListener');
      const savedAdd = object.addEventListener, savedRemove = object.removeEventListener;
      object.addEventListener = function (type, listener, ...rest) {
        added.push(type + ':' + ((listener && listener.name) || ''));
        return savedAdd.call(this, type, listener, ...rest);
      };
      object.removeEventListener = function (type, listener, ...rest) {
        removed.push(type + ':' + ((listener && listener.name) || ''));
        return savedRemove.call(this, type, listener, ...rest);
      };
      return () => {
        if (hadAdd) object.addEventListener = savedAdd; else delete object.addEventListener;
        if (hadRemove) object.removeEventListener = savedRemove; else delete object.removeEventListener;
      };
    };
    const unwrapDoc = wrap(d, docAdded, docRemoved);
    const unwrapWin = wrap(w, winAdded, winRemoved);
    w.menuOpen(trigger, panel, {});
    w.menuClose(panel);
    unwrapDoc();
    unwrapWin();
    const added = [...docAdded, ...winAdded];
    const removed = [...docRemoved, ...winRemoved];
    check('an open/close cycle adds and removes the same four listeners',
      added.length === 4 && removed.length === 4 && added.every(name => removed.includes(name)));
    d.dispatchEvent(new w.KeyboardEvent('keydown', {key: 'Escape', bubbles: true, cancelable: true}));
    check('the closed menu leaves no Escape handler behind', panel.hidden);
  }

  // Cleanup: close anything left open, drop every created node and stub.
  w.menuCloseAll();
  for (const node of created) node.remove();
  for (const panel of d.querySelectorAll('.mn-menu')) panel.remove();
  for (const restore of restores.reverse()) restore();
};
